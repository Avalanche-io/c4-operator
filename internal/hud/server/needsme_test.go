package server

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// The one-person view lists what waits for the viewer and nothing else: a
// wait for them or a party they speak for, not someone else's; bursts
// waiting for approval; pressing first, then by the time left. On a
// single-person farm the installer gets every role's waits, and still no
// other person's.
func TestNeedsMeIsOnlyWhatNeedsMe(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	at := func(m int) *time.Time { x := now.Add(time.Duration(m) * time.Minute); return &x }
	snap := &api.Snapshot{Now: now, Jobs: []api.Job{
		{Namespace: "show", Name: "sh010", Owner: "dana", Waits: []api.Wait{
			{Unit: 2, Name: "ruling", WaitsFor: "person:maya", Ask: "keep the old grade?", Choices: []string{"keep", "regrade"}, Since: at(-30)},
			{Unit: 3, Name: "client", WaitsFor: "party:northlight", Ask: "approve the look?", Since: at(-20)},
			{Unit: 4, Name: "done", WaitsFor: "person:maya", Do: "keep"},
			{Unit: 5, Name: "lead", WaitsFor: "person:wrangler", Ask: "which pool?", Since: at(-10)}}},
		{Namespace: "show", Name: "sh020", Owner: "maya", Needs: []api.Needs{
			{Unit: 0, Name: "cut", Text: "the frame rate", State: "raised", RaisedAt: now.Add(-time.Hour), AnswerBy: at(90)},
			{Unit: 1, Name: "grade", Text: "which LUT", State: "pressing", RaisedAt: now.Add(-time.Minute), AnswerBy: at(20),
				PressingWhy: "sh020 misses dailies unless answered by 09:20"},
			{Unit: 2, Name: "deliver", Text: "the codec", State: "supplied", Supplied: "ProRes"}}},
	}, Capacity: []api.Capacity{{Pool: "spot", Slots: 8, Reason: "sh020 misses", State: "PendingApproval"},
		{Pool: "spot-b", Slots: 4, State: "Provisioning"}}}
	s := &Server{DevViewersOpen: true, DevViewers: map[string]*api.Viewer{}}
	list := func(v *api.Viewer) []api.NeedsMeItem {
		dev := *v
		dev.DevBy = devOpen // as the open test farm resolves ?as=
		return s.needsMe(httptest.NewRequest("GET", "/api/v1/needs-me", nil), &dev, snap).Items
	}
	ids := func(items []api.NeedsMeItem) []string {
		var out []string
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}
	same := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	maya := list(&api.Viewer{User: "maya", All: true, Role: "producer"})
	want := []string{"question:show/sh020/1", "question:show/sh020/0", "decision:show/sh010/2", "burst:spot"}
	if !same(ids(maya), want) {
		t.Fatalf("maya's list %v, want %v", ids(maya), want)
	}
	if q := maya[0]; q.State != "pressing" || q.Severity != "urgent" || q.PressingWhy == "" || q.Actions[0].Path != "/api/v1/jobs/show/sh020/supply" ||
		q.Actions[1].Path != "/api/v1/jobs/show/sh020/needs/1/withdraw" {
		t.Fatalf("the pressing question %+v", q)
	}
	if d := maya[2]; len(d.Actions) != 2 || d.Actions[0].Body["do"] != "keep" || d.Actions[0].Body["unit"] != "ruling" {
		t.Fatalf("the decision %+v", d)
	}

	// Dana owns sh010 but none of its waits names her; a party she speaks
	// for does.
	dana := list(&api.Viewer{User: "dana", All: true, Role: "producer", Parties: []string{"northlight"}})
	if !same(ids(dana), []string{"decision:show/sh010/3", "burst:spot"}) {
		t.Fatalf("dana's list %v", ids(dana))
	}

	// A burst is only for someone who may approve it: an artist may not.
	if artist := list(&api.Viewer{User: "dana", All: true, Role: "artist", Parties: []string{"northlight"}}); !same(ids(artist), []string{"decision:show/sh010/3"}) {
		t.Fatalf("an artist's list %v", ids(artist))
	}

	// The installer of a single-person farm is every role, but not every
	// person: maya's own wait stays hers.
	s.SinglePerson = "noor"
	solo := list(&api.Viewer{User: "noor", All: true, Role: "producer"})
	for _, w := range []string{"decision:show/sh010/5", "question:show/sh020/0"} {
		found := false
		for _, id := range ids(solo) {
			found = found || id == w
		}
		if !found {
			t.Fatalf("the installer's list %v lacks %s", ids(solo), w)
		}
	}
	for _, id := range ids(solo) {
		if id == "decision:show/sh010/2" {
			t.Fatalf("the installer's list %v has maya's own wait", ids(solo))
		}
	}
}
