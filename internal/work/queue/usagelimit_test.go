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

// leasedGraphJob starts a graph job of owner noor's whose template mounts
// credential lease lease.
func leasedGraphJob(t *testing.T, ns, name, lease string, now time.Time) {
	t.Helper()
	j := job(ns, name, "graph", "cpu-"+ns, nil, now.Add(72*time.Hour))
	j.Annotations = map[string]string{c4v1.AnnotationOwner: "noor"}
	spec := &j.Spec.Template.Spec
	spec.Volumes = []corev1.Volume{{Name: "lease", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: c4v1.LeaseSecretPrefix + lease}}}}
	spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "lease", MountPath: "/etc/c4/lease", ReadOnly: true}}
	if err := k8s.Create(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

// activeLease is noor's active holder-transfer lease name, for pool.
func activeLease(t *testing.T, ns, name, pool string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	l := &c4v1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: c4v1.CredentialLeaseSpec{
		Account: "anthropic", Holder: "noor", BoundTo: c4v1.LeaseBinding{Owners: []string{"noor"}},
		Mode: c4v1.LeaseHolderTransfer, Target: c4v1.LeaseTarget{Pools: []string{pool}},
		TTL: metav1.Duration{Duration: 72 * time.Hour}, RenewBefore: metav1.Duration{Duration: 10 * time.Minute}}}
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	exp := metav1.NewTime(now.Add(72 * time.Hour))
	l.Status = c4v1.CredentialLeaseStatus{State: c4v1.LeaseActive, ExpiresAt: &exp}
	if err := k8s.Status().Update(ctx, l); err != nil {
		t.Fatal(err)
	}
}

// A usage limit is the account's: once a unit on a credential lease hits
// it, every model unit of a job on that lease waits until the same reset,
// spending no attempt, in the same round and across a restart; the job
// says which unit hit it, and the account is never named. A job on
// another lease, and a unit that is not a model's, run on.
func TestUsageLimitHoldsTheAccount(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	q.HolderTransfer = true
	pool := "cpu-" + ns
	activeLease(t, ns, "acct-1", pool, *now)
	activeLease(t, ns, "acct-2", pool, *now)
	// "held" is read before "lim" each round
	leasedGraphJob(t, ns, "lim", "acct-1", *now)
	leasedGraphJob(t, ns, "held", "acct-1", *now)
	leasedGraphJob(t, ns, "other", "acct-2", *now)
	tick(t, q)
	for name, units := range map[string]string{
		"lim":   `{"name": "cut", "reasoning": {"does": "model"}}`,
		"held":  `{"name": "draft"}, {"name": "polish", "after": ["draft"], "reasoning": {"does": "model"}}`,
		"other": `{"name": "prep"}, {"name": "cut", "after": ["prep"], "reasoning": {"does": "model"}}`,
	} {
		doc := putDoc(t, st, `{"version": 1, "units": [`+units+`]}`)
		finish(t, podOfUnit(t, ns, name, 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	}
	tick(t, q)
	// lim's cut hits the limit in the round held's draft and other's prep
	// finish, which would release the model units after them
	until := now.Add(2 * time.Hour).Truncate(time.Second).UTC()
	hitUsageLimit(t, podOfUnit(t, ns, "lim", 1), *now, "usage limit: You've hit your limit (until "+until.Format(time.RFC3339)+")", "")
	finishUnit(t, now, ns, "held", 1, nil)
	finishUnit(t, now, ns, "other", 1, nil)
	tick(t, q)
	tick(t, q)
	check := func(when string) {
		t.Helper()
		if hasPod(t, ns, "held", 2) || hasPod(t, ns, "lim", 1) {
			t.Fatalf("%s: a unit on the held account ran", when)
		}
		if !hasPod(t, ns, "other", 2) {
			t.Fatalf("%s: a unit on another account did not run", when)
		}
		lim, held, other := get(t, ns, "lim"), get(t, ns, "held"), get(t, ns, "other")
		a := lim.Status.UsageLimits["1"].Account
		if a == "" || strings.Contains(a, "acct") {
			t.Fatalf("%s: account %q", when, a)
		}
		c := meta.FindStatusCondition(held.Status.Conditions, c4v1.ConditionUsageLimit)
		if c == nil || !strings.HasPrefix(c.Message, "waiting for the model's usage limit on the job's account to reset at ") ||
			!strings.HasSuffix(c.Message, " (unit cut of job lim hit it)") || strings.Contains(c.Message, "acct") {
			t.Fatalf("%s: held's condition %+v", when, c)
		}
		if len(held.Status.UsageLimits) != 0 || strings.Contains(held.Status.Attempts, "2:") {
			t.Fatalf("%s: held's limits %+v, attempts %q", when, held.Status.UsageLimits, held.Status.Attempts)
		}
		// its waiting unit is still work to do, after the reset
		if e := held.Status.Estimate; e == nil || e.RemainingWork <= 0 || e.ETA50 == nil || !e.ETA50.Time.After(until) {
			t.Fatalf("%s: held's estimate %+v, want an ETA after the reset %s", when, held.Status.Estimate, until)
		}
		if meta.FindStatusCondition(other.Status.Conditions, c4v1.ConditionUsageLimit) != nil {
			t.Fatalf("%s: other is held", when)
		}
		// the account's hold is decided once, by the unit that hit it
		if len(decisionsOf(lim, "usage-limit")) != 1 || len(decisionsOf(held, "usage-limit")) != 0 {
			t.Fatalf("%s: decisions %+v, %+v", when, decisionsOf(lim, "usage-limit"), decisionsOf(held, "usage-limit"))
		}
	}
	check("held")
	q = newQueueFrom(q)
	q.HolderTransfer = true
	*now = now.Add(time.Hour)
	tick(t, q)
	tick(t, q)
	check("after a restart")
	*now = until.Add(time.Second)
	tick(t, q)
	tick(t, q)
	if !hasPod(t, ns, "held", 2) || !hasPod(t, ns, "lim", 1) {
		t.Fatal("not run after the reset")
	}
	if c := meta.FindStatusCondition(get(t, ns, "held").Status.Conditions, c4v1.ConditionUsageLimit); c != nil {
		t.Fatalf("still held: %+v", c)
	}
}

// A job waiting on its account's limit names the unit that hit it only
// when that unit's job is in its own project; otherwise it says another
// job on the same account hit it.
func TestAccountHoldNamesOnlyWithinTheProject(t *testing.T) {
	until := time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)
	h := &accountHold{usageRec: usageRec{until: until}, job: "studio/lim", project: "show-a", mem: newMem(), unit: 1}
	v := &view{job: &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "studio", Name: "held"}}, project: "show-a",
		accountWait: map[int]*accountHold{2: h, 3: h}}
	q := &Queue{}
	if got := q.accountMessage(v); !strings.HasSuffix(got, " (unit 1 of job lim hit it; 2 units wait)") {
		t.Fatalf("same project: %q", got)
	}
	v.project = "show-b"
	if got := q.accountMessage(v); !strings.HasSuffix(got, " (another job on the same account hit it; 2 units wait)") || strings.Contains(got, "job lim") {
		t.Fatalf("another project: %q", got)
	}
	h.probe = true
	if got := q.accountMessage(v); !strings.HasPrefix(got, "waiting for the model's usage limit on the job's account: trying again at ") {
		t.Fatalf("probe: %q", got)
	}
}
