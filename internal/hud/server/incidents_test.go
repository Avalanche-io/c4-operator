package server

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Incidents group by root cause: the same failure on two jobs is one
// item; a decision that is the viewer's needs them, another person's does
// not; three machines of a rack that stopped reporting, with the units
// they interrupted, are one handled item. Needs come first.
func TestIncidents(t *testing.T) {
	now := time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	since := now.Add(-20 * time.Minute)
	failed := func(name, msg string, n int) api.Job {
		return api.Job{Namespace: "show-a", Name: name, Class: "render", Units: api.Counts{Failed: n}, UnitStates: api.States{Failed: "1001-1004"},
			NeedsPerson: &api.NeedsPerson{Reason: "failed", Message: msg, Since: since}}
	}
	jobs := []api.Job{
		failed("sh010-light-v003", "4 frames failed: exit 137 on node r12-07 (out of memory)", 4),
		failed("sh020-light-v001", "2 frames failed: exit 137 on node r12-09 (out of memory)", 2),
		{Namespace: "show-a", Name: "sh030-comp-v002", Class: "render", NeedsPerson: &api.NeedsPerson{Reason: "decision", Message: "waits for a ruling", Since: since},
			Waits: []api.Wait{{WaitsFor: "person:maya", Ask: "keep the old grade?", NeedsYou: true}}},
		{Namespace: "show-a", Name: "sh040-comp-v001", Class: "render", NeedsPerson: &api.NeedsPerson{Reason: "decision", Message: "waits for a ruling", Since: since},
			Waits: []api.Wait{{WaitsFor: "person:ben", NeedsYou: true}}},
		{Namespace: "show-a", Name: "sh050-fx-v001", Class: "render"},
	}
	m := &model{at: now}
	for i := 1; i <= 3; i++ {
		m.nodes = append(m.nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("r7-%02d", i)},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown,
				LastTransitionTime: metav1.Time{Time: now.Add(-10 * time.Minute)}}}}})
	}
	rec := &jobRec{key: "show-a/sh050-fx-v001"}
	for i := range 6 {
		rec.job.Status.Interruptions = append(rec.job.Status.Interruptions, c4v1.WorkInterruption{Unit: int32(i),
			At: metav1.Time{Time: now.Add(-9 * time.Minute)}, Reason: "NodeLost", Node: fmt.Sprintf("r7-%02d", 1+i%3)})
	}
	m.jobs = []*jobRec{rec}

	items := incidents(m, &api.Viewer{User: "maya"}, jobs)
	if len(items) != 4 {
		for _, it := range items {
			t.Logf("%+v", it)
		}
		t.Fatalf("%d incidents, want 4", len(items))
	}
	f := items[0]
	if f.ID != "failed:show-a/sh010-light-v003" || !f.NeedsYou || f.Count != 6 || f.CountOf != "frames" || len(f.Jobs) != 2 ||
		f.Title != "6 frames on 2 jobs failed after their retries" || len(f.Checklist) != 2 || f.State != "open" {
		t.Fatalf("failures: %+v", f)
	}
	var mine, theirs, rack *api.Incident
	for i := range items {
		switch items[i].ID {
		case "decision:show-a/sh030-comp-v002":
			mine = &items[i]
		case "decision:show-a/sh040-comp-v001":
			theirs = &items[i]
		case "rack:r7":
			rack = &items[i]
		}
	}
	if mine == nil || !mine.NeedsYou || mine.Title != "sh030-comp-v002 waits for your decision" || mine.Detail != "keep the old grade?" {
		t.Fatalf("the viewer's decision: %+v", mine)
	}
	if theirs == nil || theirs.NeedsYou || theirs.Title != "sh040-comp-v001 waits for ben's decision" {
		t.Fatalf("another's decision: %+v", theirs)
	}
	if rack == nil || rack.NeedsYou || rack.Count != 6 || rack.Title != "3 machines in r7 stopped reporting" || len(rack.Jobs) != 1 || items[len(items)-1].ID != "rack:r7" {
		t.Fatalf("rack: %+v", rack)
	}
	// Someone took the failures: acknowledged.
	for i := range 2 {
		jobs[i].Acks = []api.Ack{{Kind: "acknowledge", Actor: "ben", At: now}}
	}
	if it := incidents(m, &api.Viewer{User: "maya"}, jobs)[0]; it.State != "acknowledged" {
		t.Fatalf("after acks: %+v", it)
	}
}
