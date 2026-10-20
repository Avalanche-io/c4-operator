package queue

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// askAt does what the node daemon does when a unit asks for a wait at safe
// point seq (c4-unit wait).
func askAt(t *testing.T, p *corev1.Pod, seq int, at time.Time, wait string) {
	t.Helper()
	ctx := context.Background()
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(spNote{Seq: seq, At: at, Wait: json.RawMessage(wait)})
	patch := client.MergeFrom(p.DeepCopy())
	p.Annotations[c4v1.AnnotationSafepoint] = string(data)
	if err := k8s.Patch(ctx, p, patch); err != nil {
		t.Fatal(err)
	}
}

// A running unit opens a wait for itself at a safe point: the farm tells it
// to stop there, it yields and holds no slot while the wait is open (needs
// a person, in the ETA, in the book), and survives a restart. Decided, it
// runs again, resuming, with the decision in $C4_DECISION and the reply
// mounted at $C4_REPLY. A wait the farm cannot open is refused with a
// continue that says why.
func TestBlockOpensAWait(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	ctx := context.Background()
	q.Safepoints = &SafepointCache{}
	j := job(ns, "sh030", "render", "cpu-"+ns, map[string]string{"frames": "1-2", "estimateSeconds": "600"}, now.Add(time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	p0, p1 := podOfUnit(t, ns, "sh030", 0), podOfUnit(t, ns, "sh030", 1)

	// A wait the farm cannot open: continue, saying why.
	askAt(t, p1, 1, *now, `{"server": "oracle"}`)
	tick(t, q)
	if d, _ := answerOf(t, p1); d.Seq != 1 || d.Do != "continue" || !strings.Contains(d.Why, "want person") {
		t.Fatalf("refused wait: %+v", d)
	}

	askAt(t, p0, 1, *now, `{"server": "person:owner", "ask": "keep the grade?", "choices": ["keep", "redo"], "reply": true, "expect": 7200}`)
	tick(t, q)
	d, p0 := answerOf(t, p0)
	if d.Seq != 1 || d.Do != "stop" || d.Why != "waits for person:owner" || p0.Annotations[c4v1.AnnotationYield] != yieldWait {
		t.Fatalf("the ask: %+v, yield %q", d, p0.Annotations[c4v1.AnnotationYield])
	}
	mem := q.jobs[j.UID]
	if n := len(mem.decisions); n == 0 || mem.decisions[n-1].Kind != "safepoint-wait" {
		t.Fatalf("the ask was not recorded: %+v", mem.decisions)
	}

	// It yields at the safe point, with what it saved.
	saved := viewID("c4grade-turn-1")
	*now = now.Add(time.Minute)
	finish(t, p0, corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: saved})
	tick(t, q)
	tick(t, q)
	check := func(when string) {
		t.Helper()
		got := get(t, ns, "sh030")
		if hasPod(t, ns, "sh030", 0) || !slices.Equal(states(t, got, "waits"), []int{0}) || got.Status.Resume["0"] != saved {
			t.Fatalf("%s: pod %v, states %v, resume %v", when, hasPod(t, ns, "sh030", 0), got.Status.UnitStates, got.Status.Resume)
		}
		if len(got.Status.Waits) != 1 {
			t.Fatalf("%s: waits %+v", when, got.Status.Waits)
		}
		w := got.Status.Waits[0]
		if w.Unit != 0 || !w.Block || w.Safepoint != 1 || w.Server != "person:owner" || w.Ask != "keep the grade?" ||
			!slices.Equal(w.Choices, []string{"keep", "redo"}) || !w.Reply || !w.Blocking || w.ExpectSeconds != 7200 {
			t.Fatalf("%s: wait %+v", when, w)
		}
		if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson); c == nil || c.Reason != c4v1.NeedsDecision {
			t.Fatalf("%s: needs person %+v", when, c)
		}
		if e := got.Status.Estimate; e == nil || e.ETA50 == nil || e.ETA50.Time.Before(now.Add(2*time.Hour)) {
			t.Fatalf("%s: ETA %+v, want past %s", when, got.Status.Estimate, now.Add(2*time.Hour))
		}
	}
	check("waiting")

	// A restart rebuilds the wait from status.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait,
		Stores: q.Stores, Safepoints: q.Safepoints}
	tick(t, q)
	check("after a restart")

	// Not one of its choices, and no reply: it still waits.
	*now = now.Add(time.Hour)
	decideReply(t, ns, "sh030", "0", "approve", viewID("c4note"), *now)
	decide(t, ns, "sh030", "0", "keep", now.Add(time.Second))
	tick(t, q)
	if got := get(t, ns, "sh030"); !slices.Equal(states(t, got, "waits"), []int{0}) || got.Status.Waits[0].Do != "" {
		t.Fatalf("decided without a choice or a reply: %+v", got.Status.Waits)
	}

	reply := viewID("c4keep-it-warmer")
	decideReply(t, ns, "sh030", "0", "keep", reply, now.Add(2*time.Second))
	tick(t, q)
	if hasPod(t, ns, "sh030", 0) {
		t.Fatal("relaunched before the reply and the resume view were ready")
	}
	readyMaterialization(t, ns, resumeName("sh030", 0), saved)
	readyMaterialization(t, ns, replyName("sh030", 0), reply)
	tick(t, q)
	p := podOfUnit(t, ns, "sh030", 0)
	var dec map[string]any
	if err := json.Unmarshal([]byte(envOf(p.Spec.Containers[0], "C4_DECISION")), &dec); err != nil {
		t.Fatalf("C4_DECISION %q: %v", envOf(p.Spec.Containers[0], "C4_DECISION"), err)
	}
	if dec["do"] != "keep" || dec["safepoint"] != 1.0 || dec["view"] != reply || dec["reply"] != ReplyMount || dec["actor"] != "owner" {
		t.Fatalf("C4_DECISION %v", dec)
	}
	var mounted bool
	for _, vol := range p.Spec.Volumes {
		mounted = mounted || (vol.CSI != nil && vol.CSI.VolumeAttributes[c4v1.VolumeAttrMaterialization] == replyName("sh030", 0))
	}
	if !mounted || envOf(p.Spec.Containers[0], "C4_REPLY") != ReplyMount {
		t.Fatalf("the resumed pod: reply mounted %v, env %v", mounted, p.Spec.Containers[0].Env)
	}
	got := get(t, ns, "sh030")
	if w := got.Status.Waits[0]; w.Do != "keep" || w.View != reply || w.Blocking {
		t.Fatalf("decided wait %+v", w)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson); c != nil {
		t.Fatalf("still needs a person: %+v", c)
	}
}
