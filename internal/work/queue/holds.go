package queue

import (
	"math"
	"sort"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// Holds (PRIMITIVES Q8, Q9). A unit whose claims (class.Claim: its holds
// and claims) conflict with a unit of its job that is running, or whose
// output is still being captured, is locked: it waits, "locked by" the
// holder, whatever its other readiness. Units released in one tick are
// checked against each other in the policy's order, so two conflicting
// units never start together. Each holder's waiting line, in dispatch
// order, is in status (holds), and units that must take turns form a
// chain on the critical path.

// holdsKept bounds status.holds.
const holdsKept = 32

// lockInfo says what holds a locked unit up.
type lockInfo struct {
	by    int
	on    string // the holder's claim
	claim string // the locked unit's
}

// holding are the units of a job that hold their claims now.
func holding(v *view) []int {
	var out []int
	for idx := range v.running {
		out = append(out, idx)
	}
	for idx := range v.finishing {
		if _, ok := v.running[idx]; !ok {
			out = append(out, idx)
		}
	}
	sort.Ints(out)
	return out
}

// applyHolds takes the units locked by a holder out of the runnable list.
func applyHolds(v *view) {
	plan := v.mem.plan
	held := holding(v)
	v.locked = nil
	var keep []int
	for _, idx := range v.runnable {
		u := plan.Units[idx]
		if len(u.Claims) == 0 {
			keep = append(keep, idx)
			continue
		}
		locked := false
		for _, h := range held {
			if mine, theirs, ok := class.ConflictsWith(u.Claims, plan.Units[h].Claims); ok {
				if v.locked == nil {
					v.locked = map[int]lockInfo{}
				}
				v.locked[idx] = lockInfo{by: h, on: theirs.On, claim: mine.On}
				locked = true
				break
			}
		}
		if !locked {
			keep = append(keep, idx)
		}
	}
	v.runnable = keep
}

// claimGuard checks releases within one tick: a unit may start only if
// its claims conflict with none of the units started before it.
type claimGuard map[string][]class.Claim // by job

func (g claimGuard) admit(v *view, idx int) bool {
	cs := v.mem.plan.Units[idx].Claims
	if len(cs) == 0 {
		return true
	}
	if _, _, ok := class.ConflictsWith(cs, g[v.key]); ok {
		return false
	}
	g[v.key] = append(g[v.key], cs...)
	return true
}

// holdsStatus is status.holds: each holder with its claims and the units
// it holds up, in dispatch order.
func holdsStatus(v *view) []c4v1.WorkHold {
	if len(v.locked) == 0 {
		return nil
	}
	plan := v.mem.plan
	pos := map[int]int{}
	for i, idx := range plan.Order {
		pos[idx] = i
	}
	byHolder := map[int][]int{}
	for idx, l := range v.locked {
		byHolder[l.by] = append(byHolder[l.by], idx)
	}
	var holders []int
	for h := range byHolder {
		holders = append(holders, h)
	}
	sort.Ints(holders)
	var out []c4v1.WorkHold
	for _, h := range holders {
		w := byHolder[h]
		sort.Slice(w, func(a, b int) bool { return pos[w[a]] < pos[w[b]] })
		hd := c4v1.WorkHold{Holder: int32(h), Name: unitName(plan, h)}
		for _, c := range plan.Units[h].Claims {
			hd.On = append(hd.On, c.On)
		}
		for _, idx := range w {
			hd.Waiting = append(hd.Waiting, c4v1.WorkHoldWaiter{Unit: int32(idx), Name: unitName(plan, idx), On: v.locked[idx].claim})
		}
		out = append(out, hd)
		if len(out) == holdsKept {
			break
		}
	}
	return out
}

func unitName(plan *class.Plan, i int) string {
	if n := plan.Units[i].Name; n != "" {
		return n
	}
	return plan.Units[i].Label
}

// holdChains is the longest run of remaining work that must take turns:
// unfinished units whose exclusive claims conflict, joined into groups,
// each group's remaining work summed.
func holdChains(plan *class.Plan, rem map[int]float64) float64 {
	var idx []int
	for i := range rem {
		if len(plan.Units[i].Claims) > 0 {
			idx = append(idx, i)
		}
	}
	sort.Ints(idx)
	parent := map[int]int{}
	var find func(int) int
	find = func(i int) int {
		if p, ok := parent[i]; ok && p != i {
			parent[i] = find(p)
			return parent[i]
		}
		parent[i] = i
		return i
	}
	for a := 0; a < len(idx); a++ {
		for b := a + 1; b < len(idx); b++ {
			if _, _, ok := class.ConflictsWith(plan.Units[idx[a]].Claims, plan.Units[idx[b]].Claims); ok {
				parent[find(idx[a])] = find(idx[b])
			}
		}
	}
	sum := map[int]float64{}
	longest := 0.0
	for _, i := range idx {
		r := find(i)
		sum[r] += rem[i]
		longest = math.Max(longest, sum[r])
	}
	return longest
}

func hasLock(v *view, i int) bool {
	_, ok := v.locked[i]
	return ok
}
