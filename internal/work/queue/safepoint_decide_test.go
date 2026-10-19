package queue

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// atSafepoint does what the node daemon does when a unit reaches safe
// point seq: it sets the pod's safepoint annotation.
func atSafepoint(t *testing.T, p *corev1.Pod, seq int, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(spNote{Seq: seq, At: at})
	patch := client.MergeFrom(p.DeepCopy())
	p.Annotations[c4v1.AnnotationSafepoint] = string(data)
	if err := k8s.Patch(ctx, p, patch); err != nil {
		t.Fatal(err)
	}
}

func answerOf(t *testing.T, p *corev1.Pod) (spDecision, *corev1.Pod) {
	t.Helper()
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	d, _ := podDecision(p)
	return d, p
}

func setHoldStop(t *testing.T, j *c4v1.WorkJob, on bool) {
	t.Helper()
	ctx := context.Background()
	var cur c4v1.WorkJob
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(j), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Nudge.HoldStop = on
	if err := k8s.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
}

// A safe point is a decision point: with no stop pending the unit goes
// on; asked to stop, it stops there while the reason holds; once the
// reason has passed, it goes on and the stop is cancelled. Each stop and
// each cancelled stop is in the book.
func TestSafepointDecides(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	ctx := context.Background()
	q.Safepoints = &SafepointCache{}
	j := job(ns, "sh020", "render", "cpu-"+ns, map[string]string{"frames": "1-4", "estimateSeconds": "600"}, now.Add(time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "sh020")
	if len(pods) != 2 {
		t.Fatalf("%d units running", len(pods))
	}
	p0 := &pods[0]
	var control, env bool
	for _, v := range p0.Spec.Volumes {
		control = control || (v.Name == "c4-control" && v.EmptyDir != nil)
	}
	for _, e := range p0.Spec.Containers[0].Env {
		env = env || (e.Name == "C4_CONTROL" && e.Value == "/etc/c4/control")
	}
	if !control || !env {
		t.Fatal("no control directory for the decisions")
	}

	// No stop pending: continue, and nothing is recorded.
	atSafepoint(t, p0, 1, *now)
	tick(t, q)
	d, p0 := answerOf(t, p0)
	if d.Seq != 1 || d.Do != "continue" || p0.Annotations[c4v1.AnnotationSafepointNext] != "continue" {
		t.Fatalf("seq 1: %+v next %q", d, p0.Annotations[c4v1.AnnotationSafepointNext])
	}
	mem := q.jobs[j.UID]
	if len(mem.decisions) != 0 {
		t.Fatalf("a plain continue was recorded: %+v", mem.decisions)
	}

	// Hold-stop: asked to stop, and the next safe point says so ahead.
	setHoldStop(t, j, true)
	tick(t, q)
	_, p0 = answerOf(t, p0)
	if p0.Annotations[c4v1.AnnotationYield] == "" || p0.Annotations[c4v1.AnnotationSafepointNext] != "stop" {
		t.Fatalf("after hold-stop: %v", p0.Annotations)
	}
	*now = now.Add(time.Minute)
	atSafepoint(t, p0, 2, *now)
	tick(t, q)
	d, p0 = answerOf(t, p0)
	if d.Seq != 2 || d.Do != "stop" || !strings.Contains(d.Why, "hold-stop") {
		t.Fatalf("seq 2 under hold-stop: %+v", d)
	}
	if n := len(mem.decisions); n == 0 || mem.decisions[n-1].Kind != "safepoint-stop" {
		t.Fatalf("the stop was not recorded: %+v", mem.decisions)
	}

	// The hold is released before the unit got out: at its next safe
	// point it goes on, and the stop is cancelled.
	setHoldStop(t, j, false)
	*now = now.Add(time.Minute)
	atSafepoint(t, p0, 3, *now)
	tick(t, q)
	d, p0 = answerOf(t, p0)
	if d.Seq != 3 || d.Do != "continue" || !strings.Contains(d.Why, "released") {
		t.Fatalf("seq 3 after release: %+v", d)
	}
	if p0.Annotations[c4v1.AnnotationYield] != "" || p0.Annotations[c4v1.AnnotationYieldBy] != "" {
		t.Fatalf("the cancelled stop is still asked: %v", p0.Annotations)
	}
	if _, _, ok := q.st.Pending(unitID(j.Namespace+"/"+j.Name, unitOf(*p0))); ok {
		t.Fatal("the scheduler still holds the stop")
	}
	if n := len(mem.decisions); mem.decisions[n-1].Kind != "safepoint-continue" || mem.decisions[n-1].Reason == "" {
		t.Fatalf("the cancelled stop was not recorded: %+v", mem.decisions)
	}
}

// With no stop pending, a unit of a class that allows it is moved at a
// safe point to a clearly cheaper pool with room, and its next attempt
// runs there.
func TestSafepointMovesToCheaperPool(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 1)
	ctx := context.Background()
	q.Safepoints, q.Progress = &SafepointCache{}, &ProgressCache{}
	j := job(ns, "sh030", "render", "cpu-"+ns, map[string]string{"frames": "1-1", "estimateSeconds": "7200"}, time.Time{})
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods := unitPods(t, ns, "sh030")
	if len(pods) != 1 {
		t.Fatalf("%d units running", len(pods))
	}
	p0 := &pods[0]
	p0.Status.StartTime = &metav1.Time{Time: now.Add(-30 * time.Minute)}
	p0.Status.Phase = corev1.PodRunning
	if err := k8s.Status().Update(ctx, p0); err != nil {
		t.Fatal(err)
	}
	cheap := addPool(t, ns, "cheap", 1, 1, nil)
	cheap.Spec.CostPerSlotHour = "0.10"
	if err := k8s.Update(ctx, cheap); err != nil {
		t.Fatal(err)
	}
	var cur c4v1.WorkJob
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(j), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Pools = append(cur.Spec.Pools, cheap.Name)
	if err := k8s.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	tick(t, q)

	// A quarter done in half an hour: an hour and a half left.
	q.Progress.Set(*now, map[[2]string]float64{{ns, p0.Name}: 0.25})
	atSafepoint(t, p0, 1, *now)
	tick(t, q)
	d, p0 := answerOf(t, p0)
	if d.Do != "stop" || !strings.Contains(d.Why, cheap.Name) || p0.Annotations[c4v1.AnnotationYield] != yieldMove {
		t.Fatalf("not moved: %+v %v", d, p0.Annotations)
	}
	mem := q.jobs[j.UID]
	if n := len(mem.decisions); n == 0 || mem.decisions[n-1].Kind != "safepoint-move" {
		t.Fatalf("the move was not recorded: %+v", mem.decisions)
	}

	// It stops there; the next attempt runs in the cheaper pool.
	p0.Status.Phase = corev1.PodSucceeded
	if err := k8s.Status().Update(ctx, p0); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	tick(t, q)
	pods = unitPods(t, ns, "sh030")
	var moved bool
	for _, p := range pods {
		moved = moved || (p.Name != p0.Name && p.Labels[c4v1.LabelPool] == cheap.Name)
	}
	if !moved {
		var where []string
		for _, p := range pods {
			where = append(where, p.Name+"@"+p.Labels[c4v1.LabelPool])
		}
		t.Fatalf("the next attempt is not in %s: %v", cheap.Name, where)
	}
}
