package queue

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// notePod sets one of a unit pod's relayed lists, as the node daemon does.
func notePod(t *testing.T, ns, job string, idx int, key, value string) {
	t.Helper()
	p := podOfUnit(t, ns, job, idx)
	patch := client.MergeFrom(p.DeepCopy())
	p.Annotations[key] = value
	if err := k8s.Patch(context.Background(), p, patch); err != nil {
		t.Fatal(err)
	}
}

func steerAsk(ss []unitSteer, id string) bool {
	for _, s := range ss {
		if s.Ask != nil && s.Ask.ID == id {
			return true
		}
	}
	return false
}

func steerAnswer(ss []unitSteer, id string) *unitSteer {
	for k := range ss {
		if s := &ss[k]; s.Answer != nil && s.Answer.ID == id {
			return s
		}
	}
	return nil
}

// Units ask one another: a question reaches its answerer as a steering
// message, the answer reaches the asker the same way, and an answer also
// decides a wait the answering unit serves, so the work after it runs. A
// question nobody can answer, or one that would wait in a cycle, is
// answered cannot at once.
func TestUnitsAskUnits(t *testing.T) {
	ns, q, _ := graphJob(t, "asked", func(time.Time) string {
		return `{"name": "lead"}, {"name": "worker"},
		{"name": "gate", "wait": {"server": "unit:lead", "ask": "is the API frozen?"}},
		{"name": "ship", "after": ["gate"]}`
	}, 24*time.Hour)
	// units: 1 lead, 2 worker, 3 gate, 4 ship
	tick(t, q)
	if got := podSteers(t, ns, "asked", 1); !steerAsk(got, "w3") {
		t.Fatalf("the gate's question did not reach lead: %+v", got)
	}

	notePod(t, ns, "asked", 2, c4v1.AnnotationAsks, `[{"seq": 1, "to": "unit:lead", "ask": "Which module owns the tokenizer?"},
		{"seq": 2, "to": "unit:nobody", "ask": "What is the frame rate?"}]`)
	tick(t, q)
	if got := podSteers(t, ns, "asked", 1); !steerAsk(got, "2#1") {
		t.Fatalf("worker's question did not reach lead: %+v", got)
	}
	if a := steerAnswer(podSteers(t, ns, "asked", 2), "2#2"); a == nil || !a.Answer.Cannot || !strings.Contains(a.Text, "no unit nobody") {
		t.Fatalf("an unanswerable question: %+v", a)
	}

	// lead answers both; then asks worker, which still waits on it: a cycle.
	notePod(t, ns, "asked", 2, c4v1.AnnotationAsks, `[{"seq": 1, "to": "unit:lead", "ask": "Which module owns the tokenizer?"},
		{"seq": 2, "to": "unit:nobody", "ask": "What is the frame rate?"}, {"seq": 3, "to": "unit:lead", "ask": "And the lexer?"}]`)
	tick(t, q)
	notePod(t, ns, "asked", 1, c4v1.AnnotationAnswers, `[{"seq": 1, "ask": "2#1", "answer": "tokenize/"}, {"seq": 2, "ask": "w3", "answer": "yes, frozen"}]`)
	notePod(t, ns, "asked", 1, c4v1.AnnotationAsks, `[{"seq": 1, "to": "unit:worker", "ask": "Are you done?"}]`)
	tick(t, q)
	tick(t, q)
	if a := steerAnswer(podSteers(t, ns, "asked", 2), "2#1"); a == nil || a.Answer.Cannot || !strings.Contains(a.Text, "tokenize/") || a.Actor != "lead" {
		t.Fatalf("worker's answer: %+v", a)
	}
	if a := steerAnswer(podSteers(t, ns, "asked", 1), "1#1"); a == nil || !a.Answer.Cannot || !strings.Contains(a.Text, "waits on an answer") {
		t.Fatalf("a cycle: %+v", a)
	}
	got := get(t, ns, "asked")
	if !slices.Contains(states(t, got, "succeeded"), 3) || !hasPod(t, ns, "asked", 4) {
		t.Fatalf("the gate: states %v", got.Status.UnitStates)
	}
	var w3, q21 *c4v1.WorkAsk
	for k := range got.Status.Asks {
		switch a := &got.Status.Asks[k]; a.ID {
		case "w3":
			w3 = a
		case "2#1":
			q21 = a
		}
	}
	if w3 == nil || !w3.Wait || w3.Answer != "yes, frozen" || q21 == nil || q21.To != 1 || q21.Answer != "tokenize/" || q21.Answered == nil {
		t.Fatalf("asks %+v", got.Status.Asks)
	}
	var marked bool
	for _, s := range got.Status.Steers {
		marked = marked || s.Unit == 2 && s.Answer == "2#1"
	}
	if !marked {
		t.Fatalf("worker's answer is not marked in status.steers: %+v", got.Status.Steers)
	}
	for _, w := range got.Status.Waits {
		if w.Unit == 3 && (w.Do != "answer" || w.Note != "yes, frozen" || w.Actor != "unit:lead") {
			t.Fatalf("the gate's wait %+v", w)
		}
	}
	// A restart keeps the questions, and routes none again.
	n := len(get(t, ns, "asked").Status.Asks)
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	if m := len(get(t, ns, "asked").Status.Asks); m != n {
		t.Fatalf("after a restart: %d asks, was %d", m, n)
	}
}

// A unit that waited past its hold gives up its slot to wait on its
// answerer (a block wait served by unit:<name>); the answer decides the
// wait, and the unit runs again with it in C4_DECISION.
func TestAnswerResumesAWaitingUnit(t *testing.T) {
	ns, q, now := graphJob(t, "held", func(time.Time) string {
		return `{"name": "lead"}, {"name": "worker"}`
	}, 24*time.Hour)
	q.Safepoints = &SafepointCache{}
	notePod(t, ns, "held", 2, c4v1.AnnotationAsks, `[{"seq": 1, "to": "unit:lead", "ask": "Which module owns the tokenizer?"}]`)
	tick(t, q)
	askAt(t, podOfUnit(t, ns, "held", 2), 1, *now, `{"server": "unit:lead", "ask": "Which module owns the tokenizer?"}`)
	tick(t, q)
	*now = now.Add(time.Minute)
	finishUnit(t, now, ns, "held", 2, nil)
	tick(t, q)
	if hasPod(t, ns, "held", 2) || !slices.Equal(states(t, get(t, ns, "held"), "waits"), []int{2}) {
		t.Fatalf("worker does not wait: %v", get(t, ns, "held").Status.UnitStates)
	}
	notePod(t, ns, "held", 1, c4v1.AnnotationAnswers, `[{"seq": 1, "ask": "2#1", "answer": "tokenize/"}]`)
	tick(t, q)
	readyMaterialization(t, ns, resumeName("held", 2), get(t, ns, "held").Status.Resume["2"])
	tick(t, q)
	p := podOfUnit(t, ns, "held", 2)
	if d := envOf(p.Spec.Containers[0], "C4_DECISION"); !strings.Contains(d, `"do":"answer"`) || !strings.Contains(d, "tokenize/") {
		t.Fatalf("worker's C4_DECISION %q", d)
	}
}

// An answerer that has finished is woken to answer: an extra attempt of
// its unit, marked as a wake, carrying the question; what it answers goes
// to the asker, and the unit stays as it was. A wake that ends without
// answering answers cannot.
func TestFinishedAnswererIsWoken(t *testing.T) {
	ns, q, now := graphJob(t, "woke", func(time.Time) string {
		return `{"name": "lead"}, {"name": "worker"}`
	}, 24*time.Hour)
	finishUnit(t, now, ns, "woke", 1, nil)
	tick(t, q)
	notePod(t, ns, "woke", 2, c4v1.AnnotationAsks, `[{"seq": 1, "to": "unit:lead", "ask": "Which module owns the tokenizer?"}]`)
	tick(t, q)
	tick(t, q)
	p := podOfUnit(t, ns, "woke", 1)
	if p.Annotations[c4v1.AnnotationWake] != "2#1" || envOf(p.Spec.Containers[0], "C4_ANSWER_ONLY") != "2#1" || !steerAsk(podSteers(t, ns, "woke", 1), "2#1") {
		t.Fatalf("lead's wake: %v", p.Annotations)
	}
	if !slices.Contains(states(t, get(t, ns, "woke"), "succeeded"), 1) {
		t.Fatalf("lead is no longer done: %v", get(t, ns, "woke").Status.UnitStates)
	}
	notePod(t, ns, "woke", 1, c4v1.AnnotationAnswers, `[{"seq": 1, "ask": "2#1", "answer": "tokenize/"}]`)
	*now = now.Add(time.Minute)
	finishUnit(t, now, ns, "woke", 1, nil)
	tick(t, q)
	tick(t, q)
	if a := steerAnswer(podSteers(t, ns, "woke", 2), "2#1"); a == nil || a.Answer.Cannot || !strings.Contains(a.Text, "tokenize/") {
		t.Fatalf("worker's answer: %+v", a)
	}
	got := get(t, ns, "woke")
	if !slices.Contains(states(t, got, "succeeded"), 1) || hasPod(t, ns, "woke", 1) {
		t.Fatalf("after the wake: %v", got.Status.UnitStates)
	}

	// Woken again for a second question, it ends without answering.
	notePod(t, ns, "woke", 2, c4v1.AnnotationAsks, `[{"seq": 1, "to": "unit:lead", "ask": "Which module owns the tokenizer?"},
		{"seq": 2, "to": "unit:lead", "ask": "And the lexer?"}]`)
	tick(t, q)
	tick(t, q)
	*now = now.Add(time.Minute)
	finishUnit(t, now, ns, "woke", 1, nil)
	tick(t, q)
	tick(t, q)
	if a := steerAnswer(podSteers(t, ns, "woke", 2), "2#2"); a == nil || !a.Answer.Cannot || !strings.Contains(a.Text, "did not answer") {
		t.Fatalf("an unanswered wake: %+v", a)
	}
	if hasPod(t, ns, "woke", 1) {
		t.Fatal("lead was woken again for a question its wake left")
	}
}
