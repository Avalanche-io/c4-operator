package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Decisions at safe points. Arriving at a safe point is a decision point,
// not an automatic stop. The unit asks (c4-unit safepoint), the node daemon
// sets the pod's c4.avalanche.io/safepoint annotation, and the queue
// answers in c4.avalanche.io/safepoint-decision, which the daemon passes
// back to the waiting unit:
//
//   - A soft stop asked earlier is advisory. If its reason has passed
//     (capacity returned, the job it was for is on time again or no longer
//     waits, the hold was released: sched.State.Reconsider), or the unit
//     is nearly done, the answer is continue and the stop is cancelled.
//     Otherwise it is stop, and the unit yields there.
//   - With no stop pending, a unit whose class allows it (class.Mover) may
//     be moved: if a pool the job may use is clearly cheaper, has room now,
//     and the unit has a good while left, the answer is stop, and its next
//     attempt runs there.
//   - Otherwise it is continue.
//
// Each stop, each continue that cancels a stop, and each move is signed in
// the book (decision/v1, kinds safepoint-stop, safepoint-continue and
// safepoint-move). A unit that gets no answer in time falls back on the
// last request it can see: it stops only if a stop is still pending.
//
// Between safe points, c4.avalanche.io/safepoint-next says what the queue
// would decide at the next one as things stand: stop, continue, or
// undecided (a stop is asked but the scheduler no longer holds it, as after
// a restart, until the unit gets there).

const (
	// yieldMove is the yield reason of a unit moved at a safe point, and
	// yieldDrain of one whose pod was being deleted.
	yieldMove  = "move"
	yieldDrain = "disruption"
	// yieldWait is the yield reason of a unit that stopped for a wait it
	// opened (c4-unit wait).
	yieldWait = "wait"
	// nearlyDone is the progress past which a unit asked to stop for
	// another job continues: stopping it would lose more than it frees.
	nearlyDone = 0.9
	// moveCheaper is how much cheaper a pool must be for a move: its cost
	// per slot-hour at most this fraction of the unit's pool's.
	moveCheaper = 0.75
	// moveMinLeft is the least time a unit must have left to be moved.
	moveMinLeft = 20 * time.Minute
)

// spNote and spDecision are the safepoint and safepoint-decision
// annotations' values (node.SafepointNote and node.SafepointDecision).
type spNote struct {
	Seq        int             `json:"seq"`
	At         time.Time       `json:"at"`
	Checkpoint string          `json:"checkpoint,omitempty"`
	Wait       json.RawMessage `json:"wait,omitempty"` // c4-unit wait
}

type spDecision struct {
	Seq int       `json:"seq"`
	Do  string    `json:"do"`
	Why string    `json:"why,omitempty"`
	At  time.Time `json:"at"`
}

func podSafepoint(p *corev1.Pod) (spNote, bool) {
	var n spNote
	if p == nil || json.Unmarshal([]byte(p.Annotations[c4v1.AnnotationSafepoint]), &n) != nil || n.Seq <= 0 {
		return spNote{}, false
	}
	return n, true
}

func podDecision(p *corev1.Pod) (spDecision, bool) {
	var d spDecision
	if p == nil || json.Unmarshal([]byte(p.Annotations[c4v1.AnnotationSafepointDecision]), &d) != nil || d.Seq <= 0 {
		return spDecision{}, false
	}
	return d, true
}

// spChoice is what the queue decides at a unit's safe point.
type spChoice struct {
	do, why string // stop or continue, and why
	next    string // stop, continue or undecided (AnnotationSafepointNext)
	// asked: a soft stop is pending; cancel: it no longer holds.
	asked, cancel bool
	reason        sched.PreemptReason
	forJob        string
	move          string // the pool the unit moves to
	drain         bool   // the pod is being deleted
	// wait is the wait the unit asked for and stops for.
	wait *class.Wait
}

// safepointChoice decides for one running unit as things stand. free is
// each pool's free slots after this tick's dispatches, cost its cost per
// slot-hour.
func (q *Queue) safepointChoice(v *view, idx int, pod *corev1.Pod, ri runInfo, dec sched.Decisions, free map[string]int, cost map[string]float64, now time.Time) spChoice {
	unit := unitID(v.key, idx)
	if pod.DeletionTimestamp != nil {
		// SIGTERM is a yield: the pod is being drained, evicted or
		// deleted, and its grace runs. It stops at this safe point, and
		// the next attempt resumes from what it saved.
		return spChoice{do: "stop", why: drainWhy(pod), next: "stop", drain: true}
	}
	switch y := pod.Annotations[c4v1.AnnotationYield]; {
	case y == yieldMove:
		return spChoice{do: "stop", why: "moving to another pool", next: "stop"}
	case y != "":
		c := spChoice{asked: true}
		c.reason, c.forJob, _ = q.st.Pending(unit)
		pending, stop, why := q.st.Reconsider(q.lastSnap, dec, unit)
		switch {
		case v.job.Spec.Nudge.HoldStop:
			stop, why = true, "the job is on hold-stop"
		case !pending:
			c.do, c.why, c.next, c.cancel = "continue", "the stop is no longer pending", "undecided", true
			return c
		}
		if stop && (c.reason == sched.ReasonReclaim || c.reason == sched.ReasonFeasibility || c.reason == sched.ReasonGuard) {
			if f, ok := q.Progress.Get(pod.Namespace, pod.Name, now); ok && f >= nearlyDone {
				stop, why = false, fmt.Sprintf("nearly done (%d%%): stopping now would lose more than it frees", int(f*100))
			}
		}
		if stop {
			c.do, c.why, c.next = "stop", why, "stop"
		} else {
			c.do, c.why, c.next, c.cancel = "continue", why, "continue", true
		}
		return c
	}
	if to, why := q.moveTarget(v, pod, ri, dec, free, cost, now); to != "" {
		return spChoice{do: "stop", why: why, next: "stop", move: to}
	}
	return spChoice{do: "continue", why: "no stop is pending", next: "continue"}
}

// moveTarget is the pool a unit may move to at its safe point, and why;
// "" when it stays.
func (q *Queue) moveTarget(v *view, pod *corev1.Pod, ri runInfo, dec sched.Decisions, free map[string]int, cost map[string]float64, now time.Time) (string, string) {
	if m, ok := v.driver.(class.Mover); !ok || !m.MovesAtSafepoints() || dec.Jobs[v.key].AtRisk || v.job.Spec.Nudge.Hold {
		return "", ""
	}
	f, ok := q.Progress.Get(pod.Namespace, pod.Name, now)
	if !ok || f <= 0 || f >= 1 || ri.started.IsZero() {
		return "", ""
	}
	left := time.Duration(float64(now.Sub(ri.started)) * (1 - f) / f)
	here := cost[ri.pool]
	if left < moveMinLeft || here <= 0 {
		return "", ""
	}
	best := ""
	for _, p := range v.pools {
		if p == ri.pool || free[p] < ri.slots || cost[p] > here*moveCheaper {
			continue
		}
		if best == "" || cost[p] < cost[best] {
			best = p
		}
	}
	if best == "" {
		return "", ""
	}
	return best, fmt.Sprintf("moving to %s: $%.2f a slot-hour instead of $%.2f, about %s left",
		best, cost[best], here, left.Round(time.Minute))
}

// decideSafepoints answers every running unit at a new safe point, and
// keeps each unit's safepoint-next current. It runs after act, so a stop
// asked this tick is seen.
func (q *Queue) decideSafepoints(ctx context.Context, now time.Time, dec sched.Decisions, views []*view) {
	free, cost := map[string]int{}, map[string]float64{}
	for _, p := range q.lastSnap.Pools {
		free[p.Name], cost[p.Name] = p.Slots-p.Used, p.CostPerSlotHour
	}
	for _, d := range dec.Dispatch {
		free[d.Pool] -= d.Slots
	}
	for _, v := range views {
		for idx, pod := range v.pods {
			ri, running := v.running[idx]
			if !running {
				continue
			}
			n, at := podSafepoint(pod)
			if _, known := v.safe[idx]; !at && !known {
				continue // it announces no safe points
			}
			last, _ := podDecision(pod)
			if at && n.Seq > last.Seq && len(n.Wait) > 0 {
				q.answerSafepoint(ctx, v, idx, pod, n, waitChoice(v, idx, n), free, now)
				continue
			}
			c := q.safepointChoice(v, idx, pod, ri, dec, free, cost, now)
			if !at || n.Seq <= last.Seq {
				if pod.Annotations[c4v1.AnnotationSafepointNext] != c.next {
					patch := client.MergeFrom(pod.DeepCopy())
					pod.Annotations[c4v1.AnnotationSafepointNext] = c.next
					q.Client.Patch(ctx, pod, patch)
				}
				continue
			}
			q.answerSafepoint(ctx, v, idx, pod, n, c, free, now)
		}
	}
}

// waitChoice answers a safe point that asks for a wait (c4-unit wait):
// stop, the wait opening for the unit, or continue when the wait is
// refused, saying why.
func waitChoice(v *view, idx int, n spNote) spChoice {
	name := v.mem.plan.Units[idx].Name
	if name == "" {
		name = strconv.Itoa(idx)
	}
	w, err := class.AskedWait(name, n.Wait)
	if err != nil {
		return spChoice{do: "continue", why: err.Error(), next: "continue"}
	}
	return spChoice{do: "stop", why: "waits for " + w.Server, next: "stop", wait: w}
}

// answerSafepoint writes the decision at safe point n and acts on it.
func (q *Queue) answerSafepoint(ctx context.Context, v *view, idx int, pod *corev1.Pod, n spNote, c spChoice, free map[string]int, now time.Time) {
	data, _ := json.Marshal(spDecision{Seq: n.Seq, Do: c.do, Why: c.why, At: now.UTC()})
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[c4v1.AnnotationSafepointDecision] = string(data)
	pod.Annotations[c4v1.AnnotationSafepointNext] = c.next
	if c.cancel {
		delete(pod.Annotations, c4v1.AnnotationYield)
		delete(pod.Annotations, c4v1.AnnotationYieldBy)
	}
	if c.move != "" {
		pod.Annotations[c4v1.AnnotationYield] = yieldMove
		pod.Annotations[c4v1.AnnotationYieldBy] = now.Add(yieldGrace).UTC().Format(time.RFC3339)
	}
	if c.wait != nil {
		// Its exit is a yield: it resumes once the wait is decided.
		pod.Annotations[c4v1.AnnotationYield] = yieldWait
		pod.Annotations[c4v1.AnnotationYieldBy] = now.Add(yieldGrace).UTC().Format(time.RFC3339)
	}
	if c.drain && pod.Annotations[c4v1.AnnotationYield] == "" {
		// So its exit is a yield, not the unit's end.
		pod.Annotations[c4v1.AnnotationYield] = yieldDrain
	}
	if err := q.Client.Patch(ctx, pod, patch); err != nil {
		q.Log.Error(err, "safe point decision", "job", v.key, "unit", idx)
		return
	}
	extra := map[string]any{"safepoint": n.Seq, "why": c.why}
	if n.Checkpoint != "" {
		extra["checkpoint"] = n.Checkpoint
	}
	switch {
	case c.wait != nil:
		v.mem.yieldAsked[idx] = now
		v.mem.waitRecs().recs[idx] = &waitRec{since: now, server: c.wait.Server, block: true, seq: n.Seq,
			ask: c.wait.Ask, choices: c.wait.Choices, reply: c.wait.Reply, expect: c.wait.Expect}
		extra["server"] = c.wait.Server
		if c.wait.Ask != "" {
			extra["ask"] = c.wait.Ask
		}
		q.recordDecision(ctx, v, idx, "safepoint-wait", c.why, "", now, extra)
	case c.move != "":
		v.mem.yieldAsked[idx] = now
		if v.mem.moveTo == nil {
			v.mem.moveTo = map[int]string{}
		}
		v.mem.moveTo[idx] = c.move
		free[c.move] -= v.running[idx].slots
		extra["to"] = c.move
		q.recordDecision(ctx, v, idx, "safepoint-move", c.why, "", now, extra)
	case c.cancel:
		delete(v.mem.yieldAsked, idx)
		q.st.CancelPreempt(unitID(v.key, idx))
		extra["asked"] = string(c.reason)
		q.recordDecision(ctx, v, idx, "safepoint-continue", c.why, c.forJob, now, extra)
	case c.drain:
		extra["asked"] = yieldDrain
		q.recordDecision(ctx, v, idx, "safepoint-stop", c.why, "", now, extra)
	case c.asked:
		extra["asked"] = string(c.reason)
		q.recordDecision(ctx, v, idx, "safepoint-stop", c.why, c.forJob, now, extra)
	}
	q.Log.Info("safe point", "job", v.key, "unit", idx, "seq", n.Seq, "do", c.do, "why", c.why)
}

// SafepointNextOf is what the queue would decide at a unit pod's next safe
// point (stop, continue or undecided), and its last decision at one, for
// the HUD: {seq, do, why, at}, nil before the first.
func SafepointNextOf(p *corev1.Pod) (string, map[string]any) {
	next := p.Annotations[c4v1.AnnotationSafepointNext]
	if next == "" {
		next = "undecided"
	}
	d, ok := podDecision(p)
	if !ok {
		return next, nil
	}
	return next, map[string]any{"seq": d.Seq, "do": d.Do, "why": d.Why, "at": d.At}
}

// PodSafepointOf is the last safe point the node daemon set on a unit pod.
func PodSafepointOf(p *corev1.Pod) (Safepoint, bool) {
	n, ok := podSafepoint(p)
	return Safepoint{Seq: n.Seq, At: n.At}, ok
}

// drainWhy says why a deleting pod stops.
func drainWhy(p *corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue && c.Reason != "" {
			return "the farm is stopping this pod (" + c.Reason + ")"
		}
	}
	return "the pod is being deleted"
}

// Grace (INTEGRATE 3.1: SIGTERM is a yield). Every voluntary disruption
// sends SIGTERM and waits the pod's grace period, so a unit's grace is the
// time it needs to reach a safe point and save: the job's learned cadence
// between safe points, with half again and the decision wait added; else
// the job's checkpointSeconds; else its class's checkpoint time. Bounded
// so a drain cannot stall on it.
const (
	minGrace = 30 * time.Second
	maxGrace = 15 * time.Minute
)

func (q *Queue) graceFor(v *view) time.Duration {
	var g time.Duration
	var names []string
	for _, p := range v.pods {
		names = append(names, p.Name)
	}
	if every := q.Safepoints.Cadence(v.job.Namespace, names); every > 0 {
		g = every*3/2 + 30*time.Second
	} else if s, err := strconv.Atoi(v.job.Spec.Params["checkpointSeconds"]); err == nil && s > 0 {
		g = time.Duration(s) * time.Second
	} else if c, ok := v.driver.(class.Checkpointer); ok {
		g = c.CheckpointTime(v.job)
	}
	if g <= 0 {
		return 0
	}
	return min(max(g, minGrace), maxGrace)
}

// AnnotationDoNotDisrupt is Karpenter's pod annotation: a Go duration
// protects the pod's node from consolidation and drift for that long
// after the pod starts (not from expiry, spot interruption or deletion).
const AnnotationDoNotDisrupt = "karpenter.sh/do-not-disrupt"

// doNotDisruptAfter is the shortest predicted run worth protecting.
const doNotDisruptAfter = 15 * time.Minute

// doNotDisrupt is how long a unit's node should not be disrupted for: the
// unit's predicted run plus two standard deviations, rounded up to the
// minute, when the unit cannot stop at a safe point (its class does not
// checkpoint and its job has shown none) and would lose at least
// doNotDisruptAfter of work; 0 otherwise.
func (q *Queue) doNotDisrupt(v *view, idx int) time.Duration {
	if v.driver.Checkpoints() || v.est == nil {
		return 0
	}
	var names []string
	for _, p := range v.pods {
		names = append(names, p.Name)
	}
	if len(v.safe) > 0 || q.Safepoints.Cadence(v.job.Namespace, names) > 0 {
		return 0
	}
	u := v.mem.plan.Units[idx]
	pr := v.est.Predict(v.estKey(idx), u.Pos)
	run := time.Duration((pr.Mean + 2*pr.SD()) * float64(time.Second))
	if time.Duration(pr.Mean*float64(time.Second)) < doNotDisruptAfter {
		return 0
	}
	return min(run.Truncate(time.Minute)+time.Minute, 24*time.Hour)
}
