package queue

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// hitUsageLimit ends a unit's pod as a pod block's turn at a usage limit
// does: exit 75 with the limit as its termination message, its view
// captured.
func hitUsageLimit(t *testing.T, p *corev1.Pod, end time.Time, msg, tree string) {
	t.Helper()
	ctx := context.Background()
	if tree != "" {
		patch := client.MergeFrom(p.DeepCopy())
		p.Annotations[c4v1.AnnotationOutputTree] = tree
		if err := k8s.Patch(ctx, p, patch); err != nil {
			t.Fatal(err)
		}
	}
	p.Status.Phase = corev1.PodFailed
	p.Status.StartTime = &metav1.Time{Time: end.Add(-time.Minute)}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "work", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: c4v1.UsageLimitExitCode, Message: msg, StartedAt: metav1.Time{Time: end.Add(-time.Minute)}, FinishedAt: metav1.Time{Time: end}}}}}
	if err := k8s.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
}

// A turn at a model's usage limit (exit 75) is the model's capacity, not
// the unit's failure: it spends no retry, however often it happens; the
// unit waits until the limit resets, the job says so, the wait is a yield
// and a decision; then the unit runs again from the view it captured.
func TestUsageLimitHoldsUntilItResets(t *testing.T) {
	ns, q, now := graphJob(t, "limited", func(time.Time) string {
		return `{"name": "cut", "reasoning": {"does": "model"}}, {"name": "deliver", "after": ["cut"]}`
	}, 72*time.Hour)
	// a retry is one attempt more (job's retries: 1): three limits in a row
	// would fail the unit if they counted
	var until time.Time
	for k := 0; k < 3; k++ {
		until = now.Add(2 * time.Hour).Truncate(time.Second).UTC()
		view := viewID("cut-at-the-limit-" + until.String())
		msg := "usage limit: You've hit your limit · resets 9:40pm (America/Los_Angeles) (until " + until.Format(time.RFC3339) + ")"
		hitUsageLimit(t, podOfUnit(t, ns, "limited", 1), *now, msg, view)
		tick(t, q)
		got := get(t, ns, "limited")
		l, ok := got.Status.UsageLimits["1"]
		if !ok || !l.Until.Time.Equal(until) || l.Probe || !strings.HasPrefix(l.Why, "usage limit: You've hit your limit") {
			t.Fatalf("limit %d: %+v", k, got.Status.UsageLimits)
		}
		if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionUsageLimit); c == nil || !strings.HasPrefix(c.Message, "waiting for the model's usage limit to reset at ") {
			t.Fatalf("condition %+v", c)
		}
		if len(states(t, got, "failed")) != 0 || got.Status.Attempts != "" || hasPod(t, ns, "limited", 1) {
			t.Fatalf("limit %d: states %v, attempts %q, pod %v", k, got.Status.UnitStates, got.Status.Attempts, hasPod(t, ns, "limited", 1))
		}
		if got.Status.Resume["1"] != view {
			t.Fatalf("resume %v, want %s", got.Status.Resume, view)
		}
		*now = until.Add(-time.Minute)
		tick(t, q)
		if hasPod(t, ns, "limited", 1) {
			t.Fatal("ran before the limit reset")
		}
		*now = until.Add(time.Second)
		tick(t, q)
		readyMaterialization(t, ns, resumeName("limited", 1), view)
		tick(t, q)
		// it runs again once its view is mounted to resume from
		if !hasPod(t, ns, "limited", 1) {
			t.Fatal("not run again after the reset")
		}
	}
	got := get(t, ns, "limited")
	if len(got.Status.UsageLimits) != 0 || meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionUsageLimit) != nil {
		t.Fatalf("still held: %+v", got.Status.UsageLimits)
	}
	if n := len(decisionsOf(got, "usage-limit")); n != 3 {
		t.Fatalf("%d usage-limit decisions", n)
	}
	var yields int
	for _, y := range got.Status.Yields {
		if y.Unit == 1 && strings.HasPrefix(y.Reason, "usage limit:") {
			yields++
		}
	}
	if yields != 3 {
		t.Fatalf("yields %+v", got.Status.Yields)
	}
}

// A limit that does not say when it resets is tried again after
// usage.limitProbe, which the decision names; a restart keeps the hold.
func TestUsageLimitProbe(t *testing.T) {
	ns, q, now := graphJobWith(t, "probed", map[string]string{"usage.limitProbe": "20m"}, func(time.Time) string {
		return `{"name": "cut", "reasoning": {"does": "model"}}`
	}, 72*time.Hour)
	hitUsageLimit(t, podOfUnit(t, ns, "probed", 1), *now, "usage limit: Usage limit reached for this model", "")
	tick(t, q)
	got := get(t, ns, "probed")
	l := got.Status.UsageLimits["1"]
	if !l.Probe || !l.Until.Time.Equal(now.Add(20*time.Minute).Truncate(time.Second)) {
		t.Fatalf("limit %+v", l)
	}
	if ds := decisionsOf(got, "usage-limit"); len(ds) != 1 || len(ds[0].Policy) != 1 || ds[0].Policy[0].Key != "usage.limitProbe" || ds[0].Policy[0].Value != "20m" {
		t.Fatalf("decision %+v", ds)
	}
	q = newQueueFrom(q)
	*now = now.Add(10 * time.Minute)
	tick(t, q)
	if hasPod(t, ns, "probed", 1) {
		t.Fatal("a restart lost the hold")
	}
	*now = now.Add(11 * time.Minute)
	tick(t, q)
	tick(t, q)
	if !hasPod(t, ns, "probed", 1) {
		t.Fatal("not tried again after the probe")
	}
}

// A member's pod is told how long its yields wait for background work
// (yield.backgroundWait); a tool's is not.
func TestYieldBackgroundWait(t *testing.T) {
	ns, q, _ := graphJobWith(t, "waited", map[string]string{"yield.backgroundWait": "10m"}, func(time.Time) string {
		return `{"name": "cut", "reasoning": {"does": "model"}}, {"name": "grade"}`
	}, 72*time.Hour)
	tick(t, q)
	if v := envOf(podOfUnit(t, ns, "waited", 1).Spec.Containers[0], "C4_YIELD_BACKGROUND_WAIT"); v != "10m0s" {
		t.Fatalf("member: %q", v)
	}
	if v := envOf(podOfUnit(t, ns, "waited", 2).Spec.Containers[0], "C4_YIELD_BACKGROUND_WAIT"); v != "" {
		t.Fatalf("a tool unit: %q", v)
	}
}

// Exit 75 is a usage limit only from a model's unit that says so in the
// limit's words: a build that exits 75 (EX_TEMPFAIL), or a model's turn
// that exits 75 without them, is an ordinary failure, never held.
func TestUsageLimitOnlyFromAModel(t *testing.T) {
	for _, c := range []struct{ name, unit, msg string }{
		{"build", `{"name": "build"}`, "usage limit: the build said so"},
		{"untold", `{"name": "cut", "reasoning": {"does": "model"}}`, "try again later"},
	} {
		ns, q, now := graphJob(t, c.name, func(time.Time) string { return c.unit }, 72*time.Hour)
		hitUsageLimit(t, podOfUnit(t, ns, c.name, 1), *now, c.msg, "")
		tick(t, q)
		got := get(t, ns, c.name)
		if len(got.Status.UsageLimits) != 0 || meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionUsageLimit) != nil {
			t.Errorf("%s: held as a usage limit: %+v", c.name, got.Status.UsageLimits)
		}
	}
}

// A stated reset further off than a week holds the unit a week, and the
// job says the time it gave was not taken as given.
func TestUsageLimitResetIsBounded(t *testing.T) {
	ns, q, now := graphJob(t, "faroff", func(time.Time) string {
		return `{"name": "cut", "reasoning": {"does": "model"}}`
	}, 72*time.Hour)
	stated := now.Add(30 * 24 * time.Hour).Truncate(time.Second).UTC()
	hitUsageLimit(t, podOfUnit(t, ns, "faroff", 1), *now, "usage limit: resets next month (until "+stated.Format(time.RFC3339)+")", "")
	tick(t, q)
	got := get(t, ns, "faroff")
	l, ok := got.Status.UsageLimits["1"]
	if want := now.Add(maxUsageWait).Truncate(time.Second); !ok || !l.Until.Time.Equal(want) {
		t.Fatalf("held until %v, want %v", l.Until, want)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionUsageLimit); c == nil || !strings.Contains(c.Message, "more than a week off, so the wait is held to that") {
		t.Fatalf("condition %+v", c)
	}
}
