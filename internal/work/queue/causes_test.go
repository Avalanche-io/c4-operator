package queue

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// failWith ends a pod as the kubelet does when its work container fails:
// code, reason (OOMKilled, Error) and the container's message.
func failWith(t *testing.T, p *corev1.Pod, at time.Time, code int32, reason, msg string) {
	t.Helper()
	ctx := context.Background()
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = corev1.PodFailed
	p.Status.StartTime = &metav1.Time{Time: at.Add(-time.Minute)}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "work", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, Reason: reason, Message: msg, StartedAt: metav1.Time{Time: at.Add(-time.Minute)}, FinishedAt: metav1.Time{Time: at}}}}}
	if err := k8s.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
}

func causeOf(st *c4v1.WorkJobStatus, key string) *c4v1.WorkCause {
	for i := range st.Causes {
		if st.Causes[i].Key == key {
			return &st.Causes[i]
		}
	}
	return nil
}

// A job's failures are grouped by cause: two frames killed for memory on
// one node are one cause, with both units and what the farm did; the node
// that reached the job's limit is a cause of its own, kept off the job.
// Each failed pod counts once.
func TestFailuresGroupByCause(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	j := job(ns, "sh050", "render", "cpu-"+ns, map[string]string{"frames": "1-2", "chunk": "1"}, now.Add(4*time.Hour))
	j.Spec.Retries = ptr.To[int32](0)
	j.Spec.NodeErrorLimit = 2
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	var nodes corev1.NodeList
	if err := k8s.List(ctx, &nodes, client.MatchingLabels{"pool": ns + "-cpu"}); err != nil || len(nodes.Items) != 1 {
		t.Fatalf("nodes %v %v", err, nodes.Items)
	}
	w := nodes.Items[0].Name
	for u := 0; u < 2; u++ {
		p := podOfUnit(t, ns, "sh050", u)
		bind(t, p, w)
		failWith(t, p, now.Add(time.Duration(u)*time.Minute), 137, "OOMKilled", "")
	}
	tick(t, q)
	tick(t, q)
	st := get(t, ns, "sh050").Status
	oom := causeOf(&st, "exit:137:OOMKilled")
	if oom == nil || oom.Kind != "exit" || oom.Title != "exit 137 OOMKilled" || oom.Units != "0-1" || oom.Count != 2 ||
		!slices.Equal(oom.Nodes, []string{w}) || !slices.Contains(oom.Did, "failed") || !slices.Contains(oom.Did, "blacklisted") {
		t.Fatalf("the OOM cause: %+v in %+v", oom, st.Causes)
	}
	node := causeOf(&st, "node:"+w)
	if node == nil || node.Kind != "node" || !slices.Equal(node.Did, []string{"blacklisted"}) || !slices.Contains(st.Blacklist, w) {
		t.Fatalf("the node cause: %+v, blacklist %v", node, st.Blacklist)
	}
	// Seen again before cleanup, the pods add nothing.
	tick(t, q)
	if again := causeOf(&get(t, ns, "sh050").Status, "exit:137:OOMKilled"); again == nil || again.Count != 2 {
		t.Fatalf("counted twice: %+v", again)
	}
}

// Causes survive a restart: status is their record.
func TestCausesLoadFromStatus(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	st := &c4v1.WorkJobStatus{CausesDropped: 3, Causes: []c4v1.WorkCause{
		{Kind: "exit", Key: "exit:137:OOMKilled", Title: "exit 137 OOMKilled", Units: "0-1,5", Count: 4, Sample: 0, First: at, Last: at,
			Nodes: []string{"w1"}, Did: []string{"retried", "failed"}},
		{Kind: "verdict", Key: "verdict:escalate:tables changed", Title: "review: escalate: tables changed", Units: "7", Count: 1, Sample: 7,
			First: at, Last: at, Did: []string{"escalated"}, To: "dana"},
	}}
	m := newMem()
	m.load(&c4v1.WorkJobSpec{}, st)
	got := m.causeStatus()
	if len(got) != 2 || m.causesDropped != 3 || got[0].Units != "0-1,5" || got[1].To != "dana" {
		t.Fatalf("reloaded %+v (dropped %d)", got, m.causesDropped)
	}
	// It goes on growing where it left off.
	m.noteCause("exit", "exit:137:OOMKilled", "exit 137 OOMKilled", 6, "w2", at.Add(time.Hour), "failed")
	c := m.causes["exit:137:OOMKilled"]
	if c.Count != 5 || !c.units[6] || !slices.Equal(c.Nodes, []string{"w1", "w2"}) {
		t.Fatalf("grown %+v", c)
	}
}

// At most causesKept causes: a new one drops the one seen least recently.
func TestCausesAreBounded(t *testing.T) {
	m := newMem()
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	for i := 0; i <= causesKept; i++ {
		m.noteCause("exit", "exit:"+string(rune('a'+i)), "x", i, "", at.Add(time.Duration(i)*time.Minute), "failed")
	}
	if len(m.causes) != causesKept || m.causesDropped != 1 || m.causes["exit:a"] != nil {
		t.Fatalf("%d causes, %d dropped", len(m.causes), m.causesDropped)
	}
}

// A unit's containers leave the tail of their log as their message when
// they write none, and a cause takes its last line: what a program says
// last is why it failed ("no pytest configuration ..."), not the first
// line it logged.
func TestCauseIsTheLastLineSaid(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	j := job(ns, "ci4521", "render", "cpu-"+ns, map[string]string{"frames": "1-1", "chunk": "1"}, now.Add(4*time.Hour))
	j.Spec.Retries = ptr.To[int32](0)
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	p := podOfUnit(t, ns, "ci4521", 0)
	for _, c := range p.Spec.Containers {
		if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
			t.Fatalf("container %s: termination message policy %q", c.Name, c.TerminationMessagePolicy)
		}
	}
	failWith(t, p, *now, 2, "Error", "c4-test: looking for tests in /work\nc4-test: no pytest configuration, tests/ folder or go.mod\n")
	tick(t, q)
	st := get(t, ns, "ci4521").Status
	want := "exit 2: c4-test: no pytest configuration, tests/ folder or go.mod"
	if len(st.Causes) != 1 || st.Causes[0].Title != want {
		t.Fatalf("causes %+v, want the title %q", st.Causes, want)
	}
}
