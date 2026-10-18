package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/work/estimate"
	"github.com/Avalanche-io/c4-operator/internal/work/sched"
)

// TestQuickGrid runs the quick grid (the CI grid of `make stress-quick`)
// with every invariant as an assertion. Findings of known bugs are
// logged, not failed; each has a skipped reproducer below. Set
// STRESS_STRICT=1 to fail on them too (once their fixes land).
func TestQuickGrid(t *testing.T) {
	if testing.Short() {
		t.Skip("the quick grid takes about a minute")
	}
	g := gridOpts{N: 24, Seed: 1, MaxNodes: 24, MaxDays: 0.5, Drain: 0.5}
	pts := gridPoints(g, true)
	res := runGrid(pts, runOpts{Workers: 4, Seeds: []uint64{1}, Meta: true, Repeat: true})
	strict := os.Getenv("STRESS_STRICT") != ""
	known := map[string]int{}
	for _, pr := range res {
		for _, f := range pr.Viol {
			if f.Known && !strict {
				known[f.Inv] += f.N
				continue
			}
			t.Errorf("%s: %s %s: %s\n\t%s", f.Point, f.Inv, f.Job, f.Detail, f.Repro)
		}
	}
	for _, inv := range sortedKeys(known) {
		why := knownBugs[inv]
		if why == "" {
			why = "each attributed to a known bug in its detail"
		}
		t.Logf("known bug %s: %d violations (%s)", inv, known[inv], why)
	}
}

// TestDeterminism: the same point and seed give the same decisions,
// including faults, reclaims, failures and nudges.
func TestDeterminism(t *testing.T) {
	pt := base("det")
	pt.Nodes, pt.Days, pt.Drain = 6, 0.2, 0.3
	pt.NodeFaults, pt.Interruptions, pt.FailRate, pt.BoostRate, pt.HoldRate, pt.MoveRate = 0.4, 0.2, 0.05, 0.2, 0.2, 0.2
	pol, _ := policyByName(mainPolicy)
	a := newEngine(build(pt, 7), pol, false).run()
	b := newEngine(build(pt, 7), pol, false).run()
	if a.Hash != b.Hash || a.OnTime != b.OnTime || a.Dispatches != b.Dispatches {
		t.Fatalf("hash %x/%x on time %d/%d dispatches %d/%d", a.Hash, b.Hash, a.OnTime, b.OnTime, a.Dispatches, b.Dispatches)
	}
	if a.Dispatches == 0 || a.FaultKills == 0 || a.Nudges == 0 {
		t.Fatalf("the run exercised too little: %+v", a)
	}
}

// TestPointEncoding: every grid point and corner survives encode/decode,
// so the repro commands in the report rerun the same point.
func TestPointEncoding(t *testing.T) {
	pts := append(sample(gridOpts{N: 20, Seed: 3}), corners()...)
	for _, p := range pts {
		q, err := decodePoint(p.encode())
		if err != nil {
			t.Fatal(err)
		}
		q.Name = p.Name
		if q != p {
			t.Fatalf("%s:\n%+v\n%+v", p.encode(), p, q)
		}
	}
}

// TestLatinHypercube: each dimension's strata are each used once.
func TestLatinHypercube(t *testing.T) {
	const n = 16
	pts := sample(gridOpts{N: n, Seed: 5})
	for _, d := range dims {
		// days is shortened for big farms after sampling.
		if d.lo == d.hi || d.integ || d.log || d.zero > 0 || d.name == "days" {
			continue
		}
		seen := map[int]bool{}
		for i := range pts {
			x := *d.f(&pts[i])
			seen[min(n-1, int((x-d.lo)/(d.hi-d.lo)*n))] = true
		}
		if len(seen) < n-1 { // three-digit rounding can move a value across a stratum edge
			t.Errorf("%s: %d of %d strata used", d.name, len(seen), n)
		}
	}
}

func TestSpecExport(t *testing.T) {
	path := t.TempDir() + "/spec.yaml"
	if err := writeSpec(base("spec"), 1, path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{"pools:", "costPerSlotHour", "burstBudgetPerDay", "lateCostPerHour", "pipelines:", "arrivals:"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("spec lacks %s:\n%s", want, b)
		}
	}
}

// Reproducers of scheduling bugs. Each is the smallest snapshot found
// that shows the bug, and is skipped until its fix lands in
// internal/work/sched; remove the skipRepro line with the fix.
// STRESS_REPRO=1 runs them anyway (they fail while the bug is there).

func skipRepro(t *testing.T, why string) {
	t.Helper()
	if os.Getenv("STRESS_REPRO") == "" {
		t.Skip(why)
	}
}

var t0r = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func rjob(id, proj string, n int, w90, tpath float64, due time.Duration) sched.Job {
	j := sched.Job{ID: id, Project: proj, Submitted: t0r.Add(-time.Hour), Ready: t0r.Add(-time.Hour), Pools: []string{"cpu"},
		SlotsPerUnit: 1, RunnableTotal: n, Preemptible: true, LastRunning: t0r,
		Estimate: estimate.Summary{W: w90, W90: w90, TPath: tpath}}
	for i := range n {
		j.Runnable = append(j.Runnable, sched.UnitRef{ID: fmt.Sprintf("%s/%d", id, i)})
	}
	if due > 0 {
		j.Deadline = t0r.Add(due)
	}
	return j
}

// Regression (fixed in 917dd3f): a job whose units all wait on an
// upstream job had (running+incoming)/R = 0 and was ranked first.
func TestReproBlockedRank(t *testing.T) {
	ready := rjob("ready", "p", 10, 10*3600, 600, 6*time.Hour)
	blocked := rjob("blocked", "p", 0, 10*3600, 600, 8*time.Hour)
	blocked.Runnable, blocked.RunnableTotal, blocked.Blocked = nil, 0, 10
	s := sched.Snapshot{Now: t0r, Pools: []sched.Pool{{Name: "cpu", Slots: 2, Used: 2}}, Jobs: []sched.Job{ready, blocked}}
	d := sched.Decide(s, &sched.State{}, sched.DefaultConfig())
	if d.Jobs["blocked"].Rank < d.Jobs["ready"].Rank {
		t.Fatalf("blocked job ranked %d, ahead of the ready job at %d", d.Jobs["blocked"].Rank, d.Jobs["ready"].Rank)
	}
}

// Regression (fixed in 3f4ac76 and 3f5d0ce): Moore-Hodgson evicted the
// largest job, which at its own EDF position would finish before its
// deadline, so it was at risk with LateBy 0, its lateness priced at $0,
// and a burst that closes it waited for approval.
func TestReproLatenessUnpriced(t *testing.T) {
	a := rjob("a", "p", 60, 60*3600, 0, 10*time.Hour)
	b := rjob("b", "p", 50, 50*3600, 0, 10*time.Hour+30*time.Minute)
	a.Pools = []string{"cpu", "cpu-burst"}
	b.Pools = a.Pools
	s := sched.Snapshot{Now: t0r,
		Pools: []sched.Pool{{Name: "cpu", Slots: 10}, {Name: "cpu-burst", CostPerSlotHour: 1,
			Burst: &sched.Burst{MaxSlots: 20, Provisioning: 5 * time.Minute, CostPerSlotHour: 1}}},
		Projects: []sched.Project{{Name: "p", BurstBudgetLeft: 1000, LateCostPerHour: 500}},
		Jobs:     []sched.Job{a, b}}
	d := sched.Decide(s, &sched.State{}, sched.DefaultConfig())
	st := d.Jobs["a"]
	if st.AtRisk && st.LateBy <= 0 {
		t.Fatalf("a is at risk with LateBy %s: %s", st.LateBy, st.Message)
	}
}

// Regression, found by the stress grid: preemption took a whole unit from
// a victim whose running slots were above its fractional planned share,
// leaving it below that share, so two jobs with shares under one slot each take the
// slot back and forth. The deadline tier rounds R up (needSlots uses
// ceil) while aboveShare compares running slots with the unrounded share,
// so a job released up to ceil(share) is at once a victim. On a one-slot
// farm the two jobs below preempt each other on every decision and throw
// the work away each time; in the grid it is the churn-pingpong finding.
func TestReproPreemptPingPong(t *testing.T) {
	cfg := sched.DefaultConfig()
	mk := func(runA bool) sched.Snapshot {
		a := rjob("a", "p", 4, 10383, 2238, 8*time.Hour)
		b := rjob("b", "p", 3, 8200, 1780, 8*time.Hour)
		run := &b
		if runA {
			run = &a
		}
		run.Running = map[string]int{"cpu": 1}
		run.RunningUnits = []sched.RunningUnit{{ID: run.ID + "/r", Pool: "cpu", Slots: 1, Started: t0r.Add(-time.Minute)}}
		return sched.Snapshot{Now: t0r, Pools: []sched.Pool{{Name: "cpu", Slots: 1, Used: 1}}, Jobs: []sched.Job{a, b}}
	}
	var st sched.State
	d1 := sched.Decide(mk(false), &st, cfg)
	d2 := sched.Decide(mk(true), &sched.State{}, cfg)
	if len(d1.Preempt) > 0 && len(d2.Preempt) > 0 {
		t.Fatalf("b running: %+v; a running: %+v; each preempts the other", d1.Preempt, d2.Preempt)
	}
}

// Regression, found by the stress grid: a job none of whose pools is in
// the snapshot (a pool removed, renamed, or not yet created) was left out
// of the plan, so it was never reported at risk, not even past its
// deadline, and the HUD showed it on track. It is at risk, late without
// bound, and its message says no pool can run it.
func TestReproUnplannedJobOnTrack(t *testing.T) {
	j := rjob("orphan", "p", 5, 5*3600, 600, 0)
	j.Deadline = t0r.Add(-time.Hour)
	j.Pools = []string{"gpu"}
	s := sched.Snapshot{Now: t0r, Pools: []sched.Pool{{Name: "cpu", Slots: 4}}, Jobs: []sched.Job{j}}
	d := sched.Decide(s, &sched.State{}, sched.DefaultConfig())
	st := d.Jobs["orphan"]
	if !st.AtRisk || st.LateBy <= 0 {
		t.Fatalf("past its deadline with no usable pool: AtRisk %v, LateBy %v (%q)", st.AtRisk, st.LateBy, st.Message)
	}
	if !strings.Contains(st.Message, "no pool can run it") {
		t.Fatalf("the message does not say why: %q", st.Message)
	}
	// Not yet due: at risk as well, since no capacity can be planned.
	s.Jobs[0].Deadline = t0r.Add(5 * time.Hour)
	if st := sched.Decide(s, &sched.State{}, sched.DefaultConfig()).Jobs["orphan"]; !st.AtRisk {
		t.Fatalf("due in 5h with no usable pool, AtRisk false (%q)", st.Message)
	}
}

// Regression, found by the stress grid (the worst loss to a baseline): the
// starvation guard ran before the deadline tier and gave a free slot to
// any job idle for 30 minutes, however much slack it had, while a job due
// within the hour waited. On a small farm the guard's unit then holds the slot:
// once it has run MaxLostWork (15 minutes) no preemption may take it, and
// a long unit keeps it for hours. In the grid, point g002 of the quick
// grid (three nodes, deep pipelines) finishes 11 of 33 deadline jobs on
// time against 32 of 32 for plain EDF, the guard re-starting two jobs with
// 36 hours of slack every half hour and feasibility preemption killing
// them each time until one of their units (ten hours long) got past the
// lost-work cap.
func TestReproStarvationOverDeadline(t *testing.T) {
	idle := rjob("idle", "p", 5, 5*3600, 3600, 36*time.Hour)
	idle.LastRunning = t0r.Add(-31 * time.Minute)
	urgent := rjob("urgent", "p", 2, 1800, 900, 50*time.Minute)
	s := sched.Snapshot{Now: t0r, Pools: []sched.Pool{{Name: "cpu", Slots: 1}}, Jobs: []sched.Job{idle, urgent}}
	d := sched.Decide(s, &sched.State{}, sched.DefaultConfig())
	for _, x := range d.Dispatch {
		if x.Job == "idle" {
			t.Fatalf("the only slot went to a job with %s of slack (%s tier) while %q is due in 50 minutes: %s",
				d.Jobs["idle"].Slack.Round(time.Hour), x.Tier, "urgent", d.Jobs["urgent"].Message)
		}
	}
}

// Regression, found by the stress grid: a held job releases nothing but
// had R > 0, so rank() sorted it with the jobs that need slots, ahead of
// ready work.
func TestReproHeldRank(t *testing.T) {
	ready := rjob("ready", "p", 10, 10*3600, 600, 0) // no deadline: R = 0
	ready.Running = map[string]int{"cpu": 2}
	held := rjob("held", "p", 10, 10*3600, 600, 4*time.Hour)
	held.Hold = true
	s := sched.Snapshot{Now: t0r, Pools: []sched.Pool{{Name: "cpu", Slots: 2, Used: 2}}, Jobs: []sched.Job{ready, held}}
	d := sched.Decide(s, &sched.State{}, sched.DefaultConfig())
	if d.Jobs["held"].Rank < d.Jobs["ready"].Rank {
		t.Fatalf("held job ranked %d, ahead of the ready job at %d", d.Jobs["held"].Rank, d.Jobs["ready"].Rank)
	}
}

// Regression, found by the stress grid: free slots were lent to a
// borrowing project in the same decision that preempted a borrower to
// restore another project's nominal quota. admit() holds back the guaranteed project's unmet
// deadline demand only against borrowers; a release inside another
// project's nominal quota (here the surplus tier's, by least slack) takes
// the held-back slot, the guaranteed project's own job is never served
// from free capacity, and reclaim() preempts a borrower for it.
func TestReproLendThenReclaim(t *testing.T) {
	a := rjob("a", "A", 5, 4*3600, 600, 30*time.Hour)  // guaranteed (nominal 1, none used), plenty of slack
	b := rjob("b", "B", 10, 8*3600, 600, 2*time.Hour)  // borrower with a tight deadline
	d := rjob("d", "B", 5, 10*3600, 600, 40*time.Hour) // borrower with slack: the victim
	d.Running = map[string]int{"cpu": 2}
	for i := range 2 {
		d.RunningUnits = append(d.RunningUnits, sched.RunningUnit{ID: fmt.Sprintf("d/r%d", i), Pool: "cpu", Slots: 1, Started: t0r.Add(-time.Minute)})
	}
	// c runs inside its nominal quota and already holds what its deadline
	// needs (no unmet demand), with less slack than a.
	c := rjob("c", "C", 5, 2*3600, 600, 6*time.Hour)
	c.Running = map[string]int{"cpu": 1}
	c.RunningUnits = []sched.RunningUnit{{ID: "c/r", Pool: "cpu", Slots: 1, Started: t0r.Add(-time.Minute)}}
	s := sched.Snapshot{Now: t0r, Pools: []sched.Pool{{Name: "cpu", Slots: 6, Used: 3}},
		Projects: []sched.Project{
			{Name: "A", Quotas: map[string]sched.Quota{"cpu": {Nominal: 1, BorrowingLimit: -1}}},
			{Name: "B", Quotas: map[string]sched.Quota{"cpu": {Nominal: 0, BorrowingLimit: -1}}},
			{Name: "C", Quotas: map[string]sched.Quota{"cpu": {Nominal: 2, BorrowingLimit: -1}}},
		},
		Jobs: []sched.Job{a, b, c, d}}
	dec := sched.Decide(s, &sched.State{}, sched.DefaultConfig())
	lent, reclaimed := 0, 0
	for _, x := range dec.Dispatch {
		if x.Job == "b" || x.Job == "d" {
			lent++
		}
	}
	for _, x := range dec.Preempt {
		if x.Reason == sched.ReasonReclaim {
			reclaimed++
		}
	}
	t.Logf("dispatch %+v preempt %+v", dec.Dispatch, dec.Preempt)
	if lent > 0 && reclaimed > 0 {
		t.Fatalf("lent %d slots to the borrower and preempted %d of its units for A in the same decision", lent, reclaimed)
	}
}

// Regression, found by the stress grid (metamorphic-capacity): the plan's capacity was
// every slot of the pool group, also slots no project planned in it may
// use under its borrowing limit. Two jobs of a project limited to one slot
// each fit on their own, together need 1.6 slots, and the plan, counting
// both slots of the pool, reports both on time: nothing is at risk and no
// burst is sized, while one of them will be late. Doubling the pool made
// more jobs late at that grid point (16 of 18 against 12), because the
// plan stopped seeing the shortfall.
func TestReproPlanIgnoresBorrowingLimit(t *testing.T) {
	x := rjob("x", "p", 20, 0.8*4*3600, 0, 4*time.Hour)
	y := rjob("y", "p", 20, 0.8*4*3600, 0, 4*time.Hour)
	s := sched.Snapshot{Now: t0r, Pools: []sched.Pool{{Name: "cpu", Slots: 2}},
		Projects: []sched.Project{{Name: "p", Quotas: map[string]sched.Quota{"cpu": {Nominal: 0, BorrowingLimit: 1}}}},
		Jobs:     []sched.Job{x, y}}
	d := sched.Decide(s, &sched.State{}, sched.DefaultConfig())
	if !d.Jobs["x"].AtRisk && !d.Jobs["y"].AtRisk {
		t.Fatalf("1.6 slots of work for a project limited to 1 slot, and neither job is at risk: %s / %s", d.Jobs["x"].Message, d.Jobs["y"].Message)
	}
}

// Regression, found by the spot grid: a gang that does not fit in its
// owned pool (the other units there leave too few free slots) gets a
// planned share equal to its gang, so sizeBurst, which capped the burst
// at the job's units less its share, had no room left and reported "no
// burst capacity is left" while the burst pool could run the whole gang.
func TestReproGangBurstRoom(t *testing.T) {
	g := rjob("train", "p", 1, 4*3*3600, 3*3600, 3*time.Hour+30*time.Minute)
	g.Pools, g.SlotsPerUnit = []string{"gpu", "gpu-burst"}, 4
	g.Runnable[0].Slots = 4
	other := rjob("other", "q", 6, 6*6*3600, 6*3600, 30*time.Hour)
	other.Pools = []string{"gpu"}
	other.Running = map[string]int{"gpu": 6}
	for i := range 6 {
		other.RunningUnits = append(other.RunningUnits, sched.RunningUnit{ID: fmt.Sprintf("other/r%d", i), Pool: "gpu", Slots: 1, Started: t0r.Add(-20 * time.Minute)})
	}
	s := sched.Snapshot{Now: t0r,
		Pools: []sched.Pool{{Name: "gpu", Slots: 8, Used: 6}, {Name: "gpu-burst", CostPerSlotHour: 2,
			Burst: &sched.Burst{MaxSlots: 8, Provisioning: 5 * time.Minute, CostPerSlotHour: 2}}},
		Projects: []sched.Project{{Name: "p", BurstBudgetLeft: 1000, LateCostPerHour: 500}, {Name: "q"}},
		Jobs:     []sched.Job{g, other}}
	d := sched.Decide(s, &sched.State{}, sched.DefaultConfig())
	st := d.Jobs["train"]
	t.Logf("%+v", st)
	if st.AtRisk && (st.Burst == nil || st.Burst.Slots["gpu-burst"] < 4) {
		t.Fatalf("the gang is at risk and no burst of a whole gang is sized: %s", st.Message)
	}
}

// Found by the stress grid, for a decision (it changes R for every job):
// under TailAware a job's work must be done by T - t_path, as if its
// critical path came after all of its work, so the critical path is
// counted twice. A single two-hour unit due in three hours, with its slot
// free, is planned to finish at W/A + t_path = 4h and reported at risk;
// it finishes at 2h. Graham's bound for list scheduling counts the
// critical path's own work once: finish <= (W - t_path*k)/A + t_path,
// with k the slots of the units on the path.
func TestReproTailDoubleCount(t *testing.T) {
	skipRepro(t, "sched modeling issue tail-double-count (found by hack/stress): TailAware plans W/A + t_path; proposed (W - t_path*k)/A + t_path; awaiting a decision")
	j := rjob("one", "p", 1, 2*3600, 2*3600, 3*time.Hour)
	s := sched.Snapshot{Now: t0r, Pools: []sched.Pool{{Name: "cpu", Slots: 4}}, Jobs: []sched.Job{j}}
	st := sched.Decide(s, &sched.State{}, sched.DefaultConfig()).Jobs["one"]
	if st.AtRisk {
		t.Fatalf("a 2h unit due in 3h on a free slot is at risk: %s", st.Message)
	}
}
