package sched

import (
	"math"
	"sort"
)

// Planner selects how required slots R and planned shares are computed.
type Planner string

const (
	// PlanFluid (default) plans each pool group together: earliest
	// deadline first over the group's capacity, see planFluid.
	PlanFluid Planner = "fluid-edf"
	// PlanPerJob is DESIGN.md section 9.9 as written: each job's R is
	// W90/T on its own, and shares are water-filled over R.
	PlanPerJob Planner = "per-job"
)

// plan computes, for every job with an objective, its required slots R
// (the deadline-need tier's target), its planned share of current
// capacity (owned pools plus burst already requested), whether it is at
// risk at that share, and how late. It also records the shortfall that a
// burst would have to cover.
func (a *alloc) plan() {
	groups, capacity := a.groups()
	for _, g := range groups {
		if a.cfg.Planner == PlanPerJob {
			a.planPerJob(g, capacity[g[0].root])
			continue
		}
		a.planFluid(g, capacity[g[0].root])
	}
	for _, jc := range a.jobs {
		jc.Rb = jc.R * clamp(jc.j.Boost, 1, 2)
	}
}

type groupMember struct {
	*jobCtx
	root string
	cap  float64 // planned attainable slots
}

// groups partitions jobs with an objective by the pools they may use:
// pools linked by any job that may use both are planned together, their
// slots treated as interchangeable (an approximation when jobs overlap
// only partly). Capacity counts burst slots already requested.
func (a *alloc) groups() ([][]groupMember, map[string]float64) {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		r := find(parent[x])
		parent[x] = r
		return r
	}
	var planned []*jobCtx
	for _, jc := range a.jobs {
		if jc.j.Hold || jc.j.HoldStop || !(jc.hasDeadline || jc.hasRate) {
			continue
		}
		first := ""
		for _, name := range jc.j.Pools {
			if a.pools[name] == nil {
				continue
			}
			if first == "" {
				first = find(name)
				continue
			}
			parent[find(name)] = first
		}
		if first != "" {
			planned = append(planned, jc)
		}
	}
	capacity := map[string]float64{}
	for _, ps := range a.poolList {
		c := float64(ps.p.Slots)
		if ps.p.Burst != nil {
			c += float64(max(0, ps.p.Burst.Requested-ps.p.Burst.Provisioned))
		}
		capacity[find(ps.p.Name)] += c
	}
	byRoot := map[string][]groupMember{}
	var roots []string
	for _, jc := range planned {
		var root string
		for _, name := range jc.j.Pools {
			if a.pools[name] != nil {
				root = find(name)
				break
			}
		}
		if byRoot[root] == nil {
			roots = append(roots, root)
		}
		byRoot[root] = append(byRoot[root], groupMember{jobCtx: jc, root: root, cap: a.attainable(jc, true)})
	}
	out := make([][]groupMember, 0, len(roots))
	for _, r := range roots {
		out = append(out, byRoot[r])
	}
	return out, capacity
}

// effective is the time by which a job's work must be done for its last
// wave of units to finish by the deadline: T - t_path when TailAware.
func (a *alloc) effective(jc *jobCtx) float64 {
	if a.cfg.TailAware {
		return jc.T - jc.tpath
	}
	return jc.T
}

// planPerJob is DESIGN.md 9.9 as written. R = W90/T per job (T less
// t_path when TailAware). Jobs feasible on their own (S >= 0 with all
// attainable capacity) are water-filled up to R, infeasible ones then get
// what is left by smallest remaining work, and surplus is spread evenly.
func (a *alloc) planPerJob(g []groupMember, C float64) {
	for _, m := range g {
		m.planOK = m.feasible
		if m.hasDeadline {
			m.R = m.W90 / math.Max(a.effective(m.jobCtx), a.cfg.MinRemaining.Seconds())
		}
	}
	var w, c []float64
	var fi []int
	for i, m := range g {
		if !m.feasible {
			continue
		}
		fi = append(fi, i)
		w = append(w, m.R)
		c = append(c, math.Min(m.R, m.cap))
	}
	for k, x := range waterfill(w, c, C) {
		g[fi[k]].share = x
		C -= x
	}
	var inf []int
	for i, m := range g {
		if !m.feasible {
			inf = append(inf, i)
		}
	}
	sort.SliceStable(inf, func(p, q int) bool { return g[inf[p]].W < g[inf[q]].W })
	for _, i := range inf {
		x := math.Min(g[i].cap, math.Max(0, C))
		g[i].share = x
		C -= x
	}
	spreadSurplus(g, C)
	for _, m := range g {
		switch {
		case m.hasDeadline && m.W90 > 0:
			finish := math.Max(div(m.W90, m.share), m.tpath)
			if finish > m.T {
				m.atRisk = true
				m.lateBy = secs(finish - m.T)
				m.horizon = m.T
				m.deficit = m.W90 - m.share*m.T
			}
			m.finish90 = math.Max(div(m.W90, m.share), m.tpath)
			m.finish50 = math.Max(div(m.W, m.share), m.tpath)
		case m.hasRate:
			m.atRisk = m.share < m.R
		}
	}
}

func spreadSurplus(g []groupMember, C float64) {
	if C <= 1e-9 {
		return
	}
	w := make([]float64, len(g))
	c := make([]float64, len(g))
	for i, m := range g {
		w[i] = 1
		c[i] = math.Max(0, m.cap-m.share)
	}
	for i, x := range waterfill(w, c, C) {
		g[i].share += x
	}
}

// planFluid plans a pool group as one earliest-deadline-first fluid
// schedule over its capacity C. This replaces DESIGN.md's per-job
// R = W90/T, which treats every job as if it had the farm's future to
// itself: a job due in three days then claims a steady share during a
// nightly crunch it could have waited out, and when the group as a whole
// is overloaded every job falls short by the same fraction, so all of
// them are late together, while no single job looks infeasible against
// the whole pool.
//
//  1. Throughput jobs take their rate first.
//  2. Each deadline job must have its work done by its effective time
//     t_i = T_i - t_path_i (TailAware), so that its last wave of units
//     still ends by the deadline. A job that cannot do that even with all
//     its attainable slots (W90_i/A_i > t_i) is late on its own.
//  3. The rest are taken in order of t_i, and Moore-Hodgson selects the
//     on-time set: add each job; whenever the cumulative W90 exceeds C*t_i,
//     drop the job with the largest W90 so far. On a single resource this
//     minimizes the number of late jobs, which is what the overload rule
//     of section 9.9 asks for.
//  4. The on-time set runs as a fluid EDF schedule at the least constant
//     rate that meets every t_i: rho = max_k(cumulative W90 to k / t_k).
//     A job's R is what that schedule gives it now: rho flows to jobs in
//     order of t_i, jobs with the same deadline sharing in proportion to
//     W90, each up to its attainable slots. Later jobs get R = 0 now; they
//     are on time because earlier ones finish first.
//  5. Late jobs get what is left of C, smallest W90 first. Surplus is then
//     spread evenly up to each job's attainable slots to form its share.
//
// A job's planned finish is its fluid EDF completion at full capacity,
// (cumulative W90 to it)/C, plus t_path when TailAware; a late job's
// lateness is measured the same way, as if it were inserted in order.
func (a *alloc) planFluid(g []groupMember, C float64) {
	var dl []groupMember
	for _, m := range g {
		if m.hasRate {
			x := math.Min(m.R, m.cap)
			m.share = x
			m.planOK = m.cap >= m.R
			m.atRisk = !m.planOK
			C -= x
			continue
		}
		dl = append(dl, m)
	}
	C = math.Max(C, 0)
	minT := a.cfg.MinRemaining.Seconds()
	eff := func(m groupMember) float64 { return a.effective(m.jobCtx) }
	sort.SliceStable(dl, func(i, j int) bool {
		ti, tj := eff(dl[i]), eff(dl[j])
		if ti != tj {
			return ti < tj
		}
		if !dl[i].j.Submitted.Equal(dl[j].j.Submitted) {
			return dl[i].j.Submitted.Before(dl[j].j.Submitted)
		}
		return dl[i].j.ID < dl[j].j.ID
	})
	inSet := map[*jobCtx]bool{}
	var late []groupMember
	var set []groupMember
	cum := 0.0
	for _, m := range dl {
		t := eff(m)
		if m.W90 <= 0 {
			inSet[m.jobCtx] = true
			set = append(set, m)
			continue
		}
		if t <= 0 || m.W90 > m.cap*t {
			late = append(late, m)
			continue
		}
		set = append(set, m)
		inSet[m.jobCtx] = true
		cum += m.W90
		if cum <= C*t+1e-6 {
			continue
		}
		big := -1
		for i, x := range set {
			if big < 0 || x.W90 > set[big].W90 {
				big = i
			}
		}
		cum -= set[big].W90
		delete(inSet, set[big].jobCtx)
		late = append(late, set[big])
		set = append(set[:big], set[big+1:]...)
	}
	// Fluid rate and allocation for the on-time set.
	rho := 0.0
	cum = 0
	for _, m := range set {
		cum += m.W90
		rho = math.Max(rho, cum/math.Max(eff(m), minT))
	}
	rho = math.Min(rho, C)
	left := rho
	for i := 0; i < len(set); {
		k := i + 1
		for k < len(set) && set[k].j.Deadline.Equal(set[i].j.Deadline) {
			k++
		}
		band := set[i:k]
		w := make([]float64, len(band))
		c := make([]float64, len(band))
		for b, m := range band {
			w[b] = math.Max(m.W90, 1)
			c[b] = m.cap
		}
		for b, x := range waterfill(w, c, left) {
			band[b].R = x
			band[b].share = x
			left -= x
		}
		i = k
	}
	used := rho - left
	// Late jobs: smallest remaining work first, from what is left.
	rest := C - used
	sort.SliceStable(late, func(i, j int) bool { return late[i].W90 < late[j].W90 })
	for _, m := range late {
		x := math.Min(m.cap, math.Max(0, rest))
		m.R = x
		m.share = x
		rest -= x
	}
	spreadSurplus(dl, math.Max(0, rest))
	// Planned finish, risk and shortfall.
	cum, cum50 := 0.0, 0.0
	for _, m := range set {
		m.planOK = true
		cum += m.W90
		cum50 += m.W
		m.finish90 = a.fluidFinish(m.jobCtx, cum, C)
		m.finish50 = a.fluidFinish(m.jobCtx, cum50, C)
	}
	planned := 0.0
	for _, x := range set {
		planned += x.W90
	}
	for _, m := range late {
		m.planOK = false
		m.atRisk = true
		t := eff(m)
		before := 0.0
		for _, x := range set {
			if eff(x) <= t {
				before += x.W90
			}
		}
		// Its own finish with all its attainable slots carries the same
		// tail as the test that made it late on its own (W90/A > T -
		// t_path under TailAware), so a job late on its own is late in
		// its finish too, never on time with LateBy 0.
		finish := math.Max(a.fluidFinish(m.jobCtx, before+m.W90, C), a.fluidFinish(m.jobCtx, m.W90, m.cap))
		m.finish50 = finish
		// A member evicted to make the on-time set fit (the largest one,
		// not always the latest) can finish by its deadline when inserted
		// alone in deadline order, yet the plan runs it only at its share:
		// it is late by at least what that share leaves, and that shortfall
		// is what a burst must cover. With no share it runs after the whole
		// on-time set. Without this floor its lateness was priced at $0 and
		// no burst for it could ever be requested without approval.
		late, deficit := finish, math.Max(before+m.W90-C*t, m.W90-m.cap*t)
		if m.share > 0 {
			late = math.Max(late, a.fluidFinish(m.jobCtx, m.W90, m.share))
		} else {
			late = math.Max(late, a.fluidFinish(m.jobCtx, planned+m.W90, C))
		}
		deficit = math.Max(deficit, m.W90-m.share*t)
		m.finish90 = late
		m.lateBy = secs(math.Max(0, late-m.T))
		m.horizon = t
		m.deficit = deficit
	}
}

func (a *alloc) fluidFinish(jc *jobCtx, cum, C float64) float64 {
	f := div(cum, C)
	if a.cfg.TailAware {
		return f + jc.tpath
	}
	return math.Max(f, jc.tpath)
}
