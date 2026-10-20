package sched

import "fmt"

// Reconsider decides, at a unit's safe point, whether a soft stop asked of
// it earlier still holds. Arriving at a safe point is a decision point,
// not an automatic stop: the request was made for a reason, and by now the
// reason may have passed. s and d are the snapshot and decisions of the
// call to Decide just made.
//
// pending is false when no stop of the unit is pending (it was never
// asked, or the State was rebuilt since). Otherwise stop says whether it
// stops there and why says what decided it:
//
//   - hold-stop holds while the job is on hold-stop;
//   - capacity-shrink while the pool still holds more than its slots,
//     counting the other units already being stopped there;
//   - a stop for another job (reclaim-nominal, restore-feasibility,
//     guard-unit) holds while that job still waits for slots and is still
//     at risk, and the pool has no free slots for it now. At risk is
//     priced on what that job holds without this unit (its running slots
//     and the other stops pending for it), not on the plan's share, which
//     counts the slot this stop would free: priced on the share, the job
//     the stop is for looks on time, and the stop would withdraw itself
//     at every safe point;
//   - wrangler-suspend always holds.
//
// The caller cancels a stop that does not hold (CancelPreempt).
func (st *State) Reconsider(s Snapshot, d Decisions, unit string) (pending, stop bool, why string) {
	pp, ok := st.pending[unit]
	if !ok {
		return false, false, "no stop is pending"
	}
	var job, forJob *Job
	for i := range s.Jobs {
		switch s.Jobs[i].ID {
		case pp.job:
			job = &s.Jobs[i]
		case pp.forJob:
			forJob = &s.Jobs[i]
		}
	}
	var pool *Pool
	for i := range s.Pools {
		if s.Pools[i].Name == pp.pool {
			pool = &s.Pools[i]
		}
	}
	// Free slots in the pool after this call's dispatches.
	free := 0
	if pool != nil {
		free = pool.Slots - pool.Used
		for _, x := range d.Dispatch {
			if x.Pool == pp.pool {
				free -= x.Slots
			}
		}
	}
	switch pp.reason {
	case ReasonWrangler:
		return true, true, "suspended by a wrangler"
	case ReasonHoldStop:
		if job != nil && job.HoldStop {
			return true, true, "the job is on hold-stop"
		}
		return true, false, "the job's hold-stop was released"
	case ReasonCapacity:
		if pool == nil {
			return true, true, "the pool is gone"
		}
		stopping := 0
		for id, o := range st.pending {
			if id != unit && o.pool == pp.pool {
				stopping += o.slots
			}
		}
		if excess := pool.Used - pool.Slots - stopping; excess > 0 {
			return true, true, fmt.Sprintf("pool %s still holds %d slots over its %d", pp.pool, excess, pool.Slots)
		}
		return true, false, fmt.Sprintf("pool %s has room again", pp.pool)
	}
	if pp.forJob == "" {
		return true, true, string(pp.reason)
	}
	if forJob == nil || forJob.RunnableTotal == 0 {
		return true, false, pp.forJob + " no longer waits for slots"
	}
	if free >= pp.slots {
		return true, false, fmt.Sprintf("capacity returned in %s", pp.pool)
	}
	if js, ok := d.Jobs[pp.forJob]; ok && !js.AtRisk && pp.reason != ReasonReclaim && !st.lateWithout(s, js, forJob, unit) {
		return true, false, pp.forJob + " is on time again"
	}
	return true, true, fmt.Sprintf("%s for %s, which still waits for slots", pp.reason, pp.forJob)
}

// lateWithout says whether job j (planned as js) misses its deadline on
// what it holds without unit's slots: its running slots and the other
// stops pending for it, at p90, max(W90/held, t_path).
func (st *State) lateWithout(s Snapshot, js JobState, j *Job, unit string) bool {
	if js.Deadline.IsZero() {
		return false
	}
	held := 0
	for _, n := range j.Running {
		held += n
	}
	for id, o := range st.pending {
		if id != unit && o.forJob == j.ID {
			held += o.slots
		}
	}
	w90 := j.Estimate.W90
	if j.Estimate.W > w90 {
		w90 = j.Estimate.W
	}
	finish := div(w90, float64(held))
	if j.Estimate.TPath > finish {
		finish = j.Estimate.TPath
	}
	return finish > js.Deadline.Sub(s.Now).Seconds()
}

// Pending is why a pending stop of a unit was asked, and the job it is
// for, if any.
func (st *State) Pending(unit string) (reason PreemptReason, forJob string, ok bool) {
	pp, ok := st.pending[unit]
	return pp.reason, pp.forJob, ok
}
