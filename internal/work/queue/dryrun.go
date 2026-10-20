package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// ErrInvalidJob wraps a draft job its class cannot plan.
var ErrInvalidJob = errors.New("invalid job")

// DryRun is what submitting a job would do, computed without writing
// anything.
type DryRun struct {
	Units int
	// Estimate is the job's remaining work as the queue would plan it:
	// W, its calibrated percentiles and the critical path.
	Estimate estimate.Summary
	// Band is what status.estimate.eta50..eta95 would show.
	Band forecast.Band
	// Deadline is the effective deadline the plan would use, and
	// DeadlineSource where it comes from: spec, suggested or class.
	Deadline       time.Time
	DeadlineSource string
	// Suggestion is set when the farm would suggest the deadline.
	Suggestion       *c4v1.WorkSuggestion
	Feasible, AtRisk bool
	// Message is the scheduler's warning, if any.
	Message string
}

// DryRun plans a job that does not exist yet (project is its resolved
// project) against the farm as the last tick left it: its class's plan,
// its estimate with the project's live calibrations, the deadline it
// would get, and the band it would be shown. It writes and signs
// nothing. It returns ErrNoPlan before the queue has planned, and an
// error wrapping ErrInvalidJob when the job's class cannot plan it.
func (q *Queue) DryRun(ctx context.Context, job *c4v1.WorkJob, project string) (DryRun, error) {
	var out DryRun
	d, err := class.Lookup(job.Spec.Class)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrInvalidJob, err)
	}
	plan, err := d.Plan(job)
	if err == nil {
		plan, err = class.WithReview(job, plan)
	}
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrInvalidJob, err)
	}
	if len(plan.Units) == 0 {
		return out, fmt.Errorf("%w: no units", ErrInvalidJob)
	}

	q.mu.Lock()
	snap, policy, cfg := q.lastSnap, q.Policy, q.Cfg
	if snap.Now.IsZero() || policy == nil {
		q.mu.Unlock()
		return out, ErrNoPlan
	}
	now := snap.Now
	draft := job.DeepCopy()
	if draft.CreationTimestamp.IsZero() {
		draft.CreationTimestamp = metav1.Time{Time: now}
	}
	m := newMem()
	m.plan, m.readyAt = plan, now
	key := draft.Namespace + "/" + draft.Name
	if draft.Name == "" {
		key = draft.Namespace + "/dry-run"
	}
	v := &view{job: draft, key: key, mem: m, driver: d, project: project,
		running: map[int]runInfo{}, finishing: map[int]bool{}, pools: draft.Spec.Pools}
	if len(v.pools) == 0 {
		v.pools = d.Pools()
	}
	v.runnable = append([]int(nil), plan.Order...)
	q.estimate(v, now)
	v.obj = d.Objective(draft, now)
	pol := q.policyForJob(project, draft)
	md := mode(draft, pol)
	adjust := q.adjustFor(v)
	q.mu.Unlock()

	free := 0
	for _, p := range snap.Pools {
		free += max(0, p.Slots-p.Used)
	}
	snap.Jobs = append(append([]sched.Job(nil), snap.Jobs...), schedJob(v, free))
	i := len(snap.Jobs) - 1

	out.Units, out.Estimate = len(plan.Units), v.sum
	out.Deadline, out.DeadlineSource = v.obj.Deadline, "class"
	if draft.Spec.Deadline != nil {
		out.DeadlineSource = "spec"
	}
	if md == modeSuggested {
		c, band := candidateIn(policy, cfg, now, snap, key, now, now, pol, adjust)
		if !c.IsZero() {
			out.Deadline, out.DeadlineSource = c, "suggested"
			st := forecast.Estimating
			if pol.MaxRevisions == 0 {
				st = forecast.Settled
			}
			out.Suggestion = &c4v1.WorkSuggestion{Deadline: metav1.Time{Time: c}, State: string(st), Policy: string(pol.Kind),
				Quantile: strconv.FormatFloat(pol.Quantile, 'f', -1, 64), MaxRevisions: int32(pol.MaxRevisions)}
			if pol.Kind == forecast.Sessions && !band.IsZero() && band.At(pol.Quantile).After(c) {
				out.Suggestion.NextSession = &metav1.Time{Time: pol.NextSession(c.Add(time.Minute))}
			}
		}
	}
	snap.Jobs[i].Deadline = out.Deadline
	var st sched.State
	dec := policy(snap, &st, cfg)
	js := dec.Jobs[key]
	out.Band = forecast.Display(now, v.sum, js.ETA50, js.ETA90, forecast.Tail(plan.Units[0].Slots, js.GroupSlots))
	if adjust != nil {
		out.Band = adjust(now, out.Band)
	}
	if out.Suggestion != nil && pol.Kind == forecast.Sessions && !out.Band.IsZero() && out.Band.At(pol.Quantile).After(out.Deadline) {
		out.Suggestion.NextSession = &metav1.Time{Time: pol.NextSession(out.Deadline.Add(time.Minute))}
	}
	out.Feasible, out.AtRisk = js.Feasible, js.AtRisk
	if js.AtRisk || !js.Feasible {
		out.Message = js.Message
	}
	return out, ctx.Err()
}
