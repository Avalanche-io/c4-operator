package queue

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// A render job with no deadline gets an achievable one: on a quarter
// hour, at least the minimum lead ahead, and no earlier than its own 80%
// finish. A restarted queue keeps it.
func TestRenderWithoutDeadlineGetsSuggestion(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	j := job(ns, "sh030", "render", "cpu-"+ns, map[string]string{"frames": "1001-1016", "estimateSeconds": "600"}, time.Time{})
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	st := get(t, ns, "sh030").Status
	sg := st.Suggestion
	if sg == nil || st.DeadlineSource != "suggested" {
		t.Fatalf("no suggestion: source %q, %+v", st.DeadlineSource, sg)
	}
	d := sg.Deadline.Time
	if sg.State != string(forecast.Estimating) || sg.Policy != "achievable" || sg.Quantile != "0.95" || len(sg.History) != 1 ||
		sg.History[0].Reason != forecast.ReasonFirst || sg.AtRisk {
		t.Fatalf("suggestion %+v", sg)
	}
	if d.Before(now.Add(15*time.Minute)) || d.Unix()%(15*60) != 0 {
		t.Fatalf("suggested %s: want a quarter hour at least 15 minutes ahead", d)
	}
	// 16 units of 10 minutes on 4 slots: about 40 minutes of work.
	if d.After(now.Add(4 * time.Hour)) {
		t.Fatalf("suggested %s for about 40 minutes of work", d.Sub(*now))
	}
	tick(t, q) // the deadline the policy now plans on is the suggestion
	st = get(t, ns, "sh030").Status
	if st.Deadline == nil || !st.Deadline.Time.Equal(d) {
		t.Fatalf("effective deadline %v, suggestion %s", st.Deadline, d)
	}
	if e := st.Estimate; e == nil || e.ETA95 == nil || e.ETA95.Time.After(d) || st.Suggestion.AtRisk {
		t.Fatalf("95%% finish after the suggestion: %+v, at risk %v", e, st.Suggestion.AtRisk)
	}

	q2 := &Queue{Client: k8s, Policy: sched.Decide, Cfg: sched.DefaultConfig(), Log: q.Log, Now: q.Now, CaptureWait: time.Minute}
	*now = now.Add(time.Minute)
	tick(t, q2)
	if got := get(t, ns, "sh030").Status.Suggestion; got == nil || !got.Deadline.Time.Equal(d) || len(got.History) != 1 {
		t.Fatalf("after a restart: %+v, want %s", got, d)
	}
}

// A pinned deadline is the job's; CI keeps its latency objective.
func TestPinnedAndLatencyGetNoSuggestion(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	ctx := context.Background()
	pinned := job(ns, "sh031", "render", "cpu-"+ns, map[string]string{"frames": "1-4", "estimateSeconds": "60"}, now.Add(3*time.Hour))
	ci := job(ns, "ci001", "ci", "cpu-"+ns, map[string]string{"shards": "2", "estimateSeconds": "60"}, time.Time{})
	for _, j := range []*c4v1.WorkJob{pinned, ci} {
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, q)
	if st := get(t, ns, "sh031").Status; st.Suggestion != nil || st.DeadlineSource != "spec" {
		t.Fatalf("pinned: source %q suggestion %+v", st.DeadlineSource, st.Suggestion)
	}
	if st := get(t, ns, "ci001").Status; st.Suggestion != nil || st.DeadlineSource != "class" {
		t.Fatalf("ci: source %q suggestion %+v", st.DeadlineSource, st.Suggestion)
	}
}

// A project's sessions policy gives the next session. A job that will not
// make it keeps it, settled, and is offered the following session.
func TestSessionsPolicyKeepsSessionAndOffersNext(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 1)
	ctx := context.Background()
	pr := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "dailies-" + ns}, Spec: c4v1.ProjectSpec{
		Namespaces: []string{ns},
		Store:      c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: "file:///tmp/none"}}},
		Work: &c4v1.ProjectWork{DeadlinePolicies: []c4v1.DeadlinePolicy{{
			Class: "render", Kind: "sessions", Sessions: []string{"10:00", "14:00"}, TimeZone: "UTC"}}},
	}}
	if err := k8s.Create(ctx, pr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(ctx, pr) })
	// 09:00 now. Eight hour-long frames on one slot cannot make 10:00.
	j := job(ns, "dl001", "render", "cpu-"+ns, map[string]string{"frames": "1-8", "estimateSeconds": "3600"}, time.Time{})
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	tick(t, q)
	sg := get(t, ns, "dl001").Status.Suggestion
	if sg == nil {
		t.Fatal("no suggestion")
	}
	want := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.UTC)
	if !sg.Deadline.Time.Equal(want) || sg.State != string(forecast.Settled) || sg.Policy != "sessions" {
		t.Fatalf("suggestion %+v, want settled at %s", sg, want)
	}
	if sg.NextSession == nil || !sg.NextSession.Time.Equal(want.Add(4*time.Hour)) {
		t.Fatalf("next session %v, want 14:00", sg.NextSession)
	}
}
