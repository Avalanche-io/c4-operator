package server

// Held tracker writes (internal/hud/API.md; docs/c4-sync.md). c4-sync
// journals each write to a tracker in its TrackerLink's status.written; a
// write that was interrupted, where nothing the tracker offers can tell
// whether it happened, is held (state unconfirmed) and never sent on a
// guess. A person looks in the tracker and says whether it happened, here:
// no one edits a TrackerLink by hand.

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// heldState is a held write's state in status.written (internal/effect
// Unconfirmed); condUnconfirmed the link condition c4-sync sets for it.
const (
	heldState       = "unconfirmed"
	condUnconfirmed = "Unconfirmed"
)

// heldView is a link's held write as people read it.
func heldView(m *model, l *c4v1.TrackerLink, field string, w c4v1.TrackerWritten, kind string, v *api.Viewer) api.HeldWrite {
	h := api.HeldWrite{Namespace: l.Namespace, Project: m.nsProject[l.Namespace], Link: l.Name, Field: field,
		Tracker: l.Spec.Tracker, Kind: kind, Entity: api.HeldEntity{Type: l.Spec.Ref.Type, ID: l.Spec.Ref.ID, URL: l.Spec.Ref.URL},
		Since: w.At.UTC()}
	h.State, _, _ = strings.Cut(w.Value, "#")
	if j := l.Spec.Target.WorkJob; j != "" {
		h.Job = l.Namespace + "/" + j
		if rec := m.byKey[h.Job]; rec != nil {
			h.Owner = rec.job.Annotations[c4v1.AnnotationOwner]
		}
	}
	h.Yours = h.Owner != "" && h.Owner == v.User
	h.Says = fmt.Sprintf("the %s %s on %s %s", trackerKindName(kind), field, h.Entity.Type, h.Entity.ID)
	if h.State != "" {
		h.Says += ", saying " + h.State
	}
	return h
}

// handleHeld lists held writes in projects the viewer sees: ?job=ns/name
// for one job's, ?tracker=ns/name for one Tracker's, ?mine=1 for the
// viewer's own jobs'.
func (s *Server) handleHeld(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	ctx := r.Context()
	out := api.HeldList{Held: []api.HeldWrite{}}
	var links c4v1.TrackerLinkList
	if err := s.Client.List(ctx, &links); err != nil {
		if noGrants(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	kinds := map[string]string{}
	var trs c4v1.TrackerList
	if err := s.Client.List(ctx, &trs); err == nil {
		for _, t := range trs.Items {
			kinds[t.Namespace+"/"+t.Name] = t.Spec.Kind
		}
	}
	q := r.URL.Query()
	for i := range links.Items {
		l := &links.Items[i]
		project, ok := m.nsProject[l.Namespace]
		if !ok || !v.Sees(project) {
			continue
		}
		if t := q.Get("tracker"); t != "" && t != l.Namespace+"/"+l.Spec.Tracker {
			continue
		}
		for field, wr := range l.Status.Written {
			if wr.State != heldState {
				continue
			}
			h := heldView(m, l, field, wr, kinds[l.Namespace+"/"+l.Spec.Tracker], v)
			if (q.Get("job") != "" && q.Get("job") != h.Job) || (q.Get("mine") != "" && !h.Yours) {
				continue
			}
			out.Held = append(out.Held, h)
		}
	}
	slices.SortFunc(out.Held, func(a, b api.HeldWrite) int { return a.Since.Compare(b.Since) })
	writeJSON(w, http.StatusOK, out)
}

// handleConfirm is a person's answer for a held write: it happened, and
// c4-sync takes it as written; or it did not, and c4-sync writes it again.
// The job's owner answers, or anyone allowed to change the job. Signed
// and booked.
func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	ns, name, field := r.PathValue("ns"), r.PathValue("link"), r.PathValue("field")
	project, ok := m.nsProject[ns]
	if !ok || !v.Sees(project) {
		fail(w, http.StatusNotFound, "no held write %s/%s %s", ns, name, field)
		return
	}
	var body api.ConfirmRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	if body.Happened == nil {
		fail(w, http.StatusBadRequest, "say whether it happened: happened true or false")
		return
	}
	ctx := r.Context()
	l := &c4v1.TrackerLink{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, l); err != nil {
		fail(w, http.StatusNotFound, "no held write %s/%s %s", ns, name, field)
		return
	}
	wr, ok := l.Status.Written[field]
	if !ok || wr.State != heldState {
		fail(w, http.StatusConflict, "%s/%s holds no %s write: nothing to confirm", ns, name, field)
		return
	}
	var tr c4v1.Tracker
	_ = s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: l.Spec.Tracker}, &tr)
	h := heldView(m, l, field, wr, tr.Spec.Kind, v)
	authz := "the job's owner"
	if !h.Yours {
		job := l.Spec.Target.WorkJob
		if job == "" || s.authorize(ctx, r, v, ns, "patch", "workjobs", job) != nil {
			fail(w, http.StatusForbidden, "only the job's owner, or one allowed to change the job, confirms its tracker writes")
			return
		}
		authz = s.authorization(v, "patch", "workjobs", ns, job)
	}
	action, did := "confirm-write", "happened"
	if !*body.Happened {
		action, did = "deny-write", "did not happen; c4-sync writes it again"
	}
	target := "trackerlink:" + ns + "/" + name + "#" + field
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r, v), Via: via(r), Agent: claimedAgent(r), Authorization: authz,
		Action: action, Target: target, Change: map[string]any{"field": field, "write": wr.Value, "happened": *body.Happened, "note": body.Note}}
	id, err := s.signControl(ctx, []string{project}, provenance.Subject{Name: target, Digest: map[string]string{"sha256": sha256JSON(wr)}}, pred)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	now := time.Now()
	before := l.DeepCopy()
	if *body.Happened {
		wr.State = ""
	} else {
		// No value: c4-sync's next pass finds the write not made, and
		// makes it, under the entity it made before, if any.
		wr = c4v1.TrackerWritten{Ref: wr.Ref, At: metav1.NewTime(now)}
	}
	l.Status.Written[field] = wr
	held := false
	for _, x := range l.Status.Written {
		held = held || x.State == heldState
	}
	if !held {
		meta.SetStatusCondition(&l.Status.Conditions, metav1.Condition{Type: condUnconfirmed, Status: metav1.ConditionFalse,
			Reason: "Confirmed", Message: fmt.Sprintf("%s said %s %s", v.User, h.Says, did)})
	}
	if err := s.Client.Status().Patch(ctx, l, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s: %s %s", v.User, h.Says, did)
	note := msg
	if body.Note != "" {
		note += ": " + body.Note
	}
	if id != "" {
		note += "; record " + shortID(id)
	}
	kind, onKind := "write-confirmed", "TrackerLink"
	if !*body.Happened {
		kind = "write-denied"
	}
	var on client.Object = l
	if rec := m.byKey[h.Job]; rec != nil {
		on, onKind = &rec.job, "WorkJob"
	}
	s.record(ctx, on, onKind, v.User, "WriteConfirmed", note, bookNote{actor: actorOf(r, v), kind: kind, record: id})
	out := map[string]any{"ok": true, "message": msg}
	if id != "" {
		out["record"] = id
	}
	writeJSON(w, http.StatusOK, out)
}
