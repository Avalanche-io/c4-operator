package queue

// The transition log (integrations GAPS 5): every change of a job's
// status the queue writes, numbered farm-wide in the order the queue
// wrote them, kept durably, so a client that was away (c4-sync after an
// outage) reads what it missed, in order, from GET /book?since=<seq>.
//
// The queue is the one writer: it is leader-elected and carries out every
// job control. Each tick it diffs each job's status before and after and
// appends a transition per change, then writes the log, then the statuses,
// so a change is never in status without being in the log. A crash
// between the two re-records a change with a new number and the same Key.
//
// The log follows the calibrations' pattern (persist.go): the ConfigMap
// bookConfigMap holds the next number and each project's tail; a full tail
// is sealed as an object in the project's own store, linked to the one
// before by its C4 ID (a hash chain), and the ConfigMap lists the latest
// bookSegments of them. A project's transitions never go to another
// project's store.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

const (
	// BookConfigMap holds the log's head, in the operator's namespace.
	BookConfigMap = "c4-work-book"
	// bookSeal is how many transitions a project's tail holds before it
	// is sealed into its store; bookSegments how many sealed segments the
	// head lists per project; bookTailMax the most a tail keeps while its
	// store cannot be reached.
	bookTailMax = 1024
	bookKind    = "c4.avalanche.io/work-book-segment"
)

var (
	bookSeal     = 128
	bookSegments = 64
)

// BookSegment is one sealed run of a project's transitions, as the head
// lists it.
type BookSegment struct {
	First int64  `json:"first"`
	Last  int64  `json:"last"`
	ID    string `json:"id"`
}

// BookSegmentObject is a sealed segment as stored: Prev is the C4 ID of
// the project's segment before it.
type BookSegmentObject struct {
	Kind    string           `json:"kind"`
	Project string           `json:"project"`
	First   int64            `json:"first"`
	Last    int64            `json:"last"`
	Prev    string           `json:"prev,omitempty"`
	Entries []api.Transition `json:"entries"`
}

// BookHead is the log's head as the ConfigMap holds it.
type BookHead struct {
	// Next is the number the next transition gets.
	Next     int64
	Tails    map[string][]api.Transition
	Segments map[string][]BookSegment
	// Dropped says a project's oldest segments left the head: a cursor
	// older than its first listed transition has missed some.
	Dropped map[string]bool
}

// Book key names in the ConfigMap. A project's name is a DNS name, which
// has no "_"; the synthetic project of a namespace no Project claims
// ("namespace/<ns>") has a "/", which a key may not: it is written "_".
func bookTailKey(p string) string { return "tail." + keyOf(p) }
func bookSegKey(p string) string  { return "segments." + keyOf(p) }
func bookDropKey(p string) string { return "dropped." + keyOf(p) }
func keyOf(p string) string       { return strings.ReplaceAll(p, "/", "_") }
func projectOf(k string) string   { return strings.ReplaceAll(k, "_", "/") }

// ReadBookHead reads the head from the ConfigMap's data.
func ReadBookHead(data map[string]string) (*BookHead, error) {
	h := &BookHead{Next: 1, Tails: map[string][]api.Transition{}, Segments: map[string][]BookSegment{}, Dropped: map[string]bool{}}
	if s := data["next"]; s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("book head next %q: %w", s, err)
		}
		h.Next = n
	}
	for k, v := range data {
		switch {
		case strings.HasPrefix(k, "tail."):
			var es []api.Transition
			if err := json.Unmarshal([]byte(v), &es); err != nil {
				return nil, fmt.Errorf("book %s: %w", k, err)
			}
			h.Tails[projectOf(strings.TrimPrefix(k, "tail."))] = es
		case strings.HasPrefix(k, "segments."):
			var ss []BookSegment
			if err := json.Unmarshal([]byte(v), &ss); err != nil {
				return nil, fmt.Errorf("book %s: %w", k, err)
			}
			h.Segments[projectOf(strings.TrimPrefix(k, "segments."))] = ss
		case strings.HasPrefix(k, "dropped."):
			h.Dropped[projectOf(strings.TrimPrefix(k, "dropped."))] = v == "true"
		}
	}
	return h, nil
}

func (h *BookHead) data() map[string]string {
	d := map[string]string{"next": strconv.FormatInt(h.Next, 10)}
	for p, es := range h.Tails {
		b, _ := json.Marshal(es)
		d[bookTailKey(p)] = string(b)
	}
	for p, ss := range h.Segments {
		b, _ := json.Marshal(ss)
		d[bookSegKey(p)] = string(b)
	}
	for p, dropped := range h.Dropped {
		if dropped {
			d[bookDropKey(p)] = "true"
		}
	}
	return d
}

// Oldest is the first transition a project's head still reaches.
func (h *BookHead) Oldest(p string) int64 {
	if ss := h.Segments[p]; len(ss) > 0 {
		return ss[0].First
	}
	if ts := h.Tails[p]; len(ts) > 0 {
		return ts[0].Seq
	}
	return h.Next
}

// bookLog is the queue's copy of the head and the ConfigMap's version it
// last wrote, so a write from a stale copy (another leader's) fails.
type bookLog struct {
	head *BookHead
	cm   *corev1.ConfigMap
}

// loadBook reads the head once, as the leader starts.
func (q *Queue) loadBook(ctx context.Context) error {
	if q.tlog != nil {
		return nil
	}
	var cm corev1.ConfigMap
	err := q.Client.Get(ctx, client.ObjectKey{Namespace: q.Namespace, Name: BookConfigMap}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		q.tlog = &bookLog{head: &BookHead{Next: 1, Tails: map[string][]api.Transition{}, Segments: map[string][]BookSegment{}, Dropped: map[string]bool{}}}
		return nil
	case err != nil:
		return err
	}
	h, err := ReadBookHead(cm.Data)
	if err != nil {
		return err
	}
	q.tlog = &bookLog{head: h, cm: &cm}
	return nil
}

// appendBook numbers ts, adds them to their projects' tails, seals full
// tails into their stores, and writes the head. On an error nothing is
// kept: the caller writes no status, and the same changes are found again
// next tick.
func (q *Queue) appendBook(ctx context.Context, ts []api.Transition) error {
	if len(ts) == 0 || q.Namespace == "" {
		return nil // no operator namespace (a queue in a test): no log
	}
	if err := q.loadBook(ctx); err != nil {
		return err
	}
	b := q.tlog
	h := cloneHead(b.head)
	for i := range ts {
		ts[i].Seq = h.Next
		h.Next++
		h.Tails[ts[i].Project] = append(h.Tails[ts[i].Project], ts[i])
	}
	for p, tail := range h.Tails {
		if len(tail) < bookSeal {
			continue
		}
		if err := q.sealBook(ctx, h, p); err != nil {
			q.Log.Error(err, "seal the transition log", "project", p)
			if len(tail) > bookTailMax {
				// The store has been away a long time: the oldest go, and
				// the head says so.
				h.Tails[p] = tail[len(tail)-bookTailMax:]
				h.Dropped[p] = true
			}
		}
	}
	data := h.data()
	if b.cm == nil {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: q.Namespace, Name: BookConfigMap,
			Labels: map[string]string{"app.kubernetes.io/name": "c4-operator", "app.kubernetes.io/component": "queue"}}, Data: data}
		if err := q.Client.Create(ctx, cm); err != nil {
			q.tlog = nil // read it again: another writer made it
			return err
		}
		b.cm = cm
	} else {
		cm := b.cm.DeepCopy()
		cm.Data = data
		if err := q.Client.Update(ctx, cm); err != nil {
			// Another leader wrote it, or the version moved: read it again.
			q.tlog = nil
			return err
		}
		b.cm = cm
	}
	b.head = h
	return nil
}

// sealBook stores a project's tail as a segment and lists it in the head.
func (q *Queue) sealBook(ctx context.Context, h *BookHead, p string) error {
	if q.Stores == nil {
		return fmt.Errorf("no store")
	}
	st, err := q.Stores(ctx, p)
	if err != nil || st == nil {
		return fmt.Errorf("project %s store: %v", p, err)
	}
	tail := h.Tails[p]
	seg := BookSegmentObject{Kind: bookKind, Project: p, First: tail[0].Seq, Last: tail[len(tail)-1].Seq, Entries: tail}
	if ss := h.Segments[p]; len(ss) > 0 {
		seg.Prev = ss[len(ss)-1].ID
	}
	raw, err := json.Marshal(seg)
	if err != nil {
		return err
	}
	id, err := st.PutBytes(ctx, raw)
	if err != nil {
		return err
	}
	ss := append(h.Segments[p], BookSegment{First: seg.First, Last: seg.Last, ID: id.String()})
	if len(ss) > bookSegments {
		ss = ss[len(ss)-bookSegments:]
		h.Dropped[p] = true
	}
	h.Segments[p] = ss
	h.Tails[p] = nil
	return nil
}

func cloneHead(h *BookHead) *BookHead {
	c := &BookHead{Next: h.Next, Tails: map[string][]api.Transition{}, Segments: map[string][]BookSegment{}, Dropped: map[string]bool{}}
	for p, ts := range h.Tails {
		c.Tails[p] = append([]api.Transition(nil), ts...)
	}
	for p, ss := range h.Segments {
		c.Segments[p] = append([]BookSegment(nil), ss...)
	}
	for p, d := range h.Dropped {
		c.Dropped[p] = d
	}
	return c
}

// transitions are the changes between a job's status before and after,
// in a stable order.
func transitions(job, project string, before, after *c4v1.WorkJobStatus, now time.Time) []api.Transition {
	var out []api.Transition
	add := func(kind string, unit *int32, at time.Time, actor, do, record, msg, id string) {
		t := api.Transition{At: at.UTC(), Project: project, Job: job, Kind: kind, Actor: actor, Do: do, Record: record, Message: msg}
		k := job + "/" + kind
		if unit != nil {
			u := int(*unit)
			t.Unit = &u
			k += "/" + strconv.Itoa(u)
		}
		t.Key = k + "/" + id
		if t.At.IsZero() {
			t.At = now.UTC()
		}
		out = append(out, t)
	}
	ts := func(m *metav1.Time) time.Time {
		if m == nil {
			return time.Time{}
		}
		return m.Time
	}
	if before.Started == nil && after.Started != nil {
		add("started", nil, after.Started.Time, "", "", "", "started", after.Started.UTC().Format(time.RFC3339))
	}
	if before.Phase != after.Phase && after.Phase != "" {
		add("phase", nil, now, "", after.Phase, "", fmt.Sprintf("%s → %s", or(before.Phase, "new"), after.Phase), before.Phase+">"+after.Phase+"@"+now.UTC().Format(time.RFC3339Nano))
	}
	if before.Finished == nil && after.Finished != nil {
		rec := ""
		if after.Output != nil {
			rec = after.Output.Record
		}
		add("finished", nil, after.Finished.Time, "", after.Phase, rec, "finished "+after.Phase, after.Finished.UTC().Format(time.RFC3339))
	}
	oldWait := map[string]c4v1.WorkWait{}
	for _, w := range before.Waits {
		oldWait[fmt.Sprintf("%d/%s", w.Unit, w.Name)] = w
	}
	for _, w := range after.Waits {
		o, had := oldWait[fmt.Sprintf("%d/%s", w.Unit, w.Name)]
		u := w.Unit
		if w.Since != nil && (!had || o.Since == nil || !o.Since.Equal(w.Since)) {
			what := "waits for " + w.Server
			if w.Escalated {
				what = "escalated to " + w.Server
			}
			add("wait-opened", &u, w.Since.Time, "", "", "", what, w.Since.UTC().Format(time.RFC3339))
		}
		if w.Do != "" && (!had || o.Do == "" || o.Record != w.Record) {
			add("wait-decided", &u, ts(w.At), w.Actor, w.Do, w.Record, fmt.Sprintf("%s decided %s", or(w.Actor, "someone"), w.Do), w.Record+"@"+ts(w.At).UTC().Format(time.RFC3339))
		}
	}
	seen := func(key string, keys map[string]bool) bool { return keys[key] }
	vk := map[string]bool{}
	for _, v := range before.Verdicts {
		vk[fmt.Sprintf("%d/%d", v.Unit, v.Attempt)] = true
	}
	for _, v := range after.Verdicts {
		k := fmt.Sprintf("%d/%d", v.Unit, v.Attempt)
		if seen(k, vk) {
			continue
		}
		u := v.Unit
		add("verdict", &u, v.At.Time, "", "", v.Verdict, fmt.Sprintf("passed %d, reworked %d, escalated %d, accepted %d", v.Passed, v.Reworked, v.Escalated, v.Accepted), k)
	}
	ak := map[string]bool{}
	for _, a := range before.Acks {
		ak[a.Kind+"/"+a.Units+"/"+a.At.UTC().Format(time.RFC3339)] = true
	}
	for _, a := range after.Acks {
		k := a.Kind + "/" + a.Units + "/" + a.At.UTC().Format(time.RFC3339)
		if seen(k, ak) {
			continue
		}
		msg := a.Kind + " units " + a.Units
		if a.Assignee != "" {
			msg += " to " + a.Assignee
		}
		add("ack", nil, a.At.Time, a.Actor, a.Kind, a.Record, msg, k)
	}
	dk := map[string]bool{}
	for _, d := range before.Decisions {
		dk[fmt.Sprintf("%d/%s/%s", d.Unit, d.Kind, d.At.UTC().Format(time.RFC3339))] = true
	}
	for _, d := range after.Decisions {
		k := fmt.Sprintf("%d/%s/%s", d.Unit, d.Kind, d.At.UTC().Format(time.RFC3339))
		if seen(k, dk) {
			continue
		}
		u := d.Unit
		add("decision", &u, d.At.Time, "queue", d.Kind, d.Record, d.Kind+": "+d.Reason, k)
	}
	yk := map[string]bool{}
	for _, y := range before.Yields {
		yk[fmt.Sprintf("%d/%s", y.Unit, y.At.UTC().Format(time.RFC3339))] = true
	}
	for _, y := range after.Yields {
		k := fmt.Sprintf("%d/%s", y.Unit, y.At.UTC().Format(time.RFC3339))
		if seen(k, yk) {
			continue
		}
		u := y.Unit
		add("yield", &u, y.At.Time, "", "", "", "yielded: "+y.Reason, k)
	}
	ik := map[string]bool{}
	for _, i := range before.Interruptions {
		ik[fmt.Sprintf("%d/%s", i.Unit, i.At.UTC().Format(time.RFC3339))] = true
	}
	for _, i := range after.Interruptions {
		k := fmt.Sprintf("%d/%s", i.Unit, i.At.UTC().Format(time.RFC3339))
		if seen(k, ik) {
			continue
		}
		u := i.Unit
		add("interrupted", &u, i.At.Time, "", i.Reason, "", "interrupted: "+i.Reason+onNode(i.Node), k)
	}
	needKeys := make([]string, 0, len(after.Needs))
	for k := range after.Needs {
		needKeys = append(needKeys, k)
	}
	sort.Strings(needKeys)
	for _, k := range needKeys {
		n, o := after.Needs[k], before.Needs[k]
		i, _ := strconv.Atoi(k)
		u := int32(i)
		if o.Text == "" || !o.At.Equal(&n.At) {
			add("needs", &u, n.At.Time, "", "", n.Record, "needs information: "+n.Text, n.At.UTC().Format(time.RFC3339))
		}
		if n.Supplied != "" && o.Supplied == "" {
			add("supplied", &u, now, n.By, "", "", "supplied by "+or(n.By, "someone"), n.At.UTC().Format(time.RFC3339))
		}
	}
	ck := map[string]int32{}
	for _, c := range before.Causes {
		ck[c.Key] = c.Count
	}
	for _, c := range after.Causes {
		if n, had := ck[c.Key]; had && n == c.Count {
			continue
		}
		add("cause", nil, c.Last.Time, "", strings.Join(c.Did, ","), "", fmt.Sprintf("%s: %d attempts, units %s", c.Title, c.Count, c.Units),
			fmt.Sprintf("%s/%d", c.Key, c.Count))
	}
	return out
}

func onNode(n string) string {
	if n == "" {
		return ""
	}
	return " on " + n
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
