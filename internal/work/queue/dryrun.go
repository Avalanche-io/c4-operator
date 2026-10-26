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
	"github.com/Avalanche-io/c4-operator/work/process"
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
	// Reused is how many of its units would reuse an earlier run's output
	// rather than run (at most: a share is run anyway to verify).
	Reused int
}

// DryRun plans a job that does not exist yet (project is its resolved
// project) against the farm as the last tick left it: its class's plan,
// its estimate with the project's live calibrations, the deadline it
// would get, and the band it would be shown. It writes and signs
// nothing. It returns ErrNoPlan before the queue has planned, and an
// error wrapping ErrInvalidJob when the job's class cannot plan it.
func (q *Queue) DryRun(ctx context.Context, job *c4v1.WorkJob, project string) (DryRun, error) {
	out, err := q.DryRunAll(ctx, []*c4v1.WorkJob{job}, project, nil)
	if err != nil {
		return DryRun{}, err
	}
	return out[0], nil
}

// DryRunAll plans drafts together, as DryRun plans one: each after the
// drafts and jobs its dependsOn names, all competing for the same slots,
// so a chain's last job is ready when the chain is. views are the view
// IDs of the drafts' input Materializations by name, which do not exist
// yet, for saying how many units reuse would skip.
func (q *Queue) DryRunAll(ctx context.Context, jobs []*c4v1.WorkJob, project string, views map[string]string) ([]DryRun, error) {
	return q.DryRunTables(ctx, jobs, project, views, nil)
}

// DryRunTables is DryRunAll for drafts whose unit tables are not stored
// yet: tables are the documents their table params name, by C4 ID.
func (q *Queue) DryRunTables(ctx context.Context, jobs []*c4v1.WorkJob, project string, views map[string]string, tables map[string][]byte) ([]DryRun, error) {
	type draftRun struct {
		v    *view
		plan *class.Plan
		d    class.Driver
		pol  forecast.Policy
		md   string
		adj  func(time.Time, forecast.Band) forecast.Band
	}
	runs := make([]draftRun, len(jobs))
	for n, job := range jobs {
		d, err := class.Lookup(job.Spec.Class)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidJob, err)
		}
		var plan *class.Plan
		switch f := job.Spec.Forecast; {
		case f != nil && f.SlotHours != "":
			// A forecast is planned as the forecast pass plans it
			// (forecast.go).
			plan, err = forecastPlan(job)
		case f != nil:
			if plan, err = d.Plan(job); err == nil {
				forecastUnits(plan, f)
			}
		default:
			plan, err = d.Plan(job)
			var nt *class.NeedsTable
			if errors.As(err, &nt) {
				// a draft's table as given, else one already stored
				doc, ok := tables[nt.ID]
				if !ok {
					var derr error
					if doc, derr = q.expansionDoc(ctx, project, newMem(), nt.ID); derr != nil {
						return nil, fmt.Errorf("%w: its unit table: %v", ErrInvalidJob, derr)
					}
				}
				plan, err = class.TablePlan(job, doc)
			}
			if err == nil {
				plan, err = class.WithReview(job, plan)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidJob, err)
		}
		if len(plan.Units) == 0 {
			return nil, fmt.Errorf("%w: no units", ErrInvalidJob)
		}
		runs[n] = draftRun{plan: plan, d: d}
	}

	q.mu.Lock()
	snap, policy, cfg := q.lastSnap, q.Policy, q.schedCfg()
	if snap.Now.IsZero() || policy == nil {
		q.mu.Unlock()
		return nil, ErrNoPlan
	}
	now := snap.Now
	for n, job := range jobs {
		r := &runs[n]
		draft := job.DeepCopy()
		if draft.CreationTimestamp.IsZero() {
			draft.CreationTimestamp = metav1.Time{Time: now}
		}
		m := newMem()
		m.plan, m.readyAt = r.plan, now
		key := draft.Namespace + "/" + draft.Name
		if draft.Name == "" {
			key = draft.Namespace + "/dry-run-" + strconv.Itoa(n)
		}
		v := &view{job: draft, key: key, mem: m, driver: r.d, project: project,
			running: map[int]runInfo{}, finishing: map[int]bool{}, pools: draft.Spec.Pools}
		if len(v.pools) == 0 {
			v.pools = r.d.Pools()
		}
		v.pools, v.unschedulable = q.servingPools(draft, v.pools)
		if v.unschedulable != "" {
			q.mu.Unlock()
			return nil, fmt.Errorf("%w: %s", ErrInvalidJob, v.unschedulable)
		}
		v.runnable = append([]int(nil), r.plan.Order...)
		// A forecast waits for its work to come: planned, never released.
		v.blocked = isForecast(draft)
		for _, up := range draft.Spec.DependsOn {
			v.after = append(v.after, draft.Namespace+"/"+up)
		}
		q.estimate(v, now)
		v.obj = r.d.Objective(draft, now)
		r.v, r.pol = v, q.policyForJob(project, draft)
		r.md, r.adj = mode(draft, r.pol), q.adjustFor(v)
	}
	reused := make([]int, len(runs))
	for n := range runs {
		reused[n] = q.wouldReuse(runs[n].v, views)
	}
	q.mu.Unlock()

	free := 0
	for _, p := range snap.Pools {
		free += max(0, p.Slots-p.Used)
	}
	base := len(snap.Jobs)
	snap.Jobs = append([]sched.Job(nil), snap.Jobs...)
	for _, r := range runs {
		sj := schedJob(r.v, free)
		if f := r.v.job.Spec.Forecast; f != nil && f.Start != nil {
			sj.NotBefore = f.Start.Time
		}
		snap.Jobs = append(snap.Jobs, sj)
	}

	out := make([]DryRun, len(runs))
	for n, r := range runs {
		v, o := r.v, &out[n]
		o.Units, o.Estimate, o.Reused = len(r.plan.Units), v.sum, reused[n]
		o.Deadline, o.DeadlineSource = v.obj.Deadline, "class"
		if v.job.Spec.Deadline != nil {
			o.DeadlineSource = "spec"
		}
		if r.md == modeSuggested && !isForecast(v.job) {
			c, band := candidateIn(policy, cfg, now, snap, v.key, now, now, r.pol, r.adj)
			if !c.IsZero() {
				o.Deadline, o.DeadlineSource = c, "suggested"
				st := forecast.Estimating
				if r.pol.MaxRevisions == 0 {
					st = forecast.Settled
				}
				o.Suggestion = &c4v1.WorkSuggestion{Deadline: metav1.Time{Time: c}, State: string(st), Policy: string(r.pol.Kind),
					Quantile: strconv.FormatFloat(r.pol.Quantile, 'f', -1, 64), MaxRevisions: int32(r.pol.MaxRevisions)}
				if r.pol.Kind == forecast.Sessions && !band.IsZero() && band.At(r.pol.Quantile).After(c) {
					o.Suggestion.NextSession = &metav1.Time{Time: r.pol.NextSession(c.Add(time.Minute))}
				}
			}
		}
		snap.Jobs[base+n].Deadline = o.Deadline
	}
	var st sched.State
	dec := policy(snap, &st, cfg)
	for n, r := range runs {
		v, o := r.v, &out[n]
		js := dec.Jobs[v.key]
		o.Band = forecast.Display(now, v.sum, js.ETA50, js.ETA90, forecast.Tail(r.plan.Units[0].Slots, js.GroupSlots))
		if r.adj != nil {
			o.Band = r.adj(now, o.Band)
		}
		if o.Suggestion != nil && r.pol.Kind == forecast.Sessions && !o.Band.IsZero() && o.Band.At(r.pol.Quantile).After(o.Deadline) {
			o.Suggestion.NextSession = &metav1.Time{Time: r.pol.NextSession(o.Deadline.Add(time.Minute))}
		}
		o.Feasible, o.AtRisk = js.Feasible, js.AtRisk
		// The message is a warning, or says the estimate is a guess.
		if js.AtRisk || !js.Feasible || js.NoHistory {
			o.Message = js.Message
		}
	}
	return out, ctx.Err()
}

// wouldReuse is how many of a draft's units reuse would skip, as
// reuseUnits would find them once it runs: an exact job's procedural
// units whose computation ran before on the same input views. A draft
// whose inputs are not all known views (another job's output) reuses
// none that can be said yet. Called with mu held.
func (q *Queue) wouldReuse(v *view, views map[string]string) int {
	b := q.bookOf(v.project)
	if b == nil || b.Len() == 0 || v.driver == nil || v.blocked ||
		process.ParseClass(v.job.Annotations[AnnotationDerivationClass]) != process.Exact {
		return 0
	}
	var inputs []process.Input
	for _, in := range v.job.Spec.Inputs {
		id := views[in.Materialization]
		if in.FromJob != "" || id == "" {
			return 0
		}
		inputs = append(inputs, process.Input{Mount: in.MountPath, View: id})
	}
	inID, ok := process.InputsID(inputs)
	if !ok || inID == "" {
		return 0
	}
	plan, n := v.mem.plan, 0
	for _, idx := range v.runnable {
		u := plan.Units[idx]
		if class.DoesOf(v.driver, u) != class.DoesProcedure || u.Generator || u.Gate != "" || len(u.Inputs) > 0 ||
			isWait(plan, idx) || u.Reasoning.Tools != "" {
			continue
		}
		cs := unitContainers(v.job, u, q.digests)
		proc, ok := process.ProcessID(cs, q.lastPlatform[v.key])
		if !ok {
			continue
		}
		program, _ := process.ProgramID(cs, q.lastPlatform[v.key], perUnit(u))
		if !b.Has(program) {
			continue
		}
		if _, ok := b.ReuseOf(process.Key{Process: proc, Inputs: inID}, program); ok {
			n++
		}
	}
	return n
}
