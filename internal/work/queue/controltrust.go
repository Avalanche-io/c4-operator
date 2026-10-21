package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// Controls are carried out on their records (DESIGN.md section 9.2). The
// HUD signs each decide, supply, steer, acknowledgement, withdrawal,
// requeue, allowed node and acceptance of a validated input before it
// writes the request, and the request names its record. With
// signing on (Roots set), the queue acts on a request only if that record
// verifies against the farm's roots, was signed by the HUD, is a control
// record for this job, and names the same action, actor, unit, token and
// content as the request. Any other request is left where it is, not
// carried out, logged, and reported once as a Warning event on the job.
// The admission policy keeps everyone but the manager from writing these
// annotations; this check is what holds if something writes one anyway.

// controlKind is one control annotation and how its requests match their
// records.
type controlKind struct {
	key string
	// action is the record's action for a request.
	action func(req map[string]any) string
	// fields must be equal in the request and the record's change.
	fields []string
	// last is the last token the queue carried out.
	last func(m *jobMem) string
}

var controlKinds = []controlKind{
	{key: c4v1.AnnotationAcks, action: func(r map[string]any) string { s, _ := r["kind"].(string); return s },
		fields: []string{"units", "kind"}, last: func(m *jobMem) string { return m.acked }},
	{key: c4v1.AnnotationDecide, action: func(map[string]any) string { return "decide" },
		fields: []string{"unit", "do"}, last: func(m *jobMem) string { return m.waitRecs().decided }},
	{key: c4v1.AnnotationSteer, action: func(map[string]any) string { return "steer" },
		fields: []string{"unit", "text"}, last: func(m *jobMem) string { return m.steered }},
	{key: c4v1.AnnotationSupply, action: func(map[string]any) string { return "supply" },
		fields: []string{"unit", "text"}, last: func(m *jobMem) string { return m.supplied }},
	{key: c4v1.AnnotationWithdrawNeeds, action: func(map[string]any) string { return "withdraw-needs" },
		fields: []string{"unit", "why"}, last: func(m *jobMem) string { return m.withdrawn }},
	{key: c4v1.AnnotationRequeue, action: func(map[string]any) string { return "requeue" },
		fields: []string{"units"}, last: func(m *jobMem) string { return m.requeued }},
	{key: c4v1.AnnotationAllowNodes, action: func(map[string]any) string { return "allow-node" },
		fields: []string{"node"}, last: func(m *jobMem) string { return m.allowed }},
	// An acceptance holds until a newer one, so every one is checked.
	{key: c4v1.AnnotationAcceptValidated, action: func(map[string]any) string { return "accept-validated" },
		fields: []string{"accept"}, last: func(*jobMem) string { return "" }},
}

// controlRecord is what the queue reads of a HUD control record.
type controlRecord struct {
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Change struct {
		Request map[string]any `json:"request"`
	} `json:"change"`
}

// maxCheckedRecords bounds the queue's cache of records it has read.
const maxCheckedRecords = 4096

// errTransient marks a record the queue could not read this tick.
var errTransient = errors.New("not readable now")

// trustedControls is the job with only the control requests the queue may
// carry out: j itself when signing is off.
func (q *Queue) trustedControls(ctx context.Context, v *view, now time.Time) *c4v1.WorkJob {
	j := v.job
	if q.Roots == nil {
		return j
	}
	var out *c4v1.WorkJob
	for _, k := range controlKinds {
		raw := j.Annotations[k.key]
		if raw == "" {
			continue
		}
		var reqs []map[string]any
		if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
			// Not a list of requests (an older form, written without a
			// record): nothing in it is carried out.
			q.refuseControl(ctx, v, k.key, "-", errors.New("it is not a list of signed requests"), now)
			reqs = nil
		}
		last := tokenValue(k.last(v.mem))
		kept := reqs[:0:0]
		for _, r := range reqs {
			token, _ := r["token"].(string)
			if tokenValue(token) <= last {
				kept = append(kept, r) // carried out already, or never will be
				continue
			}
			err := q.checkControl(ctx, v, k, r)
			if err == nil {
				kept = append(kept, r)
				continue
			}
			if errors.Is(err, errTransient) {
				continue // read again next tick
			}
			q.refuseControl(ctx, v, k.key, token, err, now)
		}
		if len(kept) == len(reqs) {
			continue
		}
		if out == nil {
			c := *j
			c.Annotations = maps.Clone(j.Annotations)
			out = &c
		}
		b, _ := json.Marshal(kept)
		out.Annotations[k.key] = string(b)
	}
	if out == nil {
		return j
	}
	return out
}

// checkControl says whether request r of kind k has a record that vouches
// for it.
func (q *Queue) checkControl(ctx context.Context, v *view, k controlKind, r map[string]any) error {
	id, _ := r["record"].(string)
	if id == "" {
		return errors.New("it names no signed record")
	}
	rec, err := q.controlRecord(ctx, v.project, id)
	if err != nil {
		return err
	}
	str := func(m map[string]any, f string) string { s, _ := m[f].(string); return s }
	actor := str(r, "actor")
	switch {
	case rec.Target != "workjob:"+v.job.Namespace+"/"+v.job.Name:
		return fmt.Errorf("record %s is for %s, not this job", shortRecord(id), rec.Target)
	case rec.Action != k.action(r):
		return fmt.Errorf("record %s is a %s, not a %s", shortRecord(id), rec.Action, k.action(r))
	case actor != rec.Actor && !(strings.HasPrefix(actor, "agent/") && strings.HasSuffix(actor, " for "+rec.Actor)):
		return fmt.Errorf("record %s is by %s, not %s", shortRecord(id), rec.Actor, actor)
	case str(rec.Change.Request, "token") != str(r, "token"):
		return fmt.Errorf("record %s is for another request (token %s)", shortRecord(id), str(rec.Change.Request, "token"))
	}
	for _, f := range k.fields {
		if fmt.Sprint(rec.Change.Request[f]) != fmt.Sprint(r[f]) {
			return fmt.Errorf("record %s says %s %q, the request %q", shortRecord(id), f, fmt.Sprint(rec.Change.Request[f]), fmt.Sprint(r[f]))
		}
	}
	return nil
}

// controlRecord reads, verifies and caches record id from the project's
// store.
func (q *Queue) controlRecord(ctx context.Context, project, id string) (*controlRecord, error) {
	q.ctlMu.Lock()
	if c, ok := q.ctlChecked[id]; ok {
		q.ctlMu.Unlock()
		return c.rec, c.err
	}
	q.ctlMu.Unlock()
	rec, err := q.readControlRecord(ctx, project, id)
	if errors.Is(err, errTransient) {
		return nil, err
	}
	q.ctlMu.Lock()
	if q.ctlChecked == nil || len(q.ctlChecked) >= maxCheckedRecords {
		q.ctlChecked = map[string]checkedRecord{}
	}
	q.ctlChecked[id] = checkedRecord{rec, err}
	q.ctlMu.Unlock()
	return rec, err
}

type checkedRecord struct {
	rec *controlRecord
	err error
}

func (q *Queue) readControlRecord(ctx context.Context, project, id string) (*controlRecord, error) {
	cid, err := c4.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("record %q is not a C4 ID", id)
	}
	if q.Stores == nil {
		return nil, fmt.Errorf("%w: the queue has no stores to read record %s from", errTransient, shortRecord(id))
	}
	st, err := q.Stores(ctx, project)
	if err != nil || st == nil {
		return nil, fmt.Errorf("%w: project %s store: %v", errTransient, project, err)
	}
	data, err := c4view.ReadObject(st, cid)
	switch {
	case errors.Is(err, storeurl.ErrNotFound) || errors.Is(err, fs.ErrNotExist):
		// The HUD stores a record before it writes the request.
		return nil, fmt.Errorf("record %s is not in project %s's store", shortRecord(id), project)
	case err != nil:
		return nil, fmt.Errorf("%w: record %s: %v", errTransient, shortRecord(id), err)
	}
	vr, err := provenance.VerifyBundle(data, q.Roots)
	if err != nil {
		return nil, fmt.Errorf("record %s does not verify: %v", shortRecord(id), err)
	}
	if vr.PredicateType != provenance.PredicateControl {
		return nil, fmt.Errorf("record %s is a %s record, not a control", shortRecord(id), vr.PredicateType)
	}
	if err := provenance.CheckSigner(vr); err != nil {
		return nil, fmt.Errorf("record %s: %v", shortRecord(id), err)
	}
	var rec controlRecord
	if err := json.Unmarshal(vr.Predicate, &rec); err != nil {
		return nil, fmt.Errorf("record %s: %v", shortRecord(id), err)
	}
	return &rec, nil
}

// refuseControl logs a request the queue will not carry out and reports it
// once as a Warning event on the job.
func (q *Queue) refuseControl(ctx context.Context, v *view, key, token string, why error, now time.Time) {
	seen := key + "/" + token
	if v.mem.refusedControls[seen] {
		return
	}
	if v.mem.refusedControls == nil {
		v.mem.refusedControls = map[string]bool{}
	}
	v.mem.refusedControls[seen] = true
	what := strings.TrimPrefix(key, "c4.avalanche.io/")
	msg := fmt.Sprintf("not carrying out a %s request (token %s): %v", what, token, why)
	q.Log.Info("control refused", "job", v.key, "annotation", key, "token", token, "why", why.Error())
	if q.Client == nil {
		return
	}
	t := metav1.NewTime(now)
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: v.job.Namespace, GenerateName: v.job.Name + ".control-"},
		InvolvedObject: corev1.ObjectReference{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Namespace: v.job.Namespace, Name: v.job.Name, UID: v.job.UID},
		Type:           corev1.EventTypeWarning, Reason: "ControlRefused", Message: msg,
		Source: corev1.EventSource{Component: "c4-queue"}, FirstTimestamp: t, LastTimestamp: t, Count: 1,
	}
	if err := q.Client.Create(ctx, ev); err != nil {
		q.Log.Error(err, "control refused: event", "job", v.key)
	}
}

func shortRecord(id string) string {
	if len(id) > 12 {
		return id[:12] + "..."
	}
	return id
}
