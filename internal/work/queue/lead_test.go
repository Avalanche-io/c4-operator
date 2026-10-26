package queue

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// leadJob makes a lead's job for session-1, owned by noor and answered by
// its lead, and starts its root.
func leadJob(t *testing.T, ns, name string, q *Queue, now time.Time) {
	t.Helper()
	j := job(ns, name, "graph", "cpu-"+ns, map[string]string{"estimateSeconds": "60"}, now.Add(48*time.Hour))
	j.Annotations = map[string]string{c4v1.AnnotationOwner: "noor", c4v1.AnnotationLeadSession: "session-1"}
	j.Spec.Answers = &c4v1.WorkAnswers{By: c4v1.AnswersByLead}
	j.Spec.Retries = ptr.To[int32](0)
	if err := k8s.Create(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
}

// closeJob lists a close request on a job, as the HUD writes it.
func closeJob(t *testing.T, ns, name string, r c4v1.WorkCloseRequest) {
	t.Helper()
	j := get(t, ns, name)
	if r.Token == "" {
		r.Token = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	b, _ := json.Marshal([]c4v1.WorkCloseRequest{r})
	j.Annotations[c4v1.AnnotationClose] = string(b)
	if err := k8s.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func openCondition(t *testing.T, j *c4v1.WorkJob) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(j.Status.Conditions, c4v1.ConditionOpen)
	if c == nil {
		t.Fatalf("no Open condition: %+v", j.Status.Conditions)
	}
	return c
}

// A lead's job whose workers have all ended stays Running, with nothing to
// place, waiting for its lead: an append is still taken. Closed by its
// person, it finishes as any job does, and takes no more workers. A job
// made for no lead finishes once its units have, as before.
func TestLeadJobStaysOpenUntilClosed(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	leadJob(t, ns, "session-1", q, *now)
	plain := job(ns, "noor-graph", "graph", "cpu-"+ns, map[string]string{"estimateSeconds": "60"}, now.Add(48*time.Hour))
	plain.Annotations = map[string]string{c4v1.AnnotationOwner: "noor"}
	if err := k8s.Create(ctx, plain); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if got := get(t, ns, "session-1"); got.Status.Lead == nil || got.Status.Lead.State != c4v1.LeadWorking || got.Status.Lead.Session != "session-1" {
		t.Fatalf("lead %+v", got.Status.Lead)
	}
	*now = now.Add(time.Minute)
	finish(t, podOfUnit(t, ns, "session-1", 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4root")})
	finish(t, podOfUnit(t, ns, "noor-graph", 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4plain")})
	tick(t, q)
	tick(t, q)

	got := get(t, ns, "session-1")
	l := got.Status.Lead
	if got.Status.Phase != "Running" || got.Status.Finished != nil || l == nil || l.State != c4v1.LeadIdle ||
		l.IdleSince == nil || !l.IdleSince.Time.Equal(*now) || l.ClosesAt == nil || !l.ClosesAt.Time.Equal(now.Add(DefaultLeadIdleClose)) {
		t.Fatalf("idle: phase %s finished %v lead %+v", got.Status.Phase, got.Status.Finished, l)
	}
	if c := openCondition(t, got); c.Status != metav1.ConditionTrue || c.Reason != c4v1.LeadIdle || !strings.Contains(c.Message, "waiting for its lead") {
		t.Fatalf("open %+v", c)
	}
	if p := get(t, ns, "noor-graph"); p.Status.Phase != "Succeeded" || p.Status.Lead != nil || meta.FindStatusCondition(p.Status.Conditions, c4v1.ConditionOpen) != nil {
		t.Fatalf("a job with no lead: phase %s lead %+v", p.Status.Phase, p.Status.Lead)
	}

	// Its lead adds a worker: working again, then idle again once it ends.
	w := putDoc(t, st, `{"version": 1, "dependents": [], "units": [{"name": "worker-1", "work": 60}]}`)
	appendTo(t, ns, "session-1", c4v1.WorkAppendRequest{Key: "k1", Document: w, Units: []string{"worker-1"}, Actor: "agent/session-1 for noor"})
	tick(t, q)
	if got := get(t, ns, "session-1"); got.Status.Units.Total != 2 || got.Status.Lead.State != c4v1.LeadWorking || got.Status.Lead.IdleSince != nil {
		t.Fatalf("after an append: units %+v lead %+v", got.Status.Units, got.Status.Lead)
	}
	*now = now.Add(time.Hour)
	finish(t, podOfUnit(t, ns, "session-1", 1), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4w1")})
	tick(t, q)
	tick(t, q)
	if got := get(t, ns, "session-1"); got.Status.Phase != "Running" || got.Status.Lead.State != c4v1.LeadIdle || !got.Status.Lead.IdleSince.Time.Equal(*now) {
		t.Fatalf("idle again: phase %s lead %+v", got.Status.Phase, got.Status.Lead)
	}

	// Closed by its person: Succeeded, every unit having succeeded.
	*now = now.Add(time.Minute)
	closeJob(t, ns, "session-1", c4v1.WorkCloseRequest{Why: "the feature is done", Actor: "noor", At: metav1.Time{Time: *now}})
	tick(t, q)
	got = get(t, ns, "session-1")
	l = got.Status.Lead
	if got.Status.Phase != "Succeeded" || l.State != c4v1.LeadClosed || l.Closed == nil || l.Closed.By != "noor" ||
		l.Closed.Why != "the feature is done" || !l.Closed.At.Time.Equal(*now) || l.IdleSince != nil {
		t.Fatalf("closed: phase %s lead %+v closed %+v", got.Status.Phase, l, l.Closed)
	}
	if c := openCondition(t, got); c.Status != metav1.ConditionFalse || c.Reason != c4v1.LeadClosed || !strings.Contains(c.Message, "the feature is done") {
		t.Fatalf("open %+v", c)
	}
	// A closed job takes no more workers.
	appendTo(t, ns, "session-1", c4v1.WorkAppendRequest{Key: "k2", Document: w, Units: []string{"worker-2"}, Actor: "noor"})
	tick(t, q)
	got = get(t, ns, "session-1")
	if a := got.Status.Appends; len(a) != 2 || a[1].Units != 0 || !strings.Contains(a[1].Error, "a closed lead's job takes no more workers; make a new job for them") {
		t.Fatalf("an append after close: %+v", a)
	}
	if got.Status.Units.Total != 2 || got.Status.Phase != "Succeeded" {
		t.Fatalf("after a refused append: units %+v phase %s", got.Status.Units, got.Status.Phase)
	}
}

// A lead's job idle for the farm's idle close is closed by the farm, as
// its own signed act, saying since when no worker ran; a restart keeps
// both how long it has been idle and its closing. One whose worker failed
// stays open while it is, and finishes Failed once closed.
func TestLeadJobIdleClose(t *testing.T) {
	ns, q, now, _ := graphFarm(t)
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if q.Signer, err = provenance.NewSigner(context.Background(), provenance.LocalIssuer(ca, provenance.ComponentIdentity("queue"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	leadJob(t, ns, "session-1", q, *now)
	*now = now.Add(time.Minute)
	idle := *now
	finish(t, podOfUnit(t, ns, "session-1", 0), corev1.PodFailed, *now, time.Minute, nil)
	tick(t, q)
	tick(t, q)
	if got := get(t, ns, "session-1"); got.Status.Phase != "Running" || got.Status.Lead.State != c4v1.LeadIdle || got.Status.Units.Failed != 1 {
		t.Fatalf("a failed worker: phase %s lead %+v units %+v", got.Status.Phase, got.Status.Lead, got.Status.Units)
	}

	// Short of the idle close it stays open, across a restart.
	*now = idle.Add(DefaultLeadIdleClose - time.Minute)
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores, Signer: q.Signer}
	tick(t, q)
	if got := get(t, ns, "session-1"); got.Status.Lead.State != c4v1.LeadIdle || !got.Status.Lead.IdleSince.Time.Equal(idle) {
		t.Fatalf("before the idle close: lead %+v", got.Status.Lead)
	}
	*now = idle.Add(DefaultLeadIdleClose)
	tick(t, q)
	got := get(t, ns, "session-1")
	c := got.Status.Lead.Closed
	want := "idle 24h, no worker since " + idle.UTC().Format(time.RFC3339)
	if got.Status.Phase != "Failed" || got.Status.Lead.State != c4v1.LeadClosed || c == nil || c.By != "c4" || c.Why != want || c.Record == "" || c.Control != "" {
		t.Fatalf("idle close: phase %s lead %+v closed %+v", got.Status.Phase, got.Status.Lead, c)
	}

	// The farm's value, when it sets one, is the idle close.
	leadJob(t, ns, "session-2", q, *now)
	q.LeadIdleClose = time.Hour
	*now = now.Add(time.Minute)
	finish(t, podOfUnit(t, ns, "session-2", 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4s2")})
	tick(t, q)
	tick(t, q)
	*now = now.Add(time.Hour)
	tick(t, q)
	if got := get(t, ns, "session-2"); got.Status.Phase != "Succeeded" || got.Status.Lead.Closed == nil || !strings.HasPrefix(got.Status.Lead.Closed.Why, "idle 1h,") {
		t.Fatalf("a farm's idle close: phase %s lead %+v", got.Status.Phase, got.Status.Lead)
	}

	// A restart keeps the closing.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores, Signer: q.Signer}
	tick(t, q)
	if again := get(t, ns, "session-1"); again.Status.Phase != "Failed" || again.Status.Lead.Closed == nil || *again.Status.Lead.Closed != *c {
		t.Fatalf("after a restart: phase %s closed %+v", again.Status.Phase, again.Status.Lead.Closed)
	}
}

// Each question in status says whose it is: the lead's on a job its lead
// answers, else its person's.
func TestQuestionsSayWhoseTheyAre(t *testing.T) {
	ns, q, now, _ := graphFarm(t)
	leadJob(t, ns, "session-1", q, *now)
	plain := job(ns, "noor-graph", "graph", "cpu-"+ns, map[string]string{"estimateSeconds": "60"}, now.Add(48*time.Hour))
	plain.Annotations = map[string]string{c4v1.AnnotationOwner: "noor"}
	if err := k8s.Create(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	*now = now.Add(time.Minute)
	failNeeding(t, podOfUnit(t, ns, "session-1", 0), *now, "which branch to start from", false)
	failNeeding(t, podOfUnit(t, ns, "noor-graph", 0), *now, "which branch to start from", false)
	tick(t, q)
	tick(t, q)
	if n := get(t, ns, "session-1").Status.Needs["0"]; n.Text != "which branch to start from" || n.For != "lead:session-1" {
		t.Fatalf("a lead's question: %+v", n)
	}
	if n := get(t, ns, "noor-graph").Status.Needs["0"]; n.For != "person:noor" {
		t.Fatalf("a person's question: %+v", n)
	}
}
