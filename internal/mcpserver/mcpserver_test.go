package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// fakeHUD serves a fixed farm and records control requests.
type fakeHUD struct {
	mu    sync.Mutex
	posts []string
	auth  []string
}

func (f *fakeHUD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	due := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)
	next := due.Add(4 * time.Hour)
	job := func(name string, rank int, feasible bool, slack float64) api.Job {
		eta := due.Add(time.Duration(-slack) * time.Second)
		return api.Job{Namespace: "show-a", Name: name, Project: "show-a", Class: "render", Phase: "Running", Rank: rank,
			Units: api.Counts{Total: 96, Succeeded: 40, Running: 8},
			Objective: api.Objective{Type: "deadline", Deadline: &due, Source: "suggested", Mode: "Suggested",
				Suggestion: &api.Suggestion{Deadline: due, State: "Settled", Policy: "sessions", Quantile: 0.8, NextSession: &next,
					History: []api.SuggestionRevision{{At: due.Add(-2 * time.Hour), Deadline: due, UnitsMeasured: 1, Record: "c4rec"}}}},
			Estimate: api.Estimate{Feasible: feasible, SlackSeconds: slack, EtaP50: eta.Add(-time.Hour), EtaP80: &eta, EtaP90: eta,
				EtaP95: &eta, Calibrated: true}}
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v1/snapshot":
		s := api.Snapshot{Now: due.Add(-time.Hour), Viewer: &api.Viewer{User: "maya", Projects: []string{"show-a"}},
			Farm: api.Farm{SlotsUsed: 12, SlotsTotal: 16, QueuedUnits: 40},
			Jobs: []api.Job{job("sh010", 1, true, 1200), job("sh020", 2, false, -300), job("sh030", 3, true, 60)}}
		json.NewEncoder(w).Encode(s)
	case r.Method == "GET" && r.URL.Path == "/api/v1/jobs/show-a/sh010":
		b, _ := json.Marshal(job("sh010", 1, true, 1200))
		var m map[string]any
		json.Unmarshal(b, &m)
		m["unitList"] = []map[string]any{{"i": 0, "label": "1001", "state": "succeeded", "attempts": 1}, {"i": 1, "label": "1002", "state": "running", "attempts": 1, "node": "w1"}}
		json.NewEncoder(w).Encode(m)
	case r.Method == "GET" && r.URL.Path == "/api/v1/jobs/show-a/sh010/deadline-options":
		out := api.DeadlineOptions{Job: "show-a/sh010", Now: due.Add(-time.Hour), Policy: "achievable", Quantile: 0.8,
			Floor: due.Add(-30 * time.Minute), FloorWhy: "its longest remaining unit alone takes 30m", Options: []api.DeadlineOption{}}
		for _, a := range r.URL.Query()["at"] {
			at, _ := time.Parse(time.RFC3339, a)
			out.Options = append(out.Options, api.DeadlineOption{At: at, Meets: 50, Boost: 1.3, Displaces: 2,
				Displaced: []string{"show-a/sh020"}, Unnamed: 1,
				Burst: &api.Burst{Slots: map[string]int{"spot": 4}, Pools: []string{"spot"}, Cost: 12, Closes: true}})
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "GET" && r.URL.Path == "/api/v1/book":
		u := 3
		json.NewEncoder(w).Encode(api.Book{Entries: []api.BookEntry{{Seq: 1, At: due.Add(-time.Hour), Actor: "node/w1", Signer: "node", Kind: "failed",
			Object: api.BookObject{Type: "unit", Job: r.URL.Query().Get("job"), Unit: &u}, Message: "1004 failed on w1 (exit 137): OOMKilled"}}})
	case r.Method == "POST" && r.URL.Path == "/api/v1/jobs":
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posts = append(f.posts, r.URL.Path+" "+string(body))
		f.mu.Unlock()
		var req api.SubmitRequest
		json.Unmarshal(body, &req)
		eta := due.Add(-3 * time.Hour)
		json.NewEncoder(w).Encode(api.SubmitResponse{OK: true, DryRun: req.DryRun, Namespace: "show-a", Name: "sm0140-render-v001",
			Message: "sm0140-render-v001: planned, not submitted",
			Estimate: &api.SubmitEstimate{Units: 48, ETA50: &eta, ETA90: &eta, Deadline: &due, DeadlineSource: "suggested",
				SlackSeconds: 10800, Feasible: true}})
	case r.Method == "POST":
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posts = append(f.posts, r.URL.Path+" "+string(body))
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "sh010: held"})
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "no job " + r.URL.Path})
	}
}

func session(t *testing.T, hud http.Handler, token string) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(hud)
	t.Cleanup(ts.Close)
	ctx := context.Background()
	srv := New(&Client{Base: ts.URL, Token: token})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &out)
	}
	return res, out
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestToolsAreNamedAndAnnotated(t *testing.T) {
	cs := session(t, &fakeHUD{}, "")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != len(Tools(nil)) {
		t.Fatalf("%d tools listed, %d defined", len(res.Tools), len(Tools(nil)))
	}
	for _, tl := range res.Tools {
		if !strings.HasPrefix(tl.Name, "c4_") || tl.Annotations == nil || tl.Annotations.Title == "" || tl.Description == "" {
			t.Errorf("tool %s lacks the c4_ prefix, a title, a description or annotations", tl.Name)
		}
		readOnly := strings.HasPrefix(tl.Name, "c4_list") || strings.HasPrefix(tl.Name, "c4_get") || tl.Name == "c4_farm_status" ||
			tl.Name == "c4_cache_tiers" || tl.Name == "c4_capacity_and_cost" || tl.Name == "c4_dependency_graph" || tl.Name == "c4_diff_views" || tl.Name == "c4_trace_file" || tl.Name == "c4_verify_custody" || tl.Name == "c4_read_book" ||
			tl.Name == "c4_suggest_deadline" || tl.Name == "c4_deadline_options"
		if tl.Annotations.ReadOnlyHint != readOnly {
			t.Errorf("tool %s: readOnlyHint %v", tl.Name, tl.Annotations.ReadOnlyHint)
		}
		if tl.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tl.Name)
		}
	}
}

func TestFarmStatusNamesJobsAtRisk(t *testing.T) {
	cs := session(t, &fakeHUD{}, "")
	_, out := call(t, cs, "c4_farm_status", map[string]any{})
	risk, _ := out["atRisk"].([]any)
	if out["viewer"] != "maya" || len(risk) != 1 || risk[0] != "show-a/sh020" {
		t.Fatalf("farm status %v", out)
	}
}

func TestListJobsPaginates(t *testing.T) {
	cs := session(t, &fakeHUD{}, "")
	_, out := call(t, cs, "c4_list_jobs", map[string]any{"limit": 2})
	jobs, _ := out["jobs"].([]any)
	if len(jobs) != 2 || out["total"].(float64) != 3 || out["nextOffset"].(float64) != 2 {
		t.Fatalf("first page %v", out)
	}
	_, out = call(t, cs, "c4_list_jobs", map[string]any{"limit": 2, "offset": 2})
	jobs, _ = out["jobs"].([]any)
	if len(jobs) != 1 || out["nextOffset"] != nil {
		t.Fatalf("second page %v", out)
	}
	_, out = call(t, cs, "c4_list_jobs", map[string]any{"at_risk_only": true})
	jobs, _ = out["jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["job"] != "show-a/sh020" {
		t.Fatalf("at risk %v", out)
	}
}

func TestErrorsSayWhatToDoNext(t *testing.T) {
	cs := session(t, &fakeHUD{}, "")
	res, _ := call(t, cs, "c4_get_job", map[string]any{"namespace": "show-a", "name": "nope"})
	if !res.IsError || !strings.Contains(text(res), "c4_list_jobs") {
		t.Fatalf("missing job: error %v, %q", res.IsError, text(res))
	}
	res, _ = call(t, cs, "c4_set_deadline", map[string]any{"namespace": "show-a", "name": "sh010", "deadline": "Friday"})
	if !res.IsError || !strings.Contains(text(res), "RFC 3339") {
		t.Fatalf("bad deadline: %q", text(res))
	}
	res, _ = call(t, cs, "c4_boost_job", map[string]any{"namespace": "show-a", "name": "sh010", "boost": 3})
	if !res.IsError {
		t.Fatal("boost 3 was accepted")
	}
}

func TestControlsPostAsTheCaller(t *testing.T) {
	hud := &fakeHUD{}
	cs := session(t, hud, "tok-maya")
	res, out := call(t, cs, "c4_hold_job", map[string]any{"namespace": "show-a", "name": "sh010", "hold": true, "stop_running": true})
	if res.IsError || out["message"] != "sh010: held" {
		t.Fatalf("hold: %v %q", out, text(res))
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if len(hud.posts) != 1 || !strings.Contains(hud.posts[0], "/api/v1/jobs/show-a/sh010/nudge") ||
		!strings.Contains(hud.posts[0], `"holdStop":true`) {
		t.Fatalf("posts %v", hud.posts)
	}
	for _, a := range hud.auth {
		if a != "Bearer tok-maya" {
			t.Fatalf("a request went to the HUD as %q", a)
		}
	}
}

func TestGetJobListsUnitsOnRequest(t *testing.T) {
	cs := session(t, &fakeHUD{}, "")
	_, out := call(t, cs, "c4_get_job", map[string]any{"namespace": "show-a", "name": "sh010"})
	if _, ok := out["units"]; ok {
		t.Fatal("units listed without include_units")
	}
	_, out = call(t, cs, "c4_get_job", map[string]any{"namespace": "show-a", "name": "sh010", "include_units": true, "unit_state": "running"})
	units, _ := out["units"].([]any)
	if len(units) != 1 || units[0].(map[string]any)["node"] != "w1" {
		t.Fatalf("units %v", out["units"])
	}
}

func TestReadBookFiltersToAJob(t *testing.T) {
	hud := &fakeHUD{}
	cs := session(t, hud, "")
	res, out := call(t, cs, "c4_read_book", map[string]any{"namespace": "show-a", "name": "sh010", "limit": 5})
	entries, _ := out["entries"].([]any)
	if res.IsError || len(entries) != 1 {
		t.Fatalf("book %v %q", out, text(res))
	}
	e := entries[0].(map[string]any)
	if e["kind"] != "failed" || e["object"].(map[string]any)["job"] != "show-a/sh010" {
		t.Fatalf("entry %v", e)
	}
	res, _ = call(t, cs, "c4_read_book", map[string]any{"namespace": "show-a"})
	if !res.IsError {
		t.Fatal("a namespace without a name was accepted")
	}
}

func TestSuggestDeadline(t *testing.T) {
	cs := session(t, &fakeHUD{}, "")
	res, out := call(t, cs, "c4_suggest_deadline", map[string]any{"namespace": "show-a", "name": "sh010"})
	if res.IsError {
		t.Fatalf("suggest: %q", text(res))
	}
	band, _ := out["band"].(map[string]any)
	sg, _ := out["suggestion"].(map[string]any)
	if out["source"] != "suggested" || out["mode"] != "Suggested" || out["nextSession"] != "2026-10-23T22:00:00Z" ||
		len(band) != 4 || out["calibrated"] != true || sg == nil || sg["state"] != "Settled" {
		t.Fatalf("suggestion %v", out)
	}
}

func TestDeadlineOptionsTool(t *testing.T) {
	cs := session(t, &fakeHUD{}, "")
	res, out := call(t, cs, "c4_deadline_options", map[string]any{"namespace": "show-a", "name": "sh010",
		"at": []string{"2026-10-23T16:00:00-07:00", "2026-10-23T17:00:00Z"}})
	opts, _ := out["options"].([]any)
	if res.IsError || len(opts) != 2 {
		t.Fatalf("options %v %q", out, text(res))
	}
	o := opts[0].(map[string]any)
	if o["at"] != "2026-10-23T16:00:00-07:00" || o["boost"] != 1.3 || o["unnamed"] != 1.0 || o["burst"].(map[string]any)["closes"] != true {
		t.Fatalf("option %v", o)
	}
	for _, args := range []map[string]any{
		{"namespace": "show-a", "name": "sh010", "at": []string{"tomorrow"}},
		{"namespace": "show-a", "name": "sh010", "at": []string{"2026-10-23T11:00:00Z", "2026-10-23T11:00:00Z", "2026-10-23T11:00:00Z",
			"2026-10-23T11:00:00Z", "2026-10-23T11:00:00Z", "2026-10-23T11:00:00Z", "2026-10-23T11:00:00Z", "2026-10-23T11:00:00Z", "2026-10-23T11:00:00Z"}},
	} {
		if res, _ := call(t, cs, "c4_deadline_options", args); !res.IsError {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestDeadlineModeAndNextSession(t *testing.T) {
	hud := &fakeHUD{}
	cs := session(t, hud, "")
	for _, args := range []map[string]any{
		{"namespace": "show-a", "name": "sh010", "mode": "suggested"},
		{"namespace": "show-a", "name": "sh010", "deadline": "2026-10-23T18:00:00-07:00", "mode": "pin"},
	} {
		if res, _ := call(t, cs, "c4_set_deadline", args); res.IsError {
			t.Fatalf("set deadline %v: %q", args, text(res))
		}
	}
	if res, _ := call(t, cs, "c4_move_to_next_session", map[string]any{"namespace": "show-a", "name": "sh010"}); res.IsError {
		t.Fatalf("next session: %q", text(res))
	}
	for _, args := range []map[string]any{
		{"namespace": "show-a", "name": "sh010"},
		{"namespace": "show-a", "name": "sh010", "mode": "soon"},
		{"namespace": "show-a", "name": "sh010", "deadline": "2026-10-23T18:00:00-07:00", "mode": "tracking"},
	} {
		if res, _ := call(t, cs, "c4_set_deadline", args); !res.IsError {
			t.Errorf("accepted %v", args)
		}
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	want := []string{`{"deadlineMode":"Suggested"}`, `{"deadline":"2026-10-23T18:00:00-07:00","deadlineMode":"Pinned"}`, `{"nextSession":true}`}
	if len(hud.posts) != len(want) {
		t.Fatalf("posts %v", hud.posts)
	}
	for i, w := range want {
		if hud.posts[i] != "/api/v1/jobs/show-a/sh010/nudge "+w {
			t.Errorf("post %d: %s, want %s", i, hud.posts[i], w)
		}
	}
}

func TestSubmitJobDryRun(t *testing.T) {
	hud := &fakeHUD{}
	cs := session(t, hud, "tok-noor")
	res, out := call(t, cs, "c4_submit_job", map[string]any{"project": "show-a", "type": "render", "shot": "sm0140", "class": "render",
		"params": map[string]any{"frames": "1001-1048"}, "image": "ghcr.io/avalanche-io/c4-job-hydra:v0.1.0",
		"inputs": []any{map[string]any{"view": "c4abc", "mount_path": "/show"}}, "dry_run": true})
	if res.IsError || out["job"] != "show-a/sm0140-render-v001" || out["submitted"] != false || out["onTrack"] != true || out["units"] != float64(48) {
		t.Fatalf("dry run: %v %q", out, text(res))
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if len(hud.posts) != 1 || !strings.Contains(hud.posts[0], `"dryRun":true`) || !strings.Contains(hud.posts[0], `"runAsNonRoot":true`) ||
		!strings.Contains(hud.posts[0], `"view":"c4abc"`) {
		t.Fatalf("posts %v", hud.posts)
	}
	res, _ = call(t, cs, "c4_submit_job", map[string]any{"class": "render", "image": "x", "env": map[string]any{"C4_UNIT": "1"}})
	if !res.IsError {
		t.Fatal("a C4_ variable was accepted")
	}
}
