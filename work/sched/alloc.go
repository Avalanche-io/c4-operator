package sched

import (
	"math"
	"slices"
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
	// gangsWaiting: starving gangs from the guard's first pass that did
	// not fit; they hold their pool after the deadline tier.
	gangsWaiting []*jobCtx
	// Staging (stage.go): links by name; bytes and pool slots promised to
	// stages this decision; each planned pool group's capacity and
	// required slots, by pool.
	links      map[string]*Link
	linkBytes  map[string]float64
	stageSlots map[string]int
	poolGroupC map[string]float64
	poolGroupR map[string]float64
	out        Decisions
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
	p     Project
	usage map[string]int // slots in use per pool, including this decision's dispatches
	// demand is the project's unmet deadline need per pool: slots its
	// jobs need to reach their required rate and could start now.
	demand map[string]int
}

type jobCtx struct {
	j     *Job
	next  int // cursor into Runnable
	slots int // default slots per unit
	// running is slots running before this decision plus those released
	// by it; incoming is slots being freed for this job by preemptions;
	// outgoing is slots of this job's own units being stopped.
	running, incoming, outgoing int
	proj                        *projState

	hasDeadline, hasRate bool
	// deadline is the effective deadline (Job.After); deadlineFor the
	// dependent job that set it, if not the job's own.
	deadline    time.Time
	deadlineFor string
	// startAfter is when a waiting job's upstream jobs are planned to
	// finish, seconds from now (0: none), in the plan and as they report
	// it (shownPlan).
	startAfter, startShown float64
	T, W, W90, tpath       float64 // seconds, slot-seconds, slot-seconds, seconds
	R, Rb, A               float64
	slack                  time.Duration
	feasible               bool // finishes in time with all attainable slots (DESIGN 9.9)
	planOK                 bool // on time in the plan; the deadline tier serves these first
	share                  float64
	atRisk                 bool
	lateBy                 time.Duration
	finish50, finish90     float64 // planned finish, seconds from now
	horizon, deficit       float64 // what a burst must cover: slot-seconds short by horizon
	group                  int     // 0 feasible with need, 1 infeasible with need, 2 other, 3 can use no slot
	noPool                 bool    // none of its pools is in the snapshot
	burst                  *BurstSuggestion
	// shown is what the job reports when it differs from the plan's own
	// figures above (planFluid); nil means it reports those.
	shown  *shownPlan
	groupC float64 // the planning capacity of its pool group
	// remote lists the pools in the snapshot that the job may use once its
	// inputs are staged there (Job.Sources); its Pools in the alloc's copy
	// of the snapshot are only those that have them.
	remote []string
	stage  *Stage
}

// shownPlan is a job's risk, lateness and planned finish as it reports
// them, under Graham's bound.
type shownPlan struct {
	atRisk             bool
	lateBy             time.Duration
	finish50, finish90 float64
}

// view is what the job reports.
func (jc *jobCtx) view() shownPlan {
	if jc.shown != nil {
		return *jc.shown
	}
	return shownPlan{atRisk: jc.atRisk, lateBy: jc.lateBy, finish50: jc.finish50, finish90: jc.finish90}
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
	a.links, a.linkBytes, a.stageSlots = map[string]*Link{}, map[string]float64{}, map[string]int{}
	for i := range s.Links {
		a.links[s.Links[i].Name] = &s.Links[i]
	}
	// A job with Sources runs only where its inputs are: the alloc works on
	// its own copy of the jobs, with Pools cut to those, so the caller's
	// snapshot is never written.
	for i := range s.Jobs {
		if len(s.Jobs[i].Sources) > 0 {
			s.Jobs = slices.Clone(s.Jobs)
			break
		}
	}
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
		if len(j.Sources) > 0 {
			var here []string
			for _, name := range j.Pools {
				if _, far := j.Sources[name]; !far {
					here = append(here, name)
				} else if a.pools[name] != nil {
					jc.remote = append(jc.remote, name)
				}
			}
			j.Pools = here
		}
		for pool, n := range j.Running {
			pr.usage[pool] += n
			jc.running += n
		}
		for _, ru := range j.RunningUnits {
			running[ru.ID] = true
		}
		a.jobs = append(a.jobs, jc)
		a.byID[j.ID] = jc
	}
	// Guard bookkeeping: units the guard started stay marked while they
	// run; a job whose guard unit was preempted waits a full
	// StarvationDeferMax for its next guard start.
	for id := range st.guard {
		if !running[id] {
			delete(st.guard, id)
		}
	}
	for id, at := range st.guardHit {
		if s.Now.Sub(at) >= cfg.StarvationDeferMax {
			delete(st.guardHit, id)
		}
	}
	for k, at := range st.took {
		if s.Now.Sub(at) >= time.Hour {
			delete(st.took, k)
		}
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
		if jc := a.byID[pp.job]; jc != nil {
			jc.outgoing += pp.slots
		}
	}
	return a
}

// needDemand fills each project's demand from its jobs' deadline need. It
// runs after objectives, because need depends on R.
func (a *alloc) needDemand() {
	for _, jc := range a.jobs {
		if jc.j.Hold || jc.j.HoldStop || jc.R <= 0 {
			continue
		}
		need := min(jc.needSlots(jc.R), jc.j.RunnableTotal*jc.slots)
		if need <= 0 {
			continue
		}
		for _, pool := range jc.j.Pools {
			if ps := a.pools[pool]; ps != nil && a.guaranteeFits(jc, ps) {
				jc.proj.demand[pool] += min(need, ps.p.Slots)
			}
		}
	}
}

// guaranteeFits reports whether the job's next unit could ever run in ps
// on its project's guarantee: a gang larger than the project's nominal
// quota plus the pool's slots no project is guaranteed fits only when
// other projects happen to fall below their own quota, and no reclaim can
// make room for it. Holding free slots back for such a gang (admit's
// reserve, the guarantee's hold) leaves them idle while it waits; on a
// 16-GPU pool an 8-GPU training gang of a project with 7 nominal GPUs
// kept up to 8 GPUs idle for days while renders waited. Such a gang
// still runs when capacity frees, and the starvation guard can drain a
// pool for it.
func (a *alloc) guaranteeFits(jc *jobCtx, ps *poolState) bool {
	k := jc.slots
	if jc.next < len(jc.j.Runnable) {
		k = jc.unitSlots(jc.j.Runnable[jc.next])
	}
	if k <= 1 || !a.quotas || !ps.hasQuota {
		return k <= ps.p.Slots
	}
	guaranteed := 0
	for name := range a.projects {
		guaranteed += max(0, a.quota(name, ps.p.Name).Nominal)
	}
	spare := max(0, ps.p.Slots-guaranteed)
	return k <= a.quota(jc.proj.p.Name, ps.p.Name).Nominal+spare
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
	// Borrowing: capacity another project needs, within its nominal, for
	// jobs below their required rate is not lent. Kueue lends all unused
	// quota and reclaims it later by preemption; holding back only what is
	// needed for deadlines now avoids lending slots that would be taken
	// back within minutes, without holding quota for work that has slack.
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
	if tier == TierStarvation {
		a.st.guard[u.ID] = a.now
	}
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
	a.st.pending[ru.ID] = pendingPreempt{job: victim.j.ID, forJob: forJob, pool: ru.Pool, slots: ru.Slots, at: a.now, reason: reason}
	if reason == ReasonGuard {
		a.st.guardHit[victim.j.ID] = a.now
		delete(a.st.guard, ru.ID)
	}
	if forJob != "" {
		a.st.took[[2]string{victim.j.ID, forJob}] = a.now
	}
	if reason != ReasonCapacity {
		a.st.preemptLog[ru.Pool] = append(a.st.preemptLog[ru.Pool], a.now)
	}
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
