package queue

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/order"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// statusEvery bounds how often a job's status is rewritten when only its
// estimate moved.
const statusEvery = 10 * time.Second

// report writes each job's status, then deletes the terminal pods that
// status now records. A pod is deleted only after the write succeeds, so a
// restart never loses a finished unit.
func (q *Queue) report(ctx context.Context, now time.Time, dec sched.Decisions, views []*view) {
	sts := make([]c4v1.WorkJobStatus, len(views))
	var ts []api.Transition
	for i, v := range views {
		if o := v.mem.out; o != nil && o.dirty && q.Stores != nil {
			if store, err := q.Stores(ctx, v.project); err == nil && store != nil {
				if err := o.saveIndex(ctx, store, v.key); err != nil {
					q.Log.Error(err, "save provenance index", "job", v.key)
				}
			}
		}
		sts[i] = q.status(now, dec, v)
		ts = append(ts, transitions(v.key, v.project, &v.job.Status, &sts[i], now)...)
	}
	// The log first: a change is never in a status without being in it
	// (book.go).
	if err := q.appendBook(ctx, ts); err != nil {
		q.Log.Error(err, "transition log: statuses wait for the next tick")
		return
	}
	for i, v := range views {
		st := sts[i]
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
		if err := q.release(ctx, p); err != nil {
			q.Log.V(1).Info("release finished unit pod", "pod", p.Name, "err", err.Error())
			continue
		}
		if p.DeletionTimestamp != nil {
			continue
		}
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
	if v.planWait != "" && m.plan == nil {
		// The unit table waits on the store (graph.go): keep the last
		// status, and say so.
		st = *v.job.Status.DeepCopy()
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Planned", Status: metav1.ConditionUnknown,
			Reason: "LoadingExpansions", Message: v.planWait, ObservedGeneration: v.job.Generation})
		return st
	}
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
	if m.toolsWait != "" {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Tools", Status: metav1.ConditionFalse,
			Reason: "Waiting", Message: m.toolsWait, ObservedGeneration: v.job.Generation})
	} else {
		meta.RemoveStatusCondition(&st.Conditions, "Tools")
	}
	n := len(m.plan.Units)
	st.Order = strings.ToLower(m.plan.OrderName)
	// Each unit is in one state. Units holding a slot are preflight,
	// running, retrying or egress; between attempts, backoff. A generator
	// that has not run is planned; a unit waiting on others of the job is
	// blocked (graph.go).
	isWaiting := map[int]bool{}
	for _, i := range v.waiting {
		isWaiting[i] = true
	}
	var lockedU []int
	var succ, egress, fail, flag, accepted, pre, run, retry, backoff, queued, planned, blockedU, waits, superseded []int
	for i := 0; i < n; i++ {
		_, running := v.running[i]
		switch {
		case m.superseded[i]:
			superseded = append(superseded, i)
		case !running && !m.succeeded[i] && !m.failed[i] && !v.finishing[i] && m.attempts[i] == 0 && m.plan.Units[i].Generator && !m.expanded(i):
			planned = append(planned, i)
		case v.openWaits[i]:
			waits = append(waits, i) // waits for someone or something (wait.go)
		case hasLock(v, i):
			lockedU = append(lockedU, i)
		case isWaiting[i]:
			blockedU = append(blockedU, i)
		case m.succeeded[i]:
			succ = append(succ, i)
		case m.accepted[i]:
			accepted = append(accepted, i)
		case m.flagged(i):
			flag = append(flag, i)
		case m.failed[i]:
			fail = append(fail, i)
		case v.finishing[i]:
			egress = append(egress, i)
		case running && v.preflight[i]:
			pre = append(pre, i)
		case running && m.attempts[i] > 1:
			retry = append(retry, i)
		case running:
			run = append(run, i)
		case m.attempts[i] > 0:
			backoff = append(backoff, i)
		default:
			queued = append(queued, i)
		}
	}
	held := v.job.Spec.Nudge.Hold || v.job.Spec.Nudge.HoldStop
	st.Units = c4v1.UnitCounts{Total: int32(n), Succeeded: int32(len(succ)), Egress: int32(len(egress)),
		Failed: int32(len(fail)), Accepted: int32(len(accepted)), Preflight: int32(len(pre)),
		Running: int32(len(run)), Retrying: int32(len(retry)), Backoff: int32(len(backoff)), Queued: int32(len(queued)),
		Planned: int32(len(planned)), Blocked: int32(len(blockedU)), Flagged: int32(len(flag)), Waits: int32(len(waits)),
		Locked: int32(len(lockedU)), Superseded: int32(len(superseded))}
	switch {
	case v.blocked:
		st.Units.Blocked, st.Units.Queued, st.Units.Backoff = int32(len(queued)+len(backoff)+len(blockedU)), 0, 0
	case held:
		st.Units.Held, st.Units.Queued = int32(len(queued)), 0
	}
	st.UnitStates = map[string]string{}
	for name, set := range map[string][]int{"succeeded": succ, "egress": egress, "failed": fail, "accepted": accepted,
		"preflight": pre, "running": run, "retrying": retry, "backoff": backoff, "queued": queued,
		"planned": planned, "blocked": blockedU, "flagged": flag, "waits": waits, "locked": lockedU, "superseded": superseded} {
		if len(set) > 0 {
			st.UnitStates[name] = order.EncodeSet(set)
		}
	}
	if v.blocked && len(queued)+len(backoff) > 0 {
		st.UnitStates["blocked"] = order.EncodeSet(slices.Sorted(slices.Values(append(append(append([]int{}, queued...), backoff...), blockedU...))))
		delete(st.UnitStates, "queued")
		delete(st.UnitStates, "backoff")
	}
	st.Acked = m.acked
	st.Acks = m.ackStatus()
	st.Decided, st.Waits = m.waitRecs().decided, waitStatus(m, now)
	st.Rounds = roundsStatus(m.plan, m, v.roundsExp)
	st.Steered, st.Steers = m.steered, steerStatus(m)
	st.Supplied, st.Needs = m.supplied, needsStatus(m)
	st.Asks = askStatus(m)
	taken := map[int]bool{}
	for _, a := range st.Acks {
		for _, i := range decode(a.Units) {
			taken[i] = true
		}
	}
	waiting := 0
	for _, i := range fail {
		// a unit that stopped for information is its own kind of stop
		if !taken[i] && !m.openNeeds(i) {
			waiting++
		}
	}
	flaggedWaiting := 0
	for _, i := range flag {
		if !taken[i] {
			flaggedWaiting++
		}
	}
	st.Measure = measureStatus(v)
	st.Reused, st.Units.Reused = reusedStatus(m)
	st.Spend, st.ModelUsage = spendStatus(v)
	short := measureShort(v, st.Measure)
	v.measureShort = ""
	if short && len(succ)+len(accepted) == n && len(pre)+len(run)+len(retry)+len(egress) == 0 {
		v.measureShort = measureMessage(st.Measure)
	}
	needsPerson(&st, v, q.approvals[v.key], waiting, flaggedWaiting, v.job.Generation)
	active := len(pre) + len(run) + len(retry) + len(egress)
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
	for _, d := range m.decisions[max(0, len(m.decisions)-16):] {
		st.Decisions = append(st.Decisions, c4v1.WorkDecision{Unit: int32(d.Unit), At: metav1.Time{Time: d.At.Truncate(time.Second)},
			Kind: d.Kind, Reason: d.Reason, For: d.For, Record: d.Record})
	}
	st.Expansions = expansionStatus(m.expansions)
	st.Holds = holdsStatus(v)
	st.Gates = v.gates
	st.Verdicts = verdictStatus(m)
	st.Reviews = reviewStatus(m)
	for i := range readers(m.plan) {
		if view := m.unitOut[i]; view != "" {
			if st.UnitOutputs == nil {
				st.UnitOutputs = map[string]string{}
			}
			st.UnitOutputs[strconv.Itoa(i)] = view
		}
	}
	for _, y := range m.yields[max(0, len(m.yields)-8):] {
		st.Yields = append(st.Yields, c4v1.WorkYield{Unit: int32(y.Unit), At: metav1.Time{Time: y.At.Truncate(time.Second)}, Reason: y.Reason, View: y.View})
	}
	st.Interruptions, st.Interrupted = interruptStatus(m)
	st.Causes, st.CausesDropped = m.causeStatus(), int32(m.causesDropped)
	st.Preemptions = int32(m.preemptBase)
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
	st.DeadlineSource, st.Milestone = v.source, v.milestone
	st.Suggestion = suggestionStatus(m.sugg)
	st.Derivation = q.derivationStatus(v)
	st.InputSubstitutes = substituteStatus(m)
	st.Procedural = q.proceduralCandidates(context.Background(), v)
	if !m.started.IsZero() {
		st.Started = &metav1.Time{Time: m.started.Truncate(time.Second)}
	}

	// Accepted and superseded units count as done: nothing waits on them.
	finished := len(succ) + len(accepted) + len(superseded)
	switch {
	case isForecast(v.job):
		// It never runs: a real job replaces it (forecast.go).
		st.Phase = "Forecast"
	case finished == n && short && active == 0:
		// Every unit finished, short of the measure the job requires.
		st.Phase = "Failed"
	case finished == n:
		st.Phase = "Succeeded"
	case finished+len(fail)+len(flag)+len(v.stuck) == n && active == 0:
		// Units waiting on a failed one count with it: they cannot run
		// until a person acts.
		st.Phase = "Failed"
	case v.blocked:
		st.Phase = "Blocked"
	case held && active == 0:
		st.Phase = "Held"
	case active > 0 || len(succ) > 0 || len(fail) > 0 || len(accepted) > 0:
		st.Phase = "Running"
	default:
		st.Phase = "Queued"
	}
	if st.Phase == "Succeeded" || st.Phase == "Failed" {
		if m.finished.IsZero() {
			m.finished = now
		}
		if st.Phase == "Succeeded" {
			q.learn(v)
			q.learnFinish(v)
		}
		st.Finished = &metav1.Time{Time: m.finished.Truncate(time.Second)}
		// The queue seals the job: a signed record of its output and of the
		// decisions that touched it.
		q.sealJob(context.Background(), v, &st, now)
		if st.Output != nil && m.jobRecord != "" {
			st.Output.Record = m.jobRecord
		}
		// A finished job keeps what its units cost: the heavy ranges.
		if hr := q.heavy(v); len(hr) > 0 {
			st.Estimate = &c4v1.WorkEstimate{HeavyRegions: hr}
		}
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
		RunningSlots: int32(js.RunningSlots),
		Feasible:     js.Feasible, AtRisk: js.AtRisk,
	}
	// The message is a warning: kept only when the job is at risk.
	if js.AtRisk || !js.Feasible {
		e.Message = js.Message
	}
	// What a person is shown: the calibrated band with half the tail
	// (forecast.Display), not the scheduler's conservative finish.
	raw := forecast.Display(now, v.sum, js.ETA50, js.ETA90, forecast.Tail(v.mem.plan.Units[0].Slots, js.GroupSlots))
	if m := v.mem; m.plan != nil {
		m.noteFinish(now, len(m.millis), len(m.plan.Units), raw.P50)
	}
	if b := q.shown(v, now, raw); !b.IsZero() {
		t := func(x time.Time) *metav1.Time { return &metav1.Time{Time: x.Truncate(time.Second)} }
		e.ETA50, e.ETA80, e.ETA90, e.ETA95 = t(b.P50), t(b.P80), t(b.P90), t(b.P95)
		e.RemainingSeconds = int64(b.P50.Sub(now).Seconds())
		e.P90RemainingSeconds = int64(b.P90.Sub(now).Seconds())
	}
	e.Calibrated = v.sum.Calibrated
	e.HeavyRegions = q.heavy(v)
	e.Risk = risk(now, v, js, e)
	// js.Slack (at all attainable capacity) only decides Feasible; the
	// slack shown is against the shown band, and unset without one
	if sl, ok := shownSlack(v, js, e); ok {
		e.SlackSeconds = sl
	}
	st.Estimate = e
	return st
}

// heavy lists the job's heavy ranges in axis values (frames).
func (q *Queue) heavy(v *view) []c4v1.HeavyRegion {
	m := v.mem
	if v.est == nil || m.plan == nil || len(m.plan.Units) == 0 {
		return nil
	}
	var out []c4v1.HeavyRegion
	// Positions are offsets along the axis (frames past the first).
	key, span := m.plan.Units[0].Key, m.plan.Units[len(m.plan.Units)-1].Pos+1
	for _, r := range v.est.Heavy(key, span) {
		out = append(out, c4v1.HeavyRegion{
			From: int32(m.plan.Axis.Start + r.Lo), To: int32(m.plan.Axis.Start + r.Hi), Factor: fmt.Sprintf("%.1f", r.Factor)})
	}
	return out
}
