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

// A grant's end is said as every time is, local with UTC beside it, not
// in the farm's UTC alone.
func TestGrantSaysItsEndLocally(t *testing.T) {
	until := time.Date(2026, 11, 20, 14, 42, 58, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.GrantResponse{Message: "sandbox may submit your work until " + until.Format(time.RFC3339),
			Grant: api.Grant{Namespace: "saltmarsh", Name: "sandbox", Until: until}})
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	var out bytes.Buffer
	la, _ := time.LoadLocation("America/Los_Angeles")
	o := Out{W: &out, Clock: Clock{Loc: la, Now: func() time.Time { return until.Add(-24 * time.Hour) }}}
	if err := Grant(context.Background(), c, o, "saltmarsh", api.GrantRequest{Tracker: "sandbox"}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); strings.Contains(s, "2026-11-20T14:42:58Z") || !strings.Contains(s, "14:42 UTC") {
		t.Errorf("grant:\n%s", s)
	}
}

// c4 farm grant TRACKER with no project named takes the project the
// tracker belongs to when only one has it, and names the projects when
// more than one does; a project named is used as named.
func TestGrantFindsTheTrackersProject(t *testing.T) {
	trackers := []api.Tracker{{Namespace: "saltmarsh", Name: "sandbox", Project: "saltmarsh"}, {Namespace: "glassworks", Name: "flow", Project: "glassworks"}}
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/trackers":
			json.NewEncoder(w).Encode(api.TrackerList{Trackers: trackers})
		case "/api/v1/grants":
			var req api.GrantRequest
			json.NewDecoder(r.Body).Decode(&req)
			asked = append(asked, req.Project)
			json.NewEncoder(w).Encode(api.GrantResponse{Message: "granted", Grant: api.Grant{Namespace: req.Project, Name: req.Tracker}})
		}
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "noor"})
	ctx := context.Background()
	o := Out{W: &bytes.Buffer{}}
	if err := Grant(ctx, c, o, "", api.GrantRequest{Tracker: "sandbox"}); err != nil {
		t.Fatal(err)
	}
	if err := Grant(ctx, c, o, "glassworks", api.GrantRequest{Tracker: "sandbox"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "saltmarsh,glassworks" {
		t.Errorf("granted in %v", asked)
	}
	trackers = append(trackers, api.Tracker{Namespace: "copperline", Name: "sandbox", Project: "copperline"})
	if err := Grant(ctx, c, o, "", api.GrantRequest{Tracker: "sandbox"}); err == nil || err.Error() != "tracker sandbox is in more than one project (copperline, saltmarsh): name one with --project" {
		t.Errorf("two projects: %v", err)
	}
}
