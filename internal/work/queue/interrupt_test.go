package queue

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// bind places a pod on a node, as kube-scheduler would.
func bind(t *testing.T, p *corev1.Pod, node string) {
	t.Helper()
	b := &corev1.Binding{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name},
		Target: corev1.ObjectReference{Kind: "Node", Name: node}}
	if err := k8s.SubResource("binding").Create(context.Background(), p, b); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
}

// end plays the kubelet ending a pod that was stopped: phase Failed, and
// the DisruptionTarget condition when reason is set.
func end(t *testing.T, p *corev1.Pod, reason string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = corev1.PodFailed
	p.Status.StartTime = &metav1.Time{Time: at.Add(-time.Minute)}
	if reason != "" {
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.DisruptionTarget,
			Status: corev1.ConditionTrue, Reason: reason, LastTransitionTime: metav1.Time{Time: at}})
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "work", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 143, StartedAt: metav1.Time{Time: at.Add(-time.Minute)}, FinishedAt: metav1.Time{Time: at}}}}}
	if err := k8s.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
}

// A drained or deleted attempt is interrupted: it spends no retry, marks
// no node, and runs again. Unit pods carry the queue's finalizer until
// the queue has recorded them.
func TestInterruptedAttempts(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 1)
	ctx := context.Background()
	j := job(ns, "sh070", "render", "cpu-"+ns, map[string]string{"frames": "1-1", "estimateSeconds": "600"}, now.Add(4*time.Hour))
	j.Spec.NodeErrorLimit = 1
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	node := fmt.Sprintf("%s-cpu-0", ns)
	tick(t, q)
	for i, reason := range []string{"EvictionByEvictionAPI", "TerminationByKubelet", "PreemptionByScheduler"} {
		pods := unitPods(t, ns, "sh070")
		if len(pods) != 1 || !slices.Contains(pods[0].Finalizers, c4v1.FinalizerUnit) {
			t.Fatalf("round %d: pods %v", i, pods)
		}
		p := &pods[0]
		bind(t, p, node)
		*now = now.Add(time.Minute)
		end(t, p, reason, *now)
		tick(t, q)
		got := get(t, ns, "sh070")
		if got.Status.Interrupted != int32(i+1) || got.Status.Interruptions[i].Reason != reason || got.Status.Interruptions[i].Node != node {
			t.Fatalf("round %d: %d %+v", i, got.Status.Interrupted, got.Status.Interruptions)
		}
		if len(got.Status.UnitStates["failed"]) > 0 || len(got.Status.Blacklist) > 0 || got.Status.Attempts != "" {
			t.Fatalf("round %d: an interruption counted as a failure: %v %v %q", i, got.Status.UnitStates, got.Status.Blacklist, got.Status.Attempts)
		}
		var old corev1.Pod
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), &old); !apierrors.IsNotFound(err) {
			t.Fatalf("round %d: the recorded pod is still there (finalizers %v): %v", i, old.Finalizers, err)
		}
		tick(t, q) // released again
	}

	// Deleted while it ran on its node: the pod stays, holding its slot,
	// until it ends; then it is an interruption too.
	pods := unitPods(t, ns, "sh070")
	p := &pods[0]
	bind(t, p, node)
	if err := k8s.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil || p.DeletionTimestamp == nil {
		t.Fatalf("the deleted pod: %v", err)
	}
	if got := get(t, ns, "sh070"); got.Status.Units.Running != 1 {
		t.Fatalf("the deleting unit does not hold its slot: %+v", got.Status.Units)
	}
	end(t, p, "", *now)
	tick(t, q)
	got := get(t, ns, "sh070")
	if got.Status.Interrupted != 4 || got.Status.Interruptions[3].Reason != "Deleted" {
		t.Fatalf("deleted: %+v", got.Status.Interruptions)
	}
}

// A yield gives its attempt back, like an interruption: a unit that
// yields twice runs its next attempt as attempt 1, with no failed
// attempt counted, and it is never in backoff or retrying.
func TestYieldIsNotAnAttempt(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 1)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "sh071", "ml", "cpu-"+ns, map[string]string{"estimateSeconds": "3600", "ranks": "1"}, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	for round := 0; round < 2; round++ {
		if round > 0 {
			// The next attempt resumes from the view the yield saved.
			var m c4v1.Materialization
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: resumeName("sh071", 0)}, &m); err != nil {
				t.Fatal(err)
			}
			m.Status.Phase, m.Status.ObservedGeneration = c4v1.PhaseReady, m.Generation
			if err := k8s.Status().Update(ctx, &m); err != nil {
				t.Fatal(err)
			}
			tick(t, q)
		}
		pods := unitPods(t, ns, "sh071")
		if len(pods) != 1 || pods[0].Annotations[c4v1.AnnotationAttempt] != "1" {
			t.Fatalf("round %d: pods %+v", round, pods)
		}
		p := &pods[0]
		patch := client.MergeFrom(p.DeepCopy())
		p.Annotations[c4v1.AnnotationYield] = "reclaim-nominal"
		if err := k8s.Patch(ctx, p, patch); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(10 * time.Minute)
		finish(t, p, corev1.PodSucceeded, *now, 10*time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID(fmt.Sprintf("ckpt-%d", round))})
		tick(t, q)
		got := get(t, ns, "sh071")
		if got.Status.Attempts != "" || got.Status.Units.Backoff != 0 || got.Status.Units.Retrying != 0 || len(got.Status.Yields) != round+1 {
			t.Fatalf("round %d: attempts %q, units %+v, yields %d", round, got.Status.Attempts, got.Status.Units, len(got.Status.Yields))
		}
		tick(t, q)
	}
}
