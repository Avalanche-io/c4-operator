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
