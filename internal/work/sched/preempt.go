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
//     borrowing above their nominal;
//   - feasibility: a job that is on time in the plan, is late at what it
//     holds (max(W90/held, t_path) > T), and holds fewer than
//     min(R, planned share) slots takes slots up to that number, from its
//     own project's other jobs or from borrowers.
//
// A victim unit belongs to a preemptible job with slack above
// PreemptSlack that holds more slots than its planned share (counting
// units already being stopped), has been preempted fewer than
// MaxPreemptionsPerUnit times, would lose at most MaxLostWork, and is
// chosen by least lost work. Each pool
// allows at most PreemptPerWindow preemptions per PreemptWindow.
//
// The share conditions keep preemption a correction toward the plan:
// victims are never taken below their share and candidates never above
// theirs, so two jobs cannot take units back and forth. A gang is taken
// whole, so a gang job may drop below its share. Slots being freed are
// remembered in State, so later calls do not preempt again for the same
// need.
func (a *alloc) preempt() {
	a.reclaim()
	a.restoreFeasibility()
}

type victim struct {
	jc *jobCtx
	ru RunningUnit
}

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

// aboveShare reports whether the job holds more slots than its planned
// share, not counting units already being stopped.
func (jc *jobCtx) aboveShare() bool {
	return float64(jc.running-jc.outgoing) > jc.share+1e-9
}

// victims lists eligible running units in pool, least lost work first.
func (a *alloc) victims(pool string, ok func(*jobCtx) bool) []victim {
	var out []victim
	for _, jc := range a.jobs {
		if !jc.j.Preemptible || jc.slack <= a.cfg.PreemptSlack || !jc.aboveShare() || !ok(jc) {
			continue
		}
		for _, ru := range jc.j.RunningUnits {
			if ru.Pool != pool || ru.Preemptions >= a.cfg.MaxPreemptionsPerUnit {
				continue
			}
			if a.cfg.MaxLostWork > 0 && a.victimLoss(jc, ru) > a.cfg.MaxLostWork {
				continue
			}
			if _, pending := a.st.pending[ru.ID]; pending {
				continue
			}
			out = append(out, victim{jc, ru})
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
		if !v.jc.aboveShare() || !still(v) {
			continue
		}
		if _, pending := a.st.pending[v.ru.ID]; pending {
			continue
		}
		a.preemptUnit(v.jc, v.ru, reason, forJob)
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
				if jc.canRelease() && allows(jc, pool) && (forJob == nil || jc.slack < forJob.slack) {
					forJob = jc
				}
			}
			want := min(q.Nominal-pr.usage[pool], pr.demand[pool]) - incoming - ps.free
			if want <= 0 || forJob == nil {
				continue
			}
			borrowing := func(p *projState) bool {
				return p != pr && p.usage[pool] > a.quota(p.p.Name, pool).Nominal
			}
			vs := a.victims(pool, func(v *jobCtx) bool { return borrowing(v.proj) })
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
		have := float64(jc.running + jc.incoming)
		if have >= math.Min(jc.R, jc.share) || math.Max(div(jc.W90, have), jc.tpath) <= jc.T {
			continue
		}
		cands = append(cands, jc)
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].ratio() < cands[j].ratio() })
	for _, jc := range cands {
		// Enough to be on time, and no more than the plan gives it.
		want := int(math.Ceil(math.Min(jc.R, jc.share)-1e-9)) - (jc.running + jc.incoming)
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
			vs := a.victims(pool, eligible)
			freed := a.take(pool, vs, want, ReasonFeasibility, jc.j.ID, func(v victim) bool { return eligible(v.jc) })
			jc.incoming += freed
			want -= freed
		}
	}
}
