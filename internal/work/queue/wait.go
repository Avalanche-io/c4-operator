package queue

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

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
			w.do, w.note, w.actor, w.at, w.record = r.Do, r.Note, r.Actor, at, r.Record
			if r.Do == "fail" {
				m.failed[i] = true
			} else {
				m.succeeded[i] = true
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
		server = w.server
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
	exp := waitExpect(plan, m, i)
	if w := m.waitRecs().recs[i]; w != nil && !w.since.IsZero() {
		return math.Max(0, exp-now.Sub(w.since).Seconds())
	}
	return exp
}

// blockingWait reports whether unfinished work waits on unit i.
func blockingWait(plan *class.Plan, m *jobMem, i int) bool {
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
			Do: w.do, Note: w.note, Actor: w.actor, Record: w.record}
		if u.Wait != nil {
			ww.Ask, ww.Choices = u.Wait.Ask, u.Wait.Choices
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
		r := &waitRec{escalated: w.Escalated, server: w.Server, do: w.Do, note: w.Note, actor: w.Actor, record: w.Record}
		if w.Since != nil {
			r.since = w.Since.Time
		}
		if w.At != nil {
			r.at = w.At.Time
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
