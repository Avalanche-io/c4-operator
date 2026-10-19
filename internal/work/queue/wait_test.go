package queue

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// decide adds a decision to a job's AnnotationDecide list, as the HUD or
// MCP does.
func decide(t *testing.T, ns, name, unit, do string, at time.Time) {
	t.Helper()
	j := get(t, ns, name)
	var reqs []c4v1.WorkDecideRequest
	if a := j.Annotations[c4v1.AnnotationDecide]; a != "" {
		if err := json.Unmarshal([]byte(a), &reqs); err != nil {
			t.Fatal(err)
		}
	}
	reqs = append(reqs, c4v1.WorkDecideRequest{Token: strconv.FormatInt(at.UnixNano(), 36), Unit: unit, Do: do,
		Actor: "owner", Note: "ruled", At: metav1.Time{Time: at}})
	b, _ := json.Marshal(reqs)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationDecide] = string(b)
	if err := k8s.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func hasPod(t *testing.T, ns, job string, idx int) bool {
	t.Helper()
	for _, p := range unitPods(t, ns, job) {
		if unitOf(p) == idx {
			return true
		}
	}
	return false
}

// graphJob starts a graph job whose root expands into doc's units, and
// runs the root.
func graphJob(t *testing.T, name string, units func(now time.Time) string, deadline time.Duration) (string, *Queue, *time.Time) {
	t.Helper()
	ns, q, now, st := graphFarm(t)
	if err := k8s.Create(context.Background(), job(ns, name, "graph", "cpu-"+ns, nil, now.Add(deadline))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [`+units(*now)+`]}`)
	finish(t, podOfUnit(t, ns, name, 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	return ns, q, now
}

// A ruling: a wait for a person runs after its unit, creates no pod, holds
// up the unit after it (needs a person, Decision), carries its expected
// wait in the estimate and none in slot-seconds, and completes on a
// decision, when the unit after it runs.
func TestWaitForAPerson(t *testing.T) {
	ns, q, now := graphJob(t, "ruled", func(time.Time) string {
		return `{"name": "lane", "label": "lane"},
		{"name": "ruling", "after": ["lane"], "wait": {"server": "person:owner", "ask": "split the chapter?", "expect": 46080}},
		{"name": "next", "after": ["ruling"]}`
	}, 72*time.Hour)
	// units: 0 root, 1 lane, 2 ruling, 3 next
	if hasPod(t, ns, "ruled", 2) {
		t.Fatal("the wait has a pod before its unit ran")
	}
	finishUnit(t, now, ns, "ruled", 1, nil)
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "ruled")
	if hasPod(t, ns, "ruled", 2) || hasPod(t, ns, "ruled", 3) {
		t.Fatal("a wait or the unit after it has a pod")
	}
	if !slices.Equal(states(t, got, "waits"), []int{2}) || !slices.Equal(states(t, got, "blocked"), []int{3}) || got.Status.Units.Waits != 1 {
		t.Fatalf("states %v, waits %d", got.Status.UnitStates, got.Status.Units.Waits)
	}
	if len(got.Status.Waits) != 1 {
		t.Fatalf("waits %+v", got.Status.Waits)
	}
	w := got.Status.Waits[0]
	if w.Unit != 2 || w.Server != "person:owner" || w.Ask != "split the chapter?" || !w.Blocking || w.ExpectSeconds != 46080 || w.Since == nil {
		t.Fatalf("wait %+v", w)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson)
	if c == nil || c.Reason != c4v1.NeedsDecision || !strings.Contains(c.Message, "owner") {
		t.Fatalf("needs person %+v", c)
	}
	// the expected 12.8 hours are in the ETA
	if e := got.Status.Estimate; e == nil || e.ETA50 == nil || e.ETA50.Time.Before(now.Add(12*time.Hour)) {
		t.Fatalf("ETA %+v, want past %s", got.Status.Estimate, now.Add(12*time.Hour))
	}

	*now = now.Add(3 * time.Hour)
	decide(t, ns, "ruled", "ruling", "approve", *now)
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "ruled")
	if !slices.Contains(states(t, got, "succeeded"), 2) || !hasPod(t, ns, "ruled", 3) {
		t.Fatalf("after the decision: states %v", got.Status.UnitStates)
	}
	if w := got.Status.Waits[0]; w.Do != "approve" || w.Actor != "owner" || w.At == nil || w.Blocking {
		t.Fatalf("decided wait %+v", w)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson); c != nil {
		t.Fatalf("still needs a person: %+v", c)
	}
}

// A clock wait completes at its time, by itself.
func TestWaitForAClock(t *testing.T) {
	ns, q, now := graphJob(t, "clocked", func(now time.Time) string {
		at := now.Add(time.Hour).UTC().Format(time.RFC3339)
		return `{"name": "dailies", "wait": {"server": "clock:` + at + `"}},
		{"name": "render", "after": ["dailies"]}`
	}, 72*time.Hour)
	if got := get(t, ns, "clocked"); !slices.Equal(states(t, got, "waits"), []int{1}) || hasPod(t, ns, "clocked", 2) {
		t.Fatalf("before its time: %v", got.Status.UnitStates)
	}
	*now = now.Add(90 * time.Minute)
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "clocked")
	if !slices.Contains(states(t, got, "succeeded"), 1) || !hasPod(t, ns, "clocked", 2) {
		t.Fatalf("after its time: %v", got.Status.UnitStates)
	}
	if w := got.Status.Waits[0]; w.Do != "time" || w.Actor != "clock" {
		t.Fatalf("clock wait %+v", w)
	}
}

// A unit a review escalated is a wait for the person it names: a decision
// on it reworks it (it runs again) or accepts it.
func TestEscalationIsAWait(t *testing.T) {
	ns, q, now, put := reviewJob(t, "esc", []string{"a:a", "b:b"})
	finishUnit(t, now, ns, "esc", 1, nil)
	finishUnit(t, now, ns, "esc", 2, nil)
	tick(t, q)
	v := put(`{"version": 1, "verdict": "mixed", "items": [
		{"units": ["a"], "do": "escalate", "to": "lead", "why": "third send-back"},
		{"units": ["b"], "do": "escalate", "to": "lead", "why": "flicker"}]}`)
	finishUnit(t, now, ns, "esc", 3, map[string]string{c4v1.AnnotationOutputVerdict: v})
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "esc")
	if len(got.Status.Waits) != 2 {
		t.Fatalf("waits %+v", got.Status.Waits)
	}
	for _, w := range got.Status.Waits {
		if !w.Escalated || w.Server != "person:lead" || w.Since == nil {
			t.Fatalf("escalation wait %+v", w)
		}
	}
	*now = now.Add(time.Hour)
	decide(t, ns, "esc", "a", "rework", *now)
	decide(t, ns, "esc", "b", "accept", now.Add(time.Second))
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "esc")
	if !hasPod(t, ns, "esc", 1) {
		t.Fatalf("a was not reworked: %v", got.Status.UnitStates)
	}
	if !slices.Contains(states(t, got, "accepted"), 2) {
		t.Fatalf("b was not accepted: %v", got.Status.UnitStates)
	}
	for _, w := range got.Status.Waits {
		if w.Do == "" || w.Actor != "owner" {
			t.Fatalf("decided escalation %+v", w)
		}
	}
}
