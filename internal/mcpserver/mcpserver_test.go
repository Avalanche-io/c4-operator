package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
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
	// tools serves two published tools and POST /submissions.
	tools bool
	// depth publishes a third tool; proxiesV moves proxies on (v4 when
	// empty), with a new form that a v4 pin no longer fits.
	depth    bool
	proxiesV string
}

func (f *fakeHUD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	depth, pv := f.depth, f.proxiesV
	f.mu.Unlock()
	if pv == "" {
		pv = "v4"
	}
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
	case f.tools && r.Method == "GET" && r.URL.Path == "/api/v1/tools/show-a":
		ts := []api.ToolSummary{
			{Name: "proxies", Title: "Proxies and previews", Summary: "Half-size sRGB JPEGs of an EXR plate sequence.", Kind: "frames", Version: pv, View: "c4proxies", Scope: "project"},
			{Name: "movie", Title: "Review movie", Summary: "A review movie of the proxies.", Kind: "once", Version: "v1", View: "c4movie", Scope: "farm"}}
		if depth {
			ts = append(ts, api.ToolSummary{Name: "depth", Title: "Depth maps", Kind: "frames", Version: "v1", View: "c4depth", Scope: "project"})
		}
		json.NewEncoder(w).Encode(api.ToolListResponse{Tools: ts})
	case f.tools && depth && r.Method == "GET" && r.URL.Path == "/api/v1/tools/show-a/depth":
		json.NewEncoder(w).Encode(api.ToolDetail{ToolSummary: api.ToolSummary{Name: "depth", Title: "Depth maps", Kind: "frames", Version: "v1", Scope: "project"},
			Params:      json.RawMessage(`[{"name":"plates","type":"PATH"}]`),
			ParamSchema: map[string]any{"type": "object", "properties": map[string]any{"plates": map[string]any{"type": "string"}}}})
	case f.tools && r.Method == "GET" && r.URL.Path == "/api/v1/tools/show-a/proxies":
		json.NewEncoder(w).Encode(api.ToolDetail{ToolSummary: api.ToolSummary{Name: "proxies", Title: "Proxies and previews",
			Summary: "Half-size sRGB JPEGs of an EXR plate sequence.", Kind: "frames", Version: r.URL.Query().Get("version") + pv, Scope: "project"},
			Params: json.RawMessage(`[{"name":"plates","type":"PATH"},{"name":"width","type":"INT","default":960}]`),
			ParamSchema: map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false,
				"properties": map[string]any{"plates": map[string]any{"type": "string"}, "width": map[string]any{"type": "integer", "default": 960}},
				"required":   []string{"plates"}},
			Test: &api.ToolTestState{State: "ok", Job: "show-a/proxies-test-v4"}})
	case f.tools && r.Method == "GET" && r.URL.Path == "/api/v1/tools/show-a/movie":
		json.NewEncoder(w).Encode(api.ToolDetail{ToolSummary: api.ToolSummary{Name: "movie", Title: "Review movie", Kind: "once", Version: "v1", Scope: "farm"},
			Params:      json.RawMessage(`[{"name":"range","type":"RANGE_EXPR"}]`),
			ParamSchema: map[string]any{"type": "object", "properties": map[string]any{"range": map[string]any{"type": "string"}}}})
	case f.tools && r.Method == "POST" && r.URL.Path == "/api/v1/tools/show-a":
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posts = append(f.posts, r.URL.Path+" "+string(body))
		f.mu.Unlock()
		if strings.Contains(string(body), "c4bad") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(api.ToolPublishResponse{Errors: []api.FieldError{{Path: "runs.image", Message: "image is required", Fix: "name the image the tool runs"}}})
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(api.ToolPublishResponse{OK: true, Tool: "proxies", Version: "v5", View: "c4proxies5",
			Image: "ghcr.io/avalanche-io/c4-runtime-oiio@sha256:ab12", Scope: "project", Test: &api.ToolTestState{Job: "show-a/proxies-test-v5", State: "running"}})
	case f.tools && r.Method == "POST" && r.URL.Path == "/api/v1/submissions":
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posts = append(f.posts, r.URL.Path+" "+string(body))
		f.mu.Unlock()
		if pv != "v4" && strings.Contains(string(body), "proxies@v4") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "errors": []api.FieldError{{Path: "jobs.proxies.tool",
				Message: "proxies is " + pv + " now; its form changed", Fix: "draw it again"}}})
			return
		}
		if strings.Contains(string(body), "karmah") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "errors": []api.FieldError{{Path: "jobs.light.tool", Message: `no tool "karmah" in show-a`,
				Fix: `did you mean "karma"? c4 farm tools lists them`}}})
			return
		}
		var req struct {
			DryRun bool `json:"dryRun"`
		}
		json.Unmarshal(body, &req)
		eta := due.Add(-2 * time.Hour)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "dryRun": req.DryRun, "submission": "c4sub",
			"estimate": api.SubmissionEstimate{ETA90: &eta, Deadline: &due, Feasible: true, Jobs: 1},
			"warnings": []string{"a frame-by-frame after waits for the whole job"},
			"jobs": []map[string]any{{"key": "proxies", "namespace": "show-a", "name": "sm0140-proxies-v001", "tool": "proxies", "version": "v4",
				"what": "48 frames in 12 units", "estimate": api.SubmitEstimate{Units: 12, ETA50: &eta, ETA90: &eta, Deadline: &due, Feasible: true}}},
			"explain": "sm0140-proxies-v001  WorkJob show-a/sm0140-proxies-v001"})
	case r.Method == "GET" && r.URL.Path == "/api/v1/grants":
		f.mu.Lock()
		f.posts = append(f.posts, "GET "+r.URL.String())
		f.mu.Unlock()
		json.NewEncoder(w).Encode(api.GrantList{Grants: []api.Grant{{Namespace: "show-a", Name: "github-ab12", Person: "maya", Tracker: "github",
			Until: due, Record: "c4grant", Active: true}}})
	case r.Method == "GET" && r.URL.Path == "/api/v1/capacity/forecast":
		f.mu.Lock()
		f.posts = append(f.posts, "GET "+r.URL.String())
		f.mu.Unlock()
		json.NewEncoder(w).Encode(api.CapacityForecast{At: &due, Jobs: []string{"show-a/sh050-light-forecast"},
			Bursts: []api.ForecastBurst{{Pool: "cpu", Slots: 12, Live: 4, CostPerSlotHour: 0.5, ExtraCostPerHour: 4}}, Worse: []api.ForecastRisk{}})
	case r.Method == "GET" && r.URL.Path == "/api/v1/people":
		f.mu.Lock()
		f.posts = append(f.posts, "GET "+r.URL.String())
		f.mu.Unlock()
		json.NewEncoder(w).Encode(api.PeopleList{Entries: []api.PeopleEntry{{Namespace: "show-a", Project: "show-a", Tracker: "github",
			Login: "maya-gh", Person: "maya", State: "offered", Yours: true,
			Lets: "decisions @maya-gh makes in tracker github are relayed as maya's, and serve maya's waits"}}})
	case r.Method == "GET" && r.URL.Path == "/api/v1/held":
		f.mu.Lock()
		f.posts = append(f.posts, "GET "+r.URL.String())
		f.mu.Unlock()
		json.NewEncoder(w).Encode(api.HeldList{Held: []api.HeldWrite{{Namespace: "show-a", Link: "gh-issue-7", Field: "comment", Tracker: "gh",
			Kind: "github", Entity: api.HeldEntity{Type: "Issue", ID: "7"}, Job: "show-a/gh-7", Owner: "maya", Since: due,
			Says: "the GitHub comment on Issue 7, saying running", Yours: true}}})
	case r.Method == "GET" && r.URL.Path == "/api/v1/needs-me":
		unit := 1
		json.NewEncoder(w).Encode(api.NeedsMe{Now: due, Viewer: &api.Viewer{User: "maya"}, Items: []api.NeedsMeItem{{
			ID: "question:show-a/sh010/1", Kind: "question", Job: "show-a/sh010", Unit: &unit, Title: "grade in sh010 asks you",
			Ask: "which LUT", State: "pressing", Severity: "urgent", Since: due,
			Actions: []api.NeedsMeAction{{Act: "Supply", Action: "supply", Path: "/api/v1/jobs/show-a/sh010/supply", Body: map[string]any{"unit": 1}}}}}})
	case r.Method == "GET" && r.URL.Path == "/api/v1/trackers":
		f.mu.Lock()
		f.posts = append(f.posts, "GET "+r.URL.String())
		f.mu.Unlock()
		json.NewEncoder(w).Encode(api.TrackerList{Trackers: []api.Tracker{{Namespace: "show-a", Name: "github", Project: "show-a", Kind: "github",
			Site: "github.com/petrelfinch/pipeline", Syncs: []string{"issues labeled render"}, Ready: true, Granted: 1}}})
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v1/grants/"):
		f.mu.Lock()
		f.posts = append(f.posts, "DELETE "+r.URL.Path)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "maya revoked the grant from maya to tracker github"})
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
			tl.Name == "c4_suggest_deadline" || tl.Name == "c4_deadline_options" || tl.Name == "c4_describe_tool" || tl.Name == "c4_needs_me"
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
	hud.mu.Unlock()
	res, out = call(t, cs, "c4_decide", map[string]any{"namespace": "show-a", "name": "sh010", "unit": "brief", "do": "approve", "reply_text": "Keep it whole."})
	if res.IsError {
		t.Fatalf("decide: %v %q", out, text(res))
	}
	hud.mu.Lock()
	if len(hud.posts) != 2 || !strings.Contains(hud.posts[1], "/api/v1/jobs/show-a/sh010/decide") ||
		!strings.Contains(hud.posts[1], `"reply":{"text":"Keep it whole.","view":""}`) {
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

// c4_submit_job takes a submission, as an object or as YAML (its jobs in
// the order written), and passes a refusal's errors through.
func TestSubmitSubmission(t *testing.T) {
	hud := &fakeHUD{tools: true}
	cs := session(t, hud, "tok-noor")
	res, out := call(t, cs, "c4_submit_job", map[string]any{"dry_run": true, "explain": true,
		"submission": map[string]any{"c4": "submit/1", "project": "show-a", "shot": "sm0140",
			"jobs": map[string]any{"proxies": map[string]any{"tool": "proxies", "frames": "1001-1048", "with": map[string]any{"plates": "plates/sm0140.####.exr"}}}}})
	jobs, _ := out["jobs"].([]any)
	if res.IsError || out["ok"] != true || out["submitted"] != false || len(jobs) != 1 || out["explain"] == "" ||
		out["onTrack"] != true || out["eta90"] == nil || len(out["warnings"].([]any)) != 1 {
		t.Fatalf("dry run: %v %q", out, text(res))
	}
	if j := asMap(jobs[0]); j["job"] != "show-a/sm0140-proxies-v001" || j["onTrack"] != true || j["units"] != float64(12) {
		t.Fatalf("job %v", j)
	}
	yaml := "c4: submit/1\nproject: show-a\njobs:\n  zcomp: {tool: proxies, frames: 1001-1048}\n  alight: {tool: proxies, frames: 1001-1048}\n"
	if res, _ := call(t, cs, "c4_submit_job", map[string]any{"submission_yaml": yaml}); res.IsError {
		t.Fatalf("by YAML: %q", text(res))
	}
	_, out = call(t, cs, "c4_submit_job", map[string]any{"submission": map[string]any{"c4": "submit/1", "project": "show-a",
		"jobs": map[string]any{"light": map[string]any{"tool": "karmah"}}}})
	errs, _ := out["errors"].([]any)
	if out["ok"] != false || len(errs) != 1 || asMap(errs[0])["path"] != "jobs.light.tool" || asMap(errs[0])["fix"] == "" {
		t.Fatalf("refused: %v", out)
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if len(hud.posts) != 3 || !strings.Contains(hud.posts[0], `"dryRun":true`) || !strings.Contains(hud.posts[0], `"explain":true`) {
		t.Fatalf("posts %v", hud.posts)
	}
	// YAML keeps the jobs in the order written
	if p := hud.posts[1]; strings.Index(p, "zcomp") > strings.Index(p, "alight") || strings.Index(p, "zcomp") < 0 {
		t.Fatalf("by YAML, the jobs' order: %s", p)
	}
	if res, _ := call(t, cs, "c4_submit_job", map[string]any{}); !res.IsError {
		t.Fatal("an empty submission was accepted")
	}
}

// The published tools: listed, described, published, and each its own
// c4_run_<tool>, which submits a one-job submission.
func TestToolTools(t *testing.T) {
	hud := &fakeHUD{tools: true}
	cs := session(t, hud, "tok-noor")
	_, out := call(t, cs, "c4_list_tools", map[string]any{})
	tools, _ := out["tools"].([]any)
	if len(tools) != 2 || asMap(tools[0])["name"] != "proxies" || asMap(tools[0])["project"] != "show-a" || asMap(tools[0])["run"] != "c4_run_proxies" {
		t.Fatalf("list %v", out)
	}
	_, out = call(t, cs, "c4_describe_tool", map[string]any{"tool": "proxies"})
	if out["project"] != "show-a" || out["kind"] != "frames" || asMap(out["paramSchema"])["type"] != "object" {
		t.Fatalf("describe %v", out)
	}
	res, out := call(t, cs, "c4_publish_tool", map[string]any{"view": "c4folder"})
	if res.IsError || out["ok"] != true || out["version"] != "v5" || !strings.Contains(out["message"].(string), "only once the test passes") {
		t.Fatalf("publish %v %q", out, text(res))
	}
	_, out = call(t, cs, "c4_publish_tool", map[string]any{"view": "c4bad"})
	if errs, _ := out["errors"].([]any); out["ok"] != false || len(errs) != 1 || asMap(errs[0])["path"] != "runs.image" {
		t.Fatalf("refused publish %v", out)
	}

	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range lt.Tools {
		byName[tl.Name] = tl
	}
	if len(lt.Tools) != len(Tools(nil))+2 || byName["c4_run_proxies"] == nil || byName["c4_run_movie"] == nil {
		t.Fatalf("%d tools; run tools %v %v", len(lt.Tools), byName["c4_run_proxies"], byName["c4_run_movie"])
	}
	b, _ := json.Marshal(byName["c4_run_proxies"].InputSchema)
	var sch map[string]any
	json.Unmarshal(b, &sch)
	props := asMap(sch["properties"])
	// The tool's parameters are arguments of their own, beside the
	// submission's fields; nothing is nested under with.
	if asMap(props["plates"])["type"] != "string" || asMap(props["width"])["type"] != "integer" || props["with"] != nil ||
		props["frames"] == nil || props["project"] != nil || fmt.Sprint(sch["required"]) != "[frames plates]" ||
		!strings.Contains(byName["c4_run_proxies"].Description, "Half-size sRGB") {
		t.Fatalf("c4_run_proxies schema %s, description %q", b, byName["c4_run_proxies"].Description)
	}
	b, _ = json.Marshal(byName["c4_run_movie"].InputSchema)
	if !strings.Contains(string(b), `"frames"`) {
		t.Fatalf("a once tool with a RANGE_EXPR takes frames: %s", b)
	}

	res, out = call(t, cs, "c4_run_proxies", map[string]any{"shot": "sm0140", "frames": "1001-1048", "due": "dailies",
		"plates": "plates/sm0140.####.exr", "dry_run": true})
	if res.IsError || out["ok"] != true {
		t.Fatalf("run: %v %q", out, text(res))
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	last := hud.posts[len(hud.posts)-1]
	var sent struct {
		Submission json.RawMessage `json:"submission"`
		DryRun     bool            `json:"dryRun"`
	}
	json.Unmarshal([]byte(strings.TrimPrefix(last, "/api/v1/submissions ")), &sent)
	want := `{"c4":"submit/1","project":"show-a","shot":"sm0140","due":"dailies","jobs":{"proxies":{"tool":"proxies@v4","frames":"1001-1048","with":{"plates":"plates/sm0140.####.exr"}}}}`
	if string(sent.Submission) != want || !sent.DryRun {
		t.Fatalf("the run tool's submission\n got %s\nwant %s", sent.Submission, want)
	}
}

func TestNoETAIsNotATime(t *testing.T) {
	now := time.Date(2026, 10, 17, 22, 56, 0, 0, time.UTC)
	r := row(api.Job{Name: "research", Estimate: api.Estimate{EtaP50: now, EtaP90: now, NoETA: "suspended"}})
	if r.EtaP90 != "" || r.NoETA != "suspended" {
		t.Fatalf("a job with no ETA must report why, not a time: etaP90 %q, noEta %q", r.EtaP90, r.NoETA)
	}
	r = row(api.Job{Name: "push", Estimate: api.Estimate{EtaP50: now, EtaP90: now}})
	if r.EtaP90 == "" || r.NoETA != "" {
		t.Fatalf("a job with an ETA reports it: etaP90 %q, noEta %q", r.EtaP90, r.NoETA)
	}
}

// A grant is the caller's own, through the HUD: given, listed, revoked.
func TestGrantTools(t *testing.T) {
	hud := &fakeHUD{}
	cs := session(t, hud, "tok-maya")
	res, out := call(t, cs, "c4_grant", map[string]any{"project": "show-a", "tracker": "github", "pools": []string{"cpu"}, "until": "14d"})
	if res.IsError {
		t.Fatalf("grant: %v %q", out, text(res))
	}
	res, out = call(t, cs, "c4_list_trackers", map[string]any{})
	if ts, _ := out["trackers"].([]any); res.IsError || len(ts) != 1 {
		t.Fatalf("trackers: %v %q", out, text(res))
	}
	res, out = call(t, cs, "c4_list_grants", map[string]any{"tracker": "show-a/github"})
	if gs, _ := out["grants"].([]any); res.IsError || len(gs) != 1 {
		t.Fatalf("list: %v %q", out, text(res))
	}
	res, out = call(t, cs, "c4_revoke_grant", map[string]any{"namespace": "show-a", "name": "github-ab12"})
	if res.IsError || !strings.Contains(fmt.Sprint(out["message"]), "revoked") {
		t.Fatalf("revoke: %v %q", out, text(res))
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	want := []string{
		`/api/v1/grants {"project":"show-a","tracker":"github","pools":["cpu"],"until":"14d"}`,
		"GET /api/v1/trackers",
		"GET /api/v1/grants?tracker=show-a%2Fgithub",
		"DELETE /api/v1/grants/show-a/github-ab12",
	}
	if len(hud.posts) != len(want) {
		t.Fatalf("requests %v", hud.posts)
	}
	for i, w := range want {
		if hud.posts[i] != w {
			t.Errorf("request %d: %s, want %s", i, hud.posts[i], w)
		}
	}
	for _, a := range hud.auth {
		if a != "Bearer tok-maya" {
			t.Fatalf("a request went to the HUD as %q", a)
		}
	}
}

// The farm's forecast, read through the HUD.
func TestCapacityForecastTool(t *testing.T) {
	hud := &fakeHUD{}
	cs := session(t, hud, "tok-maya")
	res, out := call(t, cs, "c4_get_capacity_forecast", map[string]any{})
	bs, _ := out["bursts"].([]any)
	if res.IsError || len(bs) != 1 || bs[0].(map[string]any)["extraCostPerHour"] != 4.0 {
		t.Fatalf("forecast: %v %q", out, text(res))
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if !slices.Equal(hud.posts, []string{"GET /api/v1/capacity/forecast"}) {
		t.Fatalf("requests %v", hud.posts)
	}
}

// A held tracker write, found and answered through the HUD.
func TestHeldWriteTools(t *testing.T) {
	hud := &fakeHUD{}
	cs := session(t, hud, "tok-maya")
	res, out := call(t, cs, "c4_list_held_writes", map[string]any{"mine": true})
	if hs, _ := out["held"].([]any); res.IsError || len(hs) != 1 {
		t.Fatalf("held: %v %q", out, text(res))
	}
	res, _ = call(t, cs, "c4_confirm_write", map[string]any{"namespace": "show-a", "link": "gh-issue-7", "field": "comment", "happened": false, "note": "not on the issue"})
	if res.IsError {
		t.Fatalf("confirm: %q", text(res))
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	want := []string{"GET /api/v1/held?mine=1", `/api/v1/held/show-a/gh-issue-7/comment {"happened":false,"note":"not on the issue"}`}
	if !slices.Equal(hud.posts, want) {
		t.Fatalf("requests %q, want %q", hud.posts, want)
	}
}

// The people map is listed through the HUD; there is no tool that confirms
// a login, which only the person does, in the HUD.
func TestPeopleMapTool(t *testing.T) {
	hud := &fakeHUD{}
	cs := session(t, hud, "tok-maya")
	res, out := call(t, cs, "c4_list_people_map", map[string]any{"mine": true})
	if es, _ := out["entries"].([]any); res.IsError || len(es) != 1 {
		t.Fatalf("people: %v %q", out, text(res))
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if strings.Contains(tl.Name, "confirm_tracker_login") || strings.Contains(tl.Name, "people_confirm") {
			t.Fatalf("an agent could confirm a login: %s", tl.Name)
		}
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if !slices.Equal(hud.posts, []string{"GET /api/v1/people?mine=1"}) {
		t.Fatalf("requests %q", hud.posts)
	}
}

// A submission goes with a key, the agent's or one made for it, which the
// answer gives back for a retry; a dry run needs none.
func TestSubmitSubmissionKey(t *testing.T) {
	hud := &fakeHUD{tools: true}
	cs := session(t, hud, "tok-noor")
	sub := map[string]any{"c4": "submit/1", "project": "show-a", "shot": "sm0140",
		"jobs": map[string]any{"proxies": map[string]any{"tool": "proxies", "frames": "1001-1048"}}}
	_, out := call(t, cs, "c4_submit_job", map[string]any{"submission": sub})
	made, _ := out["key"].(string)
	if len(made) != 24 {
		t.Fatalf("a submission's key: %v", out)
	}
	_, out = call(t, cs, "c4_submit_job", map[string]any{"submission": sub, "key": "take-2"})
	if out["key"] != "take-2" {
		t.Fatalf("the agent's own key: %v", out)
	}
	_, out = call(t, cs, "c4_submit_job", map[string]any{"submission": sub, "dry_run": true})
	if out["key"] != nil {
		t.Fatalf("a dry run's key: %v", out)
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if len(hud.posts) != 3 || !strings.Contains(hud.posts[0], `"key":"`+made+`"`) || !strings.Contains(hud.posts[1], `"key":"take-2"`) ||
		strings.Contains(hud.posts[2], `"key"`) {
		t.Fatalf("posts %v", hud.posts)
	}
}

// A run tool refuses an argument it does not take, with the name it may
// have meant, and the old nested with; a refusal is an answer, not an
// error.
func TestRunToolArguments(t *testing.T) {
	hud := &fakeHUD{tools: true}
	cs := session(t, hud, "tok-noor")
	res, out := call(t, cs, "c4_run_proxies", map[string]any{"frames": "1001-1048", "plate": "plates/a.####.exr", "widht": 480,
		"with": map[string]any{"plates": "x"}})
	errs := asMaps(out["errors"])
	if res.IsError || out["ok"] != false || len(errs) != 3 {
		t.Fatalf("%v %q", out, text(res))
	}
	if errs[0]["path"] != "plate" || !strings.Contains(errs[0]["fix"].(string), "did you mean plates?") ||
		errs[1]["path"] != "widht" || !strings.Contains(errs[1]["fix"].(string), "did you mean width?") ||
		errs[2]["path"] != "with" || !strings.Contains(errs[2]["fix"].(string), "arguments of their own") {
		t.Fatalf("errors %v", errs)
	}
	hud.mu.Lock()
	n := len(hud.posts)
	hud.mu.Unlock()
	if n != 0 {
		t.Fatal("refused arguments were sent to the farm")
	}
	// A parameter in the tool's own order, typed as given.
	if res, out := call(t, cs, "c4_run_proxies", map[string]any{"frames": "1001-1048", "width": 480, "plates": "p.####.exr"}); res.IsError || out["ok"] != true {
		t.Fatalf("%v %q", out, text(res))
	}
	hud.mu.Lock()
	defer hud.mu.Unlock()
	if !strings.Contains(hud.posts[0], `"with":{"plates":"p.####.exr","width":480}`) {
		t.Fatalf("sent %s", hud.posts[0])
	}
}

// The run tools follow the farm while a session runs: a tool published
// meanwhile appears at the next tools/list, and the client is told; a run
// tool whose pin went stale is refused (an answer, not an error) and drawn
// again with the tool's current version.
func TestRunToolsFollowTheFarm(t *testing.T) {
	hud := &fakeHUD{tools: true}
	ts := httptest.NewServer(hud)
	t.Cleanup(ts.Close)
	ctx := context.Background()
	srv := New(&Client{Base: ts.URL, Token: "tok-follow"})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	changed := make(chan struct{}, 8)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { changed <- struct{}{} }}).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	names := func() map[string]*mcp.Tool {
		lt, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*mcp.Tool{}
		for _, tl := range lt.Tools {
			out[tl.Name] = tl
		}
		return out
	}
	if n := names(); n["c4_run_depth"] != nil || n["c4_run_proxies"] == nil {
		t.Fatal("the starting tools")
	}
	hud.mu.Lock()
	hud.depth = true
	hud.mu.Unlock()
	if n := names(); n["c4_run_depth"] == nil {
		t.Fatal("a tool published meanwhile is not listed")
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("the client was not told the tools changed")
	}

	// proxies moves to v5: the v4 pin is refused and the tool drawn again.
	hud.mu.Lock()
	hud.proxiesV = "v5"
	hud.mu.Unlock()
	res, out := call(t, cs, "c4_run_proxies", map[string]any{"frames": "1001-1048", "plates": "p.####.exr"})
	if res.IsError || out["ok"] != false || !strings.Contains(out["message"].(string), "drawn again") ||
		asMaps(out["errors"])[0]["path"] != "jobs.proxies.tool" {
		t.Fatalf("stale pin: %v %q", out, text(res))
	}
	if d := names()["c4_run_proxies"].Description; !strings.Contains(d, "proxies v5") {
		t.Fatalf("not drawn again: %q", d)
	}
	if res, out := call(t, cs, "c4_run_proxies", map[string]any{"frames": "1001-1048", "plates": "p.####.exr"}); res.IsError || out["ok"] != true {
		t.Fatalf("after the redraw: %v %q", out, text(res))
	}
}

// c4_submit_job takes one job in the raw form too (an image and its
// command), through POST /jobs.
func TestSubmitRawForm(t *testing.T) {
	hud := &fakeHUD{tools: true}
	cs := session(t, hud, "tok-noor")
	res, out := call(t, cs, "c4_submit_job", map[string]any{"class": "render", "image": "ghcr.io/studio/render:1",
		"command": []string{"render", "--frame", ""}, "params": map[string]string{"frames": "1001-1048"},
		"shot": "sm0140", "dry_run": true})
	jobs := asMaps(out["jobs"])
	if res.IsError || out["ok"] != true || out["submitted"] != false || len(jobs) != 1 || jobs[0]["job"] != "show-a/sm0140-render-v001" ||
		jobs[0]["units"] != float64(48) {
		t.Fatalf("%v %q", out, text(res))
	}
	if res, out := call(t, cs, "c4_submit_job", map[string]any{"class": "render"}); res.IsError || out["ok"] != false ||
		asMaps(out["errors"])[0]["path"] != "image" {
		t.Fatalf("no image: %v %q", out, text(res))
	}
	if res, _ := call(t, cs, "c4_submit_job", map[string]any{"class": "render", "image": "x",
		"submission_yaml": "c4: submit/1"}); !res.IsError {
		t.Fatal("both forms at once")
	}
}

// c4_needs_me lists what waits for the person the agent acts for, as the
// HUD's one-person view has it.
func TestNeedsMe(t *testing.T) {
	cs := session(t, &fakeHUD{}, "")
	res, out := call(t, cs, "c4_needs_me", map[string]any{})
	if res.IsError {
		t.Fatalf("c4_needs_me: %s", text(res))
	}
	items, _ := out["items"].([]any)
	if out["for"] != "maya" || len(items) != 1 {
		t.Fatalf("out %v", out)
	}
	it := items[0].(map[string]any)
	if it["kind"] != "question" || it["state"] != "pressing" || it["job"] != "show-a/sh010" {
		t.Fatalf("item %v", it)
	}
}

// A tool's description names only tools that exist (c4_run_<tool> is
// made per published tool).
func TestDescriptionsNameRealTools(t *testing.T) {
	names := map[string]bool{}
	for _, tl := range Tools(nil) {
		names[tl.Def.Name] = true
	}
	ref := regexp.MustCompile(`c4_[a-z_]+`)
	for _, tl := range Tools(nil) {
		for _, n := range ref.FindAllString(tl.Def.Description, -1) {
			n = strings.TrimRight(n, "_")
			if !names[n] && !strings.HasPrefix(n, "c4_run") {
				t.Errorf("%s's description names %s, which is not a tool", tl.Def.Name, n)
			}
		}
	}
}
