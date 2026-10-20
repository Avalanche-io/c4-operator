package sched

import (
	"math"
	"sort"
	"time"
)

// preempt implements DESIGN.md section 9.13. It runs after every tier has
// filled free capacity, so it only acts on need that free capacity could
// not meet:
//
//   - reclaim: a project below its nominal quota in a pool, with jobs
//     below their required rate there, takes slots back from projects
//     borrowing above their nominal; for a gang only when the plan
//     needs it now, and then room for all of it (up to the quota left);
//   - feasibility: a job that is on time in the plan, is late at what it
//     holds (max(W90/held, t_path) > T), and holds fewer than
//     min(R, planned share) takes slots up to that number rounded down,
//     from its own project's other jobs or from borrowers; and up to it
//     rounded up from units the starvation guard started, whatever work
//     they lose (ReasonGuard). A job the plan makes late may take guard
//     units too, up to its planned share.
//   - time-slicing: a job planned for a fraction of a slot that would be
//     late waiting for one to free (the unit holding it is expected to run
//     past the job's latest start) takes up to that number rounded up from
//     jobs that checkpoint, which yield at their next safe point and lose
//     only their restore cost; such a victim keeps its share rounded down.
//     On a one-slot pool this is how an urgent job runs at all.
//
// A victim unit belongs to a preemptible job with slack above
// PreemptSlack that still holds its planned share, rounded up (rounded
// down for time-slicing; for reclaim a borrower above its nominal gives
// up borrowed units whatever its share, since what it borrowed was lent),
// without the unit (counting units already being stopped), has been preempted fewer than
// MaxPreemptionsPerUnit times, would lose at most MaxLostWork, and is
// chosen by least lost work. Each pool
// allows at most PreemptPerWindow preemptions per PreemptWindow.
//
// The share conditions keep preemption a correction toward the plan:
// victims are never taken below their share rounded up to whole slots
// (what the deadline tier releases to) and candidates never above theirs,
// so two jobs cannot take units back and forth. Slots being freed are
// remembered in State, so later calls do not preempt again for the same
// need.
func (a *alloc) preempt() {
	a.reclaim()
	a.restoreFeasibility()
}

type victim struct {
	jc   *jobCtx
	ru   RunningUnit
	keep keepRule
}

// keepRule is what a victim job keeps of its planned share: rounded up
// (ordinary victims), rounded down (units the starvation guard started,
// and time-slicing), or nothing (borrowed units a lender reclaims).
type keepRule int

const (
	keepCeil keepRule = iota
	keepFloor
	keepNone
)

func (a *alloc) budget(pool string) int {
	log := a.st.preemptLog[pool]
	keep := log[:0]
	for _, t := range log {
		if a.now.Sub(t) < a.cfg.PreemptWindow {
			keep = append(keep, t)
		}
	}
	a.st.preemptLog[pool] = keep
	return a.cfg.PreemptPerWindow - len(keep)
}

// victimLoss is the work a preemption of ru would throw away: only the
// restore cost for a job that yields at its next checkpoint, otherwise
// the time since its last checkpoint plus the restore cost.
func (a *alloc) victimLoss(jc *jobCtx, ru RunningUnit) time.Duration {
	if jc.j.Checkpoints {
		return ru.RestoreCost
	}
	return a.lostWork(ru)
}

// canGive reports whether the job may lose a unit of k slots and still
// hold its planned share rounded up to whole slots, not counting units
// already being stopped. The deadline tier releases up to ceil(R) (see
// needSlots), so a job at ceil(share) holds what the plan gives it; taking
// a unit from it would leave it below its share, and two jobs whose shares
// are fractions of a slot would take one slot back and forth.
//
// A unit the starvation guard started gives up that protection, as does a
// unit time-sliced for a job that checkpoints: its job need keep only its
// share rounded down (keepFloor). A borrower keeps nothing of what it
// borrowed (keepNone).
func (jc *jobCtx) canGive(k int, rule keepRule) bool {
	keep := int(math.Ceil(jc.share - 1e-9))
	switch rule {
	case keepFloor:
		keep = int(math.Floor(jc.share + 1e-9))
	case keepNone:
		keep = 0
	}
	return jc.running-jc.outgoing-k >= keep
}

// victims lists eligible running units in pool, least lost work first.
// With guardOK, units the starvation guard started are victims whatever
// work they would lose.
func (a *alloc) victims(pool string, ok func(*jobCtx) bool, guardOK bool) []victim {
	return a.victimsBy(pool, ok, func(jc *jobCtx, ru RunningUnit) (keepRule, bool) {
		if _, guard := a.st.guard[ru.ID]; guard && guardOK {
			return keepFloor, true
		}
		return keepCeil, true
	})
}

// victimsBy lists eligible running units in pool, least lost work first;
// rule says what each unit's job keeps, and whether it is a victim at all.
func (a *alloc) victimsBy(pool string, ok func(*jobCtx) bool, rule func(*jobCtx, RunningUnit) (keepRule, bool)) []victim {
	var out []victim
	for _, jc := range a.jobs {
		if !jc.j.Preemptible || jc.slack <= a.cfg.PreemptSlack || !ok(jc) {
			continue
		}
		for _, ru := range jc.j.RunningUnits {
			keep, eligible := rule(jc, ru)
			if !eligible || ru.Pool != pool || ru.Preemptions >= a.cfg.MaxPreemptionsPerUnit || !jc.canGive(ru.Slots, keep) {
				continue
			}
			_, guard := a.st.guard[ru.ID]
			guard = guard && keep == keepFloor
			if a.cfg.MaxLostWork > 0 && a.victimLoss(jc, ru) > a.cfg.MaxLostWork && !guard {
				continue
			}
			if _, pending := a.st.pending[ru.ID]; pending {
				continue
			}
			out = append(out, victim{jc, ru, keep})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := a.lostWork(out[i].ru), a.lostWork(out[j].ru)
		if li != lj {
			return li < lj
		}
		return out[i].ru.ID < out[j].ru.ID
	})
	return out
}

// take preempts victims in order until want slots are freed, the pool's
// rate limit is reached, or no victim qualifies. still re-checks a victim
// against conditions that change as units are taken.
func (a *alloc) take(pool string, vs []victim, want int, reason PreemptReason, forJob string, still func(victim) bool) int {
	freed := 0
	for _, v := range vs {
		if freed >= want || a.budget(pool) <= 0 {
			break
		}
		if !v.jc.canGive(v.ru.Slots, v.keep) || !still(v) {
			continue
		}
		if _, pending := a.st.pending[v.ru.ID]; pending {
			continue
		}
		// A job does not take a unit back, within the hour, from the job
		// that took one from it: two plans that flip as estimates move
		// would otherwise trade a slot.
		if _, back := a.st.took[[2]string{forJob, v.jc.j.ID}]; back && forJob != "" {
			continue
		}
		r := reason
		if _, guard := a.st.guard[v.ru.ID]; guard && reason == ReasonFeasibility {
			r = ReasonGuard
		}
		a.preemptUnit(v.jc, v.ru, r, forJob)
		v.jc.outgoing += v.ru.Slots
		v.jc.proj.usage[pool] -= v.ru.Slots
		freed += v.ru.Slots
	}
	return freed
}

func (a *alloc) reclaim() {
	names := make([]string, 0, len(a.projects))
	for n := range a.projects {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, ps := range a.poolList {
		if !ps.hasQuota {
			continue
		}
		pool := ps.p.Name
		for _, name := range names {
			pr := a.projects[name]
			q := a.quota(name, pool)
			incoming := 0
			var forJob *jobCtx
			for _, jc := range a.jobs {
				if jc.proj != pr {
					continue
				}
				incoming += jc.incoming
				if !jc.canRelease() || !allows(jc, pool) {
					continue
				}
				// A gang the plan does not need now waits: the guarantee
				// holds a pool only for a gang with R > 0, so part of a
				// gang's room taken back for any other would be lent
				// again in the next decision.
				if jc.unitSlots(jc.j.Runnable[jc.next]) > 1 && jc.R <= 0 {
					continue
				}
				if forJob == nil || jc.slack < forJob.slack {
					forJob = jc
				}
			}
			if forJob == nil || pr.demand[pool] <= 0 {
				continue
			}
			// A gang needs room for all of it.
			need := max(pr.demand[pool], forJob.unitSlots(forJob.j.Runnable[forJob.next]))
			want := min(q.Nominal-pr.usage[pool], need) - incoming - ps.free
			if want <= 0 {
				continue
			}
			borrowing := func(p *projState) bool {
				return p != pr && p.usage[pool] > a.quota(p.p.Name, pool).Nominal
			}
			// What a borrower borrowed was lent: it keeps none of it
			// back for its share (keepNone).
			vs := a.victimsBy(pool, func(v *jobCtx) bool { return borrowing(v.proj) },
				func(*jobCtx, RunningUnit) (keepRule, bool) { return keepNone, true })
			freed := a.take(pool, vs, want, ReasonReclaim, forJob.j.ID, func(v victim) bool { return borrowing(v.jc.proj) })
			forJob.incoming += freed
		}
	}
}

func allows(jc *jobCtx, pool string) bool {
	for _, p := range jc.j.Pools {
		if p == pool {
			return true
		}
	}
	return false
}

func (a *alloc) restoreFeasibility() {
	var cands []*jobCtx
	for _, jc := range a.jobs {
		if !jc.hasDeadline || !jc.planOK || !jc.canRelease() || jc.T <= 0 {
			continue
		}
		// A candidate is short of its plan and late at what it holds.
		have := float64(jc.running + jc.incoming)
		if have >= math.Min(jc.R, jc.share)-1e-9 || math.Max(div(jc.W90, have), jc.tpath) <= jc.T {
			continue
		}
		cands = append(cands, jc)
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].ratio() < cands[j].ratio() })
	for _, jc := range cands {
		target := math.Min(jc.R, jc.share)
		// From ordinary victims a candidate takes its plan rounded down:
		// victims keep their share rounded up (canGive), and the one-slot
		// band between the two keeps units from going back and forth, so
		// a job planned for a fraction of a slot waits for one to free.
		// A unit the starvation guard started is a floor on progress, not
		// a reservation: the candidate may take it for its plan rounded
		// up, and the guard does not restart that job for
		// StarvationDeferMax, which keeps the two from trading the slot.
		a.restoreFrom(jc, int(math.Floor(target+1e-9)), false)
		a.restoreFrom(jc, int(math.Ceil(target-1e-9)), true)
		a.timeSlice(jc, int(math.Ceil(target-1e-9)))
	}
	// A job the plan already makes late misses its deadline by more for
	// every slot it lacks: it too may take guard units, up to its planned
	// share, and time-slice a checkpointing unit. Ordinary units stay
	// protected from it, as before.
	var late []*jobCtx
	for _, jc := range a.jobs {
		if jc.hasDeadline && !jc.planOK && jc.canRelease() && float64(jc.running+jc.incoming) < jc.share-1e-9 {
			late = append(late, jc)
		}
	}
	sort.SliceStable(late, func(i, j int) bool { return late[i].W < late[j].W })
	for _, jc := range late {
		a.restoreFrom(jc, int(math.Ceil(jc.share-1e-9)), true)
		// It misses by less with the slot than waiting for it.
		a.timeSlice(jc, int(math.Ceil(jc.share-1e-9)))
	}
}

// timeSlice takes slots for jc up to upTo held from jobs that checkpoint,
// when waiting for a unit of theirs to end would make jc late: the unit is
// expected to run past jc's latest start (its deadline less the time its
// remaining work needs on upTo slots). The victim yields at its next safe
// point, loses its restore cost, keeps its share rounded down, and may not
// take the slot back within the hour (State.took).
func (a *alloc) timeSlice(jc *jobCtx, upTo int) {
	want := upTo - (jc.running + jc.incoming)
	if want <= 0 || upTo <= 0 {
		return
	}
	want = max(want, jc.unitSlots(jc.j.Runnable[jc.next]))
	startBy := jc.T - math.Max(div(jc.W90, float64(upTo)), jc.tpath)
	for _, pool := range jc.j.Pools {
		ps := a.pools[pool]
		if ps == nil || want <= 0 {
			continue
		}
		eligible := func(v *jobCtx) bool {
			if v == jc || !v.j.Checkpoints {
				return false
			}
			if v.proj == jc.proj || !a.quotas || !ps.hasQuota {
				return true
			}
			return v.proj.usage[pool] > a.quota(v.proj.p.Name, pool).Nominal
		}
		vs := a.victimsBy(pool, eligible, func(_ *jobCtx, ru RunningUnit) (keepRule, bool) {
			return keepFloor, ru.Remaining > 0 && ru.Remaining.Seconds() > startBy
		})
		freed := a.take(pool, vs, want, ReasonFeasibility, jc.j.ID, func(v victim) bool { return eligible(v.jc) })
		jc.incoming += freed
		want -= freed
	}
}

// restoreFrom takes slots for jc up to upTo held (running plus incoming),
// from ordinary victims, or with guardOnly from units the guard started.
func (a *alloc) restoreFrom(jc *jobCtx, upTo int, guardOnly bool) {
	want := upTo - (jc.running + jc.incoming)
	if want <= 0 {
		return
	}
	// A gang needs room for all of it.
	want = max(want, jc.unitSlots(jc.j.Runnable[jc.next]))
	for _, pool := range jc.j.Pools {
		ps := a.pools[pool]
		if ps == nil || want <= 0 {
			continue
		}
		eligible := func(v *jobCtx) bool {
			if v == jc {
				return false
			}
			if v.proj == jc.proj || !a.quotas || !ps.hasQuota {
				return true
			}
			return v.proj.usage[pool] > a.quota(v.proj.p.Name, pool).Nominal
		}
		vs := a.victims(pool, eligible, true)
		if guardOnly {
			kept := vs[:0]
			for _, v := range vs {
				if _, g := a.st.guard[v.ru.ID]; g {
					kept = append(kept, v)
				}
			}
			vs = kept
		}
		freed := a.take(pool, vs, want, ReasonFeasibility, jc.j.ID, func(v victim) bool { return eligible(v.jc) })
		jc.incoming += freed
		want -= freed
	}
}
