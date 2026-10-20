package sched

import (
	"testing"
	"time"
)

// oneBlock is a job holding a one-slot pool with one long checkpointing
// unit, expected to run on for left.
func oneBlock(left time.Duration) Job {
	b := job("block", "practice", 0, 1, 0)
	b.Runnable, b.RunnableTotal = nil, 0
	b.RunningUnits, b.Running = runningUnits("block", "cpu", 1, t0.Add(-10*time.Minute))
	b.RunningUnits[0].Remaining = left
	b.Checkpoints = true
	return b
}

// On a one-slot pool, a job planned for a fraction of the slot that would
// be late waiting for the slot to free has a checkpointing unit yield at
// its next safe point (time-slicing). It does not when the slot frees in
// time, nor when the unit cannot yield.
func TestTimeSliceOnAOneSlotPool(t *testing.T) {
	urgent := job("urgent", "practice", 1, 0.5, time.Hour)
	s := Snapshot{Now: t0, Pools: []Pool{{Name: "cpu", Slots: 1, Used: 1}}, Jobs: []Job{oneBlock(50 * time.Minute), urgent}}
	d := Decide(s, &State{}, DefaultConfig())
	if len(d.Preempt) != 1 {
		t.Fatalf("preempted %+v", d.Preempt)
	}
	if p := d.Preempt[0]; p.Job != "block" || p.Reason != ReasonFeasibility || p.For != "urgent" || !p.Yield {
		t.Fatalf("preempt %+v", p)
	}
	// The block frees the slot in time: the urgent job waits for it.
	s.Jobs[0] = oneBlock(10 * time.Minute)
	if d := Decide(s, &State{}, DefaultConfig()); len(d.Preempt) != 0 {
		t.Fatalf("preempted a unit that ends in time: %+v", d.Preempt)
	}
	// A unit that cannot yield is not sliced.
	s.Jobs[0] = oneBlock(50 * time.Minute)
	s.Jobs[0].Checkpoints = false
	if d := Decide(s, &State{}, DefaultConfig()); len(d.Preempt) != 0 {
		t.Fatalf("sliced a unit that cannot yield: %+v", d.Preempt)
	}
	// Not knowing how long the unit has left, nothing is sliced.
	s.Jobs[0] = oneBlock(0)
	if d := Decide(s, &State{}, DefaultConfig()); len(d.Preempt) != 0 {
		t.Fatalf("sliced a unit with no estimate: %+v", d.Preempt)
	}
}

// On a one-slot pool, a project below its nominal takes the slot back from
// one borrowing it, though the borrower's one unit is all it holds: what
// it borrowed was lent.
func TestReclaimOnAOneSlotPool(t *testing.T) {
	rush := job("rush1", "rush", 1, 0.5, 3*time.Hour)
	s := Snapshot{Now: t0, Pools: []Pool{{Name: "cpu", Slots: 1, Used: 1}},
		Projects: []Project{
			{Name: "practice", Quotas: map[string]Quota{"cpu": {Nominal: 0, BorrowingLimit: 1}}},
			{Name: "rush", Quotas: map[string]Quota{"cpu": {Nominal: 1, BorrowingLimit: -1}}},
		},
		Jobs: []Job{oneBlock(time.Hour), rush}}
	d := Decide(s, &State{}, DefaultConfig())
	if len(d.Preempt) != 1 {
		t.Fatalf("preempted %+v", d.Preempt)
	}
	if p := d.Preempt[0]; p.Job != "block" || p.Reason != ReasonReclaim || p.For != "rush1" || !p.Yield {
		t.Fatalf("preempt %+v", p)
	}
}

// A job the plan already makes late time-slices too: it misses by less.
func TestLateJobTimeSlices(t *testing.T) {
	urgent := job("urgent", "practice", 1, 0.75, 40*time.Minute)
	s := Snapshot{Now: t0, Pools: []Pool{{Name: "cpu", Slots: 1, Used: 1}}, Jobs: []Job{oneBlock(50 * time.Minute), urgent}}
	d := Decide(s, &State{}, DefaultConfig())
	if js := d.Jobs["urgent"]; !js.AtRisk {
		t.Fatalf("urgent is on time in the plan: %+v", js)
	}
	if len(d.Preempt) != 1 || d.Preempt[0].For != "urgent" || !d.Preempt[0].Yield {
		t.Fatalf("preempted %+v", d.Preempt)
	}
}

// c4-onboarding's evening: a render holding one of two GPU slots would be
// late on one (about 128 min of work, 75 min to its deadline); a training
// run that checkpoints holds the other for hours. The training run is
// asked to yield its slot at its next safe point.
func TestTrainingYieldsToALateRender(t *testing.T) {
	render := job("gw010", "show", 59, 59*140.0/3600, 75*time.Minute)
	render.Pools = []string{"gpu"}
	render.RunningUnits, render.Running = runningUnits("gw010", "gpu", 1, t0.Add(-2*time.Minute))
	render.RunningUnits[0].Remaining = 2 * time.Minute
	train := job("style-train", "show", 0, 4, 0)
	train.Class, train.Pools = "ml", []string{"gpu"}
	train.Runnable, train.RunnableTotal = nil, 0
	train.RunningUnits, train.Running = runningUnits("style-train", "gpu", 1, t0.Add(-20*time.Minute))
	train.RunningUnits[0].Remaining = 3 * time.Hour
	train.Checkpoints = true
	s := Snapshot{Now: t0, Pools: []Pool{{Name: "gpu", Slots: 2, Used: 2}}, Jobs: []Job{render, train}}
	d := Decide(s, &State{}, DefaultConfig())
	if len(d.Preempt) != 1 || d.Preempt[0].Job != "style-train" || d.Preempt[0].For != "gw010" || !d.Preempt[0].Yield {
		t.Fatalf("preempted %+v; gw010 %+v", d.Preempt, d.Jobs["gw010"])
	}
}
