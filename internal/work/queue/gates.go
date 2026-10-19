package queue

import (
	"math"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Gates (PRIMITIVES Q9; LLM-OR-PROCEDURE 7.2 item 4). A gate is a
// resource of one slot shared by every job in a namespace: a project's
// merge gate, its signing step, its one DI suite. A unit of kind gate
// names it (class.Unit.Gate). Units ready to pass a gate queue first come,
// first served, by when they became ready: while the gate is held, or
// while another is ahead, a unit is locked, with its place in the line
// and when it is expected to start. Only the head of the line may start
// when the gate is free, and only one unit passes per tick. The line is
// in each job's status (gates) and, for the farm, in the HUD's snapshot.

// gateWaiter is one unit wanting a gate.
type gateWaiter struct {
	v     *view
	idx   int
	since time.Time
}

// gateKey is a gate's farm-wide name.
func gateKey(ns, gate string) string { return ns + "/" + gate }

// applyGates sorts every gated unit into the holder, the head of the line
// and the units locked behind them, across all jobs.
func (q *Queue) applyGates(views []*view, now time.Time) {
	holders := map[string]gateWaiter{}
	lines := map[string][]gateWaiter{}
	for _, v := range views {
		v.gates = nil
		m, plan := v.mem, v.mem.plan
		if plan == nil {
			continue
		}
		for _, idx := range holding(v) {
			if g := plan.Units[idx].Gate; g != "" {
				holders[gateKey(v.job.Namespace, g)] = gateWaiter{v: v, idx: idx, since: m.gateSince[idx]}
				delete(m.gateSince, idx)
			}
		}
		ready := map[int]bool{}
		for _, idx := range v.runnable {
			g := plan.Units[idx].Gate
			if g == "" {
				continue
			}
			ready[idx] = true
			if m.gateSince == nil {
				m.gateSince = map[int]time.Time{}
			}
			if _, ok := m.gateSince[idx]; !ok {
				m.gateSince[idx] = now
			}
			k := gateKey(v.job.Namespace, g)
			lines[k] = append(lines[k], gateWaiter{v: v, idx: idx, since: m.gateSince[idx]})
		}
		for idx := range m.gateSince {
			if !ready[idx] {
				delete(m.gateSince, idx) // no longer waiting (requeued, failed, gone)
			}
		}
	}
	for k, line := range lines {
		sort.Slice(line, func(a, b int) bool {
			x, y := line[a], line[b]
			if !x.since.Equal(y.since) {
				return x.since.Before(y.since)
			}
			if x.v.key != y.v.key {
				return x.v.key < y.v.key
			}
			return x.idx < y.idx
		})
		h, held := holders[k]
		// When each waiter is expected to pass: after the holder's
		// remaining work and the work of those ahead.
		at := now
		holder := ""
		if held {
			holder = h.v.key + "#" + unitName(h.v.mem.plan, h.idx)
			at = now.Add(remaining(h.v, h.idx, now))
			h.v.gates = append(h.v.gates, c4v1.WorkGateEntry{Gate: gateName(k), Unit: int32(h.idx), Name: unitName(h.v.mem.plan, h.idx), Position: 0})
		}
		for i, w := range line {
			pos := i + 1
			e := c4v1.WorkGateEntry{Gate: gateName(k), Unit: int32(w.idx), Name: unitName(w.v.mem.plan, w.idx), Position: int32(pos),
				Since: &metav1.Time{Time: w.since.Truncate(time.Second)}, Holder: holder}
			if held || i > 0 {
				e.ExpectStart = &metav1.Time{Time: at.Truncate(time.Second)}
				lockGate(w.v, w.idx, k, holder, pos)
			}
			w.v.gates = append(w.v.gates, e)
			at = at.Add(predicted(w.v, w.idx))
		}
	}
}

func gateName(k string) string {
	for i := range k {
		if k[i] == '/' {
			return k[i+1:]
		}
	}
	return k
}

// lockGate takes a unit waiting for a gate out of the runnable list.
func lockGate(v *view, idx int, gate, holder string, pos int) {
	for i, r := range v.runnable {
		if r == idx {
			v.runnable = append(v.runnable[:i:i], v.runnable[i+1:]...)
			break
		}
	}
	if v.locked == nil {
		v.locked = map[int]lockInfo{}
	}
	v.locked[idx] = lockInfo{by: -1, gate: gate, holder: holder, position: pos}
}

// predicted is a unit's predicted duration, and remaining what is left of
// a running one's.
func predicted(v *view, idx int) time.Duration {
	if v.est == nil {
		return 0
	}
	u := v.mem.plan.Units[idx]
	return time.Duration(v.est.Predict(u.Key, u.Pos).Mean * float64(time.Second))
}

func remaining(v *view, idx int, now time.Time) time.Duration {
	d := predicted(v, idx)
	if r, ok := v.running[idx]; ok && !r.started.IsZero() {
		d -= now.Sub(r.started)
	}
	return time.Duration(math.Max(0, float64(d)))
}

// admitGate lets one unit through each gate per tick.
func (g claimGuard) admitGate(v *view, idx int) bool {
	gate := v.mem.plan.Units[idx].Gate
	if gate == "" {
		return true
	}
	k := "gate:" + gateKey(v.job.Namespace, gate)
	if _, taken := g[k]; taken {
		return false
	}
	g[k] = nil
	return true
}
