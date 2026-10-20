package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// Every control is one Kubernetes write. Outside development mode the
// user must be allowed that write (SubjectAccessReview); the HUD's service
// account makes it and records an Event naming the user.

func (s *Server) authorize(ctx context.Context, r *http.Request, v *api.Viewer, ns, verb, resource, name string) error {
	if v != nil && v.Unit != nil {
		// A unit: what its tool grants (units.go).
		return s.unitMay(v, ns, resource, name)
	}
	if s.DevViewers != nil {
		return nil
	}
	var groups []string
	if s.TrustProxy {
		for _, g := range strings.Split(r.Header.Get("X-Forwarded-Groups"), ",") {
			if g = strings.TrimSpace(g); g != "" {
				groups = append(groups, g)
			}
		}
	}
	ok, err := s.allowed(ctx, v.User, groups, ns, verb, resource, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s may not %s %s %s", v.User, verb, resource, name)
	}
	return nil
}

// Request bodies: a control's is small; an object list's and a view's are
// as large as their counts allow (maxListIDs IDs of about 93 bytes of
// JSON each; maxViewFiles entries with their paths).
const (
	controlBody = 1 << 16
	jobBody     = 1 << 20
	listBody    = 16 << 20
	viewBody    = 512 << 20
)

func decodeBody(r *http.Request, into any) error { return decodeBodyUpTo(r, into, controlBody) }

// decodeBodyUpTo reads a JSON body of at most limit bytes.
func decodeBodyUpTo(r *http.Request, into any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit))
	return dec.Decode(into)
}

// failBody answers a body that could not be read: 413 when it was too
// large, else 400.
func failBody(w http.ResponseWriter, err error) {
	var big *http.MaxBytesError
	if errors.As(err, &big) {
		fail(w, http.StatusRequestEntityTooLarge, "body over %d bytes", big.Limit)
		return
	}
	fail(w, http.StatusBadRequest, "body: %v", err)
}

// liveJob reads the job from the API, for a write.
func (s *Server) liveJob(ctx context.Context, ns, name string) (*c4v1.WorkJob, error) {
	var j c4v1.WorkJob
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &j)
	return &j, err
}

// record writes the Event of a control, naming the user. Its annotations
// carry the actor, the book kind and the control record for GET /book.
func (s *Server) record(ctx context.Context, obj client.Object, kind, user, reason, msg string, note bookNote) {
	if s.Kube == nil {
		return
	}
	ns := obj.GetNamespace()
	if ns == "" {
		ns = s.Namespace
	}
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: obj.GetName() + "-", Namespace: ns, Annotations: map[string]string{
			annoActor: note.actor, annoBookKind: note.kind}},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: obj.GetName(), Namespace: obj.GetNamespace(), UID: obj.GetUID(), APIVersion: c4v1.GroupVersion.String()},
		Reason:         reason, Message: msg + " (by " + user + " in the HUD)", Type: corev1.EventTypeNormal,
		Source: corev1.EventSource{Component: "c4-hud"}, FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if kind == "Node" {
		ev.InvolvedObject.APIVersion = "v1"
	}
	if note.record != "" {
		ev.Annotations[annoRecord] = note.record
	}
	if note.actor == "" {
		ev.Annotations[annoActor] = user
	}
	if _, err := s.Kube.CoreV1().Events(ns).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		s.Log.Error(err, "record control event")
	}
}

func (s *Server) handleNudge(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	var body struct {
		Hold     *bool    `json:"hold"`
		HoldStop *bool    `json:"holdStop"`
		Boost    *float64 `json:"boost"`
		Deadline *string  `json:"deadline"`
		// DeadlineMode is Pinned, Suggested or Tracking; NextSession moves
		// the deadline to the session the suggestion offers next.
		DeadlineMode *string `json:"deadlineMode"`
		NextSession  bool    `json:"nextSession"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, rec.job.Namespace, "patch", "workjobs", rec.job.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	patch := client.MergeFrom(j.DeepCopy())
	before := queue.SpecDigest(j)
	var did []string
	if body.Hold != nil {
		j.Spec.Nudge.Hold = *body.Hold
		if !*body.Hold {
			j.Spec.Nudge.HoldStop = false
		}
		did = append(did, map[bool]string{true: "held", false: "released"}[*body.Hold])
	}
	if body.HoldStop != nil && *body.HoldStop {
		j.Spec.Nudge.Hold, j.Spec.Nudge.HoldStop = true, true
		did = append(did, "held and its running units stopped")
	}
	if body.Boost != nil {
		b := *body.Boost
		if b < 1 || b > 2 {
			fail(w, http.StatusBadRequest, "boost must be from 1 to 2")
			return
		}
		j.Spec.Nudge.Boost = strconv.FormatFloat(b, 'f', -1, 64)
		if b == 1 {
			j.Spec.Nudge.Boost = ""
		}
		did = append(did, fmt.Sprintf("boost %.1f", b))
	}
	if body.Deadline != nil {
		t, err := s.dueOn(ctx, rec.project, *body.Deadline)
		if err != nil {
			fail(w, http.StatusBadRequest, "deadline: %v", err)
			return
		}
		j.Spec.Deadline = &metav1.Time{Time: t}
		did = append(did, "deadline "+forecast.Clock(t, s.Loc))
		if j.Spec.DeadlineMode == "Suggested" && body.DeadlineMode == nil {
			// A time a person sets is theirs.
			j.Spec.DeadlineMode = "Pinned"
		}
	}
	if body.NextSession {
		if body.Deadline != nil || (body.DeadlineMode != nil && *body.DeadlineMode != "Pinned") {
			fail(w, http.StatusBadRequest, "nextSession pins the next session: send it without deadline or another deadlineMode")
			return
		}
		sg := j.Status.Suggestion
		if sg == nil || sg.NextSession == nil {
			fail(w, http.StatusConflict, "%s has no next session to move to: only a job on a sessions policy that will miss its session is offered one", j.Name)
			return
		}
		j.Spec.Deadline = &metav1.Time{Time: sg.NextSession.Time}
		j.Spec.DeadlineMode = "Pinned"
		did = append(did, "deadline "+forecast.Clock(sg.NextSession.Time, s.Loc)+", the next session")
	}
	if body.DeadlineMode != nil && !body.NextSession {
		m := *body.DeadlineMode
		switch m {
		case "Pinned":
			if body.Deadline == nil {
				// Pinning without a time keeps the deadline the job has now
				// (a suggestion, typically).
				cur := j.Status.Deadline
				if cur == nil {
					cur = j.Spec.Deadline
				}
				if cur == nil {
					fail(w, http.StatusBadRequest, "%s has no deadline to pin: send deadline too", j.Name)
					return
				}
				j.Spec.Deadline = &metav1.Time{Time: cur.Time}
				did = append(did, "deadline "+forecast.Clock(cur.Time, s.Loc)+" pinned")
			}
		case "Suggested":
			if body.Deadline != nil {
				fail(w, http.StatusBadRequest, "a suggested deadline is the farm's: send deadline with deadlineMode Pinned or Tracking, or alone")
				return
			}
		case "Tracking":
			// With a deadline: production tracking's date, handed back to
			// tracking in one write (a pinned job included).
		default:
			fail(w, http.StatusBadRequest, "deadlineMode must be Pinned, Suggested or Tracking")
			return
		}
		j.Spec.DeadlineMode = m
		if m != "Pinned" {
			did = append(did, map[string]string{"Suggested": "deadline follows the farm's suggestion",
				"Tracking": "deadline follows production tracking"}[m])
		}
	}
	if len(did) == 0 {
		fail(w, http.StatusBadRequest, "nothing to change: send hold, holdStop, boost, deadline, deadlineMode or nextSession")
		return
	}
	msg := j.Name + ": " + strings.Join(did, ", ")
	kind := "boost"
	switch {
	case body.HoldStop != nil && *body.HoldStop:
		kind = "hold"
	case body.Hold != nil:
		kind = map[bool]string{true: "hold", false: "release"}[*body.Hold]
	case body.Deadline != nil || body.DeadlineMode != nil || body.NextSession:
		kind = "deadline"
	}
	s.finishJobControl(w, r, v, rec, j, patch, before, "nudge", kind, body, "Nudged", msg)
}

// finishJobControl signs the control record, writes the job with the
// record's ID in its annotation, records an Event and answers.
func (s *Server) finishJobControl(w http.ResponseWriter, r *http.Request, v *api.Viewer, rec *jobRec, j *c4v1.WorkJob, patch client.Patch,
	before, action, bookKind string, change any, reason, msg string) {
	ctx := r.Context()
	id, err := s.controlJob(ctx, r, v, rec, j, before, action, change)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	if err := s.Client.Patch(ctx, j, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	s.record(ctx, j, "WorkJob", v.User, reason, note, bookNote{actor: actorOf(r, v), kind: bookKind, record: id})
	m := s.current()
	rec2 := *rec
	rec2.job = *j
	out := map[string]any{"ok": true, "message": msg, "job": s.jobSummary(m, &rec2)}
	if id != "" {
		out["record"] = id
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRequeue(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	var body struct {
		Units json.RawMessage `json:"units"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	var set string
	var arr []int
	if err := json.Unmarshal(body.Units, &set); err != nil {
		if err := json.Unmarshal(body.Units, &arr); err != nil {
			fail(w, http.StatusBadRequest, "units: a unit set string or an array of indexes")
			return
		}
		set = order.EncodeSet(arr)
	}
	idx, err := order.DecodeSet(set)
	if err != nil || len(idx) == 0 {
		fail(w, http.StatusBadRequest, "units: %v", err)
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, rec.job.Namespace, "patch", "workjobs", rec.job.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	patch := client.MergeFrom(j.DeepCopy())
	before := queue.SpecDigest(j)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationRequeue] = strconv.FormatInt(time.Now().UnixNano(), 36) + ":" + order.EncodeSet(idx)
	msg := fmt.Sprintf("%s: %d units queued again", j.Name, len(idx))
	s.finishJobControl(w, r, v, rec, j, patch, before, "requeue", "requeue", map[string]any{"units": order.EncodeSet(idx)}, "Requeued", msg)
}

// handleAck is POST /jobs/{ns}/{name}/ack: a person takes failed units
// (acknowledge, or assign to someone), or accepts them as they are
// (accept). The queue carries it out and records who and when in
// status.acks; accepted units no longer hold the job back.
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	var body struct {
		Units    json.RawMessage `json:"units"`
		Kind     string          `json:"kind"`
		Assignee string          `json:"assignee"`
		Note     string          `json:"note"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	switch body.Kind {
	case "":
		body.Kind = c4v1.AckAcknowledge
	case c4v1.AckAcknowledge, c4v1.AckAccept:
	case c4v1.AckAssign:
		if body.Assignee == "" {
			fail(w, http.StatusBadRequest, "assign: name the assignee")
			return
		}
	default:
		fail(w, http.StatusBadRequest, "kind: acknowledge, assign or accept")
		return
	}
	var set string
	var arr []int
	if err := json.Unmarshal(body.Units, &set); err != nil {
		if err := json.Unmarshal(body.Units, &arr); err != nil {
			fail(w, http.StatusBadRequest, "units: a unit set string or an array of indexes")
			return
		}
		set = order.EncodeSet(arr)
	}
	idx, err := order.DecodeSet(set)
	if err != nil || len(idx) == 0 {
		fail(w, http.StatusBadRequest, "units: %v", err)
		return
	}
	failed, _ := order.DecodeSet(rec.job.Status.UnitStates["failed"])
	isFailed := map[int]bool{}
	for _, i := range failed {
		isFailed[i] = true
	}
	var take []int
	for _, i := range idx {
		if isFailed[i] {
			take = append(take, i)
		}
	}
	if len(take) == 0 {
		fail(w, http.StatusConflict, "none of units %s has failed", order.EncodeSet(idx))
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, rec.job.Namespace, "patch", "workjobs", rec.job.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	patch := client.MergeFrom(j.DeepCopy())
	before := queue.SpecDigest(j)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	var reqs []c4v1.WorkAckRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationAcks]), &reqs)
	now := time.Now()
	units := order.EncodeSet(take)
	reqs = append(reqs, c4v1.WorkAckRequest{Token: strconv.FormatInt(now.UnixNano(), 36), Kind: body.Kind, Units: units,
		Actor: actorOf(r, v), Assignee: body.Assignee, Note: body.Note, At: metav1.Time{Time: now.UTC().Truncate(time.Second)}})
	if len(reqs) > 16 {
		reqs = reqs[len(reqs)-16:]
	}
	b, _ := json.Marshal(reqs)
	j.Annotations[c4v1.AnnotationAcks] = string(b)
	var msg string
	switch body.Kind {
	case c4v1.AckAccept:
		msg = fmt.Sprintf("%s: %d failed units accepted as they are", j.Name, len(take))
	case c4v1.AckAssign:
		msg = fmt.Sprintf("%s: %d failed units assigned to %s", j.Name, len(take), body.Assignee)
	default:
		msg = fmt.Sprintf("%s: %d failed units taken by %s", j.Name, len(take), v.User)
	}
	change := map[string]any{"units": units, "kind": body.Kind}
	if body.Assignee != "" {
		change["assignee"] = body.Assignee
	}
	if body.Note != "" {
		change["note"] = body.Note
	}
	s.finishJobControl(w, r, v, rec, j, patch, before, body.Kind, body.Kind, change, "Acknowledged", msg)
}

func (s *Server) handleBlacklist(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	var body struct {
		Node   string `json:"node"`
		Remove bool   `json:"remove"`
	}
	if err := decodeBody(r, &body); err != nil || body.Node == "" {
		fail(w, http.StatusBadRequest, "send {node, remove?}")
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, rec.job.Namespace, "patch", "workjobs", rec.job.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	patch := client.MergeFrom(j.DeepCopy())
	before := queue.SpecDigest(j)
	msg := ""
	if body.Remove {
		j.Spec.AvoidNodes = slices.DeleteFunc(j.Spec.AvoidNodes, func(n string) bool { return n == body.Node })
		if j.Annotations == nil {
			j.Annotations = map[string]string{}
		}
		allow := strings.Split(j.Annotations[c4v1.AnnotationAllowNodes], ",")
		allow = append(slices.DeleteFunc(allow, func(n string) bool { return n == "" || n == body.Node }), body.Node)
		j.Annotations[c4v1.AnnotationAllowNodes] = strings.Join(allow, ",")
		msg = fmt.Sprintf("%s: %s allowed again", j.Name, body.Node)
	} else {
		if !slices.Contains(j.Spec.AvoidNodes, body.Node) {
			j.Spec.AvoidNodes = append(j.Spec.AvoidNodes, body.Node)
		}
		msg = fmt.Sprintf("%s: will not use %s", j.Name, body.Node)
	}
	s.finishJobControl(w, r, v, rec, j, patch, before, "avoid-node", map[bool]string{true: "allow", false: "avoid"}[body.Remove], body, "Blacklist", msg)
}

func (s *Server) handleDisable(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	var body struct {
		Disabled bool `json:"disabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send {disabled}")
		return
	}
	ctx := r.Context()
	name := r.PathValue("name")
	if !v.All && s.DevViewers != nil {
		fail(w, http.StatusForbidden, "%s may not disable nodes", v.User)
		return
	}
	if err := s.authorize(ctx, r, v, "", "patch", "nodes", name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	var n corev1.Node
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	patch := client.MergeFrom(n.DeepCopy())
	n.Spec.Unschedulable = body.Disabled
	if err := s.Client.Patch(ctx, &n, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := name + ": disabled; running units finish"
	if !body.Disabled {
		msg = name + ": enabled"
	}
	s.record(ctx, &n, "Node", v.User, "Disabled", msg, bookNote{actor: actorOf(r, v), kind: map[bool]string{true: "disable", false: "enable"}[body.Disabled]})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "node": map[string]any{"name": name, "disabled": body.Disabled}})
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	ctx := r.Context()
	pool := r.PathValue("pool")
	var list c4v1.CapacityRequestList
	if err := s.Client.List(ctx, &list); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	var cr *c4v1.CapacityRequest
	for i := range list.Items {
		c := &list.Items[i]
		if c.Spec.Pool == pool && !c.Spec.Approved && strings.Contains(c.Name, "-approve-") {
			cr = c
			break
		}
	}
	if cr == nil {
		fail(w, http.StatusNotFound, "no capacity request in pool %s waits for approval", pool)
		return
	}
	if err := s.authorize(ctx, r, v, "", "patch", "capacityrequests", cr.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	patch := client.MergeFrom(cr.DeepCopy())
	before := sha256JSON(cr.Spec)
	cr.Spec.Approved = true
	// The approval is recorded in the stores of the projects whose jobs
	// the capacity is for.
	m := s.current()
	var projects []string
	for _, key := range cr.Spec.Jobs {
		if jr := m.byKey[key]; jr != nil && !slices.Contains(projects, jr.project) {
			projects = append(projects, jr.project)
		}
	}
	pred := controlPredicate{Actor: v.User, Authenticated: s.authenticatedBy(r), Via: via(r),
		Authorization: s.authorization("patch", "capacityrequests", "", cr.Name), Action: "approve-capacity",
		Target: "capacityrequest:" + cr.Name,
		Change: map[string]any{"pool": pool, "slots": cr.Spec.Slots, "jobs": cr.Spec.Jobs, "estimatedCost": cr.Spec.EstimatedCost}}
	id, err := s.signControl(ctx, projects, provenance.Subject{Name: pred.Target, Digest: map[string]string{"sha256": before}}, pred)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	if id != "" {
		if cr.Annotations == nil {
			cr.Annotations = map[string]string{}
		}
		cr.Annotations[c4v1.AnnotationControlRecords] = appendRecord(cr.Annotations[c4v1.AnnotationControlRecords], id)
	}
	if err := s.Client.Patch(ctx, cr, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s: %d slots approved for %s", pool, cr.Spec.Slots, strings.Join(cr.Spec.Jobs, ", "))
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	s.record(ctx, cr, "CapacityRequest", v.User, "Approved", note, bookNote{actor: actorOf(r, v), kind: "capacity-approved", record: id})
	out := map[string]any{"ok": true, "message": msg}
	if id != "" {
		out["record"] = id
	}
	writeJSON(w, http.StatusOK, out)
}

// jsonMap turns a struct into a map through its JSON form.
func jsonMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

// handleDecide is POST /jobs/{ns}/{name}/decide: a person decides what
// waits for them (DESIGN.md 10.6, Waits): a wait unit (a ruling, an
// approval, an outside party's answer: do approve, one of its choices, or
// fail), or a unit a review escalated (do accept, rework or fail). The
// decision is signed first; its record's C4 ID goes with the request, so
// the queue's status.waits names it.
func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	var body struct {
		Unit json.RawMessage `json:"unit"`
		Do   string          `json:"do"`
		Note string          `json:"note"`
		// Reply is the decision's content, for a wait that asks for one:
		// a view already stored, or a text the HUD stores as a view of
		// one file, reply.md.
		Reply *struct {
			View string `json:"view"`
			Text string `json:"text"`
		} `json:"reply"`
		// Via is a decision a tracker's sync relays: made in the tracker,
		// by a tracker user its Tracker's people map names
		// (internal/hud/API.md, Relayed decisions).
		Via *api.Relay `json:"via"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	var ref string
	if err := json.Unmarshal(body.Unit, &ref); err != nil {
		var n int
		if err := json.Unmarshal(body.Unit, &n); err != nil {
			fail(w, http.StatusBadRequest, "unit: a unit's name or index")
			return
		}
		ref = strconv.Itoa(n)
	}
	var wait *c4v1.WorkWait
	for i := range rec.job.Status.Waits {
		ww := &rec.job.Status.Waits[i]
		if ww.Do == "" && (ww.Name == ref || strconv.Itoa(int(ww.Unit)) == ref) {
			wait = ww
		}
	}
	if wait == nil {
		fail(w, http.StatusConflict, "nothing open waits for a decision as unit %q", ref)
		return
	}
	switch {
	case body.Do == "":
		fail(w, http.StatusBadRequest, "do: the decision")
		return
	case wait.Escalated && body.Do != "accept" && body.Do != "rework" && body.Do != "fail":
		fail(w, http.StatusBadRequest, "do: accept, rework or fail, for a unit a review escalated")
		return
	case !wait.Escalated && len(wait.Choices) > 0 && body.Do != "fail" && !slices.Contains(wait.Choices, body.Do):
		fail(w, http.StatusBadRequest, "do: one of %s, or fail", strings.Join(wait.Choices, ", "))
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, rec.job.Namespace, "patch", "workjobs", rec.job.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	custody, actor := CustodyDirect, actorOf(r, v)
	standFor := ""
	if body.Via == nil {
		// Whom the wait names decides it, or someone the farm and the
		// project let stand in (whomay.go); a relay checks the person its
		// tracker maps instead.
		var err error
		if standFor, err = s.mayDecide(rec.project, wait.Server, deciderOf(r, v)); err != nil {
			fail(w, http.StatusForbidden, "%v", err)
			return
		}
		if standFor != "" {
			custody = CustodyStandIn
		}
	}
	if body.Via != nil {
		grade, who, err := s.relayed(ctx, v, rec, wait, body.Via)
		if err != nil {
			var ge *errGrant
			code := http.StatusForbidden
			if errors.As(err, &ge) {
				code = ge.code
			}
			fail(w, code, "via: %v", err)
			return
		}
		custody, actor = grade, who
	}
	reply := ""
	switch {
	case body.Reply != nil && !wait.Reply:
		fail(w, http.StatusBadRequest, "reply: this wait takes no reply")
		return
	case wait.Reply && body.Do != "fail" && (body.Reply == nil || body.Reply.View == "" && body.Reply.Text == ""):
		fail(w, http.StatusBadRequest, "reply: this wait asks for one: a view or a text")
		return
	case body.Reply != nil && body.Reply.View != "" && body.Reply.Text != "":
		fail(w, http.StatusBadRequest, "reply: a view or a text, not both")
		return
	case body.Reply != nil && body.Reply.View != "":
		if _, err := c4.Parse(body.Reply.View); err != nil {
			fail(w, http.StatusBadRequest, "reply.view %q is not a C4 ID", body.Reply.View)
			return
		}
		reply = body.Reply.View
	case body.Reply != nil && body.Reply.Text != "":
		id, err := s.textView(ctx, rec.project, body.Reply.Text)
		if err != nil {
			fail(w, http.StatusBadGateway, "reply: storing the text: %v", err)
			return
		}
		reply = id
	}
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	patch := client.MergeFrom(j.DeepCopy())
	before := queue.SpecDigest(j)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	now := time.Now()
	unit := wait.Name
	if unit == "" {
		unit = strconv.Itoa(int(wait.Unit))
	}
	change := map[string]any{"unit": unit, "do": body.Do, "waitsFor": wait.Server}
	if body.Note != "" {
		change["note"] = body.Note
	}
	if reply != "" {
		// The signed decision names its reply.
		change["reply"] = reply
	}
	id, err := s.controlJob(ctx, r, v, rec, j, before, "decide", change, func(p *controlPredicate) {
		p.Custody = custody
		// Standing in, the record names both: for the person, by the actor.
		p.For = standFor
		if body.Via != nil {
			// The decider is the tracker's person; who relayed it, and how
			// the relay authenticated, is the rest of the record.
			p.Actor = actor
			p.Authenticated = "relayed by " + v.User + " (" + s.authenticatedBy(r) + ")"
			p.Relay = body.Via
		}
	})
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	var reqs []c4v1.WorkDecideRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationDecide]), &reqs)
	reqs = append(reqs, c4v1.WorkDecideRequest{Token: strconv.FormatInt(now.UnixNano(), 36), Unit: unit, Do: body.Do,
		Note: body.Note, Actor: actor, At: metav1.Time{Time: now.UTC().Truncate(time.Second)}, Record: id, View: reply})
	if len(reqs) > 16 {
		reqs = reqs[len(reqs)-16:]
	}
	b, _ := json.Marshal(reqs)
	j.Annotations[c4v1.AnnotationDecide] = string(b)
	if err := s.Client.Patch(ctx, j, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s: %s decided %s (%s)", j.Name, unit, body.Do, strings.TrimPrefix(wait.Server, "person:"))
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	if body.Via != nil {
		note += fmt.Sprintf("; relayed from tracker %s (%s, by @%s, %s)", body.Via.Tracker, body.Via.Source, body.Via.By, custody)
	}
	s.record(ctx, j, "WorkJob", v.User, "Decided", note, bookNote{actor: actor, kind: "decide", record: id})
	m := s.current()
	rec2 := *rec
	rec2.job = *j
	out := map[string]any{"ok": true, "message": msg, "job": s.jobSummary(m, &rec2)}
	if id != "" {
		out["record"] = id
	}
	writeJSON(w, http.StatusOK, out)
}

// textView stores a text as a view of one file, reply.md, in the project's
// store, and returns the view's C4 ID.
func (s *Server) textView(ctx context.Context, project, text string) (string, error) {
	if s.Stores == nil {
		return "", errors.New("no store")
	}
	st, err := s.Stores(ctx, project)
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", fmt.Errorf("project %s has no store", project)
	}
	b := []byte(text)
	id, err := st.PutBytes(ctx, b)
	if err != nil {
		return "", err
	}
	root := c4view.NewNode("")
	root.Files["reply.md"] = c4view.FileEntry("reply.md", int64(len(b)), id, false)
	v := c4view.FromNode(root)
	for _, rec := range v.Records() {
		if _, err := st.PutBytes(ctx, rec.Data); err != nil {
			return "", err
		}
	}
	return v.ID.String(), nil
}

// handleSteer sends a message to a unit while it runs, without ending it
// (DESIGN.md 10.6, Steering): signed, then written as a request in
// c4.avalanche.io/steer that the queue carries out once, delivering it to
// the unit's pod, where the unit reads it at its next safe point (c4-unit
// steer). A unit that is not running gets it when it next runs; one that
// has ended takes none.
func (s *Server) handleSteer(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	var body struct {
		Unit json.RawMessage `json:"unit"`
		Text string          `json:"text"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	var ref string
	if err := json.Unmarshal(body.Unit, &ref); err != nil {
		var n int
		if err := json.Unmarshal(body.Unit, &n); err != nil {
			fail(w, http.StatusBadRequest, "unit: a unit's name or index")
			return
		}
		ref = strconv.Itoa(n)
	}
	switch {
	case strings.TrimSpace(body.Text) == "":
		fail(w, http.StatusBadRequest, "text: the message")
		return
	case len(body.Text) > c4v1.MaxSteerText:
		fail(w, http.StatusBadRequest, "text: %d bytes, over %d", len(body.Text), c4v1.MaxSteerText)
		return
	case rec.plan == nil:
		fail(w, http.StatusConflict, "the job has no units yet")
		return
	}
	idx := -1
	for i, u := range rec.plan.Units {
		if u.Name != "" && u.Name == ref {
			idx = i
		}
	}
	if n, err := strconv.Atoi(ref); idx < 0 && err == nil && n >= 0 && n < len(rec.plan.Units) {
		idx = n
	}
	if idx < 0 {
		fail(w, http.StatusNotFound, "no unit %q", ref)
		return
	}
	switch st := unitStateMap(&rec.job.Status, len(rec.plan.Units))[idx]; st {
	case "succeeded", "failed", "accepted", "superseded", "flagged":
		fail(w, http.StatusConflict, "unit %s has ended (%s): it reads no more messages", ref, st)
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, rec.job.Namespace, "patch", "workjobs", rec.job.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	patch := client.MergeFrom(j.DeepCopy())
	before := queue.SpecDigest(j)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	now := time.Now()
	unit := rec.plan.Units[idx].Name
	if unit == "" {
		unit = strconv.Itoa(idx)
	}
	id, err := s.controlJob(ctx, r, v, rec, j, before, "steer", map[string]any{"unit": unit, "text": body.Text})
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	var reqs []c4v1.WorkSteerRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationSteer]), &reqs)
	reqs = append(reqs, c4v1.WorkSteerRequest{Token: strconv.FormatInt(now.UnixNano(), 36), Unit: unit, Text: body.Text,
		Actor: actorOf(r, v), At: metav1.Time{Time: now.UTC().Truncate(time.Second)}, Record: id})
	if len(reqs) > 16 {
		reqs = reqs[len(reqs)-16:]
	}
	b, _ := json.Marshal(reqs)
	j.Annotations[c4v1.AnnotationSteer] = string(b)
	if err := s.Client.Patch(ctx, j, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s: a message to %s", j.Name, unit)
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	s.record(ctx, j, "WorkJob", v.User, "Steered", note, bookNote{actor: actorOf(r, v), kind: "steer", record: id})
	out := map[string]any{"ok": true, "message": msg}
	if id != "" {
		out["record"] = id
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSupply gives a unit that stopped for information its brief lacked
// (NeedsInformation) what it asked for: signed, then written as a request
// in c4.avalanche.io/supply that the queue carries out once. The unit runs
// again, resuming where it stopped, with the text in $C4_SUPPLIED.
func (s *Server) handleSupply(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	var body struct {
		Unit json.RawMessage `json:"unit"`
		Text string          `json:"text"`
	}
	if err := decodeBody(r, &body); err != nil {
		failBody(w, err)
		return
	}
	var ref string
	if err := json.Unmarshal(body.Unit, &ref); err != nil {
		var n int
		if err := json.Unmarshal(body.Unit, &n); err != nil {
			fail(w, http.StatusBadRequest, "unit: a unit's name or index")
			return
		}
		ref = strconv.Itoa(n)
	}
	if strings.TrimSpace(body.Text) == "" {
		fail(w, http.StatusBadRequest, "text: the information the unit asked for")
		return
	}
	if rec.plan == nil {
		fail(w, http.StatusConflict, "the job has no units yet")
		return
	}
	idx := -1
	for i, u := range rec.plan.Units {
		if u.Name != "" && u.Name == ref {
			idx = i
		}
	}
	if n, err := strconv.Atoi(ref); idx < 0 && err == nil && n >= 0 && n < len(rec.plan.Units) {
		idx = n
	}
	need, ok := rec.job.Status.Needs[strconv.Itoa(idx)]
	if idx < 0 || !ok || unitStateMap(&rec.job.Status, len(rec.plan.Units))[idx] != "failed" {
		fail(w, http.StatusConflict, "unit %q is not waiting for information", ref)
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, rec.job.Namespace, "patch", "workjobs", rec.job.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	// A unit's question is for a person, not for its job's other units
	// (whomay.go).
	if err := MayControl("supply", "", deciderOf(r, v)); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	patch := client.MergeFrom(j.DeepCopy())
	before := queue.SpecDigest(j)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	now := time.Now()
	unit := rec.plan.Units[idx].Name
	if unit == "" {
		unit = strconv.Itoa(idx)
	}
	id, err := s.controlJob(ctx, r, v, rec, j, before, "supply", map[string]any{"unit": unit, "needed": need.Text, "text": body.Text})
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	var reqs []c4v1.WorkSupplyRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationSupply]), &reqs)
	reqs = append(reqs, c4v1.WorkSupplyRequest{Token: strconv.FormatInt(now.UnixNano(), 36), Unit: unit, Text: body.Text,
		Actor: actorOf(r, v), At: metav1.Time{Time: now.UTC().Truncate(time.Second)}, Record: id})
	if len(reqs) > 16 {
		reqs = reqs[len(reqs)-16:]
	}
	b, _ := json.Marshal(reqs)
	j.Annotations[c4v1.AnnotationSupply] = string(b)
	if err := s.Client.Patch(ctx, j, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s: %s has what it needed and runs again", j.Name, unit)
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	s.record(ctx, j, "WorkJob", v.User, "Supplied", note, bookNote{actor: actorOf(r, v), kind: "supply", record: id})
	out := map[string]any{"ok": true, "message": msg}
	if id != "" {
		out["record"] = id
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWithdrawNeeds withdraws the question a unit stopped for
// (NeedsInformation) once it no longer applies, with why: signed, then
// written as a request in c4.avalanche.io/withdraw-needs that the queue
// carries out once. The unit runs again without an answer, with the why in
// $C4_WITHDRAWN. The same rights as supplying it.
func (s *Server) handleWithdrawNeeds(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	var body struct {
		Why string `json:"why"`
	}
	if err := decodeBody(r, &body); err != nil {
		failBody(w, err)
		return
	}
	if strings.TrimSpace(body.Why) == "" {
		fail(w, http.StatusBadRequest, "why: why the question no longer applies")
		return
	}
	if rec.plan == nil {
		fail(w, http.StatusConflict, "the job has no units yet")
		return
	}
	ref := r.PathValue("unit")
	idx := -1
	for i, u := range rec.plan.Units {
		if u.Name != "" && u.Name == ref {
			idx = i
		}
	}
	if n, err := strconv.Atoi(ref); idx < 0 && err == nil && n >= 0 && n < len(rec.plan.Units) {
		idx = n
	}
	need, ok := rec.job.Status.Needs[strconv.Itoa(idx)]
	if idx < 0 || !ok || need.Supplied != "" || need.Withdrawn != nil || unitStateMap(&rec.job.Status, len(rec.plan.Units))[idx] != "failed" {
		fail(w, http.StatusConflict, "unit %q has no open question", ref)
		return
	}
	ctx := r.Context()
	if err := s.authorize(ctx, r, v, rec.job.Namespace, "patch", "workjobs", rec.job.Name); err != nil {
		fail(w, http.StatusForbidden, "%v", err)
		return
	}
	j, err := s.liveJob(ctx, rec.job.Namespace, rec.job.Name)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	patch := client.MergeFrom(j.DeepCopy())
	before := queue.SpecDigest(j)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	now := time.Now()
	unit := rec.plan.Units[idx].Name
	if unit == "" {
		unit = strconv.Itoa(idx)
	}
	id, err := s.controlJob(ctx, r, v, rec, j, before, "withdraw-needs", map[string]any{"unit": unit, "needed": need.Text, "why": body.Why})
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "control record: %v", err)
		return
	}
	var reqs []c4v1.WorkWithdrawRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationWithdrawNeeds]), &reqs)
	reqs = append(reqs, c4v1.WorkWithdrawRequest{Token: strconv.FormatInt(now.UnixNano(), 36), Unit: unit, Why: body.Why,
		Actor: actorOf(r, v), At: metav1.Time{Time: now.UTC().Truncate(time.Second)}, Record: id})
	if len(reqs) > 16 {
		reqs = reqs[len(reqs)-16:]
	}
	b, _ := json.Marshal(reqs)
	j.Annotations[c4v1.AnnotationWithdrawNeeds] = string(b)
	if err := s.Client.Patch(ctx, j, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s: %s's question is withdrawn and it runs again without an answer", j.Name, unit)
	note := msg
	if id != "" {
		note += "; record " + shortID(id)
	}
	if via(r) == "mcp" {
		note += "; via MCP"
	}
	s.record(ctx, j, "WorkJob", v.User, "Withdrawn", note, bookNote{actor: actorOf(r, v), kind: "withdraw-needs", record: id})
	out := map[string]any{"ok": true, "message": msg}
	if id != "" {
		out["record"] = id
	}
	writeJSON(w, http.StatusOK, out)
}
