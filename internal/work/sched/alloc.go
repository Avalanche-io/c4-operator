package sched

import (
	"math"
	"sort"
	"time"
)

// alloc is one decision's working state, shared by Decide and the
// baselines. It never writes to the Snapshot.
type alloc struct {
	s        *Snapshot
	st       *State
	cfg      Config
	now      time.Time
	pools    map[string]*poolState
	poolList []*poolState // by cost, then name
	projects map[string]*projState
	jobs     []*jobCtx
	byID     map[string]*jobCtx
	quotas   bool // enforce project quotas (Decide) or not (baselines)
	out      Decisions
}

type poolState struct {
	p        *Pool
	free     int
	hasQuota bool
	// blocked pools take no further units this decision: their free slots
	// are held for a gang that is waiting to fit.
	blocked bool
}

type projState struct {
	p      Project
	usage  map[string]int // slots in use per pool, including this decision's dispatches
	demand map[string]int // runnable slots not yet released, per pool
}

type jobCtx struct {
	j     *Job
	next  int // cursor into Runnable
	slots int // default slots per unit
	// running is slots running before this decision plus those released
	// by it; incoming is slots being freed for this job by earlier
	// preemptions.
	running, incoming int
	proj              *projState

	hasDeadline, hasRate bool
	T, W, W90, tpath     float64 // seconds, slot-seconds, slot-seconds, seconds
	R, Rb, A             float64
	slack                time.Duration
	feasible             bool
	share                float64
	atRisk               bool
	lateBy               time.Duration
	group                int // 0 feasible with need, 1 infeasible with need, 2 other
	burst                *BurstSuggestion
}

func newAlloc(s *Snapshot, st *State, cfg Config, quotas bool) *alloc {
	st.init()
	a := &alloc{
		s: s, st: st, cfg: cfg, now: s.Now, quotas: quotas,
		pools:    map[string]*poolState{},
		projects: map[string]*projState{},
		byID:     map[string]*jobCtx{},
	}
	a.out.Jobs = map[string]JobState{}
	for i := range s.Pools {
		p := &s.Pools[i]
		ps := &poolState{p: p, free: max(0, p.Slots-p.Used)}
		a.pools[p.Name] = ps
		a.poolList = append(a.poolList, ps)
	}
	sort.SliceStable(a.poolList, func(i, j int) bool {
		if a.poolList[i].p.CostPerSlotHour != a.poolList[j].p.CostPerSlotHour {
			return a.poolList[i].p.CostPerSlotHour < a.poolList[j].p.CostPerSlotHour
		}
		return a.poolList[i].p.Name < a.poolList[j].p.Name
	})
	for _, p := range s.Projects {
		if p.Weight <= 0 {
			p.Weight = 1
		}
		a.projects[p.Name] = &projState{p: p, usage: map[string]int{}, demand: map[string]int{}}
		for pool, q := range p.Quotas {
			if ps := a.pools[pool]; ps != nil && (q.Nominal > 0 || q.BorrowingLimit >= 0) {
				ps.hasQuota = true
			}
		}
	}
	running := map[string]bool{}
	for i := range s.Jobs {
		j := &s.Jobs[i]
		pr := a.projects[j.Project]
		if pr == nil {
			pr = &projState{p: Project{Name: j.Project, Weight: 1}, usage: map[string]int{}, demand: map[string]int{}}
			a.projects[j.Project] = pr
		}
		jc := &jobCtx{j: j, proj: pr, slots: max(1, j.SlotsPerUnit)}
		for pool, n := range j.Running {
			pr.usage[pool] += n
			jc.running += n
		}
		for _, ru := range j.RunningUnits {
			running[ru.ID] = true
		}
		if !j.Hold && !j.HoldStop {
			left := j.RunnableTotal * jc.slots
			for _, pool := range j.Pools {
				if ps := a.pools[pool]; ps != nil {
					pr.demand[pool] += min(left, ps.p.Slots)
				}
			}
		}
		a.jobs = append(a.jobs, jc)
		a.byID[j.ID] = jc
	}
	// Preemptions still in flight count toward the job they were for.
	for id, pp := range st.pending {
		if !running[id] {
			delete(st.pending, id)
			continue
		}
		if jc := a.byID[pp.forJob]; jc != nil {
			jc.incoming += pp.slots
		}
	}
	return a
}

func (a *alloc) quota(proj, pool string) Quota {
	if pr := a.projects[proj]; pr != nil {
		if q, ok := pr.p.Quotas[pool]; ok {
			return q
		}
	}
	return Quota{Nominal: 0, BorrowingLimit: -1}
}

// admit applies Kueue's quota semantics to k more slots for proj in ps.
// ownOnly restricts to the project's nominal quota (used for boosts).
func (a *alloc) admit(pr *projState, ps *poolState, k int, ownOnly bool) bool {
	if !a.quotas || !ps.hasQuota {
		return true
	}
	name := ps.p.Name
	q := a.quota(pr.p.Name, name)
	use := pr.usage[name]
	if use+k <= q.Nominal {
		return true
	}
	if ownOnly {
		return false
	}
	if q.BorrowingLimit >= 0 && use+k-q.Nominal > q.BorrowingLimit {
		return false
	}
	// Borrowing: only capacity that no other project's unmet nominal
	// demand is waiting for may be lent.
	reserve := 0
	for _, other := range a.projects {
		if other == pr {
			continue
		}
		oq := a.quota(other.p.Name, name)
		unmet := min(oq.Nominal-other.usage[name], other.demand[name])
		if unmet > 0 {
			reserve += unmet
		}
	}
	return ps.free-k >= reserve
}

func (jc *jobCtx) unitSlots(u UnitRef) int {
	if u.Slots > 0 {
		return u.Slots
	}
	return jc.slots
}

func (jc *jobCtx) canRelease() bool {
	return !jc.j.Hold && !jc.j.HoldStop && jc.next < len(jc.j.Runnable)
}

// pickPool chooses the cheapest allowed pool with room for k slots that
// the job's project may use. only, when set, restricts the choice.
func (a *alloc) pickPool(jc *jobCtx, k int, ownOnly bool, only *poolState) *poolState {
	var best *poolState
	for _, name := range jc.j.Pools {
		ps := a.pools[name]
		if ps == nil || ps.blocked || ps.free < k {
			continue
		}
		if only != nil && ps != only {
			continue
		}
		if !a.admit(jc.proj, ps, k, ownOnly) {
			continue
		}
		if best == nil || ps.p.CostPerSlotHour < best.p.CostPerSlotHour {
			best = ps
		}
	}
	return best
}

// release dispatches the job's next unit if a pool admits it.
func (a *alloc) release(jc *jobCtx, tier Tier, ownOnly bool, only *poolState) bool {
	if !jc.canRelease() {
		return false
	}
	u := jc.j.Runnable[jc.next]
	k := jc.unitSlots(u)
	if jc.j.MaxParallel > 0 && jc.running+k > jc.j.MaxParallel {
		return false
	}
	ps := a.pickPool(jc, k, ownOnly, only)
	if ps == nil {
		return false
	}
	ps.free -= k
	jc.proj.usage[ps.p.Name] += k
	for _, name := range jc.j.Pools {
		if jc.proj.demand[name] > 0 {
			jc.proj.demand[name] = max(0, jc.proj.demand[name]-k)
		}
	}
	jc.running += k
	jc.next++
	a.out.Dispatch = append(a.out.Dispatch, Dispatch{Job: jc.j.ID, Unit: u.ID, Pool: ps.p.Name, Slots: k, Tier: tier})
	return true
}

// holdForGang reports whether the job's next unit is a gang that fits in
// none of its pools right now, and if so blocks the largest of them so its
// free slots accumulate for the gang instead of being backfilled.
func (a *alloc) holdForGang(jc *jobCtx) bool {
	if !jc.canRelease() {
		return false
	}
	k := jc.unitSlots(jc.j.Runnable[jc.next])
	if k <= 1 {
		return false
	}
	var target *poolState
	for _, name := range jc.j.Pools {
		ps := a.pools[name]
		if ps == nil || ps.p.Slots < k {
			continue
		}
		if ps.free >= k {
			return false // it fits somewhere; not blocked by size
		}
		if target == nil || ps.p.Slots > target.p.Slots {
			target = ps
		}
	}
	if target == nil {
		return false
	}
	target.blocked = true
	return true
}

// fairKey is a project's fair-share usage in one pool: usage over
// entitlement (nominal quota times weight). Projects with no nominal quota
// in a quota pool come after every project that has one.
func (a *alloc) fairKey(pr *projState, ps *poolState) float64 {
	use := float64(pr.usage[ps.p.Name])
	w := pr.p.Weight
	if !a.quotas || !ps.hasQuota {
		return use / w
	}
	q := a.quota(pr.p.Name, ps.p.Name)
	if q.Nominal <= 0 {
		return 1e9 + use/w
	}
	return use / (float64(q.Nominal) * w)
}

// projectUsage is a project's usage over entitlement across all pools,
// the tie-breaker in the deadline tier.
func (a *alloc) projectUsage(pr *projState) float64 {
	use, nom := 0, 0
	for _, n := range pr.usage {
		use += n
	}
	for _, q := range pr.p.Quotas {
		nom += max(0, q.Nominal)
	}
	return float64(use) / (pr.p.Weight * math.Max(1, float64(nom)))
}

// holdStop preempts every running unit of jobs on hold-stop.
func (a *alloc) holdStop() {
	for _, jc := range a.jobs {
		if !jc.j.HoldStop {
			continue
		}
		for _, ru := range jc.j.RunningUnits {
			if _, done := a.st.pending[ru.ID]; done {
				continue
			}
			a.preemptUnit(jc, ru, ReasonHoldStop, "")
		}
	}
}

func (a *alloc) preemptUnit(victim *jobCtx, ru RunningUnit, reason PreemptReason, forJob string) {
	yield := victim.j.Checkpoints && reason != ReasonWrangler
	lost := a.lostWork(ru)
	a.out.Preempt = append(a.out.Preempt, Preempt{
		Job: victim.j.ID, Unit: ru.ID, Pool: ru.Pool, Slots: ru.Slots,
		Reason: reason, For: forJob, Yield: yield, LostWork: lost,
	})
	a.st.pending[ru.ID] = pendingPreempt{job: victim.j.ID, forJob: forJob, pool: ru.Pool, slots: ru.Slots, at: a.now}
	a.st.preemptLog[ru.Pool] = append(a.st.preemptLog[ru.Pool], a.now)
}

// lostWork is the work a preemption throws away: time since the unit's
// last checkpoint (or start) plus its restore cost.
func (a *alloc) lostWork(ru RunningUnit) time.Duration {
	since := ru.Started
	if ru.LastCheckpoint.After(since) {
		since = ru.LastCheckpoint
	}
	return a.now.Sub(since) + ru.RestoreCost
}
