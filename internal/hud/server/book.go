package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// The book (GET /book) is read on demand, never watched: Kubernetes Events
// on WorkJobs in the viewer's namespaces, on CapacityRequests and on
// Nodes; the queue's decisions, yields and the jobs' start and finish from
// WorkJob status; and the CapacityRequests themselves. Controls taken
// through the HUD are Events whose annotations carry the actor, the book
// kind and the control record, so their entries name the record.

// Annotations on the HUD's control Events.
const (
	annoActor    = "c4.avalanche.io/actor"
	annoBookKind = "c4.avalanche.io/book-kind"
	annoRecord   = "c4.avalanche.io/record"
)

const (
	bookDefault = 100
	bookMax     = 1000
)

type bookItem struct {
	e       api.BookEntry
	project string // empty: visible to every viewer
}

func (s *Server) handleBook(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	limit := bookDefault
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			fail(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, bookMax)
	}
	var only *jobRec
	if key := r.URL.Query().Get("job"); key != "" {
		only = m.byKey[key]
		if only == nil || !v.Sees(only.project) {
			fail(w, http.StatusNotFound, "no job %s", key)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.book(ctx, m, v, only, limit))
}

// book gathers the viewer's entries, newest last, keeping the newest limit.
func (s *Server) book(ctx context.Context, m *model, v *api.Viewer, only *jobRec, limit int) api.Book {
	var items []bookItem
	add := func(it bookItem) {
		if it.project != "" && !v.Sees(it.project) {
			return
		}
		if only != nil && it.e.Object.Job != only.key {
			return
		}
		items = append(items, it)
	}

	// Status: the queue's decisions and yields, and each job's run.
	for _, rec := range m.jobs {
		if !v.Sees(rec.project) || (only != nil && rec != only) {
			continue
		}
		for _, it := range s.statusEntries(rec) {
			add(it)
		}
	}
	// CapacityRequests, visible when one of their jobs is.
	crProject := map[string]string{}
	crJob := map[string]string{}
	for _, cr := range m.crs {
		proj, job := "", ""
		for _, k := range cr.Spec.Jobs {
			if jr := m.byKey[k]; jr != nil {
				if proj == "" || v.Sees(jr.project) {
					proj = jr.project
				}
			}
		}
		if len(cr.Spec.Jobs) == 1 {
			job = cr.Spec.Jobs[0]
		}
		crProject[cr.Name], crJob[cr.Name] = proj, job
		if proj == "" && len(cr.Spec.Jobs) > 0 {
			continue // its jobs are gone: nothing to scope it by
		}
		msg := fmt.Sprintf("+%d %s slots requested: %s", cr.Spec.Slots, cr.Spec.Pool, cr.Spec.Reason)
		if cr.Spec.EstimatedCost != "" {
			msg += fmt.Sprintf(" (est. $%s)", cr.Spec.EstimatedCost)
		}
		add(bookItem{project: proj, e: api.BookEntry{At: cr.CreationTimestamp.Time.UTC(), Actor: "component/queue", Signer: api.SignerQueue,
			Kind: "capacity-requested", Object: api.BookObject{Type: "capacity", Pool: cr.Spec.Pool, Job: job}, Message: msg}})
	}

	// Events, field-selected by the kind (and name) of the involved object.
	if s.Kube != nil {
		nsSeen := map[string]bool{}
		for _, p := range m.projects {
			if !v.Sees(p.Name) {
				continue
			}
			for _, ns := range p.Spec.Namespaces {
				if nsSeen[ns] || (only != nil && ns != only.job.Namespace) {
					continue
				}
				nsSeen[ns] = true
				name := ""
				if only != nil {
					name = only.job.Name
				}
				for _, ev := range s.events(ctx, ns, "WorkJob", name) {
					rec := m.byKey[ns+"/"+ev.InvolvedObject.Name]
					proj := m.nsProject[ns]
					if rec != nil {
						proj = rec.project
					}
					add(bookItem{project: proj, e: s.eventEntry(ev, api.BookObject{Type: "job", Job: ns + "/" + ev.InvolvedObject.Name})})
				}
			}
		}
		for _, ns := range uniq(s.Namespace, metav1.NamespaceDefault) {
			for _, ev := range s.events(ctx, ns, "CapacityRequest", "") {
				name := ev.InvolvedObject.Name
				proj, ok := crProject[name]
				if !ok || (proj == "" && only != nil) {
					continue
				}
				pool := ""
				for _, cr := range m.crs {
					if cr.Name == name {
						pool = cr.Spec.Pool
					}
				}
				add(bookItem{project: proj, e: s.eventEntry(ev, api.BookObject{Type: "capacity", Pool: pool, Job: crJob[name]})})
			}
			if only != nil {
				continue
			}
			for _, ev := range s.events(ctx, ns, "Node", "") {
				add(bookItem{e: s.eventEntry(ev, api.BookObject{Type: "node", Node: ev.InvolvedObject.Name})})
			}
		}
	}

	sort.SliceStable(items, func(a, b int) bool { return items[a].e.At.Before(items[b].e.At) })
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	out := api.Book{Entries: make([]api.BookEntry, 0, len(items))}
	var last int64
	for _, it := range items {
		// seq follows time in microseconds, so it is stable from one
		// request to the next and still orders entries that share a time.
		seq := it.e.At.UnixMicro()
		if seq <= last {
			seq = last + 1
		}
		last = seq
		it.e.Seq = seq
		out.Entries = append(out.Entries, it.e)
	}
	return out
}

// statusEntries are the entries a job's status holds: its start and
// finish, and the queue's decisions and the yields they led to.
func (s *Server) statusEntries(rec *jobRec) []bookItem {
	j := &rec.job
	st := &j.Status
	job := api.BookObject{Type: "job", Job: rec.key}
	var out []bookItem
	item := func(at time.Time, actor, signer, kind string, obj api.BookObject, record, msg string) {
		out = append(out, bookItem{project: rec.project, e: api.BookEntry{At: at.UTC(), Actor: actor, Signer: signer, Kind: kind,
			Object: obj, Message: msg, Record: record}})
	}
	if st.Started != nil {
		item(st.Started.Time, "component/queue", api.SignerQueue, "started", job, "", fmt.Sprintf("%s started: units released in %s order", j.Name, orderName(st.Order)))
	}
	if st.Finished != nil {
		record, how := "", strings.ToLower(st.Phase)
		if st.Output != nil {
			record = st.Output.Record
		}
		if how == "" {
			how = "finished"
		}
		item(st.Finished.Time, "component/queue", api.SignerQueue, "finished", job, record, fmt.Sprintf("%s %s", j.Name, how))
	}
	for _, d := range st.Decisions {
		u := int(d.Unit)
		msg := fmt.Sprintf("unit %d: %s", u, d.Reason)
		if d.For != "" {
			msg += "; slots go to " + d.For
		}
		item(d.At.Time, "component/queue", api.SignerQueue, d.Kind, api.BookObject{Type: "unit", Job: rec.key, Unit: &u}, d.Record, msg)
	}
	for _, y := range st.Yields {
		u := int(y.Unit)
		msg := fmt.Sprintf("unit %d yielded at a checkpoint: %s", u, y.Reason)
		if y.View != "" {
			msg += "; saved view " + shortID(y.View)
		}
		item(y.At.Time, "component/queue", api.SignerQueue, "yielded", api.BookObject{Type: "unit", Job: rec.key, Unit: &u}, "", msg)
	}
	return out
}

func orderName(o string) string {
	if o == "" {
		return "dispatch"
	}
	return o
}

// events lists the Events in a namespace about objects of a kind (and a
// name), field-selected on the server and checked again here.
func (s *Server) events(ctx context.Context, ns, kind, name string) []corev1.Event {
	sel := "involvedObject.kind=" + kind
	if name != "" {
		sel += ",involvedObject.name=" + name
	}
	list, err := s.Kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		s.Log.V(1).Info("book: list events", "namespace", ns, "kind", kind, "error", err.Error())
		return nil
	}
	out := list.Items[:0]
	for _, ev := range list.Items {
		if ev.InvolvedObject.Kind == kind && (name == "" || ev.InvolvedObject.Name == name) {
			out = append(out, ev)
		}
	}
	return out
}

// eventEntry turns an Event into a book entry. The HUD's own control
// Events name their actor, kind and record in annotations; other Events
// are attributed to their source.
func (s *Server) eventEntry(ev corev1.Event, obj api.BookObject) api.BookEntry {
	at := ev.LastTimestamp.Time
	if at.IsZero() {
		at = ev.EventTime.Time
	}
	if at.IsZero() {
		at = ev.CreationTimestamp.Time
	}
	e := api.BookEntry{At: at.UTC(), Object: obj, Message: ev.Message, Kind: kebab(ev.Reason)}
	comp := ev.Source.Component
	if comp == "" {
		comp = ev.ReportingController
	}
	switch {
	case comp == "c4-hud" || ev.Annotations[annoActor] != "":
		e.Signer = api.SignerHUD
		e.Actor = ev.Annotations[annoActor]
		if e.Actor == "" {
			e.Actor = "component/hud"
		}
		if k := ev.Annotations[annoBookKind]; k != "" {
			e.Kind = k
		}
		e.Record = ev.Annotations[annoRecord]
	case comp == "kubelet":
		e.Signer = api.SignerNode
		host := ev.Source.Host
		if host == "" && obj.Type == "node" {
			host = obj.Node
		}
		e.Actor = "node/" + host
	case strings.Contains(comp, "queue"):
		e.Signer, e.Actor = api.SignerQueue, "component/queue"
	default:
		e.Signer = api.SignerController
		e.Actor = "component/" + strings.TrimPrefix(comp, "c4-")
		if comp == "" {
			e.Actor = "component/controller"
		}
	}
	if e.Kind == "" {
		e.Kind = "event"
	}
	return e
}

// kebab turns an Event reason (NodeNotReady) into a book kind
// (node-not-ready).
func kebab(reason string) string {
	var b strings.Builder
	rs := []rune(reason)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]))) {
				b.WriteByte('-')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

func uniq(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" && !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// bookNote is what a control's Event carries for the book.
type bookNote struct {
	actor, kind, record string
}

// actorOf names who made a control for the book: the viewer, or an agent
// acting for the viewer when the request came through MCP. The agent's
// name is the client's claim (X-C4-Agent); the viewer is authenticated.
func actorOf(r *http.Request, v *api.Viewer) string {
	if via(r) != "mcp" {
		return v.User
	}
	agent := r.Header.Get("X-C4-Agent")
	if agent == "" {
		agent = "mcp"
	}
	return "agent/" + agent + " for " + v.User
}
