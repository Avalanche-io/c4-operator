package sched

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/work/estimate"
)

var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) // a Monday

func est(w float64) estimate.Summary {
	return estimate.Summary{W: w, W90: w, TPath: 0}
}

func units(job string, n int) []UnitRef {
	out := make([]UnitRef, n)
	for i := range out {
		out[i] = UnitRef{ID: fmt.Sprintf("%s/%d", job, i)}
	}
	return out
}

// job makes a job with n runnable one-slot units of total work w
// slot-hours due in due.
func job(id, proj string, n int, wHours float64, due time.Duration) Job {
	j := Job{
		ID: id, Project: proj, Class: "render", Submitted: t0, Ready: t0,
		Pools: []string{"cpu"}, SlotsPerUnit: 1,
		Runnable: units(id, n), RunnableTotal: n,
		Estimate: est(wHours * 3600), Preemptible: true,
		LastRunning: t0,
	}
	if due > 0 {
		j.Deadline = t0.Add(due)
	}
	return j
}

func cpu(slots int) Pool { return Pool{Name: "cpu", Slots: slots} }

func count(d Decisions, jobID string) int {
	n := 0
	for _, x := range d.Dispatch {
		if x.Job == jobID {
			n += x.Slots
		}
	}
	return n
}

func TestNeedTierFillsMostBehindFirst(t *testing.T) {
	// a needs 40 slot-hours in 10h, b 20: 60 slot-hours of work on 5 slots
	// (50 slot-hours by the deadline). Planned together, one job must be
	// late; Moore-Hodgson keeps b (dropping the larger a), so b gets its
	// 2 slots and a the remaining 3, and only a is reported at risk.
	s := Snapshot{Now: t0, Pools: []Pool{cpu(5)}, Jobs: []Job{
		job("a", "p", 100, 40, 10*time.Hour),
		job("b", "p", 100, 20, 10*time.Hour),
	}}
	d := Decide(s, &State{}, DefaultConfig())
	if count(d, "a") != 3 || count(d, "b") != 2 {
		t.Fatalf("a=%d b=%d", count(d, "a"), count(d, "b"))
	}
	if !d.Jobs["a"].AtRisk || d.Jobs["b"].AtRisk || d.Jobs["b"].Required != 2 {
		t.Fatalf("%+v %+v", d.Jobs["a"], d.Jobs["b"])
	}
	// With 20 slots both fit. Same deadline: the fluid plan's rate
	// (60/10 = 6) is shared in proportion to work, 4 and 2, in the need
	// tier; the rest goes by fair share and least slack.
	s.Pools[0].Slots = 20
	d = Decide(s, &State{}, DefaultConfig())
	need := map[string]int{}
	for _, x := range d.Dispatch {
		if x.Tier == TierNeed {
			need[x.Job]++
		}
	}
	if need["a"] != 4 || need["b"] != 2 || count(d, "a")+count(d, "b") != 20 {
		t.Fatalf("need %v total a=%d b=%d", need, count(d, "a"), count(d, "b"))
	}
	// The literal DESIGN formula: each job's own W90/T.
	cfg := DefaultConfig()
	cfg.Planner, cfg.TailAware = PlanPerJob, false
	s.Pools[0].Slots = 5
	d = Decide(s, &State{}, cfg)
	if d.Jobs["a"].Required != 4 || d.Jobs["b"].Required != 2 || count(d, "a")+count(d, "b") != 5 || count(d, "b") < 1 {
		t.Fatalf("per-job: a=%d b=%d %+v", count(d, "a"), count(d, "b"), d.Jobs)
	}
}

func TestFluidPlanDefersLaterDeadlines(t *testing.T) {
	// urgent: 30 slot-hours due in 5h; later: 60 slot-hours due in 48h.
	// On 10 slots the per-job rule would give later 60/48 = 1.25 slots now
	// and urgent 6; the fluid plan gives urgent what the group's rate
	// allows (rho = max(30/5, 90/48) = 6) and later nothing yet, because
	// it is on time once urgent is done.
	s := Snapshot{Now: t0, Pools: []Pool{cpu(10)}, Jobs: []Job{
		job("urgent", "p", 100, 30, 5*time.Hour),
		job("later", "p", 100, 60, 48*time.Hour),
	}}
	d := Decide(s, &State{}, DefaultConfig())
	if d.Jobs["urgent"].Required != 6 || d.Jobs["later"].Required != 0 || d.Jobs["later"].AtRisk {
		t.Fatalf("%+v %+v", d.Jobs["urgent"], d.Jobs["later"])
	}
	// Both are planned to finish on time: urgent at 30/10 = 3h, later at
	// 90/10 = 9h.
	if got := d.Jobs["later"].ETA90.Sub(t0); got != 9*time.Hour {
		t.Fatalf("later ETA %v", got)
	}
}

func TestOverloadServesFeasibleThenSmallestWork(t *testing.T) {
	// big cannot finish even with the whole pool: 100 slot-hours in 2h on
	// 10 slots. small1 and small2 are feasible.
	s := Snapshot{Now: t0, Pools: []Pool{cpu(10)}, Jobs: []Job{
		job("big", "p", 500, 100, 2*time.Hour),
		job("mid", "p", 500, 30, 2*time.Hour),
		job("small", "p", 500, 4, 2*time.Hour),
	}}
	d := Decide(s, &State{}, DefaultConfig())
	if d.Jobs["big"].Feasible || d.Jobs["mid"].Feasible || !d.Jobs["small"].Feasible {
		t.Fatalf("feasibility %+v", d.Jobs)
	}
	// small (R=2) first, then infeasible by smallest work: mid takes the rest.
	if count(d, "small") != 2 || count(d, "mid") != 8 || count(d, "big") != 0 {
		t.Fatalf("small=%d mid=%d big=%d", count(d, "small"), count(d, "mid"), count(d, "big"))
	}
	if !strings.HasPrefix(d.Jobs["big"].Message, "big will miss Mon 14:00") {
		t.Fatalf("message %q", d.Jobs["big"].Message)
	}
	if d.Jobs["small"].Rank != 1 || d.Jobs["mid"].Rank != 2 || d.Jobs["big"].Rank != 3 {
		t.Fatalf("ranks %+v", d.Jobs)
	}
}

func TestTieBandGoesToLowerUsageProject(t *testing.T) {
	// Both jobs at ratio 0 tie; project q uses nothing, p already runs 6
	// slots of its 5 nominal, so q's job is served first.
	pa := job("pa", "p", 50, 10, 10*time.Hour)
	qa := job("qa", "q", 50, 10, 10*time.Hour)
	qa.Submitted = t0.Add(time.Minute) // younger, but lower usage wins
	other := job("prun", "p", 0, 0, 0)
	other.Running = map[string]int{"cpu": 6}
	s := Snapshot{Now: t0, Pools: []Pool{{Name: "cpu", Slots: 7, Used: 6}},
		Projects: []Project{
			{Name: "p", Quotas: map[string]Quota{"cpu": {Nominal: 5, BorrowingLimit: -1}}},
			{Name: "q", Quotas: map[string]Quota{"cpu": {Nominal: 2, BorrowingLimit: -1}}},
		},
		Jobs: []Job{pa, qa, other}}
	d := Decide(s, &State{}, DefaultConfig())
	if len(d.Dispatch) != 1 || d.Dispatch[0].Job != "qa" {
		t.Fatalf("%+v", d.Dispatch)
	}
}

func TestQuotaLendingAndBorrowingLimit(t *testing.T) {
	quotas := func(bl int) []Project {
		return []Project{
			{Name: "p", Quotas: map[string]Quota{"cpu": {Nominal: 6, BorrowingLimit: bl}}},
			{Name: "q", Quotas: map[string]Quota{"cpu": {Nominal: 4, BorrowingLimit: -1}}},
		}
	}
	// q has no work: its 4 nominal slots are lent, p takes all 10.
	s := Snapshot{Now: t0, Pools: []Pool{cpu(10)}, Projects: quotas(-1),
		Jobs: []Job{job("p1", "p", 100, 100, 10*time.Hour)}}
	if n := count(Decide(s, &State{}, DefaultConfig()), "p1"); n != 10 {
		t.Fatalf("lending: %d", n)
	}
	// Borrowing limit 2: p takes 6+2.
	s.Projects = quotas(2)
	if n := count(Decide(s, &State{}, DefaultConfig()), "p1"); n != 8 {
		t.Fatalf("borrowing limit: %d", n)
	}
	// q has a job below its required rate (R=4): its nominal is not lent
	// even though p's job is more behind.
	s.Projects = quotas(-1)
	s.Jobs = append(s.Jobs, job("q1", "q", 100, 40, 10*time.Hour))
	d := Decide(s, &State{}, DefaultConfig())
	if count(d, "q1") != 4 || count(d, "p1") != 6 {
		t.Fatalf("guarantee: p1=%d q1=%d", count(d, "p1"), count(d, "q1"))
	}
}

func runningUnits(job, pool string, n int, started time.Time) ([]RunningUnit, map[string]int) {
	var rus []RunningUnit
	for i := 0; i < n; i++ {
		rus = append(rus, RunningUnit{ID: fmt.Sprintf("%s/r%d", job, i), Pool: pool, Slots: 1, Started: started.Add(time.Duration(i) * 10 * time.Second)})
	}
	return rus, map[string]int{pool: n}
}

func TestReclaimPreemptsBorrowersByLeastLostWork(t *testing.T) {
	// p borrowed the whole pool (10 slots, nominal 6). q (nominal 4)
	// arrives with work: 4 of p's units are preempted, most recently
	// started first.
	p1 := job("p1", "p", 0, 1, 72*time.Hour)
	p1.RunnableTotal = 0
	p1.Runnable = nil
	p1.RunningUnits, p1.Running = runningUnits("p1", "cpu", 10, t0.Add(-10*time.Minute))
	q1 := job("q1", "q", 50, 40, 10*time.Hour) // R=4
	s := Snapshot{Now: t0, Pools: []Pool{{Name: "cpu", Slots: 10, Used: 10}},
		Projects: []Project{
			{Name: "p", Quotas: map[string]Quota{"cpu": {Nominal: 6, BorrowingLimit: -1}}},
			{Name: "q", Quotas: map[string]Quota{"cpu": {Nominal: 4, BorrowingLimit: -1}}},
		},
		Jobs: []Job{p1, q1}}
	st := &State{}
	d := Decide(s, st, DefaultConfig())
	if len(d.Preempt) != 4 {
		t.Fatalf("preempted %d: %+v", len(d.Preempt), d.Preempt)
	}
	for i, p := range d.Preempt {
		if p.Reason != ReasonReclaim || p.For != "q1" || p.Unit != fmt.Sprintf("p1/r%d", 9-i) {
			t.Fatalf("preempt %d: %+v", i, p)
		}
	}
	// Next call, units still stopping: nothing more is preempted.
	if d2 := Decide(s, st, DefaultConfig()); len(d2.Preempt) != 0 {
		t.Fatalf("preempted again: %+v", d2.Preempt)
	}
	// A victim with little slack is protected.
	s.Jobs[0].Deadline = t0.Add(90 * time.Minute)
	if d3 := Decide(s, &State{}, DefaultConfig()); len(d3.Preempt) != 0 {
		t.Fatalf("preempted a job without slack: %+v", d3.Preempt)
	}
	// Units that would lose more than MaxLostWork are protected.
	s.Jobs[0].Deadline = t0.Add(72 * time.Hour)
	for i := range s.Jobs[0].RunningUnits {
		s.Jobs[0].RunningUnits[i].Started = t0.Add(-time.Hour)
	}
	if d := Decide(s, &State{}, DefaultConfig()); len(d.Preempt) != 0 {
		t.Fatalf("preempted an hour of work: %+v", d.Preempt)
	}
	for i := range s.Jobs[0].RunningUnits {
		s.Jobs[0].RunningUnits[i].Started = t0.Add(-10 * time.Minute)
	}
	// Units already preempted twice are protected.
	s.Jobs[0].Deadline = t0.Add(72 * time.Hour)
	for i := range s.Jobs[0].RunningUnits {
		s.Jobs[0].RunningUnits[i].Preemptions = 2
	}
	if d4 := Decide(s, &State{}, DefaultConfig()); len(d4.Preempt) != 0 {
		t.Fatalf("preempted a unit a third time: %+v", d4.Preempt)
	}
}

func TestPreemptionRateLimit(t *testing.T) {
	p1 := job("p1", "p", 0, 1, 72*time.Hour)
	p1.Runnable, p1.RunnableTotal = nil, 0
	p1.RunningUnits, p1.Running = runningUnits("p1", "cpu", 40, t0.Add(-10*time.Minute))
	q1 := job("q1", "q", 100, 200, 10*time.Hour) // R=20
	s := Snapshot{Now: t0, Pools: []Pool{{Name: "cpu", Slots: 40, Used: 40}},
		Projects: []Project{
			{Name: "p", Quotas: map[string]Quota{"cpu": {Nominal: 20, BorrowingLimit: -1}}},
			{Name: "q", Quotas: map[string]Quota{"cpu": {Nominal: 20, BorrowingLimit: -1}}},
		},
		Jobs: []Job{p1, q1}}
	cfg := DefaultConfig()
	st := &State{}
	if d := Decide(s, st, cfg); len(d.Preempt) != cfg.PreemptPerWindow {
		t.Fatalf("first window: %d", len(d.Preempt))
	}
	// The preempted units stop; the next call in the same window may not
	// preempt more.
	s.Jobs[0].RunningUnits = s.Jobs[0].RunningUnits[:32]
	s.Jobs[0].Running = map[string]int{"cpu": 32}
	s.Pools[0].Used = 32 + 8
	s.Jobs[1].Running = map[string]int{"cpu": 8}
	s.Jobs[1].RunningUnits, _ = runningUnits("q1", "cpu", 8, t0)
	s.Now = t0.Add(time.Minute)
	if d := Decide(s, st, cfg); len(d.Preempt) != 0 {
		t.Fatalf("same window: %d", len(d.Preempt))
	}
	s.Now = t0.Add(cfg.PreemptWindow + time.Minute)
	for i := range s.Jobs[0].RunningUnits {
		s.Jobs[0].RunningUnits[i].Started = s.Now.Add(-5 * time.Minute)
	}
	if d := Decide(s, st, cfg); len(d.Preempt) != cfg.PreemptPerWindow {
		t.Fatalf("next window: %d", len(d.Preempt))
	}
}

func TestFeasibilityPreemptionWithinProject(t *testing.T) {
	// urgent is feasible on the pool but behind with 0 running; filler
	// (same project, lots of slack) holds every slot.
	filler := job("filler", "p", 0, 2, 72*time.Hour)
	filler.Runnable, filler.RunnableTotal = nil, 0
	filler.RunningUnits, filler.Running = runningUnits("filler", "cpu", 10, t0.Add(-30*time.Minute))
	filler.Checkpoints = true
	urgent := job("urgent", "p", 50, 15, 3*time.Hour)
	s := Snapshot{Now: t0, Pools: []Pool{{Name: "cpu", Slots: 10, Used: 10}}, Jobs: []Job{filler, urgent}}
	d := Decide(s, &State{}, DefaultConfig())
	if len(d.Preempt) != 5 {
		t.Fatalf("%+v", d.Preempt)
	}
	for _, p := range d.Preempt {
		if p.Reason != ReasonFeasibility || p.For != "urgent" || !p.Yield {
			t.Fatalf("%+v", p)
		}
	}
	// Not preemptible: nothing.
	s.Jobs[0].Preemptible = false
	if d := Decide(s, &State{}, DefaultConfig()); len(d.Preempt) != 0 {
		t.Fatal("preempted a non-preemptible job")
	}
}

func TestGangNeedsRoomForAll(t *testing.T) {
	g := job("train", "ml", 1, 16, 48*time.Hour)
	g.SlotsPerUnit = 4
	g.Pools = []string{"gpu"}
	g.LastRunning = time.Time{}
	g.Ready = t0
	r := job("gpurender", "show", 100, 20, 48*time.Hour)
	r.Pools = []string{"gpu"}
	s := Snapshot{Now: t0, Pools: []Pool{{Name: "gpu", Slots: 8, Used: 5}}, Jobs: []Job{g, r}}
	d := Decide(s, &State{}, DefaultConfig())
	if count(d, "train") != 0 || count(d, "gpurender") != 3 {
		t.Fatalf("train=%d render=%d", count(d, "train"), count(d, "gpurender"))
	}
	// Starving for 30 minutes: the gang still does not fit, so the pool's
	// free slots are held for it instead of going to the render.
	s.Now = t0.Add(31 * time.Minute)
	d = Decide(s, &State{}, DefaultConfig())
	if count(d, "train") != 0 || count(d, "gpurender") != 0 {
		t.Fatalf("held: train=%d render=%d", count(d, "train"), count(d, "gpurender"))
	}
	s.Pools[0].Used = 4
	d = Decide(s, &State{}, DefaultConfig())
	if count(d, "train") != 4 || d.Dispatch[0].Tier != TierStarvation {
		t.Fatalf("released: %+v", d.Dispatch)
	}
}

func TestStarvationGuard(t *testing.T) {
	// far has no objective; near takes the whole pool by need.
	near := job("near", "p", 100, 100, 10*time.Hour)
	far := job("far", "p", 100, 1, 0)
	far.LastRunning = t0.Add(-31 * time.Minute)
	s := Snapshot{Now: t0, Pools: []Pool{cpu(10)}, Jobs: []Job{near, far}}
	d := Decide(s, &State{}, DefaultConfig())
	if count(d, "far") != 1 || d.Dispatch[0].Job != "far" || d.Dispatch[0].Tier != TierStarvation {
		t.Fatalf("%+v", d.Dispatch)
	}
	s.Jobs[1].LastRunning = t0.Add(-29 * time.Minute)
	if count(Decide(s, &State{}, DefaultConfig()), "far") != 0 {
		t.Fatal("guard fired early")
	}
}

func TestHoldAndBoost(t *testing.T) {
	a := job("a", "p", 100, 20, 10*time.Hour)
	a.Hold = true
	s := Snapshot{Now: t0, Pools: []Pool{cpu(10)}, Jobs: []Job{a}}
	if len(Decide(s, &State{}, DefaultConfig()).Dispatch) != 0 {
		t.Fatal("held job released")
	}
	a.Hold = false
	a.HoldStop = true
	a.RunningUnits, a.Running = runningUnits("a", "cpu", 3, t0)
	s.Jobs[0] = a
	d := Decide(s, &State{}, DefaultConfig())
	if len(d.Preempt) != 3 || d.Preempt[0].Reason != ReasonHoldStop {
		t.Fatalf("%+v", d.Preempt)
	}
	// Boost 2 doubles R; the extra comes from p's own nominal first.
	b := job("b", "p", 100, 20, 10*time.Hour) // R=2
	b.Boost = 2
	c := job("c", "q", 100, 20, 10*time.Hour) // R=2
	s = Snapshot{Now: t0, Pools: []Pool{cpu(6)},
		Projects: []Project{
			{Name: "p", Quotas: map[string]Quota{"cpu": {Nominal: 3, BorrowingLimit: -1}}},
			{Name: "q", Quotas: map[string]Quota{"cpu": {Nominal: 3, BorrowingLimit: -1}}},
		},
		Jobs: []Job{b, c}}
	d = Decide(s, &State{}, DefaultConfig())
	boost := 0
	for _, x := range d.Dispatch {
		if x.Tier == TierBoost {
			boost++
		}
	}
	// b: need 2, boost +1 inside its nominal 3, then +1 borrowed from q,
	// whose job already has its required 2 slots.
	if count(d, "b") != 4 || count(d, "c") != 2 || boost != 2 || d.Jobs["b"].Required != 4 {
		t.Fatalf("b=%d c=%d boost=%d %+v", count(d, "b"), count(d, "c"), boost, d.Jobs["b"])
	}
}

func TestInformationTier(t *testing.T) {
	// a is planned first (earlier deadline) and g is on time after it, so
	// only a has a need now; the leftover goes first to g's generators,
	// then to fair share.
	a := job("a", "p", 100, 10, 10*time.Hour) // R=1
	g := job("g", "p", 100, 10, 20*time.Hour) // R=0.5
	g.Unevaluated = 3
	for i := 0; i < 3; i++ {
		g.Runnable[i].Info = true
	}
	s := Snapshot{Now: t0, Pools: []Pool{cpu(6)}, Jobs: []Job{a, g}}
	d := Decide(s, &State{}, DefaultConfig())
	var tiers []string
	for _, x := range d.Dispatch {
		tiers = append(tiers, x.Job+":"+string(x.Tier))
	}
	want := []string{"a:deadline-need", "g:information", "g:information", "g:information", "a:surplus", "a:surplus"}
	if !reflect.DeepEqual(tiers, want) {
		t.Fatalf("%v", tiers)
	}
}

func burstSnapshot() Snapshot {
	// 100 slot-hours due in 10h on 6 GPUs: late by a lot.
	j := job("sh040-fx-v003", "showA", 400, 100, 10*time.Hour)
	j.Pools = []string{"gpu", "gpu-spot", "gpu-od"}
	return Snapshot{Now: t0,
		Pools: []Pool{
			{Name: "gpu", Slots: 6},
			{Name: "gpu-spot", CostPerSlotHour: 0.8, Burst: &Burst{MaxSlots: 64, Provisioning: 8 * time.Minute, CostPerSlotHour: 0.8}},
			{Name: "gpu-od", CostPerSlotHour: 1.86, Burst: &Burst{MaxSlots: 16, Provisioning: 5 * time.Minute, CostPerSlotHour: 1.86}},
		},
		Projects: []Project{{Name: "showA", BurstBudgetLeft: 500, LateCostPerHour: 300}},
		Jobs:     []Job{j}}
}

func TestBurstSmallestThatCloses(t *testing.T) {
	s := burstSnapshot()
	d := Decide(s, &State{}, DefaultConfig())
	js := d.Jobs["sh040-fx-v003"]
	if !js.AtRisk || js.Burst == nil || !js.Burst.Closes || !js.Burst.Auto {
		t.Fatalf("%+v %+v", js, js.Burst)
	}
	// Deficit 100h-60h = 40 slot-hours; spot slots each give 10h-8m.
	per := 10.0 - 8.0/60
	want := int(math.Ceil(40 / per))
	if js.Burst.Slots["gpu-spot"] != want || len(js.Burst.Slots) != 1 {
		t.Fatalf("slots %v want %d", js.Burst.Slots, want)
	}
	cost := 0.8 * (float64(want)*8.0/60 + 40)
	if math.Abs(js.Burst.Cost-cost) > 1e-6 {
		t.Fatalf("cost %v want %v", js.Burst.Cost, cost)
	}
	msg := fmt.Sprintf("sh040-fx-v003 will miss Mon 22:00 by about 7h at current capacity. +%d gpu-spot slots closes it (est. $%.0f).", want, cost)
	if js.Message != msg {
		t.Fatalf("message\n got %q\nwant %q", js.Message, msg)
	}
	var spot CapacityRequest
	for _, c := range d.Capacity {
		if c.Pool == "gpu-spot" {
			spot = c
		}
	}
	if spot.Slots != want {
		t.Fatalf("capacity %+v", d.Capacity)
	}
	// One slot fewer would not close it.
	if 40-float64(want-1)*per <= 0 {
		t.Fatal("not minimal")
	}
}

func TestBurstSpillsToPricierAndSkipsSlowPools(t *testing.T) {
	s := burstSnapshot()
	s.Pools[1].Burst.MaxSlots = 2 // spot nearly exhausted
	d := Decide(s, &State{}, DefaultConfig())
	b := d.Jobs["sh040-fx-v003"].Burst
	if b.Slots["gpu-spot"] != 2 || b.Slots["gpu-od"] < 1 || !b.Closes {
		t.Fatalf("%+v", b)
	}
	if !strings.Contains(d.Jobs["sh040-fx-v003"].Message, "+2 gpu-spot and +") {
		t.Fatalf("%q", d.Jobs["sh040-fx-v003"].Message)
	}
	// Deadline in 6 minutes: spot (8m) is too slow, on-demand (5m) is not
	// enough; nothing closes it.
	s = burstSnapshot()
	s.Jobs[0].Deadline = t0.Add(6 * time.Minute)
	s.Jobs[0].Estimate = est(0.9 * 3600)
	d = Decide(s, &State{}, DefaultConfig())
	b = d.Jobs["sh040-fx-v003"].Burst
	if b == nil || b.Closes || b.Auto || b.Slots["gpu-spot"] != 0 {
		t.Fatalf("%+v", b)
	}
}

func TestBurstNeedsApproval(t *testing.T) {
	s := burstSnapshot()
	s.Projects[0].BurstBudgetLeft = 10
	d := Decide(s, &State{}, DefaultConfig())
	js := d.Jobs["sh040-fx-v003"]
	if js.Burst.Auto || !strings.Contains(js.Message, "Needs approval: over the project's remaining burst budget ($10 left).") {
		t.Fatalf("%q", js.Message)
	}
	for _, c := range d.Capacity {
		if c.Slots != 0 {
			t.Fatalf("requested without approval: %+v", c)
		}
	}
	s.Projects[0].BurstBudgetLeft = 1000
	s.Projects[0].LateCostPerHour = 1
	js = Decide(s, &State{}, DefaultConfig()).Jobs["sh040-fx-v003"]
	if js.Burst.Auto || !strings.Contains(js.Burst.Why, "costs more than the lateness it prevents") {
		t.Fatalf("%+v", js.Burst)
	}
}

func TestBurstScaleDownHysteresis(t *testing.T) {
	s := Snapshot{Now: t0, Pools: []Pool{
		{Name: "cpu", Slots: 4},
		{Name: "cpu-spot", Slots: 10, Used: 4, CostPerSlotHour: 0.6, Burst: &Burst{MaxSlots: 50, Provisioning: 6 * time.Minute, CostPerSlotHour: 0.6, Requested: 10, Provisioned: 10}},
	}}
	st := &State{}
	cfg := DefaultConfig()
	req := func() int {
		for _, c := range Decide(s, st, cfg).Capacity {
			if c.Pool == "cpu-spot" {
				return c.Slots
			}
		}
		return -1
	}
	if got := req(); got != 10 {
		t.Fatalf("t+0: %d", got)
	}
	s.Now = t0.Add(9 * time.Minute)
	if got := req(); got != 10 {
		t.Fatalf("t+9m: %d", got)
	}
	s.Now = t0.Add(10 * time.Minute)
	if got := req(); got != 4 {
		t.Fatalf("t+10m: %d", got)
	}
	// Surplus that ends resets the clock.
	st = &State{}
	s.Now = t0
	req()
	s.Pools[1].Used = 10
	s.Now = t0.Add(5 * time.Minute)
	req()
	s.Pools[1].Used = 4
	s.Now = t0.Add(12 * time.Minute)
	if got := req(); got != 10 {
		t.Fatalf("clock not reset: %d", got)
	}
}

func TestRateObjective(t *testing.T) {
	j := job("train", "ml", 10, 50, 0)
	j.RequiredRate = 4
	s := Snapshot{Now: t0, Pools: []Pool{cpu(10)}, Jobs: []Job{j}}
	d := Decide(s, &State{}, DefaultConfig())
	if d.Jobs["train"].Required != 4 || count(d, "train") != 10 {
		t.Fatalf("%+v %d", d.Jobs["train"], count(d, "train"))
	}
	if !strings.Contains(d.Jobs["train"].Message, "meeting its rate") {
		t.Fatal(d.Jobs["train"].Message)
	}
}

func TestBaselines(t *testing.T) {
	a := job("a", "p", 5, 5, 20*time.Hour)
	b := job("b", "p", 5, 5, 2*time.Hour)
	b.Submitted = t0.Add(time.Minute)
	s := Snapshot{Now: t0, Pools: []Pool{cpu(5)}, Jobs: []Job{a, b}}
	if d := FIFO(s, &State{}, DefaultConfig()); count(d, "a") != 5 {
		t.Fatalf("FIFO %+v", d.Dispatch)
	}
	if d := EDF(s, &State{}, DefaultConfig()); count(d, "b") != 5 {
		t.Fatalf("EDF %+v", d.Dispatch)
	}
	// Manual: static priority wins over submission order.
	s.Jobs[0].Priority, s.Jobs[1].Priority = 50, 60
	if d := Manual(s, &State{}, DefaultConfig()); count(d, "b") != 5 {
		t.Fatalf("Manual %+v", d.Dispatch)
	}
}

func TestManualWranglerBumpsAndSuspends(t *testing.T) {
	// A GPU render (priority 60) is late-looking: 20 units left at 30
	// minutes each on 2 running units = 5h, due in 4h. An ML run (priority
	// 30) holds 4 GPUs.
	r := job("render", "show", 20, 10, 4*time.Hour)
	r.Pools = []string{"gpu"}
	r.Priority = 60
	r.UnitsDone, r.DoneSeconds = 10, 10*1800
	r.RunningUnits, r.Running = runningUnits("render", "gpu", 2, t0.Add(-10*time.Minute))
	ml := job("ml", "rnd", 0, 8, 36*time.Hour)
	ml.Runnable, ml.RunnableTotal = nil, 0
	ml.Pools = []string{"gpu"}
	ml.Priority = 30
	ml.Checkpoints = true
	ml.RunningUnits = []RunningUnit{{ID: "ml/run", Pool: "gpu", Slots: 4, Started: t0.Add(-2 * time.Hour), LastCheckpoint: t0.Add(-7 * time.Minute)}}
	ml.Running = map[string]int{"gpu": 4}
	s := Snapshot{Now: t0, Pools: []Pool{{Name: "gpu", Slots: 6, Used: 6}}, Jobs: []Job{r, ml}}
	st := &State{}
	d := Manual(s, st, DefaultConfig())
	if !st.Bumped("render") || len(d.Preempt) != 1 || d.Preempt[0].Yield || d.Preempt[0].LostWork != 7*time.Minute {
		t.Fatalf("%+v %v", d.Preempt, st.Bumped("render"))
	}
	// No second review within two hours.
	s.Now = t0.Add(time.Hour)
	if d := Manual(s, st, DefaultConfig()); len(d.Preempt) != 0 {
		t.Fatal("reviewed early")
	}
}

func TestSnapshotUnchangedAndDeterministic(t *testing.T) {
	s := burstSnapshot()
	s.Projects = append(s.Projects, Project{Name: "q", Quotas: map[string]Quota{"gpu": {Nominal: 3, BorrowingLimit: -1}}})
	j := job("q1", "q", 30, 20, 5*time.Hour)
	j.Pools = []string{"gpu"}
	j.RunningUnits, j.Running = runningUnits("q1", "gpu", 2, t0)
	s.Jobs = append(s.Jobs, j)
	s.Pools[0].Used = 2
	before := fmt.Sprintf("%+v", s)
	d1 := Decide(s, &State{}, DefaultConfig())
	if after := fmt.Sprintf("%+v", s); after != before {
		t.Fatal("Decide modified its snapshot")
	}
	d2 := Decide(s, &State{}, DefaultConfig())
	if !reflect.DeepEqual(d1, d2) {
		t.Fatal("not deterministic")
	}
}

// TestConcurrentDecide runs Decide and the baselines concurrently on one
// shared snapshot with separate States; run with -race.
func TestConcurrentDecide(t *testing.T) {
	s := burstSnapshot()
	for i := 0; i < 20; i++ {
		j := job(fmt.Sprintf("j%d", i), "showA", 50, float64(5+i), time.Duration(2+i)*time.Hour)
		j.Pools = []string{"gpu", "gpu-spot"}
		s.Jobs = append(s.Jobs, j)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := []Policy{Decide, FIFO, EDF, Manual}[i%4]
			st := &State{}
			for k := 0; k < 20; k++ {
				p(s, st, DefaultConfig())
			}
		}(i)
	}
	wg.Wait()
}

func TestWaterfill(t *testing.T) {
	x := waterfill([]float64{1, 1, 2}, []float64{1, 10, 10}, 9)
	// level l: min(l,1)+min(l,10)+min(2l,10) = 9 -> 1 + l + 2l = 9, l = 8/3
	if math.Abs(x[0]-1) > 1e-9 || math.Abs(x[1]-8.0/3) > 1e-9 || math.Abs(x[2]-16.0/3) > 1e-9 {
		t.Fatalf("%v", x)
	}
	x = waterfill([]float64{1, 1}, []float64{2, 3}, 100)
	if x[0] != 2 || x[1] != 3 {
		t.Fatalf("%v", x)
	}
}

func TestHuman(t *testing.T) {
	cases := map[time.Duration]string{
		3 * time.Minute:  "5m",
		42 * time.Minute: "40m",
		3 * time.Hour:    "3h",
		50 * time.Hour:   "2 days",
	}
	for d, want := range cases {
		if got := human(d); got != want {
			t.Errorf("%v: %q", d, got)
		}
	}
}

// TestBlockedJobsRankAfterRunnable: a job waiting on its upstream (as the
// queue passes it: Blocked set, nothing runnable) has R > 0 and nothing
// running, so by the need ratio alone it would rank first. It ranks after
// every job that can use slots now.
func TestBlockedJobsRankAfterRunnable(t *testing.T) {
	blocked := job("comp", "p", 0, 10, 8*time.Hour)
	blocked.Runnable, blocked.RunnableTotal, blocked.Blocked = nil, 0, 100
	blocked.Ready = time.Time{}
	s := Snapshot{Now: t0, Pools: []Pool{cpu(10)}, Jobs: []Job{
		blocked,
		job("light", "p", 100, 40, 6*time.Hour),
		job("fx", "p", 100, 20, 12*time.Hour),
	}}
	d := Decide(s, &State{}, DefaultConfig())
	if r := d.Jobs["comp"].Rank; r != 3 {
		t.Fatalf("blocked job ranked %d, want last (3); light %d fx %d", r, d.Jobs["light"].Rank, d.Jobs["fx"].Rank)
	}
	if count(d, "comp") != 0 {
		t.Fatal("a blocked job was dispatched")
	}
}

// TestEvictedJobIsLateAtItsShare reproduces the overflow, evict, lateBy=0
// case. On 10 slots, a (25 slot-hours due in 3h) enters the on-time set,
// then b (16 due in 4h) overflows it (41 > 40), so the largest member, a,
// is evicted to late. Inserted alone in deadline order a finishes at 2.5h,
// before its deadline, which used to make its lateness 0 and price any
// burst for it at $0 of lateness, so none could be approved
// automatically. The plan runs a at its share (6 slots), finishing at
// about 4.2h: it is late by about 1.2h, and a burst that fits the budget
// and costs less than that lateness is requested on its own.
func TestEvictedJobIsLateAtItsShare(t *testing.T) {
	a := job("a", "p", 100, 25, 3*time.Hour)
	b := job("b", "p", 100, 16, 4*time.Hour)
	s := Snapshot{Now: t0,
		Pools: []Pool{cpu(10), {Name: "cpu-spot", CostPerSlotHour: 0.2,
			Burst: &Burst{MaxSlots: 40, Provisioning: 5 * time.Minute, CostPerSlotHour: 0.2}}},
		Projects: []Project{{Name: "p", BurstBudgetLeft: 100, LateCostPerHour: 100}},
		Jobs:     []Job{a, b}}
	for i := range s.Jobs {
		s.Jobs[i].Pools = []string{"cpu", "cpu-spot"}
	}
	d := Decide(s, &State{}, DefaultConfig())
	ja := d.Jobs["a"]
	if !ja.AtRisk || ja.LateBy < time.Hour || ja.LateBy > 90*time.Minute {
		t.Fatalf("a: at risk %v, late by %v; want about 1.2h", ja.AtRisk, ja.LateBy)
	}
	if ja.Burst == nil || !ja.Burst.Closes || !ja.Burst.Auto {
		t.Fatalf("a: burst %+v; want one that closes it, requested without approval", ja.Burst)
	}
	if d.Jobs["b"].AtRisk {
		t.Fatalf("b at risk: %+v", d.Jobs["b"])
	}
	// With c (34 due in 5h) the on-time set {b, c} takes all 10 slots and
	// a gets no share: it runs after the set (75 slot-hours at 10 slots,
	// 7.5h), so it is late by about 4.5h, not 0.
	c := job("c", "p", 100, 34, 5*time.Hour)
	c.Pools = []string{"cpu", "cpu-spot"}
	s.Jobs = append(s.Jobs, c)
	d = Decide(s, &State{}, DefaultConfig())
	ja = d.Jobs["a"]
	if !ja.AtRisk || ja.LateBy < 4*time.Hour || ja.LateBy > 5*time.Hour {
		t.Fatalf("a with no share: at risk %v, late by %v; want about 4.5h", ja.AtRisk, ja.LateBy)
	}
	if ja.Burst == nil || !ja.Burst.Closes || !ja.Burst.Auto {
		t.Fatalf("a with no share: burst %+v", ja.Burst)
	}
}
