package queue

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/order"
	"github.com/Avalanche-io/c4-operator/internal/work/sched"
)

// statusEvery bounds how often a job's status is rewritten when only its
// estimate moved.
const statusEvery = 10 * time.Second

// report writes each job's status, then deletes the terminal pods that
// status now records. A pod is deleted only after the write succeeds, so a
// restart never loses a finished unit.
func (q *Queue) report(ctx context.Context, now time.Time, dec sched.Decisions, views []*view) {
	for _, v := range views {
		if o := v.mem.out; o != nil && o.dirty && q.Stores != nil {
			if store, err := q.Stores(ctx, v.project); err == nil && store != nil {
				if err := o.saveIndex(ctx, store, v.key); err != nil {
					q.Log.Error(err, "save provenance index", "job", v.key)
				}
			}
		}
		st := q.status(now, dec, v)
		old := v.job.Status
		changed := !reflect.DeepEqual(withoutEstimate(old), withoutEstimate(st))
		if !changed && now.Sub(v.mem.wrote) < statusEvery {
			q.cleanup(ctx, v)
			continue
		}
		if !changed && reflect.DeepEqual(old, st) {
			q.cleanup(ctx, v)
			continue
		}
		patch := client.MergeFrom(v.job.DeepCopy())
		v.job.Status = st
		if err := q.Client.Status().Patch(ctx, v.job, patch); err != nil {
			q.Log.Error(err, "job status", "job", v.key)
			continue
		}
		v.mem.wrote = now
		q.cleanup(ctx, v)
	}
}

func (q *Queue) cleanup(ctx context.Context, v *view) {
	for _, p := range v.done {
		if err := q.Client.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			q.Log.V(1).Info("delete finished unit pod", "pod", p.Name, "err", err.Error())
		}
	}
}

func withoutEstimate(st c4v1.WorkJobStatus) c4v1.WorkJobStatus {
	st.Estimate = nil
	st.Rank = 0
	st.Conditions = nil
	return st
}

// status computes a job's status from the queue's memory and the latest
// decision.
func (q *Queue) status(now time.Time, dec sched.Decisions, v *view) c4v1.WorkJobStatus {
	st := c4v1.WorkJobStatus{ObservedGeneration: v.job.Generation, Conditions: v.job.Status.Conditions}
	m := v.mem
	if m.planErr != nil || m.plan == nil {
		st.Phase = "Failed"
		msg := "no plan"
		if m.planErr != nil {
			msg = m.planErr.Error()
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Planned", Status: metav1.ConditionFalse,
			Reason: "InvalidParams", Message: msg, ObservedGeneration: v.job.Generation})
		return st
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Planned", Status: metav1.ConditionTrue,
		Reason: "Planned", Message: fmt.Sprintf("%d units, %s order", len(m.plan.Units), m.plan.OrderName), ObservedGeneration: v.job.Generation})
	if v.unschedulable != "" {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Schedulable", Status: metav1.ConditionFalse,
			Reason: "NoFit", Message: v.unschedulable, ObservedGeneration: v.job.Generation})
	} else {
		meta.RemoveStatusCondition(&st.Conditions, "Schedulable")
	}
	n := len(m.plan.Units)
	st.Order = strings.ToLower(m.plan.OrderName)
	var succ, fail, run, retry, queued []int
	for i := 0; i < n; i++ {
		_, running := v.running[i]
		switch {
		case m.succeeded[i]:
			succ = append(succ, i)
		case m.failed[i]:
			fail = append(fail, i)
		case running || v.finishing[i]:
			run = append(run, i)
		case m.attempts[i] > 0:
			retry = append(retry, i)
		default:
			queued = append(queued, i)
		}
	}
	held := v.job.Spec.Nudge.Hold || v.job.Spec.Nudge.HoldStop
	st.Units = c4v1.UnitCounts{Total: int32(n), Succeeded: int32(len(succ)), Failed: int32(len(fail)),
		Running: int32(len(run)), Retrying: int32(len(retry)), Queued: int32(len(queued))}
	switch {
	case v.blocked:
		st.Units.Blocked, st.Units.Queued = int32(len(queued)+len(retry)), 0
	case held:
		st.Units.Held, st.Units.Queued = int32(len(queued)), 0
	}
	st.UnitStates = map[string]string{}
	for name, set := range map[string][]int{"succeeded": succ, "failed": fail, "running": run, "retrying": retry, "queued": queued} {
		if len(set) > 0 {
			st.UnitStates[name] = order.EncodeSet(set)
		}
	}
	if v.blocked && len(queued) > 0 {
		st.UnitStates["blocked"] = st.UnitStates["queued"]
		delete(st.UnitStates, "queued")
	}
	var att []string
	for i := 0; i < n; i++ {
		if a := m.attempts[i]; a > 1 {
			att = append(att, strconv.Itoa(i)+":"+strconv.Itoa(a))
		}
	}
	st.Attempts = strings.Join(att, ",")
	if len(m.millis) > 0 {
		st.UnitMillis = make([]int64, n)
		for i, ms := range m.millis {
			if i < n {
				st.UnitMillis[i] = ms
			}
		}
	}
	if o := m.out; o != nil && !o.view.IsNil() {
		st.Output = &c4v1.WorkOutput{View: o.view.String(), Chain: o.id.String(), Changesets: int32(o.count)}
		if !o.index.IsNil() {
			st.Output.Provenance = o.index.String()
		}
		if n := len(o.conflicts); n > 0 {
			st.Output.Conflicts = o.conflicts[max(0, n-8):]
		}
	} else if old := v.job.Status.Output; old != nil {
		st.Output = old
	}
	if len(m.resume) > 0 {
		st.Resume = map[string]string{}
		for i, view := range m.resume {
			st.Resume[strconv.Itoa(i)] = view
		}
	}
	for _, y := range m.yields[max(0, len(m.yields)-8):] {
		st.Yields = append(st.Yields, c4v1.WorkYield{Unit: int32(y.Unit), At: metav1.Time{Time: y.At.Truncate(time.Second)}, Reason: y.Reason, View: y.View})
	}
	for _, n := range m.preemptions {
		st.Preemptions += int32(n)
	}
	st.Requeued = m.requeued
	for node := range v.avoid {
		st.Blacklist = append(st.Blacklist, node)
	}
	sort.Strings(st.Blacklist)
	if !v.obj.Deadline.IsZero() {
		st.Deadline = &metav1.Time{Time: v.obj.Deadline}
	}
	if !m.started.IsZero() {
		st.Started = &metav1.Time{Time: m.started.Truncate(time.Second)}
	}

	switch {
	case len(succ) == n:
		st.Phase = "Succeeded"
	case len(succ)+len(fail) == n && len(run) == 0:
		st.Phase = "Failed"
	case v.blocked:
		st.Phase = "Blocked"
	case held && len(run) == 0:
		st.Phase = "Held"
	case len(run) > 0 || len(succ) > 0 || len(fail) > 0:
		st.Phase = "Running"
	default:
		st.Phase = "Queued"
	}
	if st.Phase == "Succeeded" || st.Phase == "Failed" {
		if m.finished.IsZero() {
			m.finished = now
		}
		st.Finished = &metav1.Time{Time: m.finished.Truncate(time.Second)}
		return st
	}

	js, ok := dec.Jobs[v.key]
	if !ok {
		return st
	}
	st.Rank = int32(js.Rank)
	e := &c4v1.WorkEstimate{
		RemainingWork: int64(v.sum.W), RemainingWorkP90: int64(v.sum.W90),
		RequiredSlots: fmt.Sprintf("%.2f", js.Required), AttainableSlots: fmt.Sprintf("%.2f", js.Attainable),
		RunningSlots: int32(js.RunningSlots), SlackSeconds: int64(js.Slack.Seconds()),
		Feasible: js.Feasible, AtRisk: js.AtRisk,
	}
	// The message is a warning: kept only when the job is at risk.
	if js.AtRisk || !js.Feasible {
		e.Message = js.Message
	}
	if !js.ETA50.IsZero() {
		e.ETA50 = &metav1.Time{Time: js.ETA50.Truncate(time.Second)}
		e.RemainingSeconds = int64(js.ETA50.Sub(now).Seconds())
	}
	if !js.ETA90.IsZero() {
		e.ETA90 = &metav1.Time{Time: js.ETA90.Truncate(time.Second)}
		e.P90RemainingSeconds = int64(js.ETA90.Sub(now).Seconds())
	}
	if v.est != nil && len(m.plan.Units) > 0 {
		// Positions are offsets along the axis (frames past the first).
		key, span := m.plan.Units[0].Key, m.plan.Units[n-1].Pos+1
		for _, r := range v.est.Heavy(key, span) {
			e.HeavyRegions = append(e.HeavyRegions, c4v1.HeavyRegion{
				From: int32(m.plan.Axis.Start + r.Lo), To: int32(m.plan.Axis.Start + r.Hi), Factor: fmt.Sprintf("%.1f", r.Factor)})
		}
	}
	st.Estimate = e
	return st
}
