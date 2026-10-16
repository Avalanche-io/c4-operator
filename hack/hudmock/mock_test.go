package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBinaryOrderVisitsEveryPositionOnce(t *testing.T) {
	for n := 1; n <= 300; n++ {
		o := binaryOrder(n)
		if len(o) != n {
			t.Fatalf("n=%d: got %d positions", n, len(o))
		}
		seen := make([]bool, n)
		for _, p := range o {
			if seen[p] {
				t.Fatalf("n=%d: position %d twice", n, p)
			}
			seen[p] = true
		}
	}
	// First and last, then the midpoint, then the quarters.
	o := binaryOrder(240)
	if o[0] != 0 || o[1] != 239 || o[2] != 120 {
		t.Fatalf("binary order starts %v", o[:5])
	}
	// After 33 units of a 240-frame shot, no gap between rendered frames is
	// wider than 8: the shot exists at every 8th frame or finer.
	got := make([]bool, 240)
	for _, p := range o[:33] {
		got[p] = true
	}
	last := 0
	for p := 1; p < 240; p++ {
		if !got[p] {
			continue
		}
		if p-last > 8 {
			t.Fatalf("after 33 units, frames %d to %d have nothing rendered between them", last, p)
		}
		last = p
	}
}

func TestRLE(t *testing.T) {
	cases := map[string][]int{"": nil, "4": {4}, "0-3,7,9-10": {9, 0, 1, 2, 3, 7, 10}}
	for want, in := range cases {
		if got := rle(in); got != want {
			t.Errorf("rle(%v) = %q, want %q", in, got, want)
		}
		if want != "" && rle(parseSet(want)) != want {
			t.Errorf("round trip of %q failed", want)
		}
	}
}

func TestC4IDShape(t *testing.T) {
	id := c4id([]byte("hello"))
	if len(id) != 90 || !strings.HasPrefix(id, "c4") {
		t.Fatalf("c4id = %q (len %d)", id, len(id))
	}
}

func frozenServer(t *testing.T) *httptest.Server {
	t.Helper()
	sv := &server{seed: 7, start: (50 * time.Minute).Seconds(), frozen: true, objects: map[string]object{}, previews: map[string]string{}}
	sv.reset()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/snapshot", sv.handleSnapshot)
	mux.HandleFunc("GET /api/v1/graph", sv.handleGraph)
	mux.HandleFunc("GET /api/v1/jobs/{ns}/{name}", sv.handleJob)
	mux.HandleFunc("GET /api/v1/jobs/{ns}/{name}/units/{i}", sv.handleUnit)
	mux.HandleFunc("GET /api/v1/jobs/{ns}/{name}/preview", sv.handlePreview)
	mux.HandleFunc("GET /api/v1/objects/{project}/{id}", sv.handleObject)
	mux.HandleFunc("POST /api/v1/jobs/{ns}/{name}/nudge", sv.handleNudge)
	mux.HandleFunc("POST /api/v1/capacity/{pool}/approve", sv.handleApprove)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if v != nil && r.StatusCode == 200 {
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}
	return r.StatusCode
}

func hasKeys(t *testing.T, where string, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s: missing %q", where, k)
		}
	}
}

// TestContract checks the response shapes documented in internal/hud/API.md.
func TestContract(t *testing.T) {
	ts := frozenServer(t)
	var snap map[string]any
	getJSON(t, ts.URL+"/api/v1/snapshot?as=wrangler", &snap)
	hasKeys(t, "snapshot", snap, "now", "viewer", "farm", "pools", "classes", "projects", "jobs", "nodes", "tiers", "capacity")
	hasKeys(t, "farm", snap["farm"].(map[string]any), "slotsUsed", "slotsTotal", "queuedUnits", "unitsPerHour", "failuresLastHour",
		"burstSlots", "burstCostPerHour", "bytesMaterialized", "bytesFetchedFromStore", "bytesAvoided", "otherProjectsSlots")
	jobs := snap["jobs"].([]any)
	if len(jobs) < 10 {
		t.Fatalf("only %d jobs", len(jobs))
	}
	var fx map[string]any
	for _, j := range jobs {
		m := j.(map[string]any)
		hasKeys(t, "job", m, "namespace", "name", "project", "class", "driver", "phase", "order", "units", "unitStates",
			"objective", "estimate", "rank", "pools", "inputs", "dependsOn", "createdAt", "nudge")
		hasKeys(t, "estimate", m["estimate"].(map[string]any), "remainingSeconds", "p90RemainingSeconds", "etaP50", "etaP90",
			"requiredSlots", "attainableSlots", "runningSlots", "slackSeconds", "feasible")
		if m["name"] == "sh040-fx-v003" {
			fx = m
		}
	}
	if fx == nil {
		t.Fatal("sh040-fx-v003 not in the snapshot")
	}
	est := fx["estimate"].(map[string]any)
	if est["feasible"] != false || est["burst"] == nil {
		t.Errorf("the FX shot should be at risk with a burst offer: %v", est)
	}

	var d map[string]any
	getJSON(t, ts.URL+"/api/v1/jobs/harbor-render/sh050-light-v002", &d)
	hasKeys(t, "detail", d, "axis", "groups", "unitList", "progress", "costByFrame", "expansions", "edges", "classSummary", "units", "riskUnits")
	if _, ok := d["units"].(map[string]any); !ok {
		t.Errorf("detail units should stay the JobSummary counts, got %T", d["units"])
	}
	if len(d["expansions"].([]any)) == 0 {
		t.Error("the generator job has not expanded")
	}

	var u map[string]any
	getJSON(t, ts.URL+"/api/v1/jobs/harbor-render/sh040-fx-v003/units/0", &u)
	hasKeys(t, "unit", u, "i", "label", "state", "attempts", "estimateSeconds", "logTail", "events")

	var p map[string]any
	getJSON(t, ts.URL+"/api/v1/jobs/harbor-render/sh040-fx-v003/preview", &p)
	var url string
	for _, f := range p["frames"].([]any) {
		if s, ok := f.(map[string]any)["url"].(string); ok {
			url = s
			break
		}
	}
	if url == "" {
		t.Fatal("no rendered preview frame")
	}
	if code := getJSON(t, ts.URL+url+"?as=wrangler", nil); code != 200 {
		t.Errorf("preview object: %d", code)
	}

	var g map[string]any
	getJSON(t, ts.URL+"/api/v1/graph", &g)
	hasKeys(t, "graph", g, "nodes", "edges")
}

// TestArtistScope checks that another project's jobs and objects do not
// leave the server.
func TestArtistScope(t *testing.T) {
	ts := frozenServer(t)
	var snap map[string]any
	getJSON(t, ts.URL+"/api/v1/snapshot?as=artist", &snap)
	for _, j := range snap["jobs"].([]any) {
		if p := j.(map[string]any)["project"]; p != "harbor" {
			t.Errorf("artist sees a %v job", p)
		}
	}
	if snap["farm"].(map[string]any)["otherProjectsSlots"].(float64) <= 0 {
		t.Error("other projects should show as a total")
	}
	for _, n := range snap["nodes"].([]any) {
		for _, r := range asList(n.(map[string]any)["running"]) {
			if !strings.HasPrefix(r.(map[string]any)["job"].(string), "harbor-") {
				t.Errorf("artist sees what a node runs for another project: %v", r)
			}
		}
	}
	if code := getJSON(t, ts.URL+"/api/v1/jobs/kestrel-render/ep104-crowd-v006?as=artist", nil); code != 404 {
		t.Errorf("another project's job: got %d, want 404", code)
	}
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// TestApproveBurst checks that approving the pending burst provisions nodes.
func TestApproveBurst(t *testing.T) {
	ts := frozenServer(t)
	r, err := http.Post(ts.URL+"/api/v1/capacity/spot/approve", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("approve: %d", r.StatusCode)
	}
	var snap map[string]any
	getJSON(t, ts.URL+"/api/v1/snapshot", &snap)
	provisioning := false
	for _, c := range snap["capacity"].([]any) {
		if c.(map[string]any)["state"] == "Provisioning" {
			provisioning = true
		}
	}
	if !provisioning {
		t.Error("no capacity request is provisioning after approval")
	}
}
