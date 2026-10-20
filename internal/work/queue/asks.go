package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
)

// Questions between units (DESIGN.md 10.6, Agents answer agents; Joshua:
// "the thing answering may be an agent not a human"). A unit that lacks a
// fact another unit of its job has asks it (c4-unit ask --to unit:<name>);
// the node daemon relays the unit's questions and answers as pod
// annotations (AnnotationAsks, AnnotationAnswers), and the queue routes
// each, as a steering message, to the unit it is for. The answerer is the
// question's unit, else the job's answerer param. A question is answered at
// once with cannot when there is no answerer, the answerer has finished, it
// waits on the asker (a cycle), or the asker has asks.max (3) questions
// open. An answer also decides a wait served by the answering unit (server
// unit:<name>): a block that gave up its slot to wait for it resumes with
// it, and a wait unit of the graph completes. Every question and answer is
// in the book.
//
// An answerer that has already finished is woken for the question: an
// extra attempt of its unit (AnnotationWake, $C4_ANSWER_ONLY), which an
// agent block takes as its own session resumed to answer, and nothing
// else. The attempt takes a slot like any unit; its end and anything it
// writes leave the unit as it was. One that ends without answering answers
// cannot.

// How many questions a unit may have open is policy asks.max (3), and who
// answers them policy answers (internal/work/policy).
const (
	maxAsksKept = 64
	// unitWaitExpect is a wait for a unit's answer before any is measured.
	unitWaitExpect = 600
)

type askRec struct {
	id         string
	from, to   int
	text       string
	at         time.Time
	answer     string
	cannot     string
	answeredAt time.Time
	// wait: the question of a wait unit (from) served by a unit (to).
	wait bool
	// needs: the question a unit stopped for, put to the job's answerer;
	// its answer supplies it (escalate.go).
	needs bool
}

func (a *askRec) open() bool { return a.answeredAt.IsZero() }

// askRef and answerRef mark a steering message as a question to the unit,
// or an answer to one it asked.
type askRef struct {
	ID   string `json:"id"`
	From string `json:"from"`
}

type answerRef struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	Cannot bool   `json:"cannot,omitempty"`
}

// unitNote is an entry of a unit's .c4/asks or .c4/answers.
type unitNote struct {
	Seq    int    `json:"seq"`
	To     string `json:"to"`
	Ask    string `json:"ask"`
	Answer string `json:"answer"`
	Cannot string `json:"cannot"`
}

func nameOf(m *jobMem, i int) string {
	if m.plan != nil && i >= 0 && i < len(m.plan.Units) && m.plan.Units[i].Name != "" {
		return m.plan.Units[i].Name
	}
	return strconv.Itoa(i)
}

// steerTo queues a message for unit i (steer.go).
func (m *jobMem) steerTo(i int, actor, text string, at time.Time, ask *askRef, answer *answerRef) {
	seq := 1
	for _, s := range m.steers {
		if s.unit == i {
			seq = max(seq, s.seq+1)
		}
	}
	m.steers = append(m.steers, &steerRec{unit: i, seq: seq, actor: actor, text: text, at: at, ask: ask, answer: answer})
	if n := len(m.steers); n > maxSteersKept {
		m.steers = m.steers[n-maxSteersKept:]
	}
}

// answerer is who answers a question unit idx asks of to, or why none can:
// the unit named, else the first of the job's answerers (policy answers)
// that can. refusedBy is the policy value that refused it, if one did.
func (v *view) answerer(q *Queue, idx int, to string) (j int, why string, refusedBy []policy.Use) {
	m := v.mem
	proj := q.projectOf(v)
	names := []string{strings.TrimPrefix(to, "unit:")}
	if to == "" {
		names, _ = policy.List(v.job, proj, idx, policy.Answers)
	}
	if len(names) == 0 || names[0] == "" {
		return -1, "no answerer: ask with --to unit:<name>, or give the job an answerer", nil
	}
	limit, lu := policy.Int(v.job, proj, idx, policy.AsksMax)
	open := 0
	for _, a := range m.asks {
		if a.open() && a.from == idx && !a.wait {
			open++
		}
	}
	if open >= limit {
		return -1, fmt.Sprintf("%d questions are open, the most a unit may have (asks.max, %s)", open, lu.From), []policy.Use{lu}
	}
	first := ""
	for _, name := range names {
		if why := v.canAnswer(idx, name); why != "" {
			first = or(first, why)
			continue
		}
		return m.plan.Names()[name], "", nil
	}
	return -1, first, nil
}

// canAnswer is why unit name cannot answer unit idx's question, if not.
func (v *view) canAnswer(idx int, name string) string {
	m := v.mem
	j, ok := m.plan.Names()[name]
	switch {
	case !ok:
		return "no unit " + name + " in the job"
	case j == idx:
		return "a unit does not answer itself"
	case m.failed[j] || m.superseded[j]:
		return name + " has failed"
	}
	for _, a := range m.asks {
		if a.open() && a.from == j && a.to == idx {
			return name + " waits on an answer from this unit"
		}
	}
	return ""
}

// ask records a question and routes it: to its answerer, or back to the
// asker as cannot.
func (q *Queue) ask(ctx context.Context, v *view, a *askRec, to string, now time.Time) {
	m := v.mem
	j, why, refusedBy := v.answerer(q, a.from, to)
	if a.wait {
		j, why, refusedBy = a.to, "", nil
	}
	a.to = j
	m.asks = append(m.asks, a)
	if n := len(m.asks); n > maxAsksKept {
		m.asks = m.asks[n-maxAsksKept:]
	}
	extra := map[string]any{"id": a.id, "question": a.text}
	if j >= 0 {
		extra["to"] = nameOf(m, j)
	}
	if a.needs {
		extra["needs"] = true
	}
	q.recordPolicyDecision(ctx, v, a.from, "ask", a.text, now, refusedBy, extra)
	if why != "" {
		q.answer(ctx, v, a, "", why, now)
		return
	}
	from := nameOf(m, a.from)
	asked := "question %s from %s: %s\n(answer with: c4-unit answer --ask %s --answer TEXT, or --cannot WHY)"
	if a.needs {
		asked = "question %s from %s, which stopped for it: %s\n(answer with: c4-unit answer --ask %s --answer TEXT, or --cannot WHY)"
	}
	m.steerTo(j, from, fmt.Sprintf(asked, a.id, from, a.text, a.id), now, &askRef{ID: a.id, From: from}, nil)
}

// answer records an answer to a question and takes it to the asker: a
// steering message, and the decision of a wait the answerer serves.
func (q *Queue) answer(ctx context.Context, v *view, a *askRec, text, cannot string, now time.Time) {
	m := v.mem
	a.answer, a.cannot, a.answeredAt = text, cannot, now
	by := "c4"
	if a.to >= 0 {
		by = nameOf(m, a.to)
	}
	note := text
	if cannot != "" {
		note = "cannot: " + cannot
	}
	q.recordDecision(ctx, v, max(a.to, a.from), "answer", note, "", now, map[string]any{"id": a.id, "from": nameOf(m, a.from)})
	ws := m.waitRecs()
	if w := ws.recs[a.from]; w != nil && !w.done() && a.to >= 0 && w.server == "unit:"+by {
		// The wait it was asked in: a wait unit completes, a block that
		// gave up its slot resumes with the answer (C4_DECISION).
		w.do, w.note, w.actor, w.at = "answer", note, "unit:"+by, now
		if a.wait {
			m.succeeded[a.from] = true
		}
	}
	if a.wait {
		return
	}
	if a.needs {
		// The asker stopped for it: an answer supplies it, by the
		// answering unit (needs.go); cannot leaves it to a person.
		if text != "" && cannot == "" && m.openNeeds(a.from) {
			q.supplyNeeds(ctx, v, a.from, text, "unit:"+v.key+"/"+by, "", now)
		}
		return
	}
	m.steerTo(a.from, by, fmt.Sprintf("answer to %s from %s: %s", a.id, by, note), now, nil, &answerRef{ID: a.id, From: by, Cannot: cannot != ""})
}

// wakeFor is the open questions to unit j, a unit that has finished, that
// no wake has been started for: why it must be woken.
func (m *jobMem) wakeFor(j int) []string {
	if !m.done(j) {
		return nil
	}
	var ids []string
	for _, a := range m.asks {
		if a.open() && a.to == j && !m.woken[a.id] {
			ids = append(ids, a.id)
		}
	}
	return ids
}

// answersFrom takes the answers unit idx's pod p wrote to the questions
// they answer.
func (q *Queue) answersFrom(ctx context.Context, v *view, idx int, p *corev1.Pod, now time.Time) {
	var answers []unitNote
	_ = json.Unmarshal([]byte(p.Annotations[c4v1.AnnotationAnswers]), &answers)
	for _, n := range answers {
		for _, a := range v.mem.asks {
			if a.id == n.Ask && a.open() && a.to == idx && (n.Answer != "" || n.Cannot != "") {
				q.answer(ctx, v, a, n.Answer, n.Cannot, now)
			}
		}
	}
}

// endWake closes a wake attempt that ended: what it answered stands, and
// the questions it was woken for that it left open are answered cannot.
func (q *Queue) endWake(ctx context.Context, v *view, idx int, p *corev1.Pod, now time.Time) {
	q.answersFrom(ctx, v, idx, p, now)
	for _, id := range strings.Split(p.Annotations[c4v1.AnnotationWake], ",") {
		for _, a := range v.mem.asks {
			if a.id == id && a.open() && a.to == idx {
				q.answer(ctx, v, a, "", nameOf(v.mem, idx)+" woke for it and did not answer", now)
			}
		}
	}
}

// routeAsks takes the questions and answers running units wrote, each
// once, to the units they are for; and asks a wait unit's question of the
// unit that serves it.
func (q *Queue) routeAsks(ctx context.Context, v *view, now time.Time) {
	m := v.mem
	if m.plan == nil {
		return
	}
	known := map[string]*askRec{}
	for _, a := range m.asks {
		known[a.id] = a
	}
	ws := m.waitRecs()
	for i, w := range ws.recs {
		srv, ok := strings.CutPrefix(w.server, "unit:")
		if !ok || w.done() || !isWait(m.plan, i) || known["w"+strconv.Itoa(i)] != nil || !v.openWaits[i] {
			continue
		}
		j, found := m.plan.Names()[srv]
		if !found {
			continue
		}
		a := &askRec{id: "w" + strconv.Itoa(i), from: i, to: j, text: m.plan.Units[i].Wait.Ask, at: now, wait: true}
		q.ask(ctx, v, a, "", now)
		known[a.id] = a
	}
	for idx, pod := range v.pods {
		if _, running := v.running[idx]; !running {
			continue
		}
		var asks, answers []unitNote
		_ = json.Unmarshal([]byte(pod.Annotations[c4v1.AnnotationAsks]), &asks)
		_ = json.Unmarshal([]byte(pod.Annotations[c4v1.AnnotationAnswers]), &answers)
		for _, n := range asks {
			id := fmt.Sprintf("%d#%d", idx, n.Seq)
			if n.Seq <= 0 || strings.TrimSpace(n.Ask) == "" || known[id] != nil {
				continue
			}
			a := &askRec{id: id, from: idx, to: -1, text: strings.TrimSpace(n.Ask), at: now}
			q.ask(ctx, v, a, n.To, now)
			known[id] = a
		}
		for _, n := range answers {
			a := known[n.Ask]
			if a == nil || !a.open() || a.to != idx || n.Answer == "" && n.Cannot == "" {
				continue
			}
			q.answer(ctx, v, a, n.Answer, n.Cannot, now)
		}
	}
}

// serveWakes adds the finished units that must be woken to answer to what
// may run.
func (v *view) serveWakes() {
	m := v.mem
	v.waking = map[int][]string{}
	for i := range m.plan.Units {
		if _, running := v.running[i]; running || v.finishing[i] {
			continue
		}
		if ids := m.wakeFor(i); len(ids) > 0 {
			v.waking[i] = ids
			v.runnable = append(v.runnable, i)
		}
	}
}

// askStatus lists the job's questions for status.asks.
func askStatus(m *jobMem) []c4v1.WorkAsk {
	var out []c4v1.WorkAsk
	for _, a := range m.asks {
		w := c4v1.WorkAsk{ID: a.id, From: int32(a.from), To: int32(a.to), Text: a.text, At: metav1.Time{Time: a.at.Truncate(time.Second)},
			Answer: a.answer, Cannot: a.cannot, Wait: a.wait, Needs: a.needs}
		if !a.answeredAt.IsZero() {
			w.Answered = &metav1.Time{Time: a.answeredAt.Truncate(time.Second)}
		}
		out = append(out, w)
	}
	return out
}

// loadAsks rebuilds the questions from status.
func (m *jobMem) loadAsks(st *c4v1.WorkJobStatus) {
	for _, w := range st.Asks {
		a := &askRec{id: w.ID, from: int(w.From), to: int(w.To), text: w.Text, at: w.At.Time, answer: w.Answer, cannot: w.Cannot, wait: w.Wait, needs: w.Needs}
		if w.Answered != nil {
			a.answeredAt = w.Answered.Time
		}
		m.asks = append(m.asks, a)
	}
}
