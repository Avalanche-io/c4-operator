package sched

import (
	"testing"
	"time"
)

// A requested burst is topped up to the shortfall the merged plan implies
// (W90 - share x horizon) when budget and burst slots are left and the
// whole burst still costs less than the lateness it prevents; otherwise
// it stays as the plan sized it.
func TestBurstTopUpToMergedShortfall(t *testing.T) {
	run := func(budget float64) int {
		t.Helper()
		j := job("lon", "p", 20, 10, 4*time.Hour)
		j.Pools = []string{"lon", "lon-spot"}
		s := Snapshot{Now: t0, Jobs: []Job{j},
			Pools: []Pool{{Name: "lon", Slots: 1},
				{Name: "lon-spot", CostPerSlotHour: 1, Burst: &Burst{MaxSlots: 50, Provisioning: 5 * time.Minute, CostPerSlotHour: 1}}},
			Projects: []Project{{Name: "p", BurstBudgetLeft: budget, LateCostPerHour: 1000}}}
		a := newAlloc(&s, &State{}, DefaultConfig(), true)
		jc := a.byID["lon"]
		// The plan it came from left a 2 slot-hour shortfall over the 4
		// hours; the merged share is 1 slot, so it needs 10 - 1 x 4 = 6.
		jc.hasDeadline, jc.atRisk = true, true
		jc.T, jc.horizon, jc.W90, jc.share = 4*3600, 4*3600, 10*3600, 1
		jc.deficit, jc.lateBy = 2*3600, time.Hour
		a.burst()
		if jc.burst == nil || !jc.burst.Auto {
			t.Fatalf("budget %v: no burst requested: %+v", budget, jc.burst)
		}
		return jc.burst.Slots["lon-spot"]
	}
	// planned: ceil(7200 / (14400 - 300)) = 1 slot; topped up by
	// ceil(14400 / 14100) = 2 more.
	if n := run(1000); n != 3 {
		t.Fatalf("with budget: %d slots, want 3", n)
	}
	// A budget that covers only the planned burst keeps it.
	if n := run(5); n != 1 {
		t.Fatalf("with a tight budget: %d slots, want 1", n)
	}
}

// A slot held back for another project's need is lent once that need is
// met elsewhere in the same decision (stress spot points s022, s023): p
// is 1 below its gpu nominal with a 2-slot gang, which only gpu-spot can
// take; q's 1-slot unit waits to borrow gpu's free slot. The gang goes to
// gpu-spot after gpu was filled, so gpu is swept again.
func TestSurplusSweepsAgain(t *testing.T) {
	g := job("g", "p", 1, 2, 0)
	g.Pools, g.SlotsPerUnit = []string{"gpu", "gpu-spot"}, 2
	g.Runnable[0].Slots = 2
	held := job("held", "p", 0, 8, 0)
	held.Pools = []string{"gpu"}
	held.RunningUnits, held.Running = runningUnits("held", "gpu", 3, t0.Add(-time.Hour))
	w := job("w", "q", 5, 5, 0)
	w.Pools = []string{"gpu"}
	other := job("other", "q", 0, 8, 0)
	other.Pools = []string{"gpu"}
	other.RunningUnits, other.Running = runningUnits("other", "gpu", 4, t0.Add(-time.Hour))
	s := Snapshot{Now: t0, Jobs: []Job{g, held, w, other},
		Pools: []Pool{{Name: "gpu", Slots: 8, Used: 7}, {Name: "gpu-spot", Slots: 4, Used: 0}},
		Projects: []Project{
			{Name: "p", Quotas: map[string]Quota{"gpu": {Nominal: 4, BorrowingLimit: -1}}},
			{Name: "q", Quotas: map[string]Quota{"gpu": {Nominal: 4, BorrowingLimit: -1}}},
		}}
	a := newAlloc(&s, &State{}, DefaultConfig(), true)
	a.projects["p"].demand["gpu"] = 1 // p's gang is owed its last nominal slot
	a.fair()
	got := map[string]string{}
	for _, x := range a.out.Dispatch {
		got[x.Job] = x.Pool
	}
	if got["g"] != "gpu-spot" || got["w"] != "gpu" {
		t.Fatalf("dispatch %v: want g on gpu-spot and w on gpu's slot", got)
	}
}

// Need that is only gangs the plan does not need now (R 0) is not held
// back from borrowers (stress spot point s033): the guarantee does not
// hold a pool for such a gang and reclaim waits for it, so held slots went
// to the next starving job while a job on time in its plan waited.
func TestAdmitHoldsNothingForWaitingGangs(t *testing.T) {
	gang := job("gang", "q", 1, 8, 24*time.Hour)
	gang.Pools, gang.SlotsPerUnit = []string{"gpu"}, 4
	gang.Runnable[0].Slots = 4
	s := Snapshot{Now: t0, Jobs: []Job{gang},
		Pools: []Pool{{Name: "gpu", Slots: 8, Used: 4}},
		Projects: []Project{
			{Name: "p", Quotas: map[string]Quota{"gpu": {Nominal: 3, BorrowingLimit: -1}}},
			{Name: "q", Quotas: map[string]Quota{"gpu": {Nominal: 1, BorrowingLimit: -1}}},
		}}
	run := func(r float64) bool {
		a := newAlloc(&s, &State{}, DefaultConfig(), true)
		a.projects["p"].usage["gpu"] = 3
		jc := a.byID["gang"]
		jc.proj.demand["gpu"] = 1
		jc.proj.demandJobs = map[string][]*jobCtx{"gpu": {jc}}
		jc.R = r
		a.noteWaiting()
		return a.admit(a.projects["p"], a.pools["gpu"], 4, false)
	}
	if !run(0) {
		t.Fatal("4 free slots held back for a gang the plan does not need now")
	}
	if run(1) {
		t.Fatal("a gang the plan needs now lost its hold-back")
	}
}
