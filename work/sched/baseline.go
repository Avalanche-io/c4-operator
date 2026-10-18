package sched

import (
	"sort"
	"time"
)

// The baselines share Decide's signature and the same release mechanics:
// a job's units are released in its dispatch order into the cheapest
// allowed pool with room, a gang only when one pool has room for all of
// it, and a gang at the head of the order that does not fit holds its
// pool's free slots (no backfill around it). They differ from Decide in
// what they leave out: no project quotas, no estimates, no information or
// fair-share tiers, no burst, and no preemption (except the Manual
// wrangler's suspensions, below). Holds and hold-stops are honored.

// FIFO releases work in submission order.
func FIFO(s Snapshot, st *State, cfg Config) Decisions {
	return baseline(&s, st, cfg, func(a *alloc, x, y *jobCtx) bool {
		if !x.j.Submitted.Equal(y.j.Submitted) {
			return x.j.Submitted.Before(y.j.Submitted)
		}
		return x.j.ID < y.j.ID
	}, false)
}

// EDF releases work in order of effective deadline (jobs with none last),
// then submission. Non-preemptive at the unit level: running units finish.
func EDF(s Snapshot, st *State, cfg Config) Decisions {
	return baseline(&s, st, cfg, func(a *alloc, x, y *jobCtx) bool {
		dx, dy := x.j.Deadline, y.j.Deadline
		switch {
		case dx.IsZero() && !dy.IsZero():
			return false
		case !dx.IsZero() && dy.IsZero():
			return true
		case !dx.Equal(dy):
			return dx.Before(dy)
		case !x.j.Submitted.Equal(y.j.Submitted):
			return x.j.Submitted.Before(y.j.Submitted)
		}
		return x.j.ID < y.j.ID
	}, false)
}

// Manual models a render farm run the usual way, by a competent wrangler.
//
// Static priorities are set at submission by show and job type (the
// caller fills Job.Priority). Between checks the queue is strict
// priority, then submission order, like Deadline's default scheduling.
//
// Every WranglerEvery (2 hours) the wrangler reviews every job with a
// deadline, using only what a render manager shows (units done, their
// average run time, units left, units running):
//
//  1. A job with completed units is projected to finish at now + units
//     left * average unit time / max(1, units running). If that is later
//     than its deadline minus WranglerMargin (1 hour), it is bumped to the
//     top.
//  2. A job with no completed and no running unit whose deadline is less
//     than WranglerStartAlarm (4 hours) away is bumped.
//  3. A bumped job whose projection is earlier than its deadline minus
//     WranglerUnbump (3 hours) is returned to its static priority.
//  4. Bumped jobs run before all others, earliest deadline first.
//  5. If a bumped job has runnable units and a pool it may use is full,
//     the wrangler suspends running units of preemptible jobs with lower
//     static priority in that pool, as many slots as the bumped job could
//     use, at most PreemptPerWindow units per review. A suspension in a
//     render manager kills the task: work since its last checkpoint is
//     lost (Preempt.Yield is false).
func Manual(s Snapshot, st *State, cfg Config) Decisions {
	return baseline(&s, st, cfg, func(a *alloc, x, y *jobCtx) bool {
		bx, by := a.st.manual.bumped[x.j.ID], a.st.manual.bumped[y.j.ID]
		switch {
		case bx != by:
			return bx
		case bx && !x.j.Deadline.Equal(y.j.Deadline):
			return x.j.Deadline.Before(y.j.Deadline)
		case x.j.Priority != y.j.Priority:
			return x.j.Priority > y.j.Priority
		case !x.j.Submitted.Equal(y.j.Submitted):
			return x.j.Submitted.Before(y.j.Submitted)
		}
		return x.j.ID < y.j.ID
	}, true)
}

func baseline(s *Snapshot, st *State, cfg Config, less func(a *alloc, x, y *jobCtx) bool, wrangler bool) Decisions {
	a := newAlloc(s, st, cfg, false)
	a.holdStop()
	a.shed()
	review := false
	if wrangler {
		review = a.wrangle()
	}
	js := append([]*jobCtx(nil), a.jobs...)
	sort.SliceStable(js, func(i, j int) bool { return less(a, js[i], js[j]) })
	for _, jc := range js {
		for jc.canRelease() && a.release(jc, TierBaseline, false, nil) {
		}
		if jc.canRelease() {
			a.holdForGang(jc)
		}
	}
	if review {
		a.suspendForBumped(js)
	}
	for i, jc := range js {
		a.out.Jobs[jc.j.ID] = JobState{Rank: i + 1, RunningSlots: float64(jc.running)}
	}
	return a.out
}

// wrangle runs the review if it is due and reports whether it ran.
func (a *alloc) wrangle() bool {
	m := &a.st.manual
	if !m.lastCheck.IsZero() && a.now.Sub(m.lastCheck) < a.cfg.WranglerEvery {
		return false
	}
	m.lastCheck = a.now
	live := map[string]bool{}
	for _, jc := range a.jobs {
		j := jc.j
		live[j.ID] = true
		if j.Deadline.IsZero() {
			continue
		}
		left := j.RunnableTotal + j.Blocked + j.Unevaluated + len(j.RunningUnits)
		if left == 0 {
			delete(m.bumped, j.ID)
			continue
		}
		if j.UnitsDone == 0 {
			if len(j.RunningUnits) == 0 && j.Deadline.Sub(a.now) < a.cfg.WranglerStartAlarm {
				m.bumped[j.ID] = true
			}
			continue
		}
		mean := j.DoneSeconds / float64(j.UnitsDone)
		par := max(1, len(j.RunningUnits))
		proj := a.now.Add(time.Duration(float64(left) * mean / float64(par) * float64(time.Second)))
		switch {
		case proj.After(j.Deadline.Add(-a.cfg.WranglerMargin)):
			m.bumped[j.ID] = true
		case m.bumped[j.ID] && proj.Before(j.Deadline.Add(-a.cfg.WranglerUnbump)):
			delete(m.bumped, j.ID)
		}
	}
	for id := range m.bumped {
		if !live[id] {
			delete(m.bumped, id)
		}
	}
	return true
}

func (a *alloc) suspendForBumped(order []*jobCtx) {
	left := a.cfg.PreemptPerWindow
	for _, jc := range order {
		if !a.st.manual.bumped[jc.j.ID] || !jc.canRelease() || left <= 0 {
			continue
		}
		want := (len(jc.j.Runnable) - jc.next) * jc.slots
		for _, pool := range jc.j.Pools {
			ps := a.pools[pool]
			if ps == nil || ps.free > 0 || want <= 0 {
				continue
			}
			var vs []victim
			for _, v := range a.jobs {
				if v == jc || !v.j.Preemptible || v.j.Priority >= jc.j.Priority || a.st.manual.bumped[v.j.ID] {
					continue
				}
				for _, ru := range v.j.RunningUnits {
					if _, p := a.st.pending[ru.ID]; ru.Pool == pool && !p {
						vs = append(vs, victim{v, ru})
					}
				}
			}
			// The wrangler suspends the lowest-priority work first.
			sort.SliceStable(vs, func(i, j int) bool { return vs[i].jc.j.Priority < vs[j].jc.j.Priority })
			for _, v := range vs {
				if want <= 0 || left <= 0 {
					break
				}
				a.preemptUnit(v.jc, v.ru, ReasonWrangler, jc.j.ID)
				want -= v.ru.Slots
				left--
			}
		}
	}
}
