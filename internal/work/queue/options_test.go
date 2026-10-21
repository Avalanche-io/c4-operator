package queue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// optionsFarm is two projects on one 4-slot pool with an empty burst pool
// beside it: show-a's hero render (8 ten-minute frames, due in eight
// hours, allowed to burst) and comp (12 frames, due in two hours and ten
// minutes), and show-b's crowd render (12 frames, due in two hours).
func optionsFarm(t *testing.T) (*Queue, *time.Time, string, string, string) {
	t.Helper()
	ctx := context.Background()
	nsA, q, now := farm(t, "cpu", 1, 4)
	nsB := nsA + "-b"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsB}}); err != nil {
		t.Fatal(err)
	}
	spot := &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "spot-" + nsA}, Spec: c4v1.PoolSpec{
		NodeSelector: map[string]string{"pool": nsA + "-spot"}, SlotsPerNode: 4, CostPerSlotHour: "0.40",
		Burst: &c4v1.PoolBurst{MaxNodes: 2, ProvisioningSeconds: 300, CostPerSlotHour: "2.00", Provider: "kind-sim"}}}
	store := c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: "file:///tmp/none"}}}
	prA := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-a-" + nsA}, Spec: c4v1.ProjectSpec{Namespaces: []string{nsA}, Store: store}}
	prB := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-b-" + nsA}, Spec: c4v1.ProjectSpec{Namespaces: []string{nsB}, Store: store}}
	for _, o := range []*c4v1.Project{prA, prB} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := k8s.Create(ctx, spot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(ctx, spot); k8s.Delete(ctx, prA); k8s.Delete(ctx, prB) })

	frames := func(n string) map[string]string { return map[string]string{"frames": n, "estimateSeconds": "600"} }
	hero := job(nsA, "hero", "render", "cpu-"+nsA, frames("1-8"), now.Add(8*time.Hour))
	hero.Spec.Pools = append(hero.Spec.Pools, spot.Name)
	// comp has ten minutes more slack than crowd, so which of the two a
	// tighter hero makes late is the plan's choice (crowd), never a
	// floating-point tie between twins that can differ by architecture.
	comp := job(nsA, "comp", "render", "cpu-"+nsA, frames("1-12"), now.Add(130*time.Minute))
	crowd := job(nsB, "crowd", "render", "cpu-"+nsA, frames("1-12"), now.Add(120*time.Minute))
	for _, j := range []*c4v1.WorkJob{hero, comp, crowd} {
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, q)
	return q, now, nsA + "/hero", nsA + "/comp", nsB + "/crowd"
}

// A deadline too early for the hero is priced: 40 minutes takes a burst
// (needing approval, since show-a has no burst budget); 45 minutes makes
// it on time but makes show-b's crowd render late, which show-a's viewer
// sees counted and show-b's sees named; 5 minutes nothing closes, and the
// floor says why.
func TestDeadlineOptions(t *testing.T) {
	q, now, hero, comp, crowd := optionsFarm(t)
	nsA, _, _ := strings.Cut(hero, "/")
	only := func(p string) func(string) bool { return func(x string) bool { return x == p } }
	viewerA, viewerB := only("show-a-"+nsA), only("show-b-"+nsA)
	at := []time.Time{now.Add(8 * time.Hour), now.Add(45 * time.Minute), now.Add(40 * time.Minute), now.Add(5 * time.Minute)}
	o, err := q.DeadlineOptions(hero, at, viewerA)
	if err != nil {
		t.Fatal(err)
	}
	if o.Policy != forecast.Achievable || o.Quantile != forecast.DefaultQuantile || !o.Deadline.Equal(now.Add(8*time.Hour)) || o.Band.IsZero() {
		t.Fatalf("readout %+v", o)
	}
	// The achievable deadline bumps nobody.
	if o.Achievable.IsZero() || o.Achievable.Before(now.Add(45*time.Minute)) {
		t.Fatalf("achievable %s", o.Achievable.Sub(*now))
	}
	if o.Floor.IsZero() || !o.Floor.After(*now) || o.FloorWhy == "" {
		t.Fatalf("floor %s %q", o.Floor.Sub(*now), o.FloorWhy)
	}
	far, ontime, early, never := o.Options[0], o.Options[1], o.Options[2], o.Options[3]
	if !far.OnTime || far.Meets != 95 || far.Burst != nil || far.Displaces != 0 || far.Boost != 0 || !far.Floor.IsZero() {
		t.Fatalf("8h: %+v", far)
	}
	if !ontime.OnTime || ontime.Displaces != 1 || len(ontime.Displaced) != 0 || ontime.Unnamed != 1 {
		t.Fatalf("45m for show-a: %+v", ontime)
	}
	if early.OnTime || early.Burst == nil || !early.Burst.Closes || early.Burst.Auto || early.Burst.Slots["spot-"+nsA] == 0 ||
		early.Burst.Cost <= 0 || early.Burst.Why == "" || !early.Floor.IsZero() {
		t.Fatalf("40m: %+v burst %+v", early, early.Burst)
	}
	if never.OnTime || never.Meets != 0 || (never.Burst != nil && never.Burst.Closes) || never.Boost != 0 ||
		!never.Floor.Equal(o.Floor) || never.FloorWhy == "" {
		t.Fatalf("5m: %+v", never)
	}

	// show-b's viewer sees its own job named.
	o, err = q.DeadlineOptions(hero, at[1:2], viewerB)
	if err != nil {
		t.Fatal(err)
	}
	if x := o.Options[0]; x.Displaces != 1 || len(x.Displaced) != 1 || x.Displaced[0] != crowd || x.Unnamed != 0 {
		t.Fatalf("45m for show-b: %+v (comp is %s)", x, comp)
	}

	if _, err := q.DeadlineOptions("nope/none", at[:1], nil); !errors.Is(err, ErrNotPlanned) {
		t.Fatalf("unknown job: %v", err)
	}
	if _, err := (&Queue{}).DeadlineOptions(hero, at[:1], nil); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("no plan: %v", err)
	}
	if _, err := q.DeadlineOptions(hero, make([]time.Time, MaxDeadlineOptions+1), nil); err == nil {
		t.Fatal("more than the maximum candidate times were accepted")
	}
}

// Boost is read as the deadline it asks slots for: x, due in four hours,
// finishes in an hour if its deadline is before y's (three hours) and in
// two otherwise; y is late if x's deadline is before two hours. To be
// ready by 1:30, a deadline at 1:30 works but makes y late; boost 1.4
// (four hours over 1.4 is 2:51) works without.
func TestDeadlineOptionsBoost(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	policy := func(s sched.Snapshot, _ *sched.State, _ sched.Config) sched.Decisions {
		var x, y sched.Job
		for _, j := range s.Jobs {
			if j.ID == "a/x" {
				x = j
			} else {
				y = j
			}
		}
		fin := now.Add(2 * time.Hour)
		if x.Deadline.Before(y.Deadline) {
			fin = now.Add(time.Hour)
		}
		late := x.Deadline.Before(now.Add(2 * time.Hour))
		return sched.Decisions{Jobs: map[string]sched.JobState{
			"a/x": {Feasible: true, ETA50: fin, ETA90: fin, Attainable: 4},
			"b/y": {Feasible: !late, AtRisk: late, ETA50: now.Add(time.Hour), ETA90: now.Add(time.Hour)},
		}}
	}
	q := &Queue{Policy: policy, lastSnap: sched.Snapshot{Now: now, Jobs: []sched.Job{
		{ID: "a/x", Project: "a", Deadline: now.Add(4 * time.Hour), Estimate: estimate.Summary{W: 3600, TPath: 600}},
		{ID: "b/y", Project: "b", Deadline: now.Add(3 * time.Hour)}}}}
	o, err := q.DeadlineOptions("a/x", []time.Time{now.Add(90 * time.Minute), now.Add(150 * time.Minute)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if x := o.Options[0]; !x.OnTime || x.Displaces != 1 || x.Displaced[0] != "b/y" || x.Boost != 1.4 {
		t.Fatalf("1:30: %+v", x)
	}
	if x := o.Options[1]; !x.OnTime || x.Displaces != 0 || x.Boost != 0 {
		t.Fatalf("2:30 needs nothing: %+v", x)
	}
	if want := now.Add(15 * time.Minute); !o.Floor.Equal(want) {
		t.Fatalf("floor %s, want W/A = 15m", o.Floor.Sub(now))
	}
}

// With no times given, the readout offers the project's upcoming sessions
// before the deadline and the earliest no-bump time at 80%, labeled,
// earliest first.
func TestTighterChoices(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC) // a Wednesday
	dailies := forecast.Policy{Kind: forecast.Sessions, Sessions: []string{"08:00"}, Location: time.UTC, Quantile: 0.95,
		Weekdays: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}}
	policies := map[string]forecast.Policy{"render": forecast.DefaultPolicy(), "dailies": dailies}
	deadline := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) // Saturday noon
	at80 := now.Add(3 * time.Hour)
	got, labels := tighter(now, deadline, policies, func() time.Time { return at80 })
	want := []time.Time{at80, time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC), time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)}
	if len(got) != len(want) {
		t.Fatalf("choices %v", got)
	}
	for i := range want {
		if !got[i].Equal(want[i]) || labels[got[i]] == "" {
			t.Fatalf("choices %v %v, want %v", got, labels, want)
		}
	}
	if labels[want[1]] != "dailies session Thu 08:00 UTC" {
		t.Fatalf("label %q", labels[want[1]])
	}
	// Nothing is offered at or after the deadline.
	if got, _ := tighter(now, now.Add(2*time.Hour), policies, func() time.Time { return at80 }); len(got) != 0 {
		t.Fatalf("choices after the deadline: %v", got)
	}
}
