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

// Known (scenarios branch): a job whose units all wait on an upstream job
// has (running+incoming)/R = 0 and is ranked first.
func TestReproBlockedRank(t *testing.T) {
	skipRepro(t, "known bug blocked-rank: rank() orders blocked jobs first; the fix is on the scenarios branch")
	ready := rjob("ready", "p", 10, 10*3600, 600, 6*time.Hour)
	blocked := rjob("blocked", "p", 0, 10*3600, 600, 8*time.Hour)
	blocked.Runnable, blocked.RunnableTotal, blocked.Blocked = nil, 0, 10
	s := sched.Snapshot{Now: t0r, Pools: []sched.Pool{{Name: "cpu", Slots: 2, Used: 2}}, Jobs: []sched.Job{ready, blocked}}
	d := sched.Decide(s, &sched.State{}, sched.DefaultConfig())
	if d.Jobs["blocked"].Rank < d.Jobs["ready"].Rank {
		t.Fatalf("blocked job ranked %d, ahead of the ready job at %d", d.Jobs["blocked"].Rank, d.Jobs["ready"].Rank)
	}
}

// Known (scenarios branch): Moore-Hodgson evicts the largest job, which
// at its own EDF position would finish before its deadline, so it is at
// risk with LateBy 0, its lateness is priced at $0, and a burst that
// closes it waits for approval.
func TestReproLatenessUnpriced(t *testing.T) {
	skipRepro(t, "known bug lateness-unpriced: planFluid gives an evicted job lateBy 0; the fix is on the scenarios branch")
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

// Found by the stress grid: preemption takes a whole unit from a victim
// whose running slots are above its fractional planned share, leaving it
// below that share, so two jobs with shares under one slot each take the
// slot back and forth. The deadline tier rounds R up (needSlots uses
// ceil) while aboveShare compares running slots with the unrounded share,
// so a job released up to ceil(share) is at once a victim. On a one-slot
// farm the two jobs below preempt each other on every decision and throw
// the work away each time; in the grid it is the churn-pingpong finding.
func TestReproPreemptPingPong(t *testing.T) {
	skipRepro(t, "sched bug churn-pingpong (found by hack/stress): a victim is taken below its fractional share; awaiting a fix in internal/work/sched")
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

// Found by the stress grid: a job none of whose pools is in the snapshot
// (a pool removed, renamed, or not yet created) is left out of the plan,
// so it is never reported at risk, not even past its deadline, and no
// burst is ever sized for it. Its message says "no capacity planned" but
// AtRisk is false, so the HUD shows it on track.
func TestReproUnplannedJobOnTrack(t *testing.T) {
	skipRepro(t, "sched bug past-due-on-track (found by hack/stress): plan() skips jobs with no known pool; awaiting a fix in internal/work/sched")
	j := rjob("orphan", "p", 5, 5*3600, 600, 0)
	j.Deadline = t0r.Add(-time.Hour)
	j.Pools = []string{"gpu"}
	s := sched.Snapshot{Now: t0r, Pools: []sched.Pool{{Name: "cpu", Slots: 4}}, Jobs: []sched.Job{j}}
	d := sched.Decide(s, &sched.State{}, sched.DefaultConfig())
	if st := d.Jobs["orphan"]; !st.AtRisk {
		t.Fatalf("past its deadline with no usable pool, AtRisk false (%q)", st.Message)
	}
}

// Found by the stress grid (the worst loss to a baseline): the starvation
// guard runs before the deadline tier and gives a free slot to any job
// idle for 30 minutes, however much slack it has, while a job due within
// the hour waits. On a small farm the guard's unit then holds the slot:
// once it has run MaxLostWork (15 minutes) no preemption may take it, and
// a long unit keeps it for hours. In the grid, point g002 of the quick
// grid (three nodes, deep pipelines) finishes 11 of 33 deadline jobs on
// time against 32 of 32 for plain EDF, the guard re-starting two jobs with
// 36 hours of slack every half hour and feasibility preemption killing
// them each time until one of their units (ten hours long) got past the
// lost-work cap.
func TestReproStarvationOverDeadline(t *testing.T) {
	skipRepro(t, "sched design bug starvation-over-deadline (found by hack/stress): the guard serves a job with 36h of slack before one due in an hour; awaiting a decision in internal/work/sched")
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
