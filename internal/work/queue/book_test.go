package queue

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// The changes between two statuses, each once, with keys that name them;
// none between a status and itself.
func TestTransitionsOfAStatus(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	later := metav1.NewTime(at.Add(time.Hour))
	before := &c4v1.WorkJobStatus{Phase: "Pending"}
	after := &c4v1.WorkJobStatus{Phase: "Running", Started: &at,
		Waits:  []c4v1.WorkWait{{Unit: 7, Name: "review", Server: "person:dana", Escalated: true, Since: &at}},
		Acks:   []c4v1.WorkAck{{Kind: "assign", Units: "3", Actor: "lee", Assignee: "dana", At: at}},
		Causes: []c4v1.WorkCause{{Kind: "exit", Key: "exit:137:OOMKilled", Title: "exit 137 OOMKilled", Units: "3", Count: 1, Last: at, Did: []string{"retried"}}},
		Needs:  map[string]c4v1.WorkNeeds{"4": {Text: "which take?", At: at}},
	}
	kinds := func(ts []api.Transition) []string {
		var out []string
		for _, x := range ts {
			out = append(out, x.Kind)
		}
		return out
	}
	ts := transitions("show-a/sh040", "show-a", before, after, at.Time)
	if got := strings.Join(kinds(ts), ","); got != "started,phase,wait-opened,ack,needs,cause" {
		t.Fatalf("transitions: %s", got)
	}
	if ts[2].Message != "escalated to person:dana" || *ts[2].Unit != 7 || ts[2].Key != "show-a/sh040/wait-opened/7/2026-10-20T09:00:00Z" {
		t.Fatalf("the escalation: %+v", ts[2])
	}
	if again := transitions("show-a/sh040", "show-a", after, after, later.Time); len(again) != 0 {
		t.Fatalf("no change, but %v", kinds(again))
	}

	// The escalation is decided, the cause grows, the information comes,
	// and the job finishes.
	done := after.DeepCopy()
	done.Phase, done.Finished = "Succeeded", &later
	done.Waits[0].Do, done.Waits[0].Actor, done.Waits[0].At, done.Waits[0].Record = "accept", "dana", &later, "c4decision"
	done.Causes[0].Count, done.Causes[0].Units = 2, "3-4"
	n := done.Needs["4"]
	n.Supplied, n.By = "take 2", "pat"
	done.Needs["4"] = n
	ts = transitions("show-a/sh040", "show-a", after, done, later.Time)
	if got := strings.Join(kinds(ts), ","); got != "phase,finished,wait-decided,supplied,cause" {
		t.Fatalf("transitions: %s", got)
	}
	if d := ts[2]; d.Do != "accept" || d.Actor != "dana" || d.Record != "c4decision" {
		t.Fatalf("the decision: %+v", d)
	}
}

// The log is numbered farm-wide, kept in the operator's namespace, and a
// full tail is sealed into the project's store, each segment naming the
// one before. A second leader writing from a stale head is refused, and
// the numbering goes on without a fork.
func TestTransitionLogSealsAndRefusesAStaleWriter(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	q.Namespace = ns
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	q.Stores = func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
	seal := bookSeal
	bookSeal = 3
	t.Cleanup(func() { bookSeal = seal })

	batch := func(q *Queue, n int) error {
		var ts []api.Transition
		for i := 0; i < n; i++ {
			ts = append(ts, api.Transition{Project: "show-a", Job: "show-a/j", Kind: "phase", Message: "x", Key: now.Format(time.RFC3339Nano)})
		}
		return q.appendBook(ctx, ts)
	}
	if err := batch(q, 2); err != nil {
		t.Fatal(err)
	}
	if err := batch(q, 2); err != nil {
		t.Fatal(err)
	}
	head := func() *BookHead {
		var cm corev1.ConfigMap
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: BookConfigMap}, &cm); err != nil {
			t.Fatal(err)
		}
		h, err := ReadBookHead(cm.Data)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := head()
	if h.Next != 5 || len(h.Segments["show-a"]) != 1 || len(h.Tails["show-a"]) != 0 {
		t.Fatalf("head after 4: next %d, segments %+v, tail %d", h.Next, h.Segments, len(h.Tails["show-a"]))
	}
	first := h.Segments["show-a"][0]
	if first.First != 1 || first.Last != 4 {
		t.Fatalf("segment %+v", first)
	}

	// Another leader, holding the head as it was before, cannot write.
	stale := &Queue{Client: k8s, Log: q.Log, Namespace: ns, Stores: q.Stores, tlog: &bookLog{head: cloneHead(h)}}
	var cm corev1.ConfigMap
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: BookConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	stale.tlog.cm = cm.DeepCopy()
	if err := batch(q, 1); err != nil {
		t.Fatal(err)
	}
	if err := batch(stale, 1); err == nil {
		t.Fatal("a stale leader wrote the log")
	}
	// Reading the head again, it goes on from where the log is.
	if err := batch(stale, 3); err != nil {
		t.Fatal(err)
	}
	h = head()
	if h.Next != 9 || len(h.Segments["show-a"]) != 2 {
		t.Fatalf("head after the stale writer: next %d, segments %+v", h.Next, h.Segments["show-a"])
	}
	second := h.Segments["show-a"][1]
	id, _ := c4.Parse(second.ID)
	raw, err := c4view.ReadObject(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var seg BookSegmentObject
	if err := json.Unmarshal(raw, &seg); err != nil {
		t.Fatal(err)
	}
	if seg.Prev != first.ID || seg.First != 5 || seg.Last != 8 || len(seg.Entries) != 4 {
		t.Fatalf("second segment: prev %s (want %s), %d-%d, %d entries", seg.Prev, first.ID, seg.First, seg.Last, len(seg.Entries))
	}
}

// A job's own changes reach the log as the queue writes its status.
func TestJobTransitionsReachTheLog(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	q.Namespace = ns
	j := job(ns, "sh070", "render", "cpu-"+ns, map[string]string{"frames": "1-1"}, now.Add(4*time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	for _, p := range unitPods(t, ns, "sh070") {
		finish(t, &p, corev1.PodSucceeded, *now, time.Minute, nil)
	}
	tick(t, q)
	tick(t, q)
	var cm corev1.ConfigMap
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: BookConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	h, err := ReadBookHead(cm.Data)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var last int64
	for _, ts := range h.Tails {
		for _, x := range ts {
			if x.Job != ns+"/sh070" {
				continue
			}
			if x.Seq <= last {
				t.Fatalf("out of order: %d after %d", x.Seq, last)
			}
			last = x.Seq
			kinds = append(kinds, x.Kind)
		}
	}
	got := strings.Join(kinds, ",")
	// (The test's pods end before the queue sees them run, so there is
	// no start here; TestTransitionsOfAStatus covers it.)
	if got != "phase,phase,finished" {
		t.Fatalf("the job's transitions: %s", got)
	}
}
