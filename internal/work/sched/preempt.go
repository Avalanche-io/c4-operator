package sched

import (
	"math"
	"sort"
)

// preempt implements DESIGN.md section 9.13. It runs after every tier has
// filled free capacity, so it only acts on need that free capacity could
// not meet:
//
//   - reclaim: a project below its nominal quota in a pool, with runnable
//     work there, takes slots back from projects borrowing above their
//     nominal;
//   - feasibility: a job that is feasible with its attainable capacity but
//     behind at its current allocation (max(W90/running, t_path) > T)
//     takes slots from its own project's other jobs, or from borrowers.
//
// Victims always belong to preemptible jobs with slack above
// PreemptSlack, have been preempted fewer than MaxPreemptionsPerUnit
// times, and are chosen by least lost work. Each pool allows at most
// PreemptPerWindow preemptions per PreemptWindow. Slots being freed are
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

// victims lists eligible running units in pool, least lost work first.
func (a *alloc) victims(pool string, ok func(*jobCtx) bool) []victim {
	var out []victim
	for _, jc := range a.jobs {
		if !jc.j.Preemptible || jc.slack <= a.cfg.PreemptSlack || !ok(jc) {
			continue
		}
		for _, ru := range jc.j.RunningUnits {
			if ru.Pool != pool || ru.Preemptions >= a.cfg.MaxPreemptionsPerUnit {
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

func (a *alloc) reclaim() {
	for _, ps := range a.poolList {
		if !ps.hasQuota {
			continue
		}
		pool := ps.p.Name
		names := make([]string, 0, len(a.projects))
		for n := range a.projects {
			names = append(names, n)
		}
		sort.Strings(names)
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
			over := map[*projState]int{}
			for _, other := range a.projects {
				if other != pr {
					over[other] = other.usage[pool] - a.quota(other.p.Name, pool).Nominal
				}
			}
			vs := a.victims(pool, func(v *jobCtx) bool { return v.proj != pr && over[v.proj] > 0 })
			freed := 0
			for _, v := range vs {
				if freed >= want || a.budget(pool) <= 0 {
					break
				}
				if over[v.jc.proj] <= 0 {
					continue
				}
				a.preemptUnit(v.jc, v.ru, ReasonReclaim, forJob.j.ID)
				over[v.jc.proj] -= v.ru.Slots
				v.jc.proj.usage[pool] -= v.ru.Slots
				freed += v.ru.Slots
			}
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
		if !jc.hasDeadline || !jc.feasible || !jc.canRelease() || jc.T <= 0 {
			continue
		}
		have := float64(jc.running + jc.incoming)
		if math.Max(div(jc.W90, have), jc.tpath) <= jc.T {
			continue
		}
		if jc.needSlots(jc.R) <= 0 {
			continue
		}
		cands = append(cands, jc)
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].ratio() < cands[j].ratio() })
	for _, jc := range cands {
		want := jc.needSlots(jc.R)
		// A gang needs room for all of it.
		want = max(want, jc.unitSlots(jc.j.Runnable[jc.next]))
		for _, pool := range jc.j.Pools {
			ps := a.pools[pool]
			if ps == nil || want <= 0 {
				continue
			}
			vs := a.victims(pool, func(v *jobCtx) bool {
				if v == jc {
					return false
				}
				if v.proj == jc.proj || !a.quotas || !ps.hasQuota {
					return true
				}
				return v.proj.usage[pool] > a.quota(v.proj.p.Name, pool).Nominal
			})
			freed := 0
			for _, v := range vs {
				if freed >= want || a.budget(pool) <= 0 {
					break
				}
				if v.jc.proj != jc.proj && ps.hasQuota && v.jc.proj.usage[pool] <= a.quota(v.jc.proj.p.Name, pool).Nominal {
					continue
				}
				a.preemptUnit(v.jc, v.ru, ReasonFeasibility, jc.j.ID)
				v.jc.proj.usage[pool] -= v.ru.Slots
				freed += v.ru.Slots
			}
			jc.incoming += freed
			want -= freed
		}
	}
}
