package queue

// Forecast jobs (DESIGN.md 10.12, Forecast jobs). A job with spec.forecast
// exists only as a forecast: work a tracker expects (a shot not yet turned
// over, its bid days) that the farm should plan for. The queue observes and
// estimates it but never releases it: it is left out of the live decision,
// so it creates no pods, reserves no quota or fair share, moves no real
// job's ETA and buys no burst capacity; the release loop refuses one
// besides. Nothing a person or a unit does to a real job applies to it (no
// reuse, no review, no waits), and nothing suggests its deadline, learns
// from it or seals it.
//
// After the live pass, at most every forecastEvery or when the forecast
// jobs change, a forecast pass plans the farm again with them added, as
// waiting work that begins at its start, against a throwaway state. Its
// results go only to the forecast jobs' status (phase Forecast, the band,
// the risk, the slots it would need), to Milestones' forecastRisk, and to
// the farm's forecast (Queue.Forecast): the burst capacity the forecast
// work would add to what the farm asks for now, and the real jobs whose
// risk it worsens.
//
// A forecast's start moves its own planned finish (sched.Job.NotBefore),
// not the shares or the bursts: the bursts shown are those it would need
// if it came now.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// forecastEvery spaces forecast passes: a second policy pass is not
// needed every tick.
const forecastEvery = 30 * time.Second

func isForecast(j *c4v1.WorkJob) bool { return j.Spec.Forecast != nil }

func forecastHours(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// forecastExists is spec.forecast.exists as a unit's Exists, where 0
// means 1: a forecast that will not come at all still weighs a little.
func forecastExists(f *c4v1.WorkForecast) float64 {
	if f.Exists == "" {
		return 1
	}
	return min(max(forecastHours(f.Exists), 1e-6), 1)
}

// forecastPlan is a forecast given as slot-hours: one generator, never
// expanded, whose children are the work, so the plan does not cap it at
// the slots of the units it has (sched.Job.Unevaluated). Its fan-out is
// set in forecastPriors.
func forecastPlan(j *c4v1.WorkJob) (*class.Plan, error) {
	f := j.Spec.Forecast
	work := forecastHours(f.SlotHours) * 3600
	if work <= 0 {
		return nil, fmt.Errorf("forecast slotHours %q: more than 0 slot-hours", f.SlotHours)
	}
	sd := forecastHours(f.SD) * 3600
	if sd <= 0 {
		sd = work / 2
	}
	u := class.Unit{Index: 0, Name: "forecast", Label: "forecast", Key: "forecast/" + j.Spec.Class, Slots: 1, Generator: true,
		Exists: forecastExists(f), Work: work, WorkSD: sd}
	return &class.Plan{Units: []class.Unit{u}, Order: []int{0}, OrderName: "forecast", Estimate: estimate.DefaultConfig()}, nil
}

// forecastUnits fits the class's plan of a forecast job: every unit
// weighted by the chance the work comes, and priced at UnitHours when
// given.
func forecastUnits(p *class.Plan, f *c4v1.WorkForecast) {
	for i := range p.Units {
		p.Units[i].Exists = forecastExists(f)
	}
	if h := forecastHours(f.UnitHours); h > 0 {
		p.Prior = &estimate.Prior{Mean: h * 3600, SD: h * 1800}
	}
}

// forecastPriors prices a forecast given as slot-hours: its generator's
// own run is nothing, and its children are the slot-hours, in units as
// long as UnitHours (else the class's recent unit time in the project,
// else an hour), which bound the critical path.
func (q *Queue) forecastPriors(e *estimate.Estimator, v *view) {
	f, plan := v.job.Spec.Forecast, v.mem.plan
	if f == nil || plan.OrderName != "forecast" || len(plan.Units) == 0 {
		return
	}
	u := plan.Units[0]
	longest := forecastHours(f.UnitHours) * 3600
	if longest <= 0 {
		longest = 3600
		if p, ok := q.classPrior(v.project, v.job.Spec.Class); ok {
			longest = p.Mean
		}
	}
	longest = min(max(longest, 1), u.Work)
	n := u.Work / longest
	e.SetPrior(u.Key, estimate.Prior{Mean: 1, SD: 0.5})
	e.SetGenPrior(u.Key, estimate.GenPrior{Children: estimate.Prior{Mean: n, SD: n / 2},
		Work: estimate.Prior{Mean: u.Work, SD: u.WorkSD}, Longest: estimate.Prior{Mean: longest, SD: longest / 2}})
}

// observeForecast finishes observing a forecast job once it has a plan:
// whether it fits a node, its deadline (its own or a Milestone's), and
// its estimate, every unit waiting for the work to come.
func (q *Queue) observeForecast(v *view, pools map[string]*pool, now time.Time) {
	j, m, plan := v.job, v.mem, v.mem.plan
	v.avoid = map[string]bool{}
	widest, known := 0, false
	for _, name := range v.pools {
		if p := pools[name]; p != nil {
			widest, known = max(widest, p.perNode), true
		}
	}
	switch {
	case v.unschedulable != "":
	case !known:
		v.unschedulable = fmt.Sprintf("none of the job's pools %v exists", v.pools)
	case plan.Units[0].Slots > widest:
		v.unschedulable = fmt.Sprintf("a unit needs %d slots and the largest node in pools %v has %d", plan.Units[0].Slots, v.pools, widest)
	}
	m.readyAt = j.CreationTimestamp.Time
	oj := j
	v.milestone, v.source = "", ""
	switch ms := milestoneFor(q.milestones[j.Namespace], j); {
	case ms != nil:
		cp := *j
		cp.Spec.Deadline = &metav1.Time{Time: ms.Spec.Due.Time}
		oj, v.milestone, v.source = &cp, ms.Name, "milestone"
	case j.Spec.Deadline != nil:
		v.source = "spec"
	}
	v.obj = v.driver.Objective(oj, m.readyAt)
	// Waiting, never runnable, even in the forecast pass.
	v.blocked = true
	v.runnable = append([]int(nil), plan.Order...)
	q.estimate(v, now)
}

// forecastWhy says why a forecast job is not in the forecast pass; empty
// when it is.
func forecastWhy(v *view) string {
	switch {
	case v.mem.plan == nil || v.mem.planErr != nil:
		return "it has no plan"
	case v.unschedulable != "":
		return v.unschedulable
	case v.source == "":
		return "a forecast needs a deadline: its own, or that of a Milestone selecting it"
	}
	return ""
}

// ForecastBurst is the burst slots suggested in a pool, approved or not,
// with the forecast work (Slots) and as the farm plans it now (Live).
type ForecastBurst struct {
	Pool  string `json:"pool"`
	Slots int    `json:"slots"`
	Live  int    `json:"live"`
}

// ForecastRisk is a real job's risk with the forecast work (Risk) and as
// the farm plans it now (Live).
type ForecastRisk struct {
	Job  string `json:"job"`
	Risk string `json:"risk"`
	Live string `json:"live"`
}

// Forecast is the farm as the last forecast pass planned it, with the
// forecast jobs added. Nothing of it is acted on.
type Forecast struct {
	At time.Time `json:"at"`
	// Jobs are the forecast jobs planned (namespace/name).
	Jobs []string `json:"jobs,omitempty"`
	// Bursts are the burst pools the forecast work would want more of.
	Bursts []ForecastBurst `json:"bursts,omitempty"`
	// Worse are the real jobs whose risk the forecast work worsens.
	Worse []ForecastRisk `json:"worse,omitempty"`
}

// forecastMem is the last forecast pass.
type forecastMem struct {
	at   time.Time
	sig  string
	dec  sched.Decisions
	risk map[string]string // by job key: real and forecast jobs' risk with the forecast work
	farm Forecast
}

// Forecast is the last forecast pass's farm view.
func (q *Queue) Forecast() Forecast {
	q.mu.Lock()
	defer q.mu.Unlock()
	f := q.fc.farm
	f.Jobs = append([]string(nil), f.Jobs...)
	f.Bursts = append([]ForecastBurst(nil), f.Bursts...)
	f.Worse = append([]ForecastRisk(nil), f.Worse...)
	return f
}

// forecastPass plans the farm with the forecast jobs added, if the last
// pass is older than forecastEvery or the forecast jobs changed. snap and
// live are the live pass's snapshot and decisions; neither is changed.
func (q *Queue) forecastPass(now time.Time, snap sched.Snapshot, cfg sched.Config, live sched.Decisions, views, fviews []*view) {
	if len(fviews) == 0 {
		q.fc = forecastMem{}
		return
	}
	var keys []string
	for _, v := range fviews {
		keys = append(keys, fmt.Sprintf("%s@%d:%s:%s", v.key, v.job.Generation, v.obj.Deadline.Format(time.RFC3339), forecastWhy(v)))
	}
	sort.Strings(keys)
	sig := strings.Join(keys, ",")
	if sig == q.fc.sig && now.Sub(q.fc.at) < forecastEvery {
		return
	}
	fs := snap
	fs.Pools = append([]sched.Pool(nil), snap.Pools...)
	fs.Projects = append([]sched.Project(nil), snap.Projects...)
	fs.Jobs = append([]sched.Job(nil), snap.Jobs...)
	free := 0
	for _, p := range snap.Pools {
		free += max(0, p.Slots-p.Used)
	}
	farm := Forecast{At: now}
	for _, v := range fviews {
		if forecastWhy(v) != "" {
			continue
		}
		j := schedJob(v, free)
		if s := v.job.Spec.Forecast.Start; s != nil {
			j.NotBefore = s.Time
		}
		fs.Jobs = append(fs.Jobs, j)
		farm.Jobs = append(farm.Jobs, v.key)
	}
	sort.Strings(farm.Jobs)
	var st sched.State
	dec := q.Policy(fs, &st, cfg)

	risk := map[string]string{}
	for _, v := range fviews {
		if js, ok := dec.Jobs[v.key]; ok {
			e, _ := q.estimateFor(now, v, js)
			risk[v.key] = e.Risk
		}
	}
	for _, v := range views {
		js, ok := dec.Jobs[v.key]
		ljs, lok := live.Jobs[v.key]
		if !ok || !lok {
			continue
		}
		e, _ := q.estimateFor(now, v, js)
		le, _ := q.estimateFor(now, v, ljs)
		risk[v.key] = e.Risk
		if riskRank[e.Risk] > riskRank[le.Risk] {
			farm.Worse = append(farm.Worse, ForecastRisk{Job: v.key, Risk: e.Risk, Live: le.Risk})
		}
	}
	sort.Slice(farm.Worse, func(a, b int) bool { return farm.Worse[a].Job < farm.Worse[b].Job })
	wanted := func(d sched.Decisions) map[string]int {
		out := map[string]int{}
		for _, b := range d.Bursts {
			for p, n := range b.Slots {
				out[p] += n
			}
		}
		return out
	}
	now0, with := wanted(live), wanted(dec)
	for p, n := range with {
		if n > now0[p] {
			farm.Bursts = append(farm.Bursts, ForecastBurst{Pool: p, Slots: n, Live: now0[p]})
		}
	}
	sort.Slice(farm.Bursts, func(a, b int) bool { return farm.Bursts[a].Pool < farm.Bursts[b].Pool })
	q.fc = forecastMem{at: now, sig: sig, dec: dec, risk: risk, farm: farm}
}

// forecastStatus marks a forecast job's status: phase Forecast, and the
// Forecast condition saying whether the forecast pass plans it.
func forecastStatus(st *c4v1.WorkJobStatus, v *view) {
	st.Phase = "Forecast"
	cond := metav1.Condition{Type: "Forecast", Status: metav1.ConditionTrue, Reason: "Planned",
		Message: "planned among the real work, never released", ObservedGeneration: v.job.Generation}
	if why := forecastWhy(v); why != "" {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "NotPlanned", why
	}
	meta.SetStatusCondition(&st.Conditions, cond)
}
