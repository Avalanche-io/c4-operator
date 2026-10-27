package queue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// The forecast of a lead's plan (DESIGN 10.6, A lead's plan; LEAD.md
// stage 3, "The forecast answers"). Every c4_plan call answers with what
// the scheduler forecasts for the lead's job with its edits applied: a
// dry run of the job, as the last tick left it, with the plan documents
// of the calls the queue has not applied yet and then the call's own,
// planned against the farm as that tick saw it, as a submission's dry run
// is (dryrun.go). It writes and signs nothing, and does not wait for the
// queue to apply the call. It answers when the plan is ready (its band),
// its critical path, how many of its units can run at once on the pools
// it uses, and every unit left with no history of its own in the job,
// with where its estimate comes from.
//
// It is the plan's forecast: a held unit counts as released, as it runs
// once its lead releases it, so a planned unit moves the forecast when it
// is added, not when it is released.

// Where a unit's estimate comes from while nothing of its key has run in
// its job (UnitEstimate.From), in the words estimate.Source uses.
const (
	// FromAgentEnv is the runs of the same agent environment and tool
	// (classprior.go).
	FromAgentEnv = "agent-env"
	// FromWork is the estimate its unit carries: a lead's work.
	FromWork = "work"
	// FromEstimate is its job's estimateSeconds.
	FromEstimate = "estimate"
	// FromHistory is its class's runs in the project.
	FromHistory = "history"
	// FromProfile is the previous version of its shot.
	FromProfile = "profile"
	// FromJobMedian is what its job's other units took.
	FromJobMedian = "job-median"
	// FromClassDefault is its class's default: a guess.
	FromClassDefault = "class-default"
)

// ErrBusy is what ForecastPlan returns when a tick holds the queue past
// a moment: the forecast follows on the next tick.
var ErrBusy = errors.New("the queue is planning: the forecast follows on the next tick")

// planForecastWait bounds how long ForecastPlan waits for a tick.
const planForecastWait = time.Second

// PlanForecast is what the queue forecasts for a lead's job, as of AsOf,
// the farm's last look.
type PlanForecast struct {
	AsOf time.Time
	// Estimate is the job's remaining work; Band is when it is ready.
	Estimate estimate.Summary
	Band     forecast.Band
	// Deadline is the deadline it is planned for (zero: none).
	Deadline         time.Time
	Feasible, AtRisk bool
	// Message is the scheduler's warning, or, for an estimate that rests
	// on the class default, says the first units set it (for agent work,
	// forecast.NoWorkerHistory).
	Message string
	// Path is the critical path, first first.
	Path []UnitEstimate
	// Workers is how many of its units can run at once: the slots the
	// scheduler can give it on its pools, in units of its units' size.
	// Slots are those pools' slots, and Pools the pools.
	Workers, Slots int
	Pools          []string
	// Unmeasured are its units left whose key nothing in the job has
	// measured, by index.
	Unmeasured []UnitEstimate
	// Agent says its units are agent work: workers.
	Agent bool
}

// UnitEstimate is one unit in a forecast: its index (New: one the edits
// add, whose index is the dry run's), its name, its predicted time left
// and where that comes from (a From word, or estimate.Source's).
type UnitEstimate struct {
	Index   int
	New     bool
	Name    string
	Seconds float64
	From    string
}

// planForecastMem is the last forecast of a job's plan, kept for the
// tick it was made in.
type planForecastMem struct {
	asOf          time.Time
	sum           [32]byte
	before, after PlanForecast
}

// ForecastPlan forecasts lead's job job with plan documents applied in
// order: before with earlier (the calls the queue has not applied yet),
// after with earlier and then edits (the call's own). With no edits the
// two are one. It waits at most a moment for a tick to end (ErrBusy),
// returns ErrNoPlan before the queue has planned the job, and an error
// wrapping ErrInvalidJob when a document does not apply to it.
func (q *Queue) ForecastPlan(ctx context.Context, job *c4v1.WorkJob, earlier, edits [][]byte) (before, after PlanForecast, err error) {
	if !q.lockSoon(ctx, planForecastWait) {
		return before, after, ErrBusy
	}
	defer q.mu.Unlock()
	m := q.jobs[job.UID]
	snap, policy, cfg := q.lastSnap, q.Policy, q.schedCfg()
	if m == nil || m.last == nil || m.plan == nil || snap.Now.IsZero() || policy == nil {
		return before, after, ErrNoPlan
	}
	// The same documents in the same tick: the forecast made already.
	h := sha256.New()
	for _, d := range earlier {
		fmt.Fprintf(h, "%d:", len(d))
		h.Write(d)
	}
	h.Write([]byte("|"))
	for _, d := range edits {
		fmt.Fprintf(h, "%d:", len(d))
		h.Write(d)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	if f := m.forecast; f != nil && f.asOf.Equal(snap.Now) && f.sum == sum {
		return f.before, f.after, nil
	}
	if before, err = q.forecastWith(m.last, earlier, snap, policy, cfg); err != nil {
		return before, after, err
	}
	after = before
	if len(edits) > 0 {
		if after, err = q.forecastWith(m.last, append(slices.Clone(earlier), edits...), snap, policy, cfg); err != nil {
			return before, after, err
		}
	}
	m.forecast = &planForecastMem{asOf: snap.Now, sum: sum, before: before, after: after}
	return before, after, ctx.Err()
}

// lockSoon takes the queue's lock, waiting at most d for a tick to end.
func (q *Queue) lockSoon(ctx context.Context, d time.Duration) bool {
	until := time.Now().Add(d)
	for !q.mu.TryLock() {
		if time.Now().After(until) || ctx.Err() != nil {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// forecastWith is the forecast of job v, as its last tick left it, with
// docs applied, against snap. It changes nothing of the job's: the dry
// run plans a copy of its memory and its table. Called with mu held.
func (q *Queue) forecastWith(v *view, docs [][]byte, snap sched.Snapshot, policy sched.Policy, cfg sched.Config) (PlanForecast, error) {
	now := snap.Now
	m := *v.mem
	m.plan = v.mem.plan.Clone()
	m.calPts = slices.Clip(m.calPts)
	w := *v
	w.mem = &m
	known := len(m.plan.Units)
	for n, doc := range docs {
		e, err := class.ReadPlan(v.job, appendParent(m.plan), doc, m.plan, "", w.started)
		if err != nil {
			return PlanForecast{}, fmt.Errorf("%w: plan call %d of %d: %v", ErrInvalidJob, n+1, len(docs), err)
		}
		m.plan.ApplyPlan(appendRoot, e)
	}
	for i := range m.plan.Units {
		m.plan.Units[i].Held = false
	}
	w.runnable, w.waiting, w.stuck = unitWaits(m.plan, &m, &w)
	q.estimate(&w, now)

	free := 0
	for _, p := range snap.Pools {
		free += max(0, p.Slots-p.Used)
	}
	sj := schedJob(&w, free)
	jobs := make([]sched.Job, 0, len(snap.Jobs)+1)
	for _, j := range snap.Jobs {
		if j.ID != v.key {
			jobs = append(jobs, j)
		}
	}
	snap.Jobs = append(jobs, sj)
	var st sched.State
	dec := policy(snap, &st, cfg)
	js := dec.Jobs[v.key]

	plan := m.plan
	out := PlanForecast{AsOf: now, Estimate: w.sum, Deadline: sj.Deadline, Feasible: js.Feasible, AtRisk: js.AtRisk, Pools: slices.Clone(w.pools)}
	out.Band = forecast.Display(now, w.sum, js.ETA50, js.ETA90, forecast.Tail(plan.Units[0].Slots, js.GroupSlots))
	if adj := q.adjustFor(&w); adj != nil {
		out.Band = adj(now, out.Band)
	}
	for _, p := range snap.Pools {
		if slices.Contains(w.pools, p.Name) {
			out.Slots += p.Slots
		}
	}
	out.Workers = int(math.Floor(js.Attainable / float64(max(1, sj.SlotsPerUnit))))
	unit := func(i int) UnitEstimate {
		u := plan.Units[i]
		return UnitEstimate{Index: i, New: i >= known, Name: u.Name, Seconds: w.rem[i]}
	}
	for _, i := range pathUnits(plan, &m, w.rem, func(key string) float64 { return w.est.Generator(key).Longest }) {
		out.Path = append(out.Path, unit(i))
	}
	idxs := make([]int, 0, len(w.rem))
	for i := range w.rem {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		u := plan.Units[i]
		out.Agent = out.Agent || agentKey(v.job, u) != ""
		k := w.estKey(i)
		if isWait(plan, i) || w.est.Count(k) > 0 {
			continue
		}
		ue := unit(i)
		ue.From = estimateFrom(w.est.Predict(k, u.Pos).Source, w.priorFrom[k])
		out.Unmeasured = append(out.Unmeasured, ue)
	}
	// The message is a warning, or says the estimate is a guess, as a
	// dry run's; agent work's units are its workers.
	if js.AtRisk || !js.Feasible || js.NoHistory {
		out.Message = js.Message
	}
	if out.Agent {
		out.Message = strings.ReplaceAll(out.Message, forecast.NoHistory(v.job.Spec.Class), forecast.NoWorkerHistory)
	}
	return out, nil
}

// estimateFrom is where a prediction of source src comes from, a key's
// prior being from prior.
func estimateFrom(src estimate.Source, prior string) string {
	switch src {
	case estimate.History:
		if prior != "" {
			return prior
		}
		return FromHistory
	case estimate.Profile:
		return FromProfile
	case estimate.JobMedian:
		return FromJobMedian
	case estimate.ClassDefault:
		return FromClassDefault
	}
	return src.String()
}

// pathUnits is the critical path criticalPath measures, first first: the
// chain of units left whose remaining work (rem), with what they wait on,
// is longest. Ties go to the lower index.
func pathUnits(plan *class.Plan, m *jobMem, rem map[int]float64, genLongest func(key string) float64) []int {
	memo := map[int]float64{}
	prev := map[int]int{}
	seen := map[int]bool{}
	var path func(i int) float64
	path = func(i int) float64 {
		if d, ok := memo[i]; ok {
			return d
		}
		if seen[i] {
			return 0
		}
		seen[i] = true
		u := plan.Units[i]
		d := rem[i]
		if u.Generator && !m.expanded(i) {
			d += genLongest(u.Key)
		}
		longest, from := 0.0, -1
		for _, a := range slices.Sorted(slices.Values(u.After)) {
			if a < 0 || a >= len(plan.Units) || !m.waitsOn(i, a) {
				continue
			}
			if l := path(a); from < 0 || l > longest {
				longest, from = l, a
			}
		}
		if _, left := rem[from]; from >= 0 && left {
			prev[i] = from
		}
		memo[i] = d + longest
		return memo[i]
	}
	last, t := -1, 0.0
	idxs := make([]int, 0, len(rem))
	for i := range rem {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		if d := path(i); last < 0 || d > t {
			last, t = i, d
		}
	}
	var out []int
	for i, ok := last, last >= 0; ok; i, ok = prev[i] {
		out = append(out, i)
		if len(out) > len(plan.Units) {
			break
		}
	}
	slices.Reverse(out)
	return out
}
