package queue

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A job with no deadline of its own takes the earliest Milestone that
// covers it, and the farm suggests none over it; a job with its own
// deadline keeps it. The Milestone's status lists the jobs it covers and
// their worst risk.
func TestMilestoneDeadline(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	ms := func(name string, due time.Duration) *c4v1.Milestone {
		m := &c4v1.Milestone{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: c4v1.MilestoneSpec{Due: metav1.NewTime(now.Add(due)),
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{c4v1.LabelShot: "sh040"}},
				Source:   &c4v1.MilestoneSource{Tracker: "flow", Ref: "Task/881"}}}
		if err := k8s.Create(ctx, m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	ms("sh040-final", 48*time.Hour)
	ms("sh040-temp", 6*time.Hour) // the earlier one wins
	params := map[string]string{"frames": "1001-1008", "estimateSeconds": "600"}
	covered := job(ns, "sh040-light-v001", "render", "cpu-"+ns, params, time.Time{})
	covered.Labels = map[string]string{c4v1.LabelShot: "sh040"}
	own := job(ns, "sh040-comp-v001", "render", "cpu-"+ns, params, now.Add(2*time.Hour))
	own.Labels = map[string]string{c4v1.LabelShot: "sh040"}
	other := job(ns, "sh041-light-v001", "render", "cpu-"+ns, params, time.Time{})
	for _, j := range []*c4v1.WorkJob{covered, own, other} {
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, q)
	tick(t, q)
	st := get(t, ns, covered.Name).Status
	if st.DeadlineSource != "milestone" || st.Milestone != "sh040-temp" || st.Deadline == nil ||
		!st.Deadline.Time.Equal(now.Add(6*time.Hour)) || st.Suggestion != nil {
		t.Fatalf("covered: source %q milestone %q deadline %v suggestion %+v", st.DeadlineSource, st.Milestone, st.Deadline, st.Suggestion)
	}
	if st := get(t, ns, own.Name).Status; st.DeadlineSource != "spec" || st.Milestone != "" {
		t.Fatalf("own deadline: source %q milestone %q", st.DeadlineSource, st.Milestone)
	}
	if st := get(t, ns, other.Name).Status; st.DeadlineSource != "suggested" {
		t.Fatalf("uncovered: source %q", st.DeadlineSource)
	}
	var m c4v1.Milestone
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sh040-temp"}, &m); err != nil {
		t.Fatal(err)
	}
	if m.Status.Count != 2 || len(m.Status.Jobs) != 2 || m.Status.Jobs[0] != own.Name || m.Status.Risk == "" || m.Status.Done != 0 {
		t.Fatalf("milestone status %+v", m.Status)
	}

	// Its due moves: the job follows on the next tick.
	patch := client.MergeFrom(m.DeepCopy())
	m.Spec.Due = metav1.NewTime(now.Add(3 * time.Hour))
	if err := k8s.Patch(ctx, &m, patch); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if st := get(t, ns, covered.Name).Status; st.Deadline == nil || !st.Deadline.Time.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("after the due moved: %v", st.Deadline)
	}
}
