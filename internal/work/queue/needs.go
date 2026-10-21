package queue

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// Missing information (DESIGN.md 10.6; Joshua: "something that
// requires higher level information is a work stopping error condition").
// A unit whose brief lacks information only a person could give stops with
// NeedsInformation: its container exits NeedsExitCode with the information
// as its termination message (c4-unit needs), or a host runner sets the
// pod's status reason. The unit fails at once, without a retry (the same
// brief would ask the same again), keeping what it saved, and the job's
// status says what it needs; the rest of the job goes on where it does not
// depend on the unit. When a person supplies it (AnnotationSupply, through
// the HUD or MCP), the unit runs again with the text in $C4_SUPPLIED (and
// what it asked in $C4_NEEDED), resuming where it stopped.
//
// The question is an escalation that rises in grade and can heal
// (escalate.go): raised, then pressing once the plan says an answer now
// costs the deadline or it has been open too long; supplied by a person
// or the job's answerer, or withdrawn by the lead or by the block itself
// on a re-check. Supplied and withdrawn are final, and every step is a
// signed decision and in the book.

// The states of a question.
const (
	needsRaised    = "raised"
	needsPressing  = "pressing"
	needsSupplied  = "supplied"
	needsWithdrawn = "withdrawn"
)

type needsRec struct {
	text, why string
	about     []string
	record    string
	at        time.Time
	state     string
	// supplied is the answer, by who, when, and its record.
	supplied       string
	by             string
	suppliedAt     time.Time
	suppliedRecord string
	// answerBy is the latest time an answer can come with the job on time
	// (zero without a deadline); pressingAt and pressingWhy when and why
	// it turned pressing.
	answerBy    time.Time
	pressingAt  time.Time
	pressingWhy string
	// pressingRecord is the decision that made it pressing.
	pressingRecord string
	// withdrawn says who withdrew it and why.
	withdrawn *c4v1.WorkWithdrawal
	// The block's re-checks: how many ran, the most allowed, the last and
	// why, whether one runs now; pending is the trigger not yet served,
	// capped whether the cap refused one (recorded once). model: the
	// unit's reasoning is a model, so it may re-check at all.
	rechecks   int
	recheckMax int
	lastCheck  time.Time
	lastWhy    string
	inFlight   bool
	pending    string
	capped     bool
	model      bool
	// askID is the question put to the job's answerer, if any.
	askID string
	// base is what the triggers compare with (status.needs[i].seen).
	base *needsBase
}

// open reports whether nobody has supplied or withdrawn it.
func (n *needsRec) open() bool { return n.state != needsSupplied && n.state != needsWithdrawn }

// isNeeds reports whether a failed pod stopped for information.
func isNeeds(p *corev1.Pod) bool {
	_, _, _, ok := needsOf(p)
	return ok
}

// needsOf reads a failed pod's NeedsInformation: the information, why,
// and what it is about (a last line "about: unit:schema, path:schema/").
func needsOf(p *corev1.Pod) (text, why string, about []string, ok bool) {
	msg := ""
	switch {
	case p.Status.Reason == c4v1.NeedsInformation:
		msg, ok = p.Status.Message, true
	default:
		for _, cs := range p.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil && t.ExitCode == c4v1.NeedsExitCode {
				msg, ok = t.Message, true
				break
			}
		}
	}
	if !ok {
		return "", "", nil, false
	}
	msg = strings.TrimSpace(msg)
	if k := strings.LastIndex(msg, "\n"); k >= 0 || strings.HasPrefix(msg, "about: ") {
		if line := strings.TrimSpace(msg[k+1:]); strings.HasPrefix(line, "about: ") {
			about = parseAbout(strings.TrimPrefix(line, "about: "))
			msg = strings.TrimSpace(msg[:max(k, 0)])
		}
	}
	text, why, _ = strings.Cut(msg, "\nwhy: ")
	text = strings.TrimSpace(text)
	if text == "" {
		text = "information the brief lacked (the unit did not say which)"
	}
	return text, strings.TrimSpace(why), about, true
}

// parseAbout reads an about line: typed refs, path:, unit: or job:,
// separated by commas; a bare value is a path.
func parseAbout(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !strings.HasPrefix(r, "path:") && !strings.HasPrefix(r, "unit:") && !strings.HasPrefix(r, "job:") {
			r = "path:" + r
		}
		out = append(out, r)
	}
	return out
}

// noteNeeds fails unit idx, whose pod p stopped for information, without
// a retry, keeping what it saved so the next attempt resumes there.
func (q *Queue) noteNeeds(ctx context.Context, v *view, idx int, p *corev1.Pod, d class.Driver, now time.Time) {
	m := v.mem
	text, why, about, _ := needsOf(p)
	m.failed[idx] = true
	tree := p.Annotations[c4v1.AnnotationOutputTree]
	if _, ok := d.(class.Resumer); ok {
		tree = p.Annotations[c4v1.AnnotationOutputCheckpoint]
	}
	if tree != "" {
		m.resume[idx] = tree
	}
	if m.needs == nil {
		m.needs = map[int]*needsRec{}
	}
	n := &needsRec{text: text, why: why, about: about, at: now, state: needsRaised}
	extra := map[string]any{"needs": text}
	if why != "" {
		extra["why"] = why
	}
	if len(about) > 0 {
		extra["about"] = about
	}
	q.recordDecision(ctx, v, idx, "needs-raised", text, "", now, extra)
	n.record = m.lastRecord(idx)
	m.needs[idx] = n
	m.noteCause("needs", "needs:"+causeText(text), "needs information: "+causeText(text), idx, p.Spec.NodeName, now, "held")
	q.Log.Info("needs information", "job", v.key, "unit", idx, "needs", text)
	q.askAnswerer(ctx, v, idx, n, now)
}

// lastRecord is the record of the last decision, if it was about unit idx.
func (m *jobMem) lastRecord(idx int) string {
	if k := len(m.decisions); k > 0 && m.decisions[k-1].Unit == idx {
		return m.decisions[k-1].Record
	}
	return ""
}

// applySupplies carries out the information people supplied, in token
// order, each once: the unit that stopped for it runs again.
func (q *Queue) applySupplies(ctx context.Context, v *view, ctl *c4v1.WorkJob, now time.Time) {
	j, m := ctl, v.mem
	a := j.Annotations[c4v1.AnnotationSupply]
	if a == "" || m.plan == nil {
		return
	}
	var reqs []c4v1.WorkSupplyRequest
	if err := json.Unmarshal([]byte(a), &reqs); err != nil {
		return
	}
	sort.SliceStable(reqs, func(x, y int) bool { return tokenValue(reqs[x].Token) < tokenValue(reqs[y].Token) })
	last := tokenValue(m.supplied)
	for _, r := range reqs {
		if tokenValue(r.Token) <= last {
			continue
		}
		m.supplied, last = r.Token, tokenValue(r.Token)
		i, ok := unitByRef(m.plan, r.Unit)
		if !ok || !m.openNeeds(i) || strings.TrimSpace(r.Text) == "" {
			continue
		}
		q.supplyNeeds(ctx, v, i, r.Text, r.Actor, r.Record, now)
	}
}

// supplyNeeds gives unit i what it stopped for: it runs again with it.
func (q *Queue) supplyNeeds(ctx context.Context, v *view, i int, text, by, control string, now time.Time) {
	m := v.mem
	if !m.finished.IsZero() {
		// An answer never reopens a finished job (the HUD refuses it): a
		// person requeues it first.
		q.Log.Info("an answer to a finished job's question dropped", "job", v.key, "unit", i, "by", by)
		return
	}
	n := m.needs[i]
	n.supplied, n.by, n.suppliedAt, n.state = text, by, now, needsSupplied
	extra := map[string]any{"needs": n.text, "supplied": text, "by": by}
	if control != "" {
		extra["control"] = control
	}
	q.recordDecision(ctx, v, i, "needs-supplied", "supplied by "+or(by, "someone"), "", now, extra)
	n.suppliedRecord = m.lastRecord(i)
	m.closeNeedsAsk(n, "supplied by "+or(by, "someone"), now)
	delete(m.failed, i)
	m.attempts[i] = 0
}

// applyWithdraws carries out the lead's withdrawals of units' questions,
// in token order, each once: the unit runs again without an answer.
func (q *Queue) applyWithdraws(ctx context.Context, v *view, ctl *c4v1.WorkJob, now time.Time) {
	j, m := ctl, v.mem
	a := j.Annotations[c4v1.AnnotationWithdrawNeeds]
	if a == "" || m.plan == nil {
		return
	}
	var reqs []c4v1.WorkWithdrawRequest
	if err := json.Unmarshal([]byte(a), &reqs); err != nil {
		return
	}
	sort.SliceStable(reqs, func(x, y int) bool { return tokenValue(reqs[x].Token) < tokenValue(reqs[y].Token) })
	last := tokenValue(m.withdrawn)
	for _, r := range reqs {
		if tokenValue(r.Token) <= last {
			continue
		}
		m.withdrawn, last = r.Token, tokenValue(r.Token)
		i, ok := unitByRef(m.plan, r.Unit)
		if !ok || !m.openNeeds(i) || strings.TrimSpace(r.Why) == "" {
			continue
		}
		q.withdrawNeeds(ctx, v, i, r.Actor, r.Why, r.Record, now)
	}
}

// withdrawNeeds makes unit i's question moot: it runs again without an
// answer, told why ($C4_WITHDRAWN).
func (q *Queue) withdrawNeeds(ctx context.Context, v *view, i int, by, why, control string, now time.Time) {
	m := v.mem
	if !m.finished.IsZero() {
		q.Log.Info("a withdrawal from a finished job dropped", "job", v.key, "unit", i, "by", by)
		return
	}
	n := m.needs[i]
	n.state, n.inFlight, n.pending = needsWithdrawn, false, ""
	n.withdrawn = &c4v1.WorkWithdrawal{By: by, Why: why, At: metav1.Time{Time: now.Truncate(time.Second)}}
	extra := map[string]any{"needs": n.text, "by": by, "why": why}
	if control != "" {
		extra["control"] = control
	}
	q.recordDecision(ctx, v, i, "needs-withdrawn", why, "", now, extra)
	n.withdrawn.Record = m.lastRecord(i)
	m.closeNeedsAsk(n, "withdrawn by "+by, now)
	delete(m.failed, i)
	m.attempts[i] = 0
	q.Log.Info("question withdrawn", "job", v.key, "unit", i, "by", by, "why", why)
}

// openNeeds reports whether unit i stopped for information nobody has
// supplied or withdrawn yet.
func (m *jobMem) openNeeds(i int) bool {
	n := m.needs[i]
	return n != nil && n.open() && m.failed[i] && !m.accepted[i]
}

// openNeedsList is the units that stopped for information, in order.
func (m *jobMem) openNeedsList() []int {
	var out []int
	for i := range m.needs {
		if m.openNeeds(i) {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// suppliedEnv is what unit idx's next attempt is told it was given, or
// why it no longer needs it.
func suppliedEnv(m *jobMem, idx int) []corev1.EnvVar {
	n := m.needs[idx]
	switch {
	case n == nil:
		return nil
	case n.state == needsSupplied && n.supplied != "":
		return []corev1.EnvVar{{Name: "C4_NEEDED", Value: n.text}, {Name: "C4_SUPPLIED", Value: n.supplied}}
	case n.state == needsWithdrawn && n.withdrawn != nil:
		return []corev1.EnvVar{{Name: "C4_NEEDED", Value: n.text}, {Name: "C4_WITHDRAWN", Value: n.withdrawn.Why}}
	}
	return nil
}

// needsStatus is status.needs.
func needsStatus(m *jobMem) map[string]c4v1.WorkNeeds {
	if len(m.needs) == 0 {
		return nil
	}
	ts := func(t time.Time) *metav1.Time {
		if t.IsZero() {
			return nil
		}
		return &metav1.Time{Time: t.Truncate(time.Second)}
	}
	out := map[string]c4v1.WorkNeeds{}
	for i, n := range m.needs {
		w := c4v1.WorkNeeds{Text: n.text, Why: n.why, At: metav1.Time{Time: n.at.Truncate(time.Second)},
			Record: n.record, Supplied: n.supplied, By: n.by, SuppliedAt: ts(n.suppliedAt), SuppliedRecord: n.suppliedRecord,
			State: n.state, AnswerBy: ts(n.answerBy), PressingAt: ts(n.pressingAt), PressingWhy: n.pressingWhy,
			PressingRecord: n.pressingRecord, About: n.about, Withdrawn: n.withdrawn}
		if b := n.base; b != nil && n.open() {
			w.Seen = &c4v1.WorkNeedsSeen{Units: int32(b.units)}
			for u, o := range b.outs {
				if w.Seen.Outputs == nil {
					w.Seen.Outputs = map[string]string{}
				}
				w.Seen.Outputs[strconv.Itoa(u)] = o
			}
			if len(b.phases) > 0 {
				w.Seen.Phases = b.phases
			}
		}
		if n.model {
			w.Rechecks = &c4v1.WorkRechecks{Done: int32(n.rechecks), Max: int32(n.recheckMax), LastAt: ts(n.lastCheck),
				LastWhy: n.lastWhy, InFlight: n.inFlight}
		}
		out[strconv.Itoa(i)] = w
	}
	return out
}

// loadNeeds rebuilds the needs from status.
func (m *jobMem) loadNeeds(st *c4v1.WorkJobStatus) {
	m.supplied, m.withdrawn = st.Supplied, st.Withdrawn
	ts := func(t *metav1.Time) time.Time {
		if t == nil {
			return time.Time{}
		}
		return t.Time
	}
	for k, w := range st.Needs {
		i, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		if m.needs == nil {
			m.needs = map[int]*needsRec{}
		}
		n := &needsRec{text: w.Text, why: w.Why, about: w.About, record: w.Record, at: w.At.Time, state: w.State,
			supplied: w.Supplied, by: w.By, suppliedAt: ts(w.SuppliedAt), suppliedRecord: w.SuppliedRecord,
			answerBy: ts(w.AnswerBy), pressingAt: ts(w.PressingAt), pressingWhy: w.PressingWhy, pressingRecord: w.PressingRecord,
			withdrawn: w.Withdrawn}
		if s := w.Seen; s != nil {
			n.base = &needsBase{units: int(s.Units), outs: map[int]string{}, phases: map[string]string{}}
			for k, o := range s.Outputs {
				if u, err := strconv.Atoi(k); err == nil {
					n.base.outs[u] = o
				}
			}
			for k, ph := range s.Phases {
				n.base.phases[k] = ph
			}
		}
		switch {
		case n.state != "":
		case n.supplied != "":
			n.state = needsSupplied
		default:
			n.state = needsRaised
		}
		if r := w.Rechecks; r != nil {
			n.model, n.rechecks, n.recheckMax, n.lastCheck, n.lastWhy = true, int(r.Done), int(r.Max), ts(r.LastAt), r.LastWhy
			n.capped = n.rechecks >= n.recheckMax
		}
		for _, a := range st.Asks {
			if a.Needs && int(a.From) == i && a.Answered == nil {
				n.askID = a.ID
			}
		}
		m.needs[i] = n
	}
}
