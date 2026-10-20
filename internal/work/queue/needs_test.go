package queue

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// failNeeding ends a unit's pod as a unit that stopped for information:
// its container exits 12 with the text as its termination message, or (a
// host runner) the pod's reason says so.
func failNeeding(t *testing.T, p *corev1.Pod, end time.Time, msg string, byReason bool) {
	t.Helper()
	p.Status.Phase = corev1.PodFailed
	p.Status.StartTime = &metav1.Time{Time: end.Add(-time.Minute)}
	term := &corev1.ContainerStateTerminated{ExitCode: c4v1.NeedsExitCode, Message: msg,
		StartedAt: metav1.Time{Time: end.Add(-time.Minute)}, FinishedAt: metav1.Time{Time: end}}
	if byReason {
		term.ExitCode, term.Message = 1, ""
		p.Status.Reason, p.Status.Message = c4v1.NeedsInformation, msg
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "work", State: corev1.ContainerState{Terminated: term}}}
	if err := k8s.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func supplyUnit(t *testing.T, ns, name, unit, text string, at time.Time) {
	t.Helper()
	j := get(t, ns, name)
	var reqs []c4v1.WorkSupplyRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationSupply]), &reqs)
	reqs = append(reqs, c4v1.WorkSupplyRequest{Token: strconv.FormatInt(at.UnixNano(), 36), Unit: unit, Text: text, Actor: "dana", At: metav1.Time{Time: at}})
	b, _ := json.Marshal(reqs)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationSupply] = string(b)
	if err := k8s.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

// A unit that stops for information its brief lacked fails at once, with
// no retry and no mark on its node, and the job says what it needs; the
// rest of the job goes on. Supplied, it runs again with the information,
// and keeps the record of what it asked. A host runner says it with the
// pod's reason instead.
func TestNeedsInformation(t *testing.T) {
	ns, q, now := graphJob(t, "briefed", func(time.Time) string {
		return `{"name": "cut"}, {"name": "grade"}, {"name": "deliver", "after": ["cut", "grade"]}`
	}, 24*time.Hour)
	// units: 0 root, 1 cut, 2 grade, 3 deliver
	failNeeding(t, podOfUnit(t, ns, "briefed", 1), *now, "the delivery frame rate\nwhy: the brief gives 24 and 25", false)
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "briefed")
	if !slices.Contains(states(t, got, "failed"), 1) || hasPod(t, ns, "briefed", 1) {
		t.Fatalf("cut: states %v, pod %v", got.Status.UnitStates, hasPod(t, ns, "briefed", 1))
	}
	n := got.Status.Needs["1"]
	if n.Text != "the delivery frame rate" || n.Why != "the brief gives 24 and 25" || n.Supplied != "" {
		t.Fatalf("needs %+v", got.Status.Needs)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson)
	if c == nil || c.Reason != c4v1.NeedsInformation || c.Message != "needs information: the delivery frame rate" {
		t.Fatalf("needs person %+v", c)
	}
	if !hasPod(t, ns, "briefed", 2) {
		t.Fatal("grade, which does not depend on cut, stopped")
	}
	if len(q.jobs[got.UID].blacklist) != 0 {
		t.Fatalf("the node was marked: %v", q.jobs[got.UID].blacklist)
	}

	*now = now.Add(time.Hour)
	supplyUnit(t, ns, "briefed", "cut", "24 fps", *now)
	tick(t, q)
	p := podOfUnit(t, ns, "briefed", 1)
	if envOf(p.Spec.Containers[0], "C4_SUPPLIED") != "24 fps" || envOf(p.Spec.Containers[0], "C4_NEEDED") != "the delivery frame rate" {
		t.Fatalf("cut's next attempt: %v", p.Spec.Containers[0].Env)
	}
	got = get(t, ns, "briefed")
	if n := got.Status.Needs["1"]; n.Supplied != "24 fps" || n.By != "dana" {
		t.Fatalf("needs %+v", got.Status.Needs)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson); c != nil {
		t.Fatalf("still needs a person: %+v", c)
	}

	// A host runner's unit says it by the pod's reason.
	failNeeding(t, podOfUnit(t, ns, "briefed", 2), *now, "the client's logo files", true)
	tick(t, q)
	if n := get(t, ns, "briefed").Status.Needs["2"]; n.Text != "the client's logo files" {
		t.Fatalf("grade's needs %+v", n)
	}
}
