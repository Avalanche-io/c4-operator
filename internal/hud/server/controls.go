package server

import (
	"context"
	"encoding/json"
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

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/order"
)

// Every control is one Kubernetes write. Outside development mode the
// user must be allowed that write (SubjectAccessReview); the HUD's service
// account makes it and records an Event naming the user.

func (s *Server) authorize(ctx context.Context, r *http.Request, v *api.Viewer, ns, verb, resource, name string) error {
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

func decodeBody(r *http.Request, into any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16))
	return dec.Decode(into)
}

// liveJob reads the job from the API, for a write.
func (s *Server) liveJob(ctx context.Context, ns, name string) (*c4v1.WorkJob, error) {
	var j c4v1.WorkJob
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &j)
	return &j, err
}

func (s *Server) record(ctx context.Context, obj client.Object, kind, user, reason, msg string) {
	if s.Kube == nil {
		return
	}
	ns := obj.GetNamespace()
	if ns == "" {
		ns = s.Namespace
	}
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{GenerateName: obj.GetName() + "-", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: obj.GetName(), Namespace: obj.GetNamespace(), UID: obj.GetUID(), APIVersion: c4v1.GroupVersion.String()},
		Reason:         reason, Message: msg + " (by " + user + " in the HUD)", Type: corev1.EventTypeNormal,
		Source: corev1.EventSource{Component: "c4-hud"}, FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if kind == "Node" {
		ev.InvolvedObject.APIVersion = "v1"
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
		t, err := time.Parse(time.RFC3339, *body.Deadline)
		if err != nil {
			fail(w, http.StatusBadRequest, "deadline: %v", err)
			return
		}
		j.Spec.Deadline = &metav1.Time{Time: t}
		did = append(did, "deadline "+t.In(s.Loc).Format("Mon 15:04"))
	}
	if len(did) == 0 {
		fail(w, http.StatusBadRequest, "nothing to change: send hold, holdStop, boost or deadline")
		return
	}
	if err := s.Client.Patch(ctx, j, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := j.Name + ": " + strings.Join(did, ", ")
	s.record(ctx, j, "WorkJob", v.User, "Nudged", msg)
	m := s.current()
	rec2 := *rec
	rec2.job = *j
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "job": s.jobSummary(m, &rec2)})
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
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationRequeue] = strconv.FormatInt(time.Now().UnixNano(), 36) + ":" + order.EncodeSet(idx)
	if err := s.Client.Patch(ctx, j, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s: %d units queued again", j.Name, len(idx))
	s.record(ctx, j, "WorkJob", v.User, "Requeued", msg)
	m := s.current()
	rec2 := *rec
	rec2.job = *j
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "job": s.jobSummary(m, &rec2)})
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
	if err := s.Client.Patch(ctx, j, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	s.record(ctx, j, "WorkJob", v.User, "Blacklist", msg)
	m := s.current()
	rec2 := *rec
	rec2.job = *j
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "job": s.jobSummary(m, &rec2)})
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
	s.record(ctx, &n, "Node", v.User, "Disabled", msg)
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
	cr.Spec.Approved = true
	if err := s.Client.Patch(ctx, cr, patch); err != nil {
		fail(w, http.StatusConflict, "%v", err)
		return
	}
	msg := fmt.Sprintf("%s: %d slots approved for %s", pool, cr.Spec.Slots, strings.Join(cr.Spec.Jobs, ", "))
	s.record(ctx, cr, "CapacityRequest", v.User, "Approved", msg)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
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
