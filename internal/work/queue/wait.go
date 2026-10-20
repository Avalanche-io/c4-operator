package queue

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// Waits (PRIMITIVES Q6). A wait is a unit served by something other than a
// slot: a person, an outside party, or a clock. It creates no pod and holds
// no capacity. Once every unit it runs after is done it is open; a person's
// or a party's wait completes on a signed decision (AnnotationDecide,
// through the HUD or MCP), a clock's at its time. Its expected wait comes
// from the server's measured decisions in the job, else the unit's own
// figure, and counts on the critical path, never as slot-seconds: a person
// is never late in the queue's terms.
//
// A running unit may open a wait for itself at a safe point (c4-unit wait,
// openBlockWait): it stops there, its slot is freed, and it runs again,
// resuming from what it saved, once the wait is decided, with the decision
// in $C4_DECISION and a reply mounted at $C4_REPLY (podFor).
//
// A unit a review escalated (verdict.go: failed and flagged) is a wait too,
// for the person the verdict named: a decision on it accepts it, reworks
// it or fails it, so a third failed rework becomes that person's ruling,
// decided the same way.

// defaultWaitExpect is the expected wait when nothing else says: Ra's
// owner rulings took a median of 12.8 hours.
const defaultWaitExpect = 12.8 * 3600

type waitMem struct {
	recs    map[int]*waitRec
	decided string // the last decision's token
}

type waitRec struct {
	since     time.Time
	escalated bool
	server    string // for an escalation, person:<to>
	do, note  string
	actor     string
	at        time.Time
	record    string
	view      string // the decision's reply
	// block: the unit opened the wait for itself at safe point seq, asking
	// ask, with these choices, for a reply or not, expected to take
	// expect seconds.
	block   bool
	seq     int
	ask     string
	choices []string
	reply   bool
	expect  float64
}

// clock is a clock wait's time, for a wait a unit opened.
func (w *waitRec) clock() (time.Time, bool) {
	at, ok := strings.CutPrefix(w.server, "clock:")
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, at)
	return t, err == nil
}

// ReplyMount is where a unit resumed after a wait it opened finds the
// reply ($C4_REPLY).
const ReplyMount = "/etc/c4/reply"

// replyName is the Materialization a unit mounts the reply to the wait it
// opened through.
func replyName(job string, idx int) string { return fmt.Sprintf("%s-reply-u%d", job, idx) }

// blockDecision is the decision on the wait unit i opened for itself, for
// its next attempt: nil if there is none, or it failed the unit.
func (m *jobMem) blockDecision(i int) *waitRec {
	w := m.waitRecs().recs[i]
	if w == nil || !w.block || !w.done() || w.do == "withdrawn" || w.do == "fail" {
		return nil
	}
	return w
}

// applyDecision gives a unit's pod the decision on the wait it opened:
// $C4_DECISION, {safepoint, do, note, actor, at, view, reply}, and the
// reply mounted read-only at $C4_REPLY.
func applyDecision(pod *corev1.Pod, job string, idx int, w *waitRec) {
	d := map[string]any{"safepoint": w.seq, "do": w.do, "actor": w.actor, "at": w.at.UTC().Format(time.RFC3339)}
	if w.note != "" {
		d["note"] = w.note
	}
	env := []corev1.EnvVar{}
	if w.view != "" {
		d["view"], d["reply"] = w.view, ReplyMount
		env = append(env, corev1.EnvVar{Name: "C4_REPLY", Value: ReplyMount})
		ro := true
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "c4-reply", VolumeSource: corev1.VolumeSource{
			CSI: &corev1.CSIVolumeSource{Driver: c4v1.DriverName, ReadOnly: &ro,
				VolumeAttributes: map[string]string{c4v1.VolumeAttrMaterialization: replyName(job, idx)}}}})
	}
	b, _ := json.Marshal(d)
	env = append(env, corev1.EnvVar{Name: "C4_DECISION", Value: string(b)})
	for c := range pod.Spec.Containers {
		pod.Spec.Containers[c].Env = append(pod.Spec.Containers[c].Env, env...)
		if w.view != "" {
			pod.Spec.Containers[c].VolumeMounts = append(pod.Spec.Containers[c].VolumeMounts,
				corev1.VolumeMount{Name: "c4-reply", MountPath: ReplyMount, ReadOnly: true})
		}
	}
}

// blockOpen reports whether unit i waits on a wait it opened for itself.
func (m *jobMem) blockOpen(i int) bool {
	w := m.waitRecs().recs[i]
	return w != nil && w.block && !w.done()
}

func (w *waitRec) done() bool { return w.do != "" }

func (m *jobMem) waitRecs() *waitMem {
	if m.waits == nil {
		m.waits = &waitMem{recs: map[int]*waitRec{}}
	}
	return m.waits
}

// isWait reports whether unit i is a wait unit.
func isWait(plan *class.Plan, i int) bool {
	return i >= 0 && i < len(plan.Units) && plan.Units[i].Wait != nil
}

// serveWaits takes the job's wait units out of what may run: each one
// whose units are done opens (and a clock's completes when its time has
// come). Escalated units open as waits for their person.
func (q *Queue) serveWaits(v *view, now time.Time) {
	m, plan := v.mem, v.mem.plan
	ws := m.waitRecs()
	keep := v.runnable[:0]
	v.openWaits = map[int]bool{}
	for _, i := range v.runnable {
		if w := ws.recs[i]; w != nil && w.block && !w.done() {
			// It stopped at a safe point to ask, and runs again once
			// decided.
			if t, ok := w.clock(); ok && !now.Before(t) {
				w.do, w.actor, w.at = "time", "clock", now
				keep = append(keep, i)
				continue
			}
			v.openWaits[i] = true
			continue
		}
		if !isWait(plan, i) {
			keep = append(keep, i)
			continue
		}
		w := ws.recs[i]
		if w == nil {
			w = &waitRec{since: now, server: plan.Units[i].Wait.Server}
			ws.recs[i] = w
		}
		if u := plan.Units[i]; !u.Wait.At.IsZero() && !now.Before(u.Wait.At) {
			w.do, w.actor, w.at = "time", "clock", now
			m.succeeded[i] = true
			continue
		}
		v.openWaits[i] = true
	}
	v.runnable = keep
	for i, w := range ws.recs {
		if w.block && !w.done() && (m.done(i) || m.failed[i]) {
			// The unit ended without stopping for its wait.
			w.do, w.actor, w.at = "withdrawn", "unit", now
		}
	}
	for i, r := range m.reviews {
		if !m.flagged(i) {
			continue
		}
		w := ws.recs[i]
		if w == nil || w.done() {
			w = &waitRec{since: now, escalated: true}
			ws.recs[i] = w
		}
		w.server = "person:" + r.To
		if r.To == "" {
			w.server = "person:"
		}
	}
}

// applyDecides carries out the job's new decisions, in token order, each
// once.
func applyDecides(j *c4v1.WorkJob, m *jobMem, now time.Time) {
	a := j.Annotations[c4v1.AnnotationDecide]
	if a == "" || m.plan == nil {
		return
	}
	var reqs []c4v1.WorkDecideRequest
	if err := json.Unmarshal([]byte(a), &reqs); err != nil {
		return
	}
	sort.SliceStable(reqs, func(x, y int) bool { return tokenValue(reqs[x].Token) < tokenValue(reqs[y].Token) })
	ws := m.waitRecs()
	last := tokenValue(ws.decided)
	plan := m.plan
	for _, r := range reqs {
		if tokenValue(r.Token) <= last {
			continue
		}
		ws.decided, last = r.Token, tokenValue(r.Token)
		i, ok := unitByRef(plan, r.Unit)
		if !ok || r.Do == "" {
			continue
		}
		at := r.At.Time
		if at.IsZero() {
			at = now
		}
		switch {
		case m.blockOpen(i) && !m.done(i) && !m.failed[i]:
			w := ws.recs[i]
			if len(w.choices) > 0 && r.Do != "fail" && !contains(w.choices, r.Do) {
				continue
			}
			if w.reply && r.Do != "fail" && r.View == "" {
				continue
			}
			w.do, w.note, w.actor, w.at, w.record = r.Do, r.Note, r.Actor, at, r.Record
			if r.Do == "fail" {
				m.failed[i] = true
			} else {
				w.view = r.View
			}
		case isWait(plan, i) && !m.done(i) && !m.failed[i]:
			w := ws.recs[i]
			if w == nil {
				// decided before it opened: it opens and closes at once
				w = &waitRec{since: at, server: plan.Units[i].Wait.Server}
				ws.recs[i] = w
			}
			if c := plan.Units[i].Wait.Choices; len(c) > 0 && r.Do != "fail" && !contains(c, r.Do) {
				continue
			}
			if plan.Units[i].Wait.Reply && r.Do != "fail" && r.View == "" {
				continue // a wait that asks for a reply completes only with one
			}
			w.do, w.note, w.actor, w.at, w.record = r.Do, r.Note, r.Actor, at, r.Record
			if r.Do == "fail" {
				m.failed[i] = true
			} else {
				m.succeeded[i] = true
				if r.View != "" {
					// The reply is the wait unit's output: what reads it
					// mounts it (graph.go).
					w.view, m.unitOut[i] = r.View, r.View
				}
			}
		case m.flagged(i):
			w := ws.recs[i]
			if w == nil {
				w = &waitRec{since: at, escalated: true, server: "person:" + m.reviews[i].To}
				ws.recs[i] = w
			}
			switch r.Do {
			case "accept":
				m.accepted[i] = true
				m.acks = append(m.acks, &ackRecord{WorkAck: c4v1.WorkAck{Kind: c4v1.AckAccept, Actor: r.Actor,
					Note: r.Note, At: metav1.Time{Time: at.Truncate(time.Second)}, Record: r.Record}, units: map[int]bool{i: true}})
			case "rework":
				delete(m.succeeded, i)
				delete(m.failed, i)
				delete(m.millis, i)
				delete(m.ended, i)
				m.forgetAcks(i)
				m.attempts[i] = 0
			case "fail":
				// it stays failed, no longer flagged: an ordinary failure
				m.reviews[i].Do = "fail"
			default:
				continue
			}
			w.do, w.note, w.actor, w.at, w.record = r.Do, r.Note, r.Actor, at, r.Record
		}
	}
}

// unitByRef finds a unit by name, else by index.
func unitByRef(plan *class.Plan, ref string) (int, bool) {
	for i, u := range plan.Units {
		if u.Name != "" && u.Name == ref {
			return i, true
		}
	}
	i, err := strconv.Atoi(strings.TrimPrefix(ref, "#"))
	return i, err == nil && i >= 0 && i < len(plan.Units)
}

// waitExpect is how long unit i's wait is expected to take in all, in
// seconds: the median of the job's decided waits for the same server,
// else the unit's own figure, else the default.
func waitExpect(plan *class.Plan, m *jobMem, i int) float64 {
	server := ""
	own := 0.0
	if isWait(plan, i) {
		server, own = plan.Units[i].Wait.Server, plan.Units[i].Wait.Expect
	} else if w := m.waitRecs().recs[i]; w != nil {
		server, own = w.server, w.expect
	}
	var took []float64
	for _, w := range m.waitRecs().recs {
		if w.done() && w.server == server && w.actor != "clock" {
			took = append(took, w.at.Sub(w.since).Seconds())
		}
	}
	if len(took) > 0 {
		sort.Float64s(took)
		return took[len(took)/2]
	}
	if own > 0 {
		return own
	}
	return defaultWaitExpect
}

// waitRemaining is the expected wait left for unit i at now.
func waitRemaining(plan *class.Plan, m *jobMem, i int, now time.Time) float64 {
	if isWait(plan, i) && !plan.Units[i].Wait.At.IsZero() {
		return math.Max(0, plan.Units[i].Wait.At.Sub(now).Seconds())
	}
	if w := m.waitRecs().recs[i]; w != nil && w.block {
		if t, ok := w.clock(); ok {
			return math.Max(0, t.Sub(now).Seconds())
		}
	}
	exp := waitExpect(plan, m, i)
	if w := m.waitRecs().recs[i]; w != nil && !w.since.IsZero() {
		return math.Max(0, exp-now.Sub(w.since).Seconds())
	}
	return exp
}

// blockingWait reports whether unfinished work waits on unit i: a unit
// waiting on a wait it opened holds up itself.
func blockingWait(plan *class.Plan, m *jobMem, i int) bool {
	if m.blockOpen(i) {
		return !m.done(i) && !m.failed[i]
	}
	for j, u := range plan.Units {
		if m.done(j) || m.failed[j] {
			continue
		}
		for _, a := range u.After {
			if a == i {
				return true
			}
		}
	}
	return false
}

// waitStatus lists the job's waits for status.waits, in unit order.
func waitStatus(m *jobMem, now time.Time) []c4v1.WorkWait {
	plan := m.plan
	if plan == nil || m.waits == nil {
		return nil
	}
	ws := m.waits
	idx := make([]int, 0, len(ws.recs))
	for i := range ws.recs {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var out []c4v1.WorkWait
	for _, i := range idx {
		w := ws.recs[i]
		u := plan.Units[i]
		ww := c4v1.WorkWait{Unit: int32(i), Name: u.Name, Server: w.server, Escalated: w.escalated,
			Do: w.do, Note: w.note, Actor: w.actor, Record: w.record, View: w.view}
		if w.block {
			ww.Block, ww.Safepoint, ww.Ask, ww.Choices, ww.Reply = true, int32(w.seq), w.ask, w.choices, w.reply
		} else if u.Wait != nil {
			ww.Ask, ww.Choices, ww.Reply = u.Wait.Ask, u.Wait.Choices, u.Wait.Reply
		} else if r := m.reviews[i]; r != nil {
			ww.Ask = r.Why
		}
		if !w.since.IsZero() {
			ww.Since = &metav1.Time{Time: w.since.Truncate(time.Second)}
		}
		if w.done() {
			ww.At = &metav1.Time{Time: w.at.Truncate(time.Second)}
		} else {
			ww.ExpectSeconds = int64(math.Round(waitExpect(plan, m, i)))
			ww.Blocking = blockingWait(plan, m, i)
		}
		out = append(out, ww)
	}
	return out
}

// loadWaits rebuilds the waits from status.
func (m *jobMem) loadWaits(st *c4v1.WorkJobStatus) {
	ws := m.waitRecs()
	ws.decided = st.Decided
	for _, w := range st.Waits {
		r := &waitRec{escalated: w.Escalated, server: w.Server, do: w.Do, note: w.Note, actor: w.Actor, record: w.Record, view: w.View}
		if w.View != "" {
			if m.unitOut == nil {
				m.unitOut = map[int]string{}
			}
			m.unitOut[int(w.Unit)] = w.View
		}
		if w.Since != nil {
			r.since = w.Since.Time
		}
		if w.At != nil {
			r.at = w.At.Time
		}
		if w.Block {
			r.block, r.seq, r.ask, r.choices, r.reply = true, int(w.Safepoint), w.Ask, w.Choices, w.Reply
			r.expect = float64(w.ExpectSeconds)
		}
		ws.recs[int(w.Unit)] = r
	}
}

// personWaitsBlocking counts the open waits for a person that hold up work.
func personWaitsBlocking(m *jobMem) (n int, who []string) {
	if m.plan == nil || m.waits == nil {
		return 0, nil
	}
	for i, w := range m.waits.recs {
		if w.done() || w.escalated || !strings.HasPrefix(w.server, "person:") || !blockingWait(m.plan, m, i) {
			continue
		}
		n++
		if p := strings.TrimPrefix(w.server, "person:"); p != "" && !contains(who, p) {
			who = append(who, p)
		}
	}
	sort.Strings(who)
	return n, who
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func personWaitsBlockingN(m *jobMem) int {
	n, _ := personWaitsBlocking(m)
	return n
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
