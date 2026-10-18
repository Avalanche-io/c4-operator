package main

import (
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

// lightParams is a smaller week, so the tests run in seconds.
func lightParams() params {
	p := defaultParams()
	p.DailiesPerEvening = map[string]float64{"showA": 3, "showB": 2, "showC": 2}
	p.PreviewPerHour = map[string]float64{"showA": 0.5, "showB": 0.5, "showC": 0.3}
	p.DeliveriesPerShow = map[string]int{"showA": 2, "showB": 1, "showC": 1}
	p.CIRatePerHour = 8
	p.MLWeekdayRuns, p.MLWeekendRuns = 2, 1
	return p
}

func TestWorkloadDeterministic(t *testing.T) {
	p := defaultParams()
	a, b := generate(p, 3), generate(p, 3)
	if len(a.jobs) != len(b.jobs) {
		t.Fatal("job counts differ")
	}
	for i := range a.jobs {
		if a.jobs[i].id != b.jobs[i].id || !reflect.DeepEqual(a.jobs[i].units, b.jobs[i].units) {
			t.Fatalf("job %d differs", i)
		}
	}
	if c := generate(p, 4); len(c.jobs) == len(a.jobs) && c.jobs[0].submit == a.jobs[0].submit {
		t.Fatal("seeds 3 and 4 gave the same week")
	}
	// Every generator's children are hidden, depend only on siblings, and
	// every job has a rank for each position.
	for _, js := range a.jobs {
		if len(js.rank) != js.nPos {
			t.Fatalf("%s: %d ranks for %d positions", js.id, len(js.rank), js.nPos)
		}
		for _, u := range js.units {
			for _, c := range u.children {
				if !js.units[c].hidden || js.units[c].parent < 0 {
					t.Fatalf("%s: child %d not hidden", js.id, c)
				}
			}
		}
	}
}

// TestEveryPolicyFinishesAndIsDeterministic runs each policy on a light
// week twice. The engine panics if a policy overfills a pool or releases a
// unit that is not runnable, so finishing is itself a check.
func TestEveryPolicyFinishesAndIsDeterministic(t *testing.T) {
	p := lightParams()
	w := generate(p, 1)
	h := buildHistory(p, w.targets)
	for _, pol := range policies {
		if pol.ablation {
			continue
		}
		r1 := newEngine(w, h, 1, pol, mainFarm).run()
		r2 := newEngine(w, h, 1, pol, mainFarm).run()
		if r1.Unfinished != 0 {
			t.Errorf("%s: %d jobs unfinished", pol.name, r1.Unfinished)
		}
		if !reflect.DeepEqual(r1.Jobs, r2.Jobs) || r1.BurstCost != r2.BurstCost || r1.preemptions != r2.preemptions {
			t.Errorf("%s: two runs of the same seed differ", pol.name)
		}
		var ci, other int
		for _, j := range r1.Jobs {
			if j.Kind == "ci" {
				ci++
			} else {
				other++
			}
		}
		if ci+other != len(w.jobs) || len(r1.CITTG) != ci {
			t.Errorf("%s: %d outcomes for %d jobs", pol.name, ci+other, len(w.jobs))
		}
		// Work conservation: every unit's work is done exactly once as
		// useful work, whatever the policy.
		want := 0.0
		for _, js := range w.jobs {
			if js.kind == "ci" {
				continue
			}
			for _, u := range js.units {
				want += u.dur * float64(u.slots)
			}
		}
		got := (r1.UsefulCPUh + r1.UsefulGPUh) * hour
		if math.Abs(got-want) > 1e-6*want {
			t.Errorf("%s: useful work %.1f h, generated %.1f h", pol.name, got/hour, want/hour)
		}
	}
}

func TestBurstProvisionsAndBills(t *testing.T) {
	p := lightParams()
	p.DailiesPerEvening = map[string]float64{"showA": 14, "showB": 10, "showC": 8}
	w := generate(p, 2)
	h := buildHistory(p, w.targets)
	var pol policySpec
	for _, x := range policies {
		if x.name == "objective+burst" {
			pol = x
		}
	}
	r := newEngine(w, h, 2, pol, gpuTightFarm()).run()
	if r.burstRequests == 0 || r.BurstCost <= 0 || r.BurstSlotHours <= 0 {
		t.Fatalf("no burst on an overloaded farm: requests %d cost %.0f", r.burstRequests, r.BurstCost)
	}
	// Billing covers at least the slot-hours of work that ran there.
	if r.BurstSlotHours < 1 {
		t.Fatalf("slot-hours %.1f", r.BurstSlotHours)
	}
}

func TestSchedule(t *testing.T) {
	dur := []float64{4, 1, 1, 1, 1}
	start, end := schedule(dur, []int{0, 1, 2, 3, 4}, 2)
	if start[0] != 0 || start[1] != 0 || end[4] != 4 || start[4] != 3 {
		t.Fatalf("start %v end %v", start, end)
	}
}

func TestOrderArmsOnAHeavyShot(t *testing.T) {
	p := defaultOrderParams(1)
	prior := orderPrior(p)
	r := rand.New(rand.NewPCG(9, 9))
	var s shot
	for {
		s = makeShot(r, p, 240, 8)
		if s.heavyLo > 60 && s.heavyHi < 180 {
			break // a region in the middle, away from the first frames
		}
	}
	b := runArm(p, prior, s, "binary", nil)
	q := runArm(p, prior, s, "sequential", nil)
	if b.FlagAt < 0 || q.FlagAt < 0 || b.FlagAt >= q.FlagAt {
		t.Fatalf("binary flagged at %.2f, sequential at %.2f", b.FlagAt, q.FlagAt)
	}
	if len(b.AbsErr) != p.GridPoints || len(b.Pts) != p.GridPoints {
		t.Fatalf("grid %d %d", len(b.AbsErr), len(b.Pts))
	}
	// Both list schedules respect the lower bounds: total work over the
	// slots, and the longest frame.
	total, longest := 0.0, 0.0
	for _, d := range s.dur {
		total += d
		longest = math.Max(longest, d)
	}
	for _, m := range []float64{b.Makespan, q.Makespan} {
		if m < total/float64(p.Slots)-1e-6 || m < longest-1e-6 {
			t.Fatalf("makespan %.0f below bound (work/slots %.0f, longest %.0f)", m, total/float64(p.Slots), longest)
		}
	}
}
