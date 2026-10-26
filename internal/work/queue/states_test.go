package queue

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// run plays kubelet starting a pod's work.
func run(t *testing.T, p *corev1.Pod, at time.Time) {
	t.Helper()
	p.Status.Phase = corev1.PodRunning
	p.Status.StartTime = &metav1.Time{Time: at}
	if err := k8s.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func annotate(t *testing.T, j *c4v1.WorkJob, k, v string) {
	t.Helper()
	patch := client.MergeFrom(j.DeepCopy())
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[k] = v
	if err := k8s.Patch(context.Background(), j, patch); err != nil {
		t.Fatal(err)
	}
}

func ackReq(t *testing.T, at time.Time, reqs ...c4v1.WorkAckRequest) string {
	t.Helper()
	for i := range reqs {
		reqs[i].Token = strconv.FormatInt(at.UnixNano()+int64(i), 36)
		reqs[i].At = metav1.Time{Time: at}
	}
	b, err := json.Marshal(reqs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A unit's life as a person sees it: preflight, running, backoff,
// retrying, failed (needs a person), taken, accepted, requeued.
func TestUnitStatesThroughFailureAndAcks(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 1)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "sh020", "render", "cpu-"+ns, map[string]string{"frames": "1-2", "estimateSeconds": "60"}, now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	states := func() c4v1.WorkJobStatus { t.Helper(); return get(t, ns, "sh020").Status }

	tick(t, q)
	tick(t, q)
	pods := unitPods(t, ns, "sh020")
	if len(pods) != 1 {
		t.Fatalf("%d pods in one slot", len(pods))
	}
	u := strconv.Itoa(unitOf(pods[0]))
	if st := states(); st.UnitStates["preflight"] != u || st.Units.Preflight != 1 || st.Units.Running != 0 {
		t.Fatalf("a pod not yet placed is in preflight: %v %+v", st.UnitStates, st.Units)
	}
	run(t, &pods[0], *now)
	tick(t, q)
	if st := states(); st.UnitStates["running"] != u || st.Units.Running != 1 {
		t.Fatalf("a running pod: %v %+v", st.UnitStates, st.Units)
	}

	// The first attempt fails: the unit waits for its retry (backoff), and
	// its retry runs as retrying.
	*now = now.Add(time.Minute)
	finish(t, &pods[0], corev1.PodFailed, *now, 30*time.Second, nil)
	tick(t, q)
	st := states()
	if st.UnitStates["backoff"] != u && st.UnitStates["preflight"] != u {
		t.Fatalf("after a failed attempt: %v", st.UnitStates)
	}
	tick(t, q)
	pods = unitPods(t, ns, "sh020")
	if len(pods) != 1 || pods[0].Annotations[c4v1.AnnotationAttempt] != "2" {
		t.Fatalf("the retry was not released: %v", unitsOf(pods))
	}
	run(t, &pods[0], *now)
	tick(t, q)
	if st := states(); st.UnitStates["retrying"] != u || st.Units.Retrying != 1 {
		t.Fatalf("a running retry: %v %+v", st.UnitStates, st.Units)
	}

	// The retry fails too: the unit needs a person.
	*now = now.Add(time.Minute)
	finish(t, &pods[0], corev1.PodFailed, *now, 30*time.Second, nil)
	tick(t, q)
	tick(t, q)
	st = states()
	c := meta.FindStatusCondition(st.Conditions, c4v1.ConditionNeedsPerson)
	if st.UnitStates["failed"] != u || c == nil || c.Reason != c4v1.NeedsFailed {
		t.Fatalf("a failed unit needs a person: %v %+v", st.UnitStates, c)
	}

	// Ana takes it: still failed, no longer waiting on anyone.
	j := get(t, ns, "sh020")
	annotate(t, j, c4v1.AnnotationAcks, ackReq(t, *now, c4v1.WorkAckRequest{Kind: c4v1.AckAcknowledge, Units: u, Actor: "ana"}))
	tick(t, q)
	st = states()
	if st.UnitStates["failed"] != u || len(st.Acks) != 1 || st.Acks[0].Actor != "ana" || st.Acks[0].Units != u ||
		meta.FindStatusCondition(st.Conditions, c4v1.ConditionNeedsPerson) != nil {
		t.Fatalf("taken: %v %+v %v", st.UnitStates, st.Acks, st.Conditions)
	}
	// The same request again changes nothing.
	tick(t, q)
	if st2 := states(); len(st2.Acks) != 1 || st2.Acked != st.Acked {
		t.Fatalf("a request was carried out twice: %+v", st2.Acks)
	}

	// Ben accepts it: the unit is accepted, recorded with who and when.
	*now = now.Add(time.Minute)
	j = get(t, ns, "sh020")
	annotate(t, j, c4v1.AnnotationAcks, ackReq(t, *now, c4v1.WorkAckRequest{Kind: c4v1.AckAccept, Units: u, Actor: "ben", Note: "bad frame is fine"}))
	tick(t, q)
	st = states()
	if st.UnitStates["accepted"] != u || st.UnitStates["failed"] != "" || st.Units.Accepted != 1 || st.Units.Failed != 0 {
		t.Fatalf("accepted: %v %+v", st.UnitStates, st.Units)
	}
	if len(st.Acks) != 1 || st.Acks[0].Kind != c4v1.AckAccept || st.Acks[0].Actor != "ben" || !st.Acks[0].At.Time.Equal(now.Truncate(time.Second)) {
		t.Fatalf("the acceptance records who and when: %+v", st.Acks)
	}

	// A restarted queue keeps it.
	q2 := &Queue{Client: k8s, Policy: sched.Decide, Cfg: sched.DefaultConfig(), Log: q.Log, Now: q.Now, CaptureWait: time.Minute}
	tick(t, q2)
	if st := states(); st.UnitStates["accepted"] != u || len(st.Acks) != 1 {
		t.Fatalf("after a restart: %v %+v", st.UnitStates, st.Acks)
	}

	// A requeue clears it.
	j = get(t, ns, "sh020")
	annotate(t, j, c4v1.AnnotationRequeue, "r1:"+u)
	tick(t, q2)
	if st := states(); st.UnitStates["accepted"] != "" || len(st.Acks) != 0 {
		t.Fatalf("after a requeue: %v %+v", st.UnitStates, st.Acks)
	}
}

// A finished unit whose output is still being captured is egress, not
// running and not yet succeeded.
func TestFinishedUnitAwaitingCaptureIsEgress(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 1)
	ctx := context.Background()
	j := job(ns, "sh030", "render", "cpu-"+ns, map[string]string{"frames": "1-2", "estimateSeconds": "60"}, now.Add(time.Hour))
	j.Spec.Inputs = []c4v1.WorkInput{{Materialization: "out", MountPath: "/out"}}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "sh030")
	*now = now.Add(time.Minute)
	finish(t, &pods[0], corev1.PodSucceeded, *now, 50*time.Second, nil)
	tick(t, q)
	u := strconv.Itoa(unitOf(pods[0]))
	st := get(t, ns, "sh030").Status
	if st.UnitStates["egress"] != u || st.Units.Egress != 1 || st.Units.Succeeded != 0 {
		t.Fatalf("awaiting capture: %v %+v", st.UnitStates, st.Units)
	}
	var p corev1.Pod
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(&pods[0]), &p); err != nil {
		t.Fatal(err)
	}
	if e := EgressOf(&p); e == nil || e.Sealed {
		t.Fatalf("egress of an uncaptured pod: %+v", e)
	}
	patch := client.MergeFrom(p.DeepCopy())
	p.Annotations[c4v1.AnnotationOutputTree] = "c45xZeXwMSpqXjpDumcHMA6mhoAmGHkUo7r9WmN2UgSEQzj9KjgseaQdkEJ11fGb5S1bNtipx6VnLY9Ux8i8"
	if err := k8s.Patch(ctx, &p, patch); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if st := get(t, ns, "sh030").Status; st.UnitStates["succeeded"] != u || st.Units.Egress != 0 {
		t.Fatalf("captured: %v %+v", st.UnitStates, st.Units)
	}
}

func TestRisk(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	d := now.Add(4 * time.Hour)
	at := func(h float64) *metav1.Time {
		return &metav1.Time{Time: now.Add(time.Duration(h * float64(time.Hour)))}
	}
	for _, c := range []struct {
		name   string
		dl     time.Time
		source string
		js     sched.JobState
		e      c4v1.WorkEstimate
		want   string
	}{
		{"no deadline", time.Time{}, "spec", sched.JobState{Feasible: true}, c4v1.WorkEstimate{ETA50: at(9)}, c4v1.RiskNone},
		{"comfortable", d, "spec", sched.JobState{Feasible: true}, c4v1.WorkEstimate{ETA50: at(1), ETA90: at(2), ETA95: at(3)}, c4v1.RiskNone},
		{"p90 late", d, "spec", sched.JobState{Feasible: true}, c4v1.WorkEstimate{ETA50: at(3), ETA90: at(5), ETA95: at(6)}, c4v1.RiskAt},
		{"p95 late, spec deadline", d, "spec", sched.JobState{Feasible: true}, c4v1.WorkEstimate{ETA50: at(2), ETA90: at(3), ETA95: at(5)}, c4v1.RiskNone},
		{"p95 late, suggested", d, "suggested", sched.JobState{Feasible: true}, c4v1.WorkEstimate{ETA50: at(2), ETA90: at(3), ETA95: at(5)}, c4v1.RiskAt},
		{"scheduler at risk", d, "spec", sched.JobState{Feasible: true, AtRisk: true}, c4v1.WorkEstimate{ETA50: at(1), ETA90: at(2)}, c4v1.RiskAt},
		{"infeasible", d, "spec", sched.JobState{Feasible: false}, c4v1.WorkEstimate{ETA50: at(1), ETA90: at(2)}, c4v1.RiskWillMiss},
		{"median late", d, "spec", sched.JobState{Feasible: true}, c4v1.WorkEstimate{ETA50: at(5), ETA90: at(6)}, c4v1.RiskWillMiss},
		{"passed", now.Add(-time.Minute), "spec", sched.JobState{Feasible: true}, c4v1.WorkEstimate{ETA50: at(1)}, c4v1.RiskMissed},
	} {
		v := &view{source: c.source}
		v.obj.Deadline = c.dl
		if got := risk(now, v, c.js, &c.e); got != c.want {
			t.Errorf("%s: risk %s, want %s", c.name, got, c.want)
		}
	}
}

func TestPreflightOf(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
	if pf := PreflightOf(p); pf == nil || pf.Step != "place" {
		t.Fatalf("unplaced: %+v", pf)
	}
	p.Spec.NodeName = "n1"
	p.Spec.InitContainers = []corev1.Container{{Name: "stage"}}
	if pf := PreflightOf(p); pf == nil || pf.Step != "stage" {
		t.Fatalf("init pending: %+v", pf)
	}
	p.Annotations[c4v1.AnnotationPreflight] = `{"step":"fetch","bytes":10,"of":40}`
	if pf := PreflightOf(p); pf == nil || pf.Step != "fetch" || pf.Of != 40 {
		t.Fatalf("reported: %+v", pf)
	}
	p.Status.Phase = corev1.PodRunning
	p.Annotations[c4v1.AnnotationPreflight] = `{"step":"license"}`
	if pf := PreflightOf(p); pf == nil || pf.Step != "license" {
		t.Fatalf("license checkout while running: %+v", pf)
	}
	p.Annotations[c4v1.AnnotationPreflight] = `{"step":"done"}`
	if pf := PreflightOf(p); pf != nil {
		t.Fatalf("done: %+v", pf)
	}
}

func TestShownSlack(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	d := now.Add(4 * time.Hour)
	at := func(h float64) *metav1.Time {
		return &metav1.Time{Time: now.Add(time.Duration(h * float64(time.Hour)))}
	}
	for _, c := range []struct {
		name   string
		dl     time.Time
		source string
		e      c4v1.WorkEstimate
		want   int64
		ok     bool
	}{
		{"no deadline", time.Time{}, "spec", c4v1.WorkEstimate{ETA90: at(2)}, 0, false},
		{"no ETA", d, "spec", c4v1.WorkEstimate{}, 0, false},
		{"early, against p90", d, "spec", c4v1.WorkEstimate{ETA50: at(1), ETA90: at(3), ETA95: at(5)}, 3600, true},
		{"late, against p90", d, "spec", c4v1.WorkEstimate{ETA50: at(3), ETA90: at(41)}, -37 * 3600, true},
		{"suggested, against p95", d, "suggested", c4v1.WorkEstimate{ETA90: at(3), ETA95: at(5)}, -3600, true},
	} {
		v := &view{source: c.source}
		v.obj.Deadline = c.dl
		got, ok := shownSlack(v, sched.JobState{}, &c.e)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: slack %d %v, want %d %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestShownSlackUsesThePlannedDeadline(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	eta := &metav1.Time{Time: now.Add(3 * time.Hour)}
	// an upstream job with no deadline of its own, planned for its
	// dependent's: slack and risk against that one
	v := &view{source: "spec"}
	js := sched.JobState{Feasible: true, Deadline: now.Add(2 * time.Hour), DeadlineFor: "comp"}
	e := &c4v1.WorkEstimate{ETA50: eta, ETA90: eta}
	if got, ok := shownSlack(v, js, e); !ok || got != -3600 {
		t.Fatalf("slack %d %v, want -3600 against the planned deadline", got, ok)
	}
	if r := risk(now, v, js, e); r != c4v1.RiskWillMiss {
		t.Fatalf("risk %s, want willMiss against the planned deadline", r)
	}
	// the job's own deadline when sched planned none
	v.obj.Deadline = now.Add(5 * time.Hour)
	if got, _ := shownSlack(v, sched.JobState{}, e); got != 2*3600 {
		t.Fatalf("slack %d, want 7200 against the job's own deadline", got)
	}
}

// A tool's test that fails says to fix the tool and publish it again,
// which tests it again: it takes no requeue, so it never offers one.
func TestFailedToolTestPointsToPublish(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	j := job(ns, "comp-farmtest-v1", "render", "cpu-"+ns, map[string]string{"frames": "1-1", "chunk": "1"}, now.Add(4*time.Hour))
	j.Spec.Retries = ptr.To[int32](0)
	j.Labels = map[string]string{c4v1.LabelToolTest: "comp"}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	failWith(t, podOfUnit(t, ns, "comp-farmtest-v1", 0), *now, 1, "Error", "")
	tick(t, q)
	tick(t, q)
	c := meta.FindStatusCondition(get(t, ns, "comp-farmtest-v1").Status.Conditions, c4v1.ConditionNeedsPerson)
	want := "1 units failed after their retries; fix the tool comp, then c4 farm tool publish again, which tests it again"
	if c == nil || c.Reason != c4v1.NeedsFailed || c.Message != want {
		t.Fatalf("got %+v, want the message %q", c, want)
	}
}
