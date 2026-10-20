package queue

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// steerUnit adds a message to a job's AnnotationSteer list, as the HUD or
// MCP does.
func steerUnit(t *testing.T, ns, name, unit, text string, at time.Time) {
	t.Helper()
	j := get(t, ns, name)
	var reqs []c4v1.WorkSteerRequest
	if a := j.Annotations[c4v1.AnnotationSteer]; a != "" {
		if err := json.Unmarshal([]byte(a), &reqs); err != nil {
			t.Fatal(err)
		}
	}
	reqs = append(reqs, c4v1.WorkSteerRequest{Token: strconv.FormatInt(at.UnixNano(), 36), Unit: unit, Text: text, Actor: "dana", At: metav1.Time{Time: at}})
	b, _ := json.Marshal(reqs)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationSteer] = string(b)
	if err := k8s.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func podSteers(t *testing.T, ns, job string, idx int) []unitSteer {
	t.Helper()
	p := podOfUnit(t, ns, job, idx)
	var out []unitSteer
	if a := p.Annotations[c4v1.AnnotationUnitSteer]; a != "" {
		if err := json.Unmarshal([]byte(a), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// A message reaches a running unit's pod, numbered for the unit, and
// leaves it once the unit has read it; a unit not running yet gets its
// messages on its first pod. The messages survive a restart, each carried
// out once.
func TestSteerARunningUnit(t *testing.T) {
	ns, q, now := graphJob(t, "steered", func(time.Time) string {
		return `{"name": "lane"}, {"name": "next", "after": ["lane"]}`
	}, 24*time.Hour)
	// units: 0 root, 1 lane, 2 next
	ctx := context.Background()
	steerUnit(t, ns, "steered", "lane", "skip the lighting pass", *now)
	tick(t, q)
	if got := podSteers(t, ns, "steered", 1); len(got) != 1 || got[0].Seq != 1 || got[0].Text != "skip the lighting pass" || got[0].Actor != "dana" {
		t.Fatalf("lane's pod carries %+v", got)
	}
	st := get(t, ns, "steered").Status
	if len(st.Steers) != 1 || st.Steers[0].Name != "lane" || st.Steers[0].Delivered == nil || st.Steers[0].Seen {
		t.Fatalf("steers %+v", st.Steers)
	}

	// A second message, then the unit reads both.
	steerUnit(t, ns, "steered", "lane", "and use the v3 plates", now.Add(time.Second))
	tick(t, q)
	if got := podSteers(t, ns, "steered", 1); len(got) != 2 || got[1].Seq != 2 {
		t.Fatalf("lane's pod carries %+v", got)
	}
	p := podOfUnit(t, ns, "steered", 1)
	patch := client.MergeFrom(p.DeepCopy())
	p.Annotations[c4v1.AnnotationSteerSeen] = "2"
	if err := k8s.Patch(ctx, p, patch); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if got := podSteers(t, ns, "steered", 1); len(got) != 0 {
		t.Fatalf("read messages kept on the pod: %+v", got)
	}
	if st := get(t, ns, "steered").Status; !st.Steers[0].Seen || !st.Steers[1].Seen {
		t.Fatalf("steers %+v", st.Steers)
	}

	// A message to a unit that has not started waits for its pod.
	steerUnit(t, ns, "steered", "next", "start from frame 1040", now.Add(2*time.Second))
	tick(t, q)
	if st := get(t, ns, "steered").Status; len(st.Steers) != 3 || st.Steers[2].Delivered != nil || st.Steers[2].Seq != 1 {
		t.Fatalf("steers %+v", st.Steers)
	}
	// A restart keeps the messages and does not carry any out again.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	if st := get(t, ns, "steered").Status; len(st.Steers) != 3 {
		t.Fatalf("after a restart: steers %+v", st.Steers)
	}
	finishUnit(t, now, ns, "steered", 1, nil)
	tick(t, q)
	if got := podSteers(t, ns, "steered", 2); len(got) != 1 || got[0].Text != "start from frame 1040" {
		t.Fatalf("next's first pod carries %+v", got)
	}
	if st := get(t, ns, "steered").Status; st.Steers[2].Delivered == nil {
		t.Fatalf("steers %+v", st.Steers)
	}
}
