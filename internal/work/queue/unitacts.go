package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/needs"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// A unit signs what it asks, answers, needs, withdraws and waits on as
// itself (PEOPLE-KEYS 5.1, c4-unit's acts), and its node daemon or host
// runner relays those acts to its pod (AnnotationUnitActs) ahead of the
// controls themselves. Where the queue reads a unit's control, it looks
// for the act of that kind and number among its pod's acts and holds it
// to that pod: signed by exactly that pod's unit and attempt (the job's
// whole UID), for the job's owner, on that pod (by UID, with the job spec
// it was made under), its params the control's. A control whose act is
// there and doesn't hold is not carried out, and is reported once as a
// Warning event on the job, as a HUD control that doesn't hold is. One
// with no act is carried out on its node's word, as before, and booked
// "relayed, unsigned"; from K8, it is refused. On a farm that doesn't
// sign (no Roots), nothing is checked.

// Custodies a unit's control is booked with.
const (
	custodyUnitSigned   = "signed"
	custodyUnitUnsigned = "relayed, unsigned"
)

// unitReport is one of a pod's acts as the queue read it: its kind and
// params, the C4 ID of its record, or why it doesn't hold.
type unitReport struct {
	act    string
	params map[string]any
	record string
	err    error
}

// podReports are a pod's acts, read from its annotation as it was.
type podReports struct {
	raw     string
	reports []unitReport
}

// reportsOf reads and checks pod p's acts, once for each new annotation.
func (q *Queue) reportsOf(v *view, p *corev1.Pod, now time.Time) []unitReport {
	raw := p.Annotations[c4v1.AnnotationUnitActs]
	m := v.mem
	if c := m.reports[p.UID]; c != nil && c.raw == raw {
		return c.reports
	}
	var list []json.RawMessage
	if raw != "" && json.Unmarshal([]byte(raw), &list) != nil {
		list = nil
	}
	c := &podReports{raw: raw}
	for _, b := range list {
		c.reports = append(c.reports, q.checkReport(v, p, b, now))
	}
	if m.reports == nil {
		m.reports = map[types.UID]*podReports{}
	}
	m.reports[p.UID] = c
	return c.reports
}

// checkReport verifies one act and holds it to pod p.
func (q *Queue) checkReport(v *view, p *corev1.Pod, b []byte, now time.Time) unitReport {
	r := unitReport{record: c4.Identify(bytes.NewReader(b)).String()}
	vr, a, err := provenance.VerifyUnitReport(b, q.Roots, now)
	if err != nil {
		r.err = err
		return r
	}
	r.act = a.Predicate.Act
	_ = json.Unmarshal(a.Predicate.Params, &r.params)
	ref := metav1.GetControllerOf(p)
	if ref == nil || ref.UID != v.job.UID {
		r.err = fmt.Errorf("pod %s is not this job's", p.Name)
		return r
	}
	attempt, _ := strconv.Atoi(p.Annotations[c4v1.AnnotationAttempt])
	unit := provenance.UnitIdentity(p.Namespace, p.Labels[c4v1.LabelJob], p.Labels[c4v1.LabelUnit], string(ref.UID), attempt)
	owner := v.job.Annotations[c4v1.AnnotationOwner]
	on := provenance.ActSubject("pod", p.Namespace, p.Name, string(p.UID), p.Annotations[c4v1.AnnotationUnitSpec])
	switch {
	case vr.Identity != unit:
		r.err = fmt.Errorf("signed by %s, and pod %s is %s", vr.Identity, p.Name, unit)
	case owner == "" || a.Predicate.Chain.For.Party != owner:
		r.err = fmt.Errorf("for %s, and the job's owner is %q", a.Predicate.Chain.For.Party, owner)
	case len(vr.Subjects) != 1 || vr.Subjects[0].Name != on.Name || vr.Subjects[0].Digest["sha256"] != on.Digest["sha256"]:
		r.err = fmt.Errorf("on another pod or spec, not %s", on.Name)
	case vr.Runner != "" && vr.Runner != p.Spec.NodeName:
		// a host unit's key is held by its runner: the one it runs under
		r.err = fmt.Errorf("its key is held by runner %s, and pod %s runs on %q", vr.Runner, p.Name, p.Spec.NodeName)
	}
	return r
}

// unitControl is how unit idx's control on pod p stands against its act:
// the act of kind act whose params select it (pick), and whose params
// must then equal want. It returns what the control's record is booked
// with, or why the control isn't carried out (reported once on the job).
func (q *Queue) unitControl(ctx context.Context, v *view, p *corev1.Pod, act string, pick func(map[string]any) bool, want map[string]any, now time.Time) (map[string]any, error) {
	if q.Roots == nil || p == nil {
		return nil, nil
	}
	reports := q.reportsOf(v, p, now)
	// an act that doesn't verify can't say what it is for: reported once,
	// and no other control rides on it
	for _, r := range reports {
		if r.err != nil && r.act == "" {
			q.refuseControl(ctx, v, c4v1.AnnotationUnitActs, string(p.UID)+"/"+r.record,
				fmt.Errorf("an act on unit %s's pod does not verify: %v", p.Labels[c4v1.LabelUnit], r.err), now)
		}
	}
	var found *unitReport
	for i := range reports {
		if r := &reports[i]; r.act == act && pick(r.params) {
			found = r
		}
	}
	var err error
	switch {
	case found == nil:
		return map[string]any{"custody": custodyUnitUnsigned}, nil
	case found.err != nil:
		err = fmt.Errorf("unit %s's %s: its act does not hold (%v)", p.Labels[c4v1.LabelUnit], act, found.err)
	case !sameParams(found.params, want):
		err = fmt.Errorf("unit %s's %s is not what it signed: %s, signed %s", p.Labels[c4v1.LabelUnit], act, canon(want), canon(found.params))
	default:
		return map[string]any{"custody": custodyUnitSigned, "act": found.record}, nil
	}
	q.refuseControl(ctx, v, c4v1.AnnotationUnitActs, string(p.UID)+"/"+found.record, err, now)
	return nil, err
}

// bySeq picks an act by its number.
func bySeq(seq int) func(map[string]any) bool {
	return func(p map[string]any) bool {
		n, ok := p["seq"].(float64)
		return ok && int(n) == seq
	}
}

// anyOne picks every act of its kind (needs, withdraw): the last stands.
func anyOne(map[string]any) bool { return true }

// sameParams compares params as JSON, keys sorted.
func sameParams(a, b map[string]any) bool { return canon(a) == canon(b) }

func canon(m map[string]any) string {
	b, _ := json.Marshal(m)
	var x any
	_ = json.Unmarshal(b, &x) // numbers as JSON has them
	b, _ = json.Marshal(x)
	return string(b)
}

// answerReport is how a unit's answer n on pod p stands against its act.
func (q *Queue) answerReport(ctx context.Context, v *view, p *corev1.Pod, n unitNote, now time.Time) (map[string]any, error) {
	want := map[string]any{"seq": n.Seq, "ask": n.Ask}
	if n.Answer != "" {
		want["answer"] = n.Answer
	} else {
		want["cannot"] = n.Cannot
	}
	return q.unitControl(ctx, v, p, "answer", bySeq(n.Seq), want, now)
}

// needsReport is how the needs pod p ended on stands against its act.
func (q *Queue) needsReport(ctx context.Context, v *view, p *corev1.Pod, now time.Time) (map[string]any, error) {
	// as written, before the farm names an empty question for it
	msg, _ := needsMessage(p)
	text, why, about := needs.Parse(msg)
	return q.unitControl(ctx, v, p, "needs", anyOne, map[string]any{"what": text, "why": why, "about": about}, now)
}

// needsHolds reports whether the needs pod p ended on is taken as one:
// not when its act is there and doesn't hold, which makes it a plain
// failure.
func (q *Queue) needsHolds(ctx context.Context, v *view, p *corev1.Pod, now time.Time) bool {
	_, err := q.needsReport(ctx, v, p, now)
	return err == nil
}

// withdrawReport is how the withdrawal pod p ended on stands against its
// act.
func (q *Queue) withdrawReport(ctx context.Context, v *view, p *corev1.Pod, now time.Time) (map[string]any, error) {
	return q.unitControl(ctx, v, p, "withdraw", anyOne, map[string]any{"why": withdrawWhy(p)}, now)
}

// waitReport is how the wait a unit asked for at safe point n stands
// against its act.
func (q *Queue) waitReport(ctx context.Context, v *view, p *corev1.Pod, n spNote, now time.Time) (map[string]any, error) {
	want := map[string]any{}
	_ = json.Unmarshal(n.Wait, &want)
	want["seq"] = n.Seq
	return q.unitControl(ctx, v, p, "wait", bySeq(n.Seq), want, now)
}

// withdrawHolds reports whether the withdrawal pod p ended on is taken:
// not when its act is there and doesn't hold, so the question stands.
func (q *Queue) withdrawHolds(ctx context.Context, v *view, p *corev1.Pod, now time.Time) bool {
	_, err := q.withdrawReport(ctx, v, p, now)
	return err == nil
}
