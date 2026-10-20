package sched

import (
	"math"
	"sort"
)

// heldPricing prices a deadline job's reported finish on the slots it can
// actually get. The fluid plan gives a job its share of the pools as
// though slots moved freely between jobs; they move only as units end or
// are stopped. A job whose share is above what it holds (its running
// slots, those released by this decision and those being freed for it)
// has, until the slots it lacks free up, only what it holds and the free
// slots of its pools; after that, its share. It reports that finish, at
// p90 max(t_path, the time W90 takes at those rates), when it is later
// than the plan's, and is at risk when that passes its deadline. The
// slots it lacks free as other jobs' units end, soonest first; a unit
// being stopped frees its slot for the job it is stopped for, and a unit
// whose time left is unknown counts as ending now, as does one this
// decision releases for another job; burst slots requested and not yet
// provisioned come once they are (Burst.Provisioning). A job a burst is
// planned for is left as planned: the burst is its capacity. A job
// waiting on one priced so reports no earlier a finish than it allows
// (showDownstream). Shares and bursts are unchanged: this is what the job
// reports, and what a stop asked for another job is reconsidered against
// (Reconsider).
func (a *alloc) heldPricing() {
	changed := map[*jobCtx]bool{}
	defer func() { a.showDownstream(changed) }()
	for _, jc := range a.jobs {
		if !jc.hasDeadline || jc.T <= 0 || jc.noPool || jc.j.Hold || jc.j.HoldStop || jc.waiting() || jc.burst != nil {
			continue
		}
		held := float64(jc.running + jc.incoming)
		if held <= 0 || jc.share <= held+1e-9 {
			continue
		}
		now, until := a.slotsFor(jc, int(math.Ceil(jc.share-held-1e-9)))
		if until <= 0 {
			continue
		}
		c1 := math.Min(held+float64(now), jc.share)
		at := func(w float64) float64 {
			if w <= c1*until {
				return math.Max(div(w, c1), jc.tpath)
			}
			return math.Max(until+div(w-c1*until, jc.share), jc.tpath)
		}
		v := jc.view()
		f90 := at(jc.W90)
		if f90 <= v.finish90 {
			continue
		}
		s := shownPlan{finish50: math.Max(v.finish50, at(jc.W)), finish90: f90}
		if f90 > jc.T {
			s.atRisk, s.lateBy = true, secs(math.Min(f90, bigSeconds)-jc.T)
		}
		jc.shown = &s
		changed[jc] = true
	}
}

// showDownstream moves what a deadline job still waiting on upstream jobs
// (Job.After) reports, when an upstream job it waits on reports a later
// finish than the plan's (heldPricing): it starts no earlier than that
// finish and then takes its own duration, as in startAfterUpstream. In
// rounds, so a chain follows, at most one round per job.
func (a *alloc) showDownstream(changed map[*jobCtx]bool) {
	for round := 0; round < len(a.jobs) && len(changed) > 0; round++ {
		moved := false
		for _, jc := range a.jobs {
			if !jc.hasDeadline || len(jc.j.After) == 0 || !jc.waiting() || jc.noPool || jc.j.Hold || jc.j.HoldStop {
				continue
			}
			g50, g90, upChanged := 0.0, 0.0, false
			for _, id := range jc.j.After {
				u := a.byID[id]
				if u == nil || u == jc {
					continue
				}
				v := u.view()
				g50, g90 = math.Max(g50, v.finish50), math.Max(g90, v.finish90)
				upChanged = upChanged || changed[u]
			}
			if !upChanged {
				continue
			}
			v := jc.view()
			f90 := g90 + jc.duration()
			if f90 <= v.finish90+1e-6 {
				continue
			}
			s := shownPlan{finish50: math.Max(v.finish50, g50+math.Max(div(jc.W, jc.A), jc.tpath)), finish90: f90}
			if f90 > jc.T {
				s.atRisk, s.lateBy = true, secs(math.Min(f90, bigSeconds)-jc.T)
			}
			jc.shown = &s
			if !changed[jc] {
				changed[jc], moved = true, true
			}
		}
		if !moved {
			return
		}
	}
}

// slotsFor is how many of the lack slots jc wants are free now in the
// pools it may use, and when (seconds from now) the rest will have freed
// as other jobs' units end: 0 when all are free now, +Inf when the units
// there are too few.
func (a *alloc) slotsFor(jc *jobCtx, lack int) (now int, until float64) {
	var ends []float64
	for _, name := range jc.j.Pools {
		ps := a.pools[name]
		if ps == nil {
			continue
		}
		now += max(0, ps.free)
		if b := ps.p.Burst; b != nil {
			for k := 0; k < b.Requested-b.Provisioned; k++ {
				ends = append(ends, b.Provisioning.Seconds())
			}
		}
		for _, o := range a.jobs {
			if o == jc {
				continue
			}
			for _, ru := range o.j.RunningUnits {
				if ru.Pool != name {
					continue
				}
				if _, stopping := a.st.pending[ru.ID]; stopping {
					continue
				}
				for k := 0; k < max(1, ru.Slots); k++ {
					ends = append(ends, math.Max(0, ru.Remaining.Seconds()))
				}
			}
		}
		for _, d := range a.out.Dispatch {
			if d.Pool == name && d.Job != jc.j.ID {
				for k := 0; k < max(1, d.Slots); k++ {
					ends = append(ends, 0)
				}
			}
		}
	}
	need := lack - now
	if need <= 0 {
		return lack, 0
	}
	if need > len(ends) {
		return now, math.Inf(1)
	}
	sort.Float64s(ends)
	return now, ends[need-1]
}
