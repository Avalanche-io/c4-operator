package main

import (
	"fmt"

	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Capacity shrinks (Point.CapShrink per owned pool per day): a pool's
// ready slots drop by a quarter to three quarters for half an hour to
// three and a half hours, without killing anything, as when a host
// runner lowers its node's slots under a power cap. Units keep running
// over the line until sched sheds them (ReasonCapacity); checkpointing
// units yield at their next checkpoint.

func (e *engine) scheduleShrinks(p *pool) {
	if e.w.pt.CapShrink <= 0 || p.def.burst != nil {
		return
	}
	e.nextShrink(p, 0, 0)
}

func (e *engine) nextShrink(p *pool, k int, from float64) {
	t := from + e.expo(e.w.pt.CapShrink/day, e.hrand(9, uint64(p.idx), 0, uint64(k)))
	if t < e.w.horizon {
		e.push(&event{t: t, kind: evShrink, p: p, attempt: k})
	}
}

func (e *engine) shrink(p *pool, k int) {
	e.nextShrink(p, k+1, e.now)
	if p.capped > 0 || p.slots <= 0 {
		return
	}
	n := max(1, int(float64(p.slots)*(0.25+0.5*e.hrand(10, uint64(p.idx), 1, uint64(k)))))
	n = min(n, p.slots)
	p.meter(e.now)
	p.capped = n
	p.slots -= n
	e.res.Shrinks++
	dur := (0.5 + 3*e.hrand(11, uint64(p.idx), 2, uint64(k))) * hour
	e.push(&event{t: e.now + dur, kind: evUnshrink, p: p})
	e.requestDecide()
}

func (e *engine) unshrink(p *pool) {
	if p.capped == 0 {
		return
	}
	p.meter(e.now)
	p.slots += p.capped
	p.capped = 0
	e.requestDecide()
}

// checkCapacity checks a decision against every pool over its slots:
// sched asks to stop at least the excess, beyond what is already being
// stopped and what non-preemptible units hold (capacity-shed); it sheds no
// unit it could have kept (capacity-overshed); and it does not shed a job
// at risk while preemptible jobs on time with slack above PreemptSlack
// keep running in the pool with enough slots to cover the excess
// (capacity-order).
func (c *checker) checkCapacity(v *view) {
	shed := map[string][]sched.Preempt{}
	// stopNow is every unit this decision stops; before is what was
	// already stopping when the shed ran (hold-stops come before it,
	// reclaim and feasibility after).
	stopNow, before := map[string]int{}, map[string]int{}
	for _, x := range v.d.Preempt {
		stopNow[x.Pool] += x.Slots
		switch x.Reason {
		case sched.ReasonCapacity:
			shed[x.Pool] = append(shed[x.Pool], x)
		case sched.ReasonHoldStop:
			before[x.Pool] += x.Slots
		}
	}
	for _, p := range v.s.Pools {
		over := p.Used - p.Slots
		if over <= 0 && len(shed[p.Name]) == 0 {
			continue
		}
		yielding, fixed := 0, 0
		keep := map[string]bool{} // jobs on time with slack still running in the pool
		keepSlots := 0
		gone := map[string]bool{}
		for _, x := range v.d.Preempt {
			gone[x.Unit] = true
		}
		for i := range v.s.Jobs {
			j := &v.s.Jobs[i]
			st := v.d.Jobs[j.ID]
			for _, ru := range j.RunningUnits {
				if ru.Pool != p.Name {
					continue
				}
				switch {
				case !j.Preemptible:
					fixed += ru.Slots
				case c.asked[ru.ID]:
					// Asked to stop in an earlier decision: yielding at its
					// next checkpoint, or ending before it.
					yielding += ru.Slots
				case !gone[ru.ID] && !st.AtRisk && st.Slack > c.cfg.PreemptSlack:
					keep[j.ID] = true
					keepSlots += ru.Slots
				}
			}
		}
		if over > yielding+stopNow[p.Name]+fixed {
			c.violate("capacity-shed", p.Name, fmt.Sprintf("%d slots over (%d used of %d) with %d being stopped, %d stopped now and %d not preemptible", over, p.Used, p.Slots, yielding, stopNow[p.Name], fixed))
		}
		need := over - yielding - before[p.Name]
		for _, x := range shed[p.Name] {
			if slotsOf(shed[p.Name])-x.Slots >= need {
				c.violate("capacity-overshed", x.Job, fmt.Sprintf("shed %s (%d slots) from %s though the other shed units cover the %d over", x.Unit, x.Slots, p.Name, need))
				break
			}
		}
		// A job at risk may be shed when the units of jobs on time with
		// slack could not have covered the excess without it.
		calm := 0
		for _, x := range shed[p.Name] {
			if !v.d.Jobs[x.Job].AtRisk {
				calm += x.Slots
			}
		}
		for _, x := range shed[p.Name] {
			if !v.d.Jobs[x.Job].AtRisk || calm+keepSlots < need {
				continue
			}
			for id := range keep {
				if id != x.Job {
					c.violate("capacity-order", x.Job, fmt.Sprintf("shed %s from %s, at risk, while %s with more than PreemptSlack of slack keeps running", x.Unit, p.Name, id))
					break
				}
			}
		}
	}
}

func slotsOf(ps []sched.Preempt) int {
	n := 0
	for _, x := range ps {
		n += x.Slots
	}
	return n
}
