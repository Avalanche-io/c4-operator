package main

import (
	"sort"

	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Baselines with sched.Policy's signature that sched does not have. Both
// use the same release mechanics as sched's FIFO and EDF: units in the
// job's dispatch order into the cheapest allowed pool with room, a gang
// only when one pool has room for all of it, and a gang that fits nowhere
// holds the largest pool it fits in. No quotas, estimates, preemption
// (other than hold-stop) or burst.

// StaticPriority is a render manager's default queue, as in Deadline 10:
// a static priority set at submission (show and stage), then submission
// order. No wrangler intervenes.
func StaticPriority(s sched.Snapshot, _ *sched.State, _ sched.Config) sched.Decisions {
	js := order(s, func(a, b *sched.Job) bool {
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		return a.Submitted.Before(b.Submitted)
	})
	return greedy(s, js)
}

// FairShare hands out one unit at a time to the project with the fewest
// slots in use over its weight, and within a project to its oldest job.
func FairShare(s sched.Snapshot, _ *sched.State, _ sched.Config) sched.Decisions {
	g := newGreedy(s)
	use := map[string]float64{}
	weight := map[string]float64{}
	for _, p := range s.Projects {
		weight[p.Name] = max(p.Weight, 1e-9)
		if p.Weight <= 0 {
			weight[p.Name] = 1
		}
	}
	byProj := map[string][]*gjob{}
	var names []string
	for _, j := range g.jobs {
		for _, n := range j.j.Running {
			use[j.j.Project] += float64(n)
		}
		if byProj[j.j.Project] == nil {
			names = append(names, j.j.Project)
		}
		byProj[j.j.Project] = append(byProj[j.j.Project], j)
	}
	sort.Strings(names)
	for _, n := range names {
		js := byProj[n]
		sort.SliceStable(js, func(a, b int) bool { return js[a].j.Submitted.Before(js[b].j.Submitted) })
	}
	out := map[string]bool{}
	for {
		best := ""
		for _, n := range names {
			if out[n] {
				continue
			}
			w := weight[n]
			if w == 0 {
				w = 1
			}
			if best == "" || use[n]/w < use[best]/weight1(weight, best) {
				best = n
			}
		}
		if best == "" {
			break
		}
		// A job that cannot release now will not later in this
		// decision (free slots only shrink), so it leaves the list.
		released := false
		js := byProj[best]
		for len(js) > 0 {
			if k := g.release(js[0]); k > 0 {
				use[best] += float64(k)
				released = true
				break
			}
			js = js[1:]
		}
		byProj[best] = js
		if !released {
			out[best] = true
		}
	}
	return g.finish(nil)
}

func weight1(w map[string]float64, n string) float64 {
	if w[n] == 0 {
		return 1
	}
	return w[n]
}

func order(s sched.Snapshot, less func(a, b *sched.Job) bool) []*sched.Job {
	js := make([]*sched.Job, len(s.Jobs))
	for i := range s.Jobs {
		js[i] = &s.Jobs[i]
	}
	sort.SliceStable(js, func(a, b int) bool {
		if less(js[a], js[b]) != less(js[b], js[a]) {
			return less(js[a], js[b])
		}
		return js[a].ID < js[b].ID
	})
	return js
}

type gjob struct {
	j    *sched.Job
	next int
}

type greedyState struct {
	s       sched.Snapshot
	free    map[string]int
	slots   map[string]int
	cost    map[string]float64
	blocked map[string]bool
	jobs    []*gjob
	out     sched.Decisions
}

func newGreedy(s sched.Snapshot) *greedyState {
	g := &greedyState{s: s, free: map[string]int{}, slots: map[string]int{}, cost: map[string]float64{}, blocked: map[string]bool{}}
	g.out.Jobs = map[string]sched.JobState{}
	for _, p := range s.Pools {
		g.free[p.Name] = max(0, p.Slots-p.Used)
		g.slots[p.Name] = p.Slots
		g.cost[p.Name] = p.CostPerSlotHour
	}
	for i := range s.Jobs {
		j := &s.Jobs[i]
		g.jobs = append(g.jobs, &gjob{j: j})
		if !j.HoldStop {
			continue
		}
		for _, ru := range j.RunningUnits {
			g.out.Preempt = append(g.out.Preempt, sched.Preempt{Job: j.ID, Unit: ru.ID, Pool: ru.Pool, Slots: ru.Slots, Reason: sched.ReasonHoldStop})
		}
	}
	return g
}

// release dispatches the job's next unit and returns its slots, or 0.
// A gang that fits nowhere blocks the largest pool it fits in.
func (g *greedyState) release(gj *gjob) int {
	j := gj.j
	if j.Hold || j.HoldStop || gj.next >= len(j.Runnable) {
		return 0
	}
	u := j.Runnable[gj.next]
	k := unitSlots(j, u)
	best := ""
	for _, p := range j.Pools {
		if _, ok := g.free[p]; !ok || g.blocked[p] || g.free[p] < k {
			continue
		}
		if best == "" || g.cost[p] < g.cost[best] {
			best = p
		}
	}
	if best == "" {
		if k > 1 {
			target := ""
			for _, p := range j.Pools {
				if g.slots[p] >= k && (target == "" || g.slots[p] > g.slots[target]) {
					target = p
				}
			}
			if target != "" {
				g.blocked[target] = true
			}
		}
		return 0
	}
	g.free[best] -= k
	gj.next++
	g.out.Dispatch = append(g.out.Dispatch, sched.Dispatch{Job: j.ID, Unit: u.ID, Pool: best, Slots: k, Tier: sched.TierBaseline})
	return k
}

func greedy(s sched.Snapshot, js []*sched.Job) sched.Decisions {
	g := newGreedy(s)
	by := map[*sched.Job]*gjob{}
	for _, gj := range g.jobs {
		by[gj.j] = gj
	}
	var ord []*gjob
	for _, j := range js {
		gj := by[j]
		ord = append(ord, gj)
		for g.release(gj) > 0 {
		}
	}
	return g.finish(ord)
}

func (g *greedyState) finish(ord []*gjob) sched.Decisions {
	if ord == nil {
		ord = g.jobs
	}
	for i, gj := range ord {
		g.out.Jobs[gj.j.ID] = sched.JobState{Rank: i + 1}
	}
	return g.out
}

var policies = []policy{
	{name: "objective+burst", fn: sched.Decide, burst: true, usesEs: true, checked: true},
	{name: "objective", fn: sched.Decide, usesEs: true, checked: true},
	{name: "edf", fn: sched.EDF},
	{name: "fifo", fn: sched.FIFO},
	{name: "fair-share", fn: FairShare},
	{name: "static-priority", fn: StaticPriority},
}

func policyByName(n string) (policy, bool) {
	for _, p := range policies {
		if p.name == n {
			return p, true
		}
	}
	return policy{}, false
}
