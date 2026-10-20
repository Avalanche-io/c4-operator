package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Questions between units (agent-jobs ASKS.md; Joshua: "the thing
// answering may be an agent not a human"). A unit that lacks a fact
// another unit of its job has asks it (c4-unit ask --to unit:<name>); the
// node daemon relays the unit's questions and answers as pod annotations
// (AnnotationAsks, AnnotationAnswers), and the queue routes each, as a
// steering message, to the unit it is for. The answerer is the question's
// unit, else the job's answerer param. A question is answered at once with
// cannot when there is no answerer, the answerer has finished, it waits on
// the asker (a cycle), or the asker has maxAsks (3) questions open. An
// answer also decides a wait served by the answering unit (server
// unit:<name>): a block that gave up its slot to wait for it resumes with
// it, and a wait unit of the graph completes. Every question and answer is
// in the book.

const (
	defaultMaxAsks = 3
	maxAsksKept    = 64
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

// answerer is who answers a question unit idx asks of to, or why none can.
func (v *view) answerer(idx int, to string) (int, string) {
	m := v.mem
	name := strings.TrimPrefix(to, "unit:")
	if to == "" {
		name = strings.TrimSpace(v.job.Spec.Params["answerer"])
	}
	if name == "" {
		return -1, "no answerer: ask with --to unit:<name>, or give the job an answerer"
	}
	j, ok := m.plan.Names()[name]
	switch {
	case !ok:
		return -1, "no unit " + name + " in the job"
	case j == idx:
		return -1, "a unit does not answer itself"
	case m.done(j) || m.failed[j]:
		return -1, name + " has finished"
	}
	for _, a := range m.asks {
		if a.open() && a.from == j && a.to == idx {
			return -1, name + " waits on an answer from this unit"
		}
	}
	limit := defaultMaxAsks
	if n, err := strconv.Atoi(v.job.Spec.Params["maxAsks"]); err == nil && n > 0 {
		limit = n
	}
	open := 0
	for _, a := range m.asks {
		if a.open() && a.from == idx && !a.wait {
			open++
		}
	}
	if open >= limit {
		return -1, fmt.Sprintf("%d questions are open, the most a unit may have (maxAsks)", open)
	}
	return j, ""
}

// ask records a question and routes it: to its answerer, or back to the
// asker as cannot.
func (q *Queue) ask(ctx context.Context, v *view, a *askRec, to string, now time.Time) {
	m := v.mem
	j, why := v.answerer(a.from, to)
	if a.wait {
		j, why = a.to, ""
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
	q.recordDecision(ctx, v, a.from, "ask", a.text, "", now, extra)
	if why != "" {
		q.answer(ctx, v, a, "", why, now)
		return
	}
	from := nameOf(m, a.from)
	m.steerTo(j, from, fmt.Sprintf("question %s from %s: %s\n(answer with: c4-unit answer --ask %s --answer TEXT, or --cannot WHY)", a.id, from, a.text, a.id),
		now, &askRef{ID: a.id, From: from}, nil)
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
	m.steerTo(a.from, by, fmt.Sprintf("answer to %s from %s: %s", a.id, by, note), now, nil, &answerRef{ID: a.id, From: by, Cannot: cannot != ""})
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

// askStatus lists the job's questions for status.asks.
func askStatus(m *jobMem) []c4v1.WorkAsk {
	var out []c4v1.WorkAsk
	for _, a := range m.asks {
		w := c4v1.WorkAsk{ID: a.id, From: int32(a.from), To: int32(a.to), Text: a.text, At: metav1.Time{Time: a.at.Truncate(time.Second)},
			Answer: a.answer, Cannot: a.cannot, Wait: a.wait}
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
		a := &askRec{id: w.ID, from: int(w.From), to: int(w.To), text: w.Text, at: w.At.Time, answer: w.Answer, cannot: w.Cannot, wait: w.Wait}
		if w.Answered != nil {
			a.answeredAt = w.Answered.Time
		}
		m.asks = append(m.asks, a)
	}
}
