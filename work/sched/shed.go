package sched

import "sort"

// shed stops units in a pool that holds more than its slots: its ready
// capacity fell below what runs in it, as when a host runner lowers its
// node's slots under a power cap. The excess is the pool's used slots
// less its slots and less the slots of units already being stopped there
// (State.pending: a yield in progress counts once).
//
// The least valuable units go first: jobs on time in what they report
// before jobs at risk (slack is what a job could do with all its
// attainable slots, so a job at risk in the plan can have more of it than
// one on time), then most slack (a job with no deadline before any with
// one), then least lost work (a
// checkpointing unit yields at its next checkpoint and loses only its
// restore cost), then ID. The fewest slots that cover the excess are
// shed: units that fit what is left are taken in that order; if the rest
// needs a larger unit, the most valuable-to-keep of the smaller ones
// taken are given back as long as the excess stays covered. Only
// preemptible jobs' units are shed; what non-preemptible units hold
// stays over until they finish.
//
// It is not a trade between jobs, so the per-pool preemption rate limit,
// MaxPreemptionsPerUnit and MaxLostWork do not apply, and it does not
// count against the rate limit. Checkpointing units yield (Preempt.Yield).
func (a *alloc) shed() {
	stopping := map[string]int{}
	for _, pp := range a.st.pending {
		stopping[pp.pool] += pp.slots
	}
	for _, ps := range a.poolList {
		name := ps.p.Name
		excess := ps.p.Used - ps.p.Slots - stopping[name]
		if excess <= 0 {
			continue
		}
		var vs []victim
		for _, jc := range a.jobs {
			if !jc.j.Preemptible {
				continue
			}
			for _, ru := range jc.j.RunningUnits {
				if _, pending := a.st.pending[ru.ID]; ru.Pool != name || pending {
					continue
				}
				vs = append(vs, victim{jc, ru})
			}
		}
		sort.SliceStable(vs, func(i, j int) bool {
			x, y := vs[i], vs[j]
			if rx, ry := x.jc.view().atRisk, y.jc.view().atRisk; rx != ry {
				return ry
			}
			if x.jc.slack != y.jc.slack {
				return x.jc.slack > y.jc.slack
			}
			if lx, ly := a.victimLoss(x.jc, x.ru), a.victimLoss(y.jc, y.ru); lx != ly {
				return lx < ly
			}
			return x.ru.ID < y.ru.ID
		})
		// Jobs on time cover the excess first, a larger unit of theirs
		// before any unit of a job at risk; only what they cannot cover is
		// shed from jobs at risk.
		var calm, risky []victim
		for _, v := range vs {
			if v.jc.view().atRisk {
				risky = append(risky, v)
			} else {
				calm = append(calm, v)
			}
		}
		take := pickShed(calm, excess)
		if got := victimSlots(take); got < excess {
			take = append(take, pickShed(risky, excess-got)...)
		}
		for _, v := range take {
			a.preemptUnit(v.jc, v.ru, ReasonCapacity, "")
			v.jc.outgoing += v.ru.Slots
			v.jc.proj.usage[name] -= v.ru.Slots
		}
	}
}

// pickShed chooses from vs (least valuable first) the units to shed for
// excess slots (all of vs when they cannot cover it): those that fit what is left, in order; then, if that is
// not enough, the first unit of the rest, giving back the most valuable of
// those already chosen that are no longer needed.
func pickShed(vs []victim, excess int) []victim {
	var take, rest []victim
	left := excess
	for _, v := range vs {
		if left <= 0 {
			break
		}
		if v.ru.Slots <= left {
			take = append(take, v)
			left -= v.ru.Slots
			continue
		}
		rest = append(rest, v)
	}
	if left <= 0 || len(rest) == 0 {
		return take
	}
	take = append(take, rest[0])
	freed := excess - left + rest[0].ru.Slots
	// Give back the most valuable (last chosen) while still covered.
	for i := len(take) - 2; i >= 0; i-- {
		if freed-take[i].ru.Slots >= excess {
			freed -= take[i].ru.Slots
			take = append(take[:i], take[i+1:]...)
		}
	}
	return take
}

func victimSlots(vs []victim) int {
	n := 0
	for _, v := range vs {
		n += v.ru.Slots
	}
	return n
}
