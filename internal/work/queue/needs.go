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

// Missing information (agent-jobs ASKS.md; Joshua: "something that
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

type needsRec struct {
	text, why string
	record    string
	at        time.Time
	supplied  string
	by        string
}

// isNeeds reports whether a failed pod stopped for information.
func isNeeds(p *corev1.Pod) bool {
	_, _, ok := needsOf(p)
	return ok
}

// needsOf reads a failed pod's NeedsInformation: the information and why.
func needsOf(p *corev1.Pod) (text, why string, ok bool) {
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
		return "", "", false
	}
	text, why, _ = strings.Cut(strings.TrimSpace(msg), "\nwhy: ")
	text = strings.TrimSpace(text)
	if text == "" {
		text = "information the brief lacked (the unit did not say which)"
	}
	return text, strings.TrimSpace(why), true
}

// noteNeeds fails unit idx, whose pod p stopped for information, without
// a retry, keeping what it saved so the next attempt resumes there.
func (q *Queue) noteNeeds(ctx context.Context, v *view, idx int, p *corev1.Pod, d class.Driver, now time.Time) {
	m := v.mem
	text, why, _ := needsOf(p)
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
	n := &needsRec{text: text, why: why, at: now}
	extra := map[string]any{"needs": text}
	if why != "" {
		extra["why"] = why
	}
	q.recordDecision(ctx, v, idx, "needs-information", text, "", now, extra)
	if k := len(m.decisions); k > 0 && m.decisions[k-1].Unit == idx {
		n.record = m.decisions[k-1].Record
	}
	m.needs[idx] = n
	m.noteCause("needs", "needs:"+causeText(text), "needs information: "+causeText(text), idx, p.Spec.NodeName, now, "held")
	q.Log.Info("needs information", "job", v.key, "unit", idx, "needs", text)
}

// applySupplies carries out the information people supplied, in token
// order, each once: the unit that stopped for it runs again.
func applySupplies(j *c4v1.WorkJob, m *jobMem) {
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
		n := m.needs[i]
		if !ok || n == nil || n.supplied != "" || !m.failed[i] || strings.TrimSpace(r.Text) == "" {
			continue
		}
		n.supplied, n.by = r.Text, r.Actor
		delete(m.failed, i)
		m.attempts[i] = 0
	}
}

// openNeeds reports whether unit i stopped for information nobody has
// supplied yet.
func (m *jobMem) openNeeds(i int) bool {
	n := m.needs[i]
	return n != nil && m.failed[i] && !m.accepted[i]
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

// suppliedEnv is what unit idx's next attempt is told it was given.
func suppliedEnv(m *jobMem, idx int) []corev1.EnvVar {
	n := m.needs[idx]
	if n == nil || n.supplied == "" {
		return nil
	}
	return []corev1.EnvVar{{Name: "C4_NEEDED", Value: n.text}, {Name: "C4_SUPPLIED", Value: n.supplied}}
}

// needsStatus is status.needs.
func needsStatus(m *jobMem) map[string]c4v1.WorkNeeds {
	if len(m.needs) == 0 {
		return nil
	}
	out := map[string]c4v1.WorkNeeds{}
	for i, n := range m.needs {
		out[strconv.Itoa(i)] = c4v1.WorkNeeds{Text: n.text, Why: n.why, At: metav1.Time{Time: n.at.Truncate(time.Second)},
			Record: n.record, Supplied: n.supplied, By: n.by}
	}
	return out
}

// loadNeeds rebuilds the needs from status.
func (m *jobMem) loadNeeds(st *c4v1.WorkJobStatus) {
	m.supplied = st.Supplied
	for k, w := range st.Needs {
		i, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		if m.needs == nil {
			m.needs = map[int]*needsRec{}
		}
		m.needs[i] = &needsRec{text: w.Text, why: w.Why, record: w.Record, at: w.At.Time, supplied: w.Supplied, by: w.By}
	}
}
