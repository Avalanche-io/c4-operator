package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// c4 farm ls lists every job the snapshot holds, a stuck one with its
// line, Running and Blocked alike, with or without --all.
func TestLsListsEveryJob(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	dl := now.Add(time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Snapshot{Jobs: []api.Job{
			{Namespace: "saltmarsh", Name: "sm0170-depth-v001", Phase: "Running", CreatedAt: now, Owner: "noor",
				NeedsPerson: &api.NeedsPerson{Reason: "quota", Message: "the namespace's quota is used up"}},
			{Namespace: "saltmarsh", Name: "sm0170-light-v002", Phase: "Running", CreatedAt: now, Owner: "noor", Rank: 1,
				Objective: api.Objective{Deadline: &dl}},
			{Namespace: "saltmarsh", Name: "sm0170-comp-v001", Phase: "Blocked", CreatedAt: now, Owner: "noor"},
			{Namespace: "saltmarsh", Name: "sm0170-review-v001", Phase: "Blocked", CreatedAt: now, Owner: "noor"},
		}})
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	for _, all := range []bool{false, true} {
		var out bytes.Buffer
		if err := Ls(context.Background(), c, Out{W: &out}, LsArgs{All: all}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"sm0170-depth-v001", "sm0170-light-v002", "sm0170-comp-v001", "sm0170-review-v001"} {
			if strings.Count(out.String(), name) < 1 || !strings.Contains(out.String(), "saltmarsh/"+name+" ") {
				t.Errorf("all=%v: no row for %s:\n%s", all, name, out.String())
			}
		}
	}
}

// A late job's slack reads "~2h 34m late": "late" carries the sign, so the
// span has none.
func TestLsLateSlackHasNoMinus(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	dl := now.Add(-time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Snapshot{Jobs: []api.Job{
			{Namespace: "saltmarsh", Name: "sm0170-depth-v001", Phase: "Running", CreatedAt: now, Owner: "noor",
				Objective: api.Objective{Deadline: &dl},
				Estimate:  api.Estimate{EtaP50: now.Add(94 * time.Minute), SlackSeconds: -(2*3600 + 34*60)}},
		}})
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	var out bytes.Buffer
	if err := Ls(context.Background(), c, Out{W: &out}, LsArgs{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "~2h 34m late") || strings.Contains(out.String(), "~-") {
		t.Errorf("want ~2h 34m late, no minus:\n%s", out.String())
	}
}

// A job the queue has not planned yet reads "planning", not the snapshot's
// own time as if it were ready now; a deadline the farm will suggest reads
// "suggested", and another reason for no time reads in its own word.
func TestLsSaysPlanningBeforeThePlan(t *testing.T) {
	now := time.Now().UTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Snapshot{Jobs: []api.Job{
			{Namespace: "saltmarsh", Name: "sm0170-depth-v001", Phase: "Pending", CreatedAt: now, Owner: "noor",
				Objective: api.Objective{Mode: "Suggested"},
				Estimate:  api.Estimate{EtaP50: now, EtaP90: now, NoETA: "no estimate yet", Feasible: true}},
			{Namespace: "saltmarsh", Name: "sm0170-comp-v001", Phase: "Blocked", CreatedAt: now, Owner: "noor",
				Estimate: api.Estimate{EtaP50: now, EtaP90: now, NoETA: "held"}},
		}})
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	var out bytes.Buffer
	if err := Ls(context.Background(), c, Out{W: &out, Clock: Clock{Now: func() time.Time { return now }}}, LsArgs{}); err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(out.String(), "\n")
	if len(rows) < 3 || !strings.Contains(rows[1], " planning ") || !strings.Contains(rows[1], " suggested ") || !strings.Contains(rows[2], " held ") || strings.Contains(out.String(), "~") {
		t.Errorf("rows:\n%s", out.String())
	}
}

// deadline says the new deadline once, on this machine's clock.
func TestDeadlineIsSaidOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(api.Snapshot{Jobs: []api.Job{{Namespace: "saltmarsh", Name: "sm0140-render-v001", Project: "saltmarsh"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "sm0140-render-v001: deadline Wed 00:43 PDT (07:43 UTC)"})
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	var out bytes.Buffer
	if err := Deadline(context.Background(), c, Out{W: &out}, "saltmarsh", "sm0140-render-v001", "+45m"); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "deadline") != 1 || !strings.HasPrefix(out.String(), "deadline  ") {
		t.Errorf("said:\n%s", out.String())
	}
}
