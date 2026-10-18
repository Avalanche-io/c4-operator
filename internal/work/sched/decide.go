package sched

import (
	"container/heap"
	"math"
	"sort"
	"time"
)

// Decide is the objective-driven policy of DESIGN.md section 9.9.
//
// Order of work in one call:
//
//  0. Hold-stop preemptions, then the starvation guard: a job with no
//     running unit for StarvationAfter gets one unit (a gang that does not
//     fit reserves its pool's free slots instead). A starving job with
//     more than PreemptSlack of slack is served right after the deadline
//     tier instead, until it has starved for StarvationDeferMax.
//  1. Deadline need: jobs below their required slots R (see plan.go for
//     how R is computed), jobs on time in the plan first, most behind
//     first by (running+incoming)/R; ratios in the same TieBand bucket go
//     to the project with lower fair-share usage, then to the older job.
//     Jobs late in the plan follow in order of smallest remaining work.
//     Releases inside each project's nominal quota go first; then the
//     guarantee (projects below their nominal quota with deadline demand
//     get units from free slots, inside that quota, and a guaranteed gang
//     that does not fit holds its pool); then releases that borrow; then
//     starving jobs that could wait (step 0).
//  2. Boost: the extra slots of a boosted job (R*(boost-1)), from its own
//     project's nominal quota first, then from lendable capacity.
//  3. Information: unevaluated generators and coarse binary-order units,
//     round robin over jobs by least slack.
//  4. Surplus: per pool, cheapest first, one unit at a time, by least
//     planned slack with fair share as the tiebreaker (or, with
//     SurplusFairShare, by fair share first as DESIGN.md 9.9 has it).
//  5. Preemption (section 9.13) to reclaim nominal quota and to restore
//     feasibility.
//  6. Burst (section 9.12), when enabled.
func Decide(s Snapshot, st *State, cfg Config) Decisions {
	a := newAlloc(&s, st, cfg, true)
	a.objectives()
	a.needDemand()
	a.plan()
	a.holdStop()
	a.starvation(true)
	a.needPass(true)
	a.guarantee()
	a.needPass(false)
	a.starvation(false)
	a.boost()
	a.info()
	a.fair()
	a.preempt()
	a.burst()
	a.rank()
	a.report()
	return a.out
}

const bigSeconds = 1e9

func div(w, a float64) float64 {
	if w <= 0 {
		return 0
	}
	if a <= 0 {
		return math.Inf(1)
	}
	return w / a
}

func secs(x float64) time.Duration {
	const lim = float64(100000 * time.Hour)
	d := x * float64(time.Second)
	return time.Duration(math.Max(-lim, math.Min(lim, d)))
}

func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// attainable is the most slots the job could hold: its pools' capacity,
// limited per pool by its project's nominal plus borrowing limit, then by
// the parallel cap and by how many units it has left (unless unexpanded
// generators may add more). With planned set, burst slots already
// requested but not yet provisioned count too.
func (a *alloc) attainable(jc *jobCtx, planned bool) float64 {
	total := 0.0
	for _, name := range jc.j.Pools {
		ps := a.pools[name]
		if ps == nil {
			continue
		}
		c := float64(ps.p.Slots)
		if planned && ps.p.Burst != nil {
			c += float64(max(0, ps.p.Burst.Requested-ps.p.Burst.Provisioned))
		}
		if a.quotas && ps.hasQuota {
			q := a.quota(jc.proj.p.Name, name)
			if q.BorrowingLimit >= 0 {
				c = math.Min(c, float64(q.Nominal+q.BorrowingLimit))
			}
		}
		total += c
	}
	if jc.j.MaxParallel > 0 {
		total = math.Min(total, float64(jc.j.MaxParallel))
	}
	if jc.j.Unevaluated == 0 {
		units := jc.j.RunnableTotal + jc.j.Blocked + len(jc.j.RunningUnits)
		total = math.Min(total, float64(units*jc.slots))
	}
	return total
}

func (a *alloc) objectives() {
	for _, jc := range a.jobs {
		j := jc.j
		jc.W, jc.W90, jc.tpath = j.Estimate.W, math.Max(j.Estimate.W90, j.Estimate.W), j.Estimate.TPath
		jc.A = a.attainable(jc, false)
		have := float64(jc.running + jc.incoming)
		switch {
		case !j.Deadline.IsZero():
			jc.hasDeadline = true
			jc.T = j.Deadline.Sub(a.now).Seconds()
			jc.R = jc.W90 / math.Max(a.effective(jc), a.cfg.MinRemaining.Seconds())
			finish := math.Max(div(jc.W90, jc.A), jc.tpath)
			jc.slack = secs(jc.T - finish)
			jc.feasible = jc.T-finish >= 0
		case j.RequiredRate > 0:
			jc.hasRate = true
			jc.R = j.RequiredRate
			jc.feasible = jc.A >= jc.R
			if have >= jc.R {
				jc.slack = a.cfg.RateSlack
			}
		default:
			jc.slack = secs(bigSeconds)
			jc.feasible = true
		}
		jc.Rb = jc.R * clamp(j.Boost, 1, 2)
	}
}

// waterfill gives x_i = min(level*w_i, cap_i) with sum x_i = min(C, sum
// cap_i).
func waterfill(w, caps []float64, C float64) []float64 {
	n := len(w)
	x := make([]float64, n)
	if n == 0 || C <= 0 {
		return x
	}
	sum := 0.0
	for _, c := range caps {
		sum += c
	}
	if sum <= C {
		copy(x, caps)
		return x
	}
	idx := make([]int, 0, n)
	for i := range n {
		if w[i] > 0 && caps[i] > 0 {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(p, q int) bool { return caps[idx[p]]/w[idx[p]] < caps[idx[q]]/w[idx[q]] })
	left := C
	wsum := 0.0
	for _, i := range idx {
		wsum += w[i]
	}
	for k, i := range idx {
		level := left / wsum
		if caps[i]/w[i] <= level {
			x[i] = caps[i]
			left -= caps[i]
			wsum -= w[i]
			continue
		}
		for _, r := range idx[k:] {
			x[r] = level * w[r]
		}
		break
	}
	return x
}

// starvation runs twice: before the deadline tier for starving jobs that
// cannot wait (first), and after it for the rest. A starving job can wait
// for the deadline tier when it has more than PreemptSlack of slack and
// has starved for less than StarvationDeferMax, so the guard does not
// give a job due in days the slot a job due within the hour needs, and no
// job waits past StarvationDeferMax however busy the deadline tier is.
func (a *alloc) starvation(first bool) {
	var starving []*jobCtx
	since := func(jc *jobCtx) time.Time {
		switch {
		case !jc.j.LastRunning.IsZero():
			return jc.j.LastRunning
		case !jc.j.Ready.IsZero():
			return jc.j.Ready
		}
		return jc.j.Submitted
	}
	for _, jc := range a.jobs {
		if !jc.canRelease() || jc.running+jc.incoming > 0 || len(jc.j.RunningUnits) > 0 {
			continue
		}
		waited := a.now.Sub(since(jc))
		if waited < a.cfg.StarvationAfter {
			continue
		}
		if a.canWait(jc, waited) == first {
			continue
		}
		starving = append(starving, jc)
	}
	sort.SliceStable(starving, func(i, j int) bool { return since(starving[i]).Before(since(starving[j])) })
	for _, jc := range starving {
		if a.release(jc, TierStarvation, false, nil) {
			continue
		}
		a.holdForGang(jc)
	}
}

// canWait reports whether a starving job may let the deadline tier go
// first.
func (a *alloc) canWait(jc *jobCtx, waited time.Duration) bool {
	if a.cfg.StarvationDeferMax > 0 && waited >= a.cfg.StarvationDeferMax {
		return false
	}
	return jc.slack > a.cfg.PreemptSlack
}

// needHeap orders feasible jobs for the deadline tier.
type needHeap struct {
	a  *alloc
	js []*jobCtx
}

func (h needHeap) Len() int { return len(h.js) }
func (h needHeap) Less(i, j int) bool {
	x, y := h.js[i], h.js[j]
	bx, by := h.a.bucket(x), h.a.bucket(y)
	if bx != by {
		return bx < by
	}
	ux, uy := h.a.projectUsage(x.proj), h.a.projectUsage(y.proj)
	if ux != uy {
		return ux < uy
	}
	if !x.j.Submitted.Equal(y.j.Submitted) {
		return x.j.Submitted.Before(y.j.Submitted)
	}
	return x.j.ID < y.j.ID
}
func (h needHeap) Swap(i, j int) { h.js[i], h.js[j] = h.js[j], h.js[i] }
func (h *needHeap) Push(x any)   { h.js = append(h.js, x.(*jobCtx)) }
func (h *needHeap) Pop() any {
	old := h.js
	n := len(old)
	x := old[n-1]
	h.js = old[:n-1]
	return x
}

func (jc *jobCtx) ratio() float64 {
	if jc.R <= 0 {
		return math.Inf(1)
	}
	return float64(jc.running+jc.incoming) / jc.R
}

func (a *alloc) bucket(jc *jobCtx) int {
	band := a.cfg.TieBand
	if band <= 0 {
		band = 0.1
	}
	r := jc.ratio()
	if math.IsInf(r, 1) {
		return math.MaxInt32
	}
	return int(math.Floor(r / band))
}

func (jc *jobCtx) needSlots(r float64) int {
	return int(math.Ceil(r-1e-9)) - (jc.running + jc.incoming)
}

// needPass is the deadline tier. Decide runs it twice: first only
// releases inside each project's nominal quota, then (after the
// guarantee) those that borrow. A borrowing release passes admit's
// hold-back for other projects' guaranteed demand when it is made, but a
// release inside a nominal quota later in the same call does not check
// it; serving nominal quota first keeps a slot from being lent and then
// reclaimed from the borrower in the same decision.
func (a *alloc) needPass(ownOnly bool) {
	h := &needHeap{a: a}
	var infeasible []*jobCtx
	for _, jc := range a.jobs {
		if jc.R <= 0 || !jc.canRelease() || jc.needSlots(jc.R) <= 0 {
			continue
		}
		if jc.planOK {
			h.js = append(h.js, jc)
			continue
		}
		infeasible = append(infeasible, jc)
	}
	heap.Init(h)
	for h.Len() > 0 {
		jc := heap.Pop(h).(*jobCtx)
		if jc.needSlots(jc.R) <= 0 || !a.release(jc, TierNeed, ownOnly, nil) {
			continue
		}
		heap.Push(h, jc)
	}
	sort.SliceStable(infeasible, func(i, j int) bool { return infeasible[i].W < infeasible[j].W })
	for _, jc := range infeasible {
		for jc.needSlots(jc.R) > 0 && a.release(jc, TierNeed, ownOnly, nil) {
		}
	}
}

// guarantee serves, from free slots, the deadline demand of projects below
// their nominal quota: the demand admit() holds slots back for. Without
// it those slots could go to borrowers or to the information or surplus
// tiers of a third project inside its own quota, and reclaim() would then
// preempt a borrower for the guaranteed project in the same decision.
// A guaranteed gang that does not fit holds its pool's free slots, as the
// starvation guard's gangs do, so they are not lent while reclaim makes
// room for it. Projects go in order of fair-share usage, their jobs by
// least slack.
func (a *alloc) guarantee() {
	if !a.quotas {
		return
	}
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
		prs := make([]*projState, 0, len(names))
		for _, n := range names {
			pr := a.projects[n]
			if pr.demand[pool] > 0 && pr.usage[pool] < a.quota(n, pool).Nominal {
				prs = append(prs, pr)
			}
		}
		sort.SliceStable(prs, func(i, j int) bool { return a.fairKey(prs[i], ps) < a.fairKey(prs[j], ps) })
		for _, pr := range prs {
			var js []*jobCtx
			for _, jc := range a.jobs {
				if jc.proj == pr && jc.canRelease() && allows(jc, pool) {
					js = append(js, jc)
				}
			}
			sort.SliceStable(js, func(i, j int) bool { return js[i].slack < js[j].slack })
			for _, jc := range js {
				for pr.demand[pool] > 0 && pr.usage[pool] < a.quota(pr.p.Name, pool).Nominal && a.release(jc, TierGuarantee, true, ps) {
				}
				if pr.demand[pool] > 0 && pr.usage[pool] < a.quota(pr.p.Name, pool).Nominal && jc.R > 0 && a.holdForGang(jc) {
					break
				}
			}
		}
	}
}

func (a *alloc) boost() {
	var bs []*jobCtx
	for _, jc := range a.jobs {
		if jc.Rb > jc.R && jc.canRelease() {
			bs = append(bs, jc)
		}
	}
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].ratio() < bs[j].ratio() })
	for _, ownOnly := range []bool{true, false} {
		for _, jc := range bs {
			for jc.needSlots(jc.Rb) > 0 && a.release(jc, TierBoost, ownOnly, nil) {
			}
		}
	}
}

func (a *alloc) info() {
	if a.cfg.NoInfoTier {
		return
	}
	var js []*jobCtx
	for _, jc := range a.jobs {
		if jc.canRelease() && jc.j.Runnable[jc.next].Info {
			js = append(js, jc)
		}
	}
	sort.SliceStable(js, func(i, j int) bool { return js[i].slack < js[j].slack })
	count := map[*jobCtx]int{}
	for progress := true; progress; {
		progress = false
		for _, jc := range js {
			if !jc.canRelease() || !jc.j.Runnable[jc.next].Info {
				continue
			}
			if a.cfg.InfoUnitsPerJob > 0 && count[jc] >= a.cfg.InfoUnitsPerJob {
				continue
			}
			if a.release(jc, TierInfo, false, nil) {
				count[jc]++
				progress = true
			}
		}
	}
}

// fair fills what is left after the deadline, boost and information
// tiers.
//
// SurplusSlack (default): per pool, cheapest first, one unit at a time to
// the job with the least planned slack (deadline minus planned finish),
// slack in the same SlackBand going to the project with the lower
// fair-share usage in that pool, then to the older job. This is the rule
// in the requirements ("the jobs most at risk get capacity first, with
// fair share across projects as the tiebreaker").
//
// SurplusFairShare: DESIGN.md 9.9 as written: per pool, one unit at a time
// to the project with the lowest usage over entitlement, and within it to
// the job with least slack.
func (a *alloc) fair() {
	if a.cfg.Surplus == SurplusFairShare {
		a.fairShareFirst()
		return
	}
	var js []*jobCtx
	for _, jc := range a.jobs {
		if jc.canRelease() {
			js = append(js, jc)
		}
	}
	band := a.cfg.SlackBand.Seconds()
	if band <= 0 {
		band = 3600
	}
	key := func(jc *jobCtx) float64 { return math.Floor(a.planSlack(jc) / band) }
	for _, ps := range a.poolList {
		out := map[*jobCtx]bool{}
		for ps.free > 0 && !ps.blocked {
			var best *jobCtx
			var bk, bf float64
			for _, jc := range js {
				if out[jc] || !jc.canRelease() || !allows(jc, ps.p.Name) {
					continue
				}
				k, f := key(jc), a.fairKey(jc.proj, ps)
				switch {
				case best == nil, k < bk, k == bk && f < bf,
					k == bk && f == bf && jc.j.Submitted.Before(best.j.Submitted),
					k == bk && f == bf && jc.j.Submitted.Equal(best.j.Submitted) && jc.j.ID < best.j.ID:
					best, bk, bf = jc, k, f
				}
			}
			if best == nil {
				break
			}
			if !a.release(best, TierFair, false, ps) {
				out[best] = true
			}
		}
	}
}

// planSlack is a job's deadline less its planned finish, in seconds.
func (a *alloc) planSlack(jc *jobCtx) float64 {
	switch {
	case jc.hasDeadline:
		if math.IsInf(jc.finish90, 1) {
			return -bigSeconds
		}
		return jc.T - jc.finish90
	case jc.hasRate:
		return jc.slack.Seconds()
	}
	return bigSeconds
}

func (a *alloc) fairShareFirst() {
	byProject := map[*projState][]*jobCtx{}
	for _, jc := range a.jobs {
		if jc.canRelease() {
			byProject[jc.proj] = append(byProject[jc.proj], jc)
		}
	}
	for _, js := range byProject {
		sort.SliceStable(js, func(i, j int) bool {
			if js[i].slack != js[j].slack {
				return js[i].slack < js[j].slack
			}
			return js[i].j.Submitted.Before(js[j].j.Submitted)
		})
	}
	for _, ps := range a.poolList {
		out := map[*projState]bool{}
		for ps.free > 0 && !ps.blocked {
			var best *projState
			bestKey := math.Inf(1)
			for pr, js := range byProject {
				if out[pr] || !anyAllows(js, ps.p.Name) {
					continue
				}
				k := a.fairKey(pr, ps)
				if k < bestKey || (k == bestKey && best != nil && pr.p.Name < best.p.Name) {
					best, bestKey = pr, k
				}
			}
			if best == nil {
				break
			}
			released := false
			for _, jc := range byProject[best] {
				if a.release(jc, TierFair, false, ps) {
					released = true
					break
				}
			}
			if !released {
				out[best] = true
			}
		}
	}
}

func anyAllows(js []*jobCtx, pool string) bool {
	for _, jc := range js {
		if !jc.canRelease() {
			continue
		}
		for _, p := range jc.j.Pools {
			if p == pool {
				return true
			}
		}
	}
	return false
}

// rank orders jobs the way the tiers serve them: feasible jobs with a
// need by deadline ratio, then infeasible ones by remaining work, then the
// rest by slack.
func (a *alloc) rank() {
	js := append([]*jobCtx(nil), a.jobs...)
	for _, jc := range js {
		switch {
		case jc.waiting(), jc.j.Hold, jc.j.HoldStop:
			// Nothing it could run now (its upstream jobs or its own
			// dependencies are unfinished, or it is on hold): ranked
			// after every job that can use slots, though its R may be
			// above 0.
			jc.group = 3
		case jc.R > 0 && jc.planOK:
			jc.group = 0
		case jc.R > 0:
			jc.group = 1
		default:
			jc.group = 2
		}
	}
	sort.SliceStable(js, func(i, j int) bool {
		x, y := js[i], js[j]
		if x.group != y.group {
			return x.group < y.group
		}
		switch x.group {
		case 0:
			if rx, ry := x.ratio(), y.ratio(); rx != ry {
				return rx < ry
			}
		case 1:
			if x.W != y.W {
				return x.W < y.W
			}
		}
		if x.slack != y.slack {
			return x.slack < y.slack
		}
		return x.j.ID < y.j.ID
	})
	for i, jc := range js {
		st := a.out.Jobs[jc.j.ID]
		st.Rank = i + 1
		a.out.Jobs[jc.j.ID] = st
	}
}

// waiting reports whether a job can use no slot now: nothing runnable and
// nothing running, but units still to come. The queue passes jobs whose
// upstream jobs have not finished this way (Blocked set, Runnable empty).
// Generators not yet evaluated do not make a job ready: one that could
// run would be in Runnable.
func (jc *jobCtx) waiting() bool {
	j := jc.j
	return j.RunnableTotal == 0 && len(j.Runnable) == 0 && len(j.RunningUnits) == 0 && (j.Blocked > 0 || j.Unevaluated > 0)
}
