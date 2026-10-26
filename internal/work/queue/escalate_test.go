package queue

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// graphJobWith is graphJob with job params.
func graphJobWith(t *testing.T, name string, params map[string]string, units func(now time.Time) string, deadline time.Duration) (string, *Queue, *time.Time) {
	t.Helper()
	ns, q, now, st := graphFarm(t)
	if err := k8s.Create(context.Background(), job(ns, name, "graph", "cpu-"+ns, params, now.Add(deadline))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [`+units(*now)+`]}`)
	finish(t, podOfUnit(t, ns, name, 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	return ns, q, now
}

// endRecheck ends a re-check pod: exit 0 with msg as its termination
// message, or exit code with msg.
func endRecheckPod(t *testing.T, p *corev1.Pod, end time.Time, code int32, msg string) {
	t.Helper()
	p.Status.Phase = corev1.PodSucceeded
	if code != 0 {
		p.Status.Phase = corev1.PodFailed
	}
	p.Status.StartTime = &metav1.Time{Time: end.Add(-time.Minute)}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "work", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, Message: msg, StartedAt: metav1.Time{Time: end.Add(-time.Minute)}, FinishedAt: metav1.Time{Time: end}}}}}
	if err := k8s.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func withdrawUnit(t *testing.T, ns, name, unit, why string, at time.Time) {
	t.Helper()
	j := get(t, ns, name)
	var reqs []c4v1.WorkWithdrawRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationWithdrawNeeds]), &reqs)
	reqs = append(reqs, c4v1.WorkWithdrawRequest{Token: strconv.FormatInt(at.UnixNano(), 36), Unit: unit, Why: why, Actor: "lead", At: metav1.Time{Time: at}})
	b, _ := json.Marshal(reqs)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationWithdrawNeeds] = string(b)
	if err := k8s.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func decisionsOf(j *c4v1.WorkJob, kind string) []c4v1.WorkDecision {
	var out []c4v1.WorkDecision
	for _, d := range j.Status.Decisions {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

// An open question turns pressing once it has been open pressAfter (2h by
// default), with the reason in words and the value that decided it, from
// where, in the decision.
func TestNeedsPressByAge(t *testing.T) {
	ns, q, now := graphJob(t, "aged", func(time.Time) string {
		return `{"name": "cut"}, {"name": "deliver", "after": ["cut"]}`
	}, 72*time.Hour)
	failNeeding(t, podOfUnit(t, ns, "aged", 1), *now, "the delivery frame rate", false)
	tick(t, q)
	got := get(t, ns, "aged")
	n := got.Status.Needs["1"]
	if n.State != needsRaised || n.AnswerBy == nil || n.PressingAt != nil || len(decisionsOf(got, "needs-raised")) != 1 {
		t.Fatalf("raised: %+v, decisions %+v", n, got.Status.Decisions)
	}
	if !n.AnswerBy.Time.Before(now.Add(72*time.Hour)) || n.AnswerBy.Time.Before(*now) {
		t.Fatalf("answerBy %s, deadline %s", n.AnswerBy, now.Add(72*time.Hour))
	}
	*now = now.Add(time.Hour)
	tick(t, q)
	if s := get(t, ns, "aged").Status.Needs["1"].State; s != needsRaised {
		t.Fatalf("after an hour: %s", s)
	}
	*now = now.Add(time.Hour)
	tick(t, q)
	got = get(t, ns, "aged")
	n = got.Status.Needs["1"]
	if n.State != needsPressing || n.PressingWhy != "unanswered for 2h" || n.PressingAt == nil {
		t.Fatalf("after two hours: %+v", n)
	}
	ds := decisionsOf(got, "needs-pressing")
	if len(ds) != 1 || len(ds[0].Policy) != 1 || ds[0].Policy[0].Key != "escalation.pressAfter" || ds[0].Policy[0].Value != "2h" || ds[0].Policy[0].From.Scope != "farm" {
		t.Fatalf("pressing decision %+v", ds)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson); c == nil || c.Reason != c4v1.NeedsInformation {
		t.Fatalf("needs person %+v", c)
	}
	*now = now.Add(time.Hour)
	tick(t, q)
	if ds := decisionsOf(get(t, ns, "aged"), "needs-pressing"); len(ds) != 1 {
		t.Fatalf("pressed again: %+v", ds)
	}
}

// It turns pressing earlier when the plan says an answer now costs the
// deadline: answerBy is the deadline less the work from the unit to the
// job's end, and it is pressing pressMargin before. A job's longer margin
// tightens the farm's.
func TestNeedsPressByPlan(t *testing.T) {
	ns, q, now := graphJob(t, "tight", func(time.Time) string {
		return `{"name": "cut", "work": 3600}, {"name": "deliver", "after": ["cut"], "work": 3600}`
	}, 3*time.Hour)
	failNeeding(t, podOfUnit(t, ns, "tight", 1), *now, "the delivery frame rate", false)
	tick(t, q)
	n := get(t, ns, "tight").Status.Needs["1"]
	deadline := now.Add(3 * time.Hour)
	if n.State != needsRaised || n.AnswerBy == nil {
		t.Fatalf("raised: %+v", n)
	}
	if left := deadline.Sub(n.AnswerBy.Time); left < 110*time.Minute || left > 130*time.Minute {
		t.Fatalf("answerBy %s: %s before the deadline, want about the two hours of work", n.AnswerBy, left)
	}
	*now = n.AnswerBy.Time.Add(-10 * time.Minute)
	tick(t, q)
	got := get(t, ns, "tight")
	n = got.Status.Needs["1"]
	if n.State != needsPressing || !strings.HasPrefix(n.PressingWhy, "blocks cut, then deliver; tight misses its deadline (") ||
		!strings.Contains(n.PressingWhy, "unless answered by") {
		t.Fatalf("pressing: %+v", n)
	}
	if ds := decisionsOf(got, "needs-pressing"); len(ds) != 1 || ds[0].Policy[0].Key != "escalation.pressMargin" || ds[0].Policy[0].Value != "15m" {
		t.Fatalf("decision %+v", ds)
	}

	// A job's own margin, longer than the farm's, presses at once.
	ns, q, now = graphJobWith(t, "early", map[string]string{"escalation.pressMargin": "3h", "escalation.pressAfter": "9h"}, func(time.Time) string {
		return `{"name": "cut", "work": 3600}, {"name": "deliver", "after": ["cut"], "work": 3600}`
	}, 3*time.Hour)
	failNeeding(t, podOfUnit(t, ns, "early", 1), *now, "the delivery frame rate", false)
	tick(t, q)
	got = get(t, ns, "early")
	if ds := decisionsOf(got, "needs-pressing"); len(ds) != 1 || ds[0].Policy[0].Value != "3h" || ds[0].Policy[0].From.Scope != "job" ||
		ds[0].Policy[0].From.Ref != ns+"/early" {
		t.Fatalf("job margin: %+v", ds)
	}
}

// A model unit's question is re-checked when work it is about lands: an
// extra attempt marked as a re-check, told what it asked and why it runs.
// Stopping for information again keeps the question, with its new text.
// Re-checks are spaced by minInterval and capped by recheck.max, after
// which it waits for a person; the refusal names the value. A tool unit
// never re-checks.
func TestNeedsRecheck(t *testing.T) {
	ns, q, now := graphJobWith(t, "rechecked", map[string]string{"escalation.recheck.max": "2"}, func(time.Time) string {
		return `{"name": "cut", "reasoning": {"does": "model"}}, {"name": "grade"}, {"name": "dub"}, {"name": "tool"},
		{"name": "deliver", "after": ["cut", "grade", "tool"]}`
	}, 72*time.Hour)
	// units: 1 cut (a model), 2 grade, 3 dub, 4 tool, 5 deliver
	failNeeding(t, podOfUnit(t, ns, "rechecked", 1), *now, "the delivery frame rate\nwhy: the brief gives 24 and 25\nabout: unit:grade, job:plates", false)
	failNeeding(t, podOfUnit(t, ns, "rechecked", 4), *now, "the codec\nabout: unit:grade", false)
	tick(t, q)
	got := get(t, ns, "rechecked")
	n := got.Status.Needs["1"]
	if n.Text != "the delivery frame rate" || n.Why != "the brief gives 24 and 25" || strings.Join(n.About, ",") != "unit:grade,job:plates" ||
		n.Rechecks == nil || n.Rechecks.Done != 0 || n.Rechecks.Max != 2 {
		t.Fatalf("raised: %+v", n)
	}
	if got.Status.Needs["4"].Rechecks != nil {
		t.Fatalf("a tool unit can re-check: %+v", got.Status.Needs["4"])
	}

	*now = now.Add(time.Minute)
	finishUnit(t, now, ns, "rechecked", 2, nil)
	tick(t, q)
	tick(t, q)
	p := podOfUnit(t, ns, "rechecked", 1)
	c := p.Spec.Containers[0]
	if p.Annotations[c4v1.AnnotationRecheck] != "grade finished" || envOf(c, "C4_RECHECK") != "1" || envOf(c, "C4_NEEDED") != "the delivery frame rate" ||
		envOf(c, "C4_RECHECK_WHY") != "grade finished" || envOf(c, "C4_RECHECK_SECONDS") != "300" ||
		envOf(c, "C4_NEEDED_WHY") != "the brief gives 24 and 25" || envOf(c, "C4_NEEDED_ABOUT") != "unit:grade, job:plates" {
		t.Fatalf("re-check pod: %v %v", p.Annotations, c.Env)
	}
	if hasPod(t, ns, "rechecked", 4) {
		t.Fatal("the tool unit re-checks")
	}
	got = get(t, ns, "rechecked")
	if r := got.Status.Needs["1"].Rechecks; r.Done != 1 || !r.InFlight || r.LastWhy != "grade finished" || len(decisionsOf(got, "needs-recheck")) != 1 {
		t.Fatalf("rechecks %+v", r)
	}
	if got.Status.Needs["1"].State != needsRaised {
		t.Fatalf("state %s", got.Status.Needs["1"].State)
	}

	// Still needed, said better.
	endRecheckPod(t, p, *now, c4v1.NeedsExitCode, "the delivery frame rate, 24 or 25\nwhy: grade did not settle it")
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "rechecked")
	n = got.Status.Needs["1"]
	if n.Text != "the delivery frame rate, 24 or 25" || n.Why != "grade did not settle it" || n.State != needsRaised || n.Rechecks.InFlight ||
		hasPod(t, ns, "rechecked", 1) || !containsInt(states(t, got, "needs"), 1) {
		t.Fatalf("after the re-check: %+v, states %v", n, got.Status.UnitStates)
	}

	// Another trigger within minInterval waits for it.
	*now = now.Add(time.Minute)
	requeueAndFinish(t, q, now, ns, "rechecked", 2)
	if hasPod(t, ns, "rechecked", 1) {
		t.Fatal("re-checked again within the interval")
	}
	*now = now.Add(10 * time.Minute)
	tick(t, q)
	tick(t, q)
	p = podOfUnit(t, ns, "rechecked", 1)
	if p.Annotations[c4v1.AnnotationRecheck] != "grade finished" {
		t.Fatalf("second re-check: %v", p.Annotations)
	}
	endRecheckPod(t, p, *now, 1, "")
	tick(t, q)
	tick(t, q)
	if n := get(t, ns, "rechecked").Status.Needs["1"]; n.State != needsRaised || n.Rechecks.Done != 2 {
		t.Fatalf("a re-check that failed: %+v", n)
	}

	// The cap: a third trigger is refused, once, naming the value.
	*now = now.Add(20 * time.Minute)
	requeueAndFinish(t, q, now, ns, "rechecked", 2)
	tick(t, q)
	got = get(t, ns, "rechecked")
	ds := decisionsOf(got, "needs-recheck-refused")
	if hasPod(t, ns, "rechecked", 1) || len(ds) != 1 || len(ds[0].Policy) != 1 || ds[0].Policy[0].Key != "escalation.recheck.max" ||
		ds[0].Policy[0].Value != "2" || ds[0].Policy[0].From.Scope != "job" {
		t.Fatalf("the cap: pod %v, decisions %+v", hasPod(t, ns, "rechecked", 1), ds)
	}
	if r := got.Status.Needs["1"].Rechecks; r.Done != r.Max {
		t.Fatalf("rechecks %+v", r)
	}
}

// requeueAndFinish runs unit idx again and finishes it with a new output.
func requeueAndFinish(t *testing.T, q *Queue, now *time.Time, ns, name string, idx int) {
	t.Helper()
	j := get(t, ns, name)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationRequeue] = strconv.FormatInt(now.UnixNano(), 36) + ":" + strconv.Itoa(idx)
	if err := k8s.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	finishUnit(t, now, ns, name, idx, nil)
	tick(t, q)
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// A re-check that exits 0 withdraws the question, by the block, with its
// message as the why: the unit runs again, told it was withdrawn and why,
// and nobody is asked any more. The lead may withdraw one too.
func TestNeedsWithdrawn(t *testing.T) {
	ns, q, now := graphJob(t, "mooted", func(time.Time) string {
		return `{"name": "cut", "reasoning": {"does": "model"}}, {"name": "grade"}, {"name": "dub"}, {"name": "deliver", "after": ["cut", "grade", "dub"]}`
	}, 72*time.Hour)
	failNeeding(t, podOfUnit(t, ns, "mooted", 1), *now, "the delivery frame rate\nabout: unit:grade", false)
	failNeeding(t, podOfUnit(t, ns, "mooted", 3), *now, "the dub's language", false)
	tick(t, q)
	*now = now.Add(time.Minute)
	finishUnit(t, now, ns, "mooted", 2, nil)
	tick(t, q)
	tick(t, q)
	endRecheckPod(t, podOfUnit(t, ns, "mooted", 1), *now, 0, "withdrawn: grade settled it: 24 fps")
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "mooted")
	n := got.Status.Needs["1"]
	if n.State != needsWithdrawn || n.Withdrawn == nil || n.Withdrawn.By != "unit:"+ns+"/mooted/cut" || n.Withdrawn.Why != "grade settled it: 24 fps" {
		t.Fatalf("withdrawn: %+v %+v", n, n.Withdrawn)
	}
	if len(decisionsOf(got, "needs-withdrawn")) != 1 {
		t.Fatalf("decisions %+v", got.Status.Decisions)
	}
	p := podOfUnit(t, ns, "mooted", 1)
	if p.Annotations[c4v1.AnnotationRecheck] != "" || envOf(p.Spec.Containers[0], "C4_WITHDRAWN") != "grade settled it: 24 fps" ||
		envOf(p.Spec.Containers[0], "C4_NEEDED") != "the delivery frame rate" || envOf(p.Spec.Containers[0], "C4_RECHECK") != "" {
		t.Fatalf("the next attempt: %v %v", p.Annotations, p.Spec.Containers[0].Env)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson); c == nil || c.Message != "needs information: the dub's language" {
		t.Fatalf("needs person %+v", c)
	}

	// The lead withdraws the dub's.
	*now = now.Add(time.Minute)
	withdrawUnit(t, ns, "mooted", "dub", "the dub is cut from this delivery", *now)
	tick(t, q)
	got = get(t, ns, "mooted")
	if n := got.Status.Needs["3"]; n.State != needsWithdrawn || n.Withdrawn.By != "lead" || got.Status.Withdrawn == "" {
		t.Fatalf("the lead's withdrawal: %+v", n)
	}
	if p := podOfUnit(t, ns, "mooted", 3); envOf(p.Spec.Containers[0], "C4_WITHDRAWN") != "the dub is cut from this delivery" {
		t.Fatalf("dub's next attempt: %v", p.Spec.Containers[0].Env)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson); c != nil {
		t.Fatalf("still needs a person: %+v", c)
	}
	// Withdrawn is final: a supply after it changes nothing.
	supplyUnit(t, ns, "mooted", "dub", "French", now.Add(time.Second))
	tick(t, q)
	if n := get(t, ns, "mooted").Status.Needs["3"]; n.Supplied != "" || n.State != needsWithdrawn {
		t.Fatalf("supplied after withdrawn: %+v", n)
	}
}

// A job's answerer is asked a unit's question when the unit raises it;
// its answer supplies it, by the answering unit.
func TestNeedsAnswerer(t *testing.T) {
	ns, q, now := graphJobWith(t, "led", map[string]string{"answerer": "lead"}, func(time.Time) string {
		return `{"name": "lead"}, {"name": "worker"}`
	}, 72*time.Hour)
	failNeeding(t, podOfUnit(t, ns, "led", 2), *now, "which module owns the tokenizer", false)
	tick(t, q)
	tick(t, q)
	var id string
	for _, a := range get(t, ns, "led").Status.Asks {
		if a.Needs && a.From == 2 && a.To == 1 {
			id = a.ID
		}
	}
	if id == "" || !steerAsk(podSteers(t, ns, "led", 1), id) {
		t.Fatalf("the answerer was not asked: %+v", get(t, ns, "led").Status.Asks)
	}
	notePod(t, ns, "led", 1, c4v1.AnnotationAnswers, `[{"seq": 1, "ask": "`+id+`", "answer": "tokenize/"}]`)
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "led")
	n := got.Status.Needs["2"]
	if n.State != needsSupplied || n.Supplied != "tokenize/" || n.By != "unit:"+ns+"/led/lead" || n.SuppliedAt == nil {
		t.Fatalf("supplied: %+v", n)
	}
	if p := podOfUnit(t, ns, "led", 2); envOf(p.Spec.Containers[0], "C4_SUPPLIED") != "tokenize/" {
		t.Fatalf("worker's next attempt: %v", p.Spec.Containers[0].Env)
	}
	if len(decisionsOf(got, "needs-supplied")) != 1 {
		t.Fatalf("decisions %+v", got.Status.Decisions)
	}
	_ = now
}

// The triggers, one by one: a unit the asker waits on or reads succeeds,
// a unit it is about changes its output, the unit table grows, a path it
// is about is written, a job it is about finishes. Nothing fires on the
// first look.
func TestRecheckTriggers(t *testing.T) {
	plan := &class.Plan{Units: []class.Unit{{Name: "root"}, {Name: "cut", After: []int{0}}, {Name: "grade"}, {Name: "edit"}}}
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show", Name: "j"}, Spec: c4v1.WorkJobSpec{
		Inputs: []c4v1.WorkInput{{FromJob: "plates"}}}}
	m := newMem()
	m.plan = plan
	v := &view{job: j, key: "show/j", mem: m}
	n := &needsRec{about: []string{"unit:grade", "path:schema/", "job:other/conform"}}
	q := &Queue{}
	phases := map[string]string{"show/plates": "Running", "other/conform": "Running"}
	fire := func() string { return q.triggered(v, 1, n, phases) }
	if why := fire(); why != "" {
		t.Fatalf("the first look fired: %s", why)
	}
	if why := fire(); why != "" {
		t.Fatalf("nothing changed and it fired: %s", why)
	}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	end := func(u int, out string) {
		at = at.Add(time.Minute)
		m.succeeded[u], m.unitOut[u], m.ended[u] = true, out, at
	}
	end(0, "v1")
	if why := fire(); why != "root finished" {
		t.Fatalf("a unit it waits on: %q", why)
	}
	end(2, "g1")
	if why := fire(); why != "grade finished" {
		t.Fatalf("a unit it is about: %q", why)
	}
	end(2, "g2")
	if why := fire(); why != "grade's output changed" {
		t.Fatalf("its output: %q", why)
	}
	end(2, "g2")
	if why := fire(); why != "grade ran again" {
		t.Fatalf("an identical re-run: %q", why)
	}
	// After a restart the queue knows grade succeeded, not when ("ok" in
	// what status kept): nothing fires until it runs again, and then it does.
	n.base.outs[2] = "ok"
	delete(m.ended, 2)
	if why := fire(); why != "" {
		t.Fatalf("after a restart: %q", why)
	}
	end(2, "g2")
	if why := fire(); why != "grade ran again" {
		t.Fatalf("a re-run after a restart: %q", why)
	}
	plan.Units = append(plan.Units, class.Unit{Name: "more"})
	if why := fire(); why != "the job's unit table grew from 4 to 5 units" {
		t.Fatalf("an expansion: %q", why)
	}
	m.out.fresh = []freshPath{{unit: 1, path: "schema/own.json"}, {unit: 3, path: "notes/x"}}
	if why := fire(); why != "" {
		t.Fatalf("its own write or another path fired: %q", why)
	}
	m.out.fresh = []freshPath{{unit: 3, path: "schema/v2.json"}}
	if why := fire(); why != "edit wrote schema/v2.json" {
		t.Fatalf("a path: %q", why)
	}
	m.out.fresh = nil
	phases["other/conform"] = "Succeeded"
	if why := fire(); why != "job other/conform finished (Succeeded)" {
		t.Fatalf("a job it is about: %q", why)
	}
	phases["show/plates"] = "Failed"
	if why := fire(); why != "job show/plates finished (Failed)" {
		t.Fatalf("a job it reads from: %q", why)
	}
}

// answerBy counts the unit's own work and the longest chain after it.
func TestDownstream(t *testing.T) {
	plan := &class.Plan{Units: []class.Unit{{Name: "a", Key: "k"}, {Name: "b", Key: "k", After: []int{0}}, {Name: "c", Key: "k", After: []int{1}},
		{Name: "d", Key: "k", After: []int{0}}}}
	m := newMem()
	m.plan = plan
	e := estimate.New(estimate.Config{})
	e.SetPrior("k", estimate.Prior{Mean: 100, SD: 10})
	v := &view{mem: m, est: e}
	if d := v.downstream(0); d < 299 || d > 301 {
		t.Fatalf("downstream %v, want 300 (a, b, c)", d)
	}
	m.succeeded[1] = true
	if d := v.downstream(0); d < 199 || d > 201 {
		t.Fatalf("downstream %v, want 200 (a, d)", d)
	}
}

// The book has each step of a question: raised, pressing, supplied or
// withdrawn, with who; a decision a policy value made names it.
func TestNeedsBooked(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	later := metav1.NewTime(at.Add(2 * time.Hour))
	before := &c4v1.WorkJobStatus{Needs: map[string]c4v1.WorkNeeds{"4": {Text: "which take?", At: at, State: needsRaised}}}
	after := before.DeepCopy()
	n := after.Needs["4"]
	n.State, n.PressingAt, n.PressingWhy, n.PressingRecord = needsPressing, &later, "unanswered for 2h", "c4p"
	after.Needs["4"] = n
	after.Decisions = []c4v1.WorkDecision{{Unit: 4, At: later, Kind: "needs-pressing", Reason: "unanswered for 2h", Record: "c4p",
		Policy: []c4v1.WorkPolicyUse{{Key: "escalation.pressAfter", Value: "2h", From: c4v1.WorkPolicyFrom{Scope: "farm"}}}}}
	ts := transitions("show-a/sh040", "show-a", before, after, later.Time)
	if len(ts) != 2 || ts[0].Kind != "decision" || ts[1].Kind != "pressing" || ts[1].Message != "pressing: unanswered for 2h" || ts[1].Record != "c4p" {
		t.Fatalf("pressing: %+v", ts)
	}
	if p := ts[0].Policy; len(p) != 1 || p[0].Key != "escalation.pressAfter" || p[0].Value != "2h" || p[0].From.Scope != "farm" {
		t.Fatalf("the decision's policy: %+v", ts[0])
	}
	done := after.DeepCopy()
	n = done.Needs["4"]
	n.State, n.Withdrawn = needsWithdrawn, &c4v1.WorkWithdrawal{By: "unit:show-a/sh040/cut", Why: "moot", At: later, Record: "c4w"}
	done.Needs["4"] = n
	ts = transitions("show-a/sh040", "show-a", after, done, later.Time)
	if len(ts) != 1 || ts[0].Kind != "needs-withdrawn" || ts[0].Actor != "unit:show-a/sh040/cut" || ts[0].Record != "c4w" || ts[0].Message != "withdrawn by unit:show-a/sh040/cut: moot" {
		t.Fatalf("withdrawn: %+v", ts)
	}
}

func newQueueFrom(q *Queue) *Queue {
	return &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
}

// An open question holds the job, never fails it: its unit is in state
// needs, not failed, the job's phase is Held with NeedsPerson
// NeedsInformation, and a job after it waits rather than gives up, across
// a restart. Once the question is withdrawn and the unit then fails for
// real, the job fails and the job after it is stuck.
func TestNeedsHoldsTheJob(t *testing.T) {
	ns, q, now := graphJob(t, "held", func(time.Time) string {
		return `{"name": "cut"}, {"name": "deliver", "after": ["cut"]}`
	}, 72*time.Hour)
	ctx := context.Background()
	next := job(ns, "next", "graph", "cpu-"+ns, nil, now.Add(72*time.Hour))
	next.Spec.DependsOn = []string{"held"}
	if err := k8s.Create(ctx, next); err != nil {
		t.Fatal(err)
	}
	failNeeding(t, podOfUnit(t, ns, "held", 1), *now, "the delivery frame rate", false)
	tick(t, q)
	tick(t, q)
	check := func(when string) {
		t.Helper()
		got := get(t, ns, "held")
		c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson)
		if got.Status.Phase != "Held" || !containsInt(states(t, got, "needs"), 1) || len(states(t, got, "failed")) != 0 ||
			got.Status.Units.Needs != 1 || got.Status.Units.Failed != 0 || got.Status.Finished != nil || c == nil || c.Reason != c4v1.NeedsInformation {
			t.Fatalf("%s: phase %s, states %v, units %+v, needs person %+v", when, got.Status.Phase, got.Status.UnitStates, got.Status.Units, c)
		}
		n := get(t, ns, "next")
		if n.Status.Phase != "Blocked" {
			t.Fatalf("%s: the job after it is %s", when, n.Status.Phase)
		}
		if c := meta.FindStatusCondition(n.Status.Conditions, c4v1.ConditionNeedsPerson); c != nil {
			t.Fatalf("%s: the job after it needs a person: %+v", when, c)
		}
	}
	check("raised")
	q = newQueueFrom(q)
	tick(t, q)
	check("after a restart")

	*now = now.Add(time.Minute)
	withdrawUnit(t, ns, "held", "cut", "the delivery spec is out of scope", *now)
	tick(t, q)
	for k := 0; k < 4 && hasPod(t, ns, "held", 1); k++ {
		*now = now.Add(time.Minute)
		finish(t, podOfUnit(t, ns, "held", 1), corev1.PodFailed, *now, time.Minute, nil)
		tick(t, q)
		tick(t, q)
	}
	got := get(t, ns, "held")
	if got.Status.Phase != "Failed" || !containsInt(states(t, got, "failed"), 1) || got.Status.Units.Needs != 0 {
		t.Fatalf("failed for real: phase %s, states %v", got.Status.Phase, got.Status.UnitStates)
	}
	*now = now.Add(time.Minute) // a change of conditions alone is written at most every statusEvery
	tick(t, q)
	if c := meta.FindStatusCondition(get(t, ns, "next").Status.Conditions, c4v1.ConditionNeedsPerson); c == nil || c.Reason != c4v1.NeedsStuck {
		t.Fatalf("the job after it: %+v", c)
	}
}

// Only an explicit withdrawal withdraws: a re-check that exits 0 without
// one (a block that ignored $C4_RECHECK and ran its task) keeps the
// question. The re-check's turn and its pod's deadline are
// escalation.recheck.seconds; a host runner withdraws by reason.
func TestRecheckNeedsAnExplicitWithdraw(t *testing.T) {
	ns, q, now := graphJobWith(t, "explicit", map[string]string{"escalation.recheck.seconds": "120", "escalation.recheck.minInterval": "10m"}, func(time.Time) string {
		return `{"name": "cut", "reasoning": {"does": "model"}}, {"name": "grade"}, {"name": "dub"}, {"name": "deliver", "after": ["cut", "grade", "dub"]}`
	}, 72*time.Hour)
	failNeeding(t, podOfUnit(t, ns, "explicit", 1), *now, "the delivery frame rate\nabout: unit:grade, unit:dub", false)
	tick(t, q)
	// A restart between the question and the work it is about loses no
	// event: what the triggers saw is in status.
	q = newQueueFrom(q)
	tick(t, q)
	*now = now.Add(time.Minute)
	finishUnit(t, now, ns, "explicit", 2, nil)
	tick(t, q)
	tick(t, q)
	p := podOfUnit(t, ns, "explicit", 1)
	if p.Annotations[c4v1.AnnotationRecheck] != "grade finished" || envOf(p.Spec.Containers[0], "C4_RECHECK_SECONDS") != "120" ||
		p.Spec.ActiveDeadlineSeconds == nil || *p.Spec.ActiveDeadlineSeconds != 120+recheckGrace {
		t.Fatalf("re-check pod: %v, deadline %v", p.Annotations, p.Spec.ActiveDeadlineSeconds)
	}
	endRecheckPod(t, p, *now, 0, "rendered 48 frames")
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "explicit")
	if n := got.Status.Needs["1"]; n.State != needsRaised || n.Withdrawn != nil || hasPod(t, ns, "explicit", 1) || len(decisionsOf(got, "needs-withdrawn")) != 0 {
		t.Fatalf("exit 0 without a withdrawal: %+v", n)
	}

	// A host runner says it by the pod's reason.
	*now = now.Add(11 * time.Minute)
	finishUnit(t, now, ns, "explicit", 3, nil)
	tick(t, q)
	tick(t, q)
	p = podOfUnit(t, ns, "explicit", 1)
	if p.Annotations[c4v1.AnnotationRecheck] != "dub finished" {
		t.Fatalf("second re-check: %v", p.Annotations)
	}
	p.Status.Phase, p.Status.Reason, p.Status.Message = corev1.PodSucceeded, c4v1.Withdrawn, "dub settled it: 25 fps"
	p.Status.StartTime = &metav1.Time{Time: now.Add(-time.Minute)}
	if err := k8s.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	tick(t, q)
	if n := get(t, ns, "explicit").Status.Needs["1"]; n.State != needsWithdrawn || n.Withdrawn.Why != "dub settled it: 25 fps" {
		t.Fatalf("a host runner's withdrawal: %+v", n)
	}
}

// A unit whose retries are spent fails the job even beside an open
// question; the question stays listed, and its unit in state needs.
func TestNeedsBesideARealFailure(t *testing.T) {
	ns, q, now := graphJob(t, "both", func(time.Time) string {
		return `{"name": "cut"}, {"name": "grade"}, {"name": "deliver", "after": ["cut", "grade"]}`
	}, 72*time.Hour)
	failNeeding(t, podOfUnit(t, ns, "both", 1), *now, "the delivery frame rate", false)
	tick(t, q)
	for k := 0; k < 4 && hasPod(t, ns, "both", 2); k++ {
		*now = now.Add(time.Minute)
		finish(t, podOfUnit(t, ns, "both", 2), corev1.PodFailed, *now, time.Minute, nil)
		tick(t, q)
		tick(t, q)
	}
	got := get(t, ns, "both")
	if got.Status.Phase != "Failed" || !containsInt(states(t, got, "failed"), 2) || !containsInt(states(t, got, "needs"), 1) ||
		got.Status.Needs["1"].State != needsRaised {
		t.Fatalf("phase %s, states %v, needs %+v", got.Status.Phase, got.Status.UnitStates, got.Status.Needs)
	}
	// The failure is the reason a person is needed, its remedy requeue or
	// accept; the question is mentioned beside it.
	c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson)
	if c == nil || c.Reason != c4v1.NeedsFailed || c.Message != "1 unit failed after its retries; requeue, take or accept it; a unit waits for information too" {
		t.Fatalf("needs person %+v", c)
	}
	// An answer never reopens the finished job.
	*now = now.Add(time.Minute)
	supplyUnit(t, ns, "both", "cut", "24 fps", *now)
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "both")
	if n := got.Status.Needs["1"]; n.State != needsRaised || n.Supplied != "" || hasPod(t, ns, "both", 1) || got.Status.Phase != "Failed" {
		t.Fatalf("answered a finished job: %+v, phase %s, pod %v", n, got.Status.Phase, hasPod(t, ns, "both", 1))
	}
}
