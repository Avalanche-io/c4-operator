package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/flow"
)

type flowEnv struct {
	t    *testing.T
	hud  *fakeHUD
	srv  *httptest.Server
	a    *flow.Adapter
	flow *flowSite
}

func setupFlow(t *testing.T) *flowEnv {
	hud := &fakeHUD{jobs: map[string]api.Job{}, granted: map[string]bool{}}
	hs := httptest.NewServer(hud)
	t.Cleanup(hs.Close)
	g := New(Config{Owner: "petrel-finch", Repo: "saltmarsh", Token: "tok", URL: "http://sandbox",
		HUD: hs.URL, HUDToken: func() (string, error) { return "sandbox-token", nil }, Project: "saltmarsh", Namespace: "saltmarsh", Tracker: "sandbox",
		People: []Person{{Login: "artist", Person: "artist"}, {Login: "supervisor", Person: "supervisor"}}, FlowKey: "flow-key"})
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	a, err := flow.New(tracker.Config{Endpoint: srv.URL + FlowAPI,
		Scope: map[string]string{"project": strconv.Itoa(g.flow.site.Project().ID), "script": "c4-sync", "site_url": FlowPrefix[:len(FlowPrefix)-1]},
		Token: func(context.Context) (string, error) { return "flow-key", nil }})
	if err != nil {
		t.Fatal(err)
	}
	return &flowEnv{t: t, hud: hud, srv: srv, a: a, flow: g.flow}
}

func (e *flowEnv) post(path string, in any) (done, string) {
	e.t.Helper()
	b, _ := json.Marshal(in)
	resp, err := http.Post(e.srv.URL+FlowPrefix+path, "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var d struct {
		done
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		e.t.Fatalf("%s: %v", path, err)
	}
	return d.done, d.Error
}

func (e *flowEnv) must(path string, in any) done {
	e.t.Helper()
	d, msg := e.post(path, in)
	if msg != "" {
		e.t.Fatalf("%s: %s", path, msg)
	}
	return d
}

func (e *flowEnv) get(path string) (int, string) {
	e.t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var ready = tracker.Query{Type: "Task", Fields: map[string]string{"sg_status_list": "rdy", "step.Step.short_name": "LGT"}}

// The stand-in Flow, walked as the demo walks it, with c4-sync's own
// adapter on its API: a waiting Task the intake rule leaves alone; the
// supervisor sets it ready; the Version c4-sync publishes shows its still,
// frames and artist; the supervisor's fix and note come back as a decision
// by them, read from the event log.
func TestFlowStandIn(t *testing.T) {
	e := setupFlow(t)
	ctx := context.Background()
	if id := e.flow.site.Project().ID; id != FlowProject {
		t.Fatalf("the site's project is %d, the Tracker's %d", id, FlowProject)
	}
	due := time.Now().AddDate(0, 0, 4).Format("2006-01-02")
	task := e.must("shots", map[string]any{"as": "supervisor", "shot": "sh040", "task": "Lighting", "step": "LGT", "assignee": "artist", "due": due})
	if items, _, err := e.a.List(ctx, ready, ""); err != nil || len(items) != 0 {
		t.Fatalf("a waiting Task matched intake: %v %v", items, err)
	}
	if _, page := e.get(FlowPrefix + "?live=1&as=supervisor"); !strings.Contains(page, "c4 hasn&#39;t been allowed to submit work for artist from Flow yet") {
		t.Fatalf("the page should say the artist has not allowed it:\n%s", page)
	}
	e.hud.set(func() { e.hud.granted["artist"] = true })
	if _, page := e.get(FlowPrefix + "?live=1&as=supervisor"); !strings.Contains(page, "Set ready") || !strings.Contains(page, "Waiting. Set it ready") {
		t.Fatalf("a waiting Task offers Set ready:\n%s", page)
	}

	if _, msg := e.post("status", map[string]any{"as": "supervisor", "type": "Task", "id": task.Number, "status": "apr"}); msg == "" {
		t.Fatal("the page approved a Task")
	}
	e.must("status", map[string]any{"as": "supervisor", "type": "Task", "id": task.Number, "status": "rdy"})
	items, _, err := e.a.List(ctx, ready, "")
	if err != nil || len(items) != 1 || items[0].Ref.ID != strconv.Itoa(task.Number) || len(items[0].Assignees) == 0 || items[0].Assignees[0].ID != "artist" {
		t.Fatalf("intake: %+v %v", items, err)
	}

	png := []byte("\x89PNG\r\n\x1a\nsh040")
	v, err := e.a.Publish(ctx, items[0].Ref, tracker.Output{Name: "saltmarsh/flow-sh040", Version: 1, View: "c4view0001",
		FirstFrame: 1001, LastFrame: 1008, By: "artist", Preview: &tracker.Still{Name: "sh040.1004.png", Data: png}}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if v.URL != "/sandbox/flow/detail/Version/"+v.ID {
		t.Fatalf("the Version's link: %s", v.URL)
	}
	if code, page := e.get(v.URL + "?as=supervisor"); code != 200 || !strings.Contains(page, `id="Version`+v.ID+`"`) {
		t.Fatalf("the Version's link answers %d", code)
	}
	if code, b := e.get(FlowPrefix + "thumbnail/" + v.ID); code != 200 || b != string(png) {
		t.Fatalf("the still: %d %q", code, b)
	}
	_, page := e.get(FlowPrefix + "?live=1&as=supervisor")
	for _, want := range []string{"sh040_lgt_v001", "frames 1001-1008", "artist artist", "Pending review", "Needs fix", `src="/sandbox/flow/thumbnail/` + v.ID + `"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page lacks %q:\n%s", want, page)
		}
	}

	since := time.Now().Add(-time.Minute)
	vid, _ := strconv.Atoi(v.ID)
	if _, msg := e.post("status", map[string]any{"as": "supervisor", "type": "Version", "id": vid, "status": "fix"}); msg == "" {
		t.Fatal("a fix with no note was taken")
	}
	e.must("status", map[string]any{"as": "supervisor", "type": "Version", "id": vid, "status": "fix", "note": "Fill light down a stop."})
	ds, err := e.a.Decisions(ctx, v, since)
	if err != nil || len(ds) != 1 || ds[0].Native != "status:fix" || ds[0].By.ID != "supervisor" || !strings.Contains(ds[0].Body, "Fill light down a stop.") {
		t.Fatalf("decisions: %+v %v", ds, err)
	}
	if _, page := e.get(FlowPrefix + "?live=1&as=supervisor"); !strings.Contains(page, "Fill light down a stop.") || strings.Contains(page, "Needs fix</button>") {
		t.Fatalf("the note shows and the decision is made:\n%s", page)
	}
}

// The API takes only its script's key.
func TestFlowStandInKey(t *testing.T) {
	e := setupFlow(t)
	resp, err := http.Post(e.srv.URL+FlowAPI+"/api/v1.1/auth/access_token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=client_credentials&client_id=c4-sync&client_secret=wrong"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a wrong key: %s", resp.Status)
	}
}
