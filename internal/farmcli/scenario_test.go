package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

func envOf(spec c4v1.WorkJobSpec) map[string]string {
	out := map[string]string{}
	for _, e := range spec.Template.Spec.Containers[0].Env {
		if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			out[e.Name] = "field:" + e.ValueFrom.FieldRef.FieldPath
			continue
		}
		out[e.Name] = e.Value
	}
	return out
}

func TestScenarios(t *testing.T) {
	all, err := Scenarios()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Name != "evening" || all[1].Name != "tight-deadline" || all[2].Name != "tracker" || len(all[2].Issues) != 1 {
		t.Fatalf("scenarios %+v", all)
	}
	clock := eastern(t)
	specs := map[string]c4v1.WorkJobSpec{}
	for _, sc := range all {
		for _, j := range sc.Jobs {
			spec, err := j.spec(clock)
			if err != nil {
				t.Fatalf("%s/%s: %v", sc.Name, j.Name, err)
			}
			c := spec.Template.Spec.Containers[0]
			if c.Image != StandInImage || len(c.Args) != 1 || c.Args[0] != standIn || envOf(spec)["NODE_NAME"] != "field:spec.nodeName" {
				t.Errorf("%s: not the stand-in: %s %v", j.Name, c.Image, envOf(spec))
			}
			if spec.Params["estimateSeconds"] == "" {
				t.Errorf("%s: no first estimate", j.Name)
			}
			specs[j.Name] = spec
		}
	}
	gw := specs["gw010-light-v007"]
	if gw.Deadline == nil || clock.At(gw.Deadline.Time) != "15:40 EDT" || gw.DeadlineMode != "Pinned" || gw.Pools[0] != "gpu" ||
		gw.Params["frames"] != "1001-1060" || gw.Params["estimateSeconds"] != "140" {
		t.Errorf("gw010: %v %+v", gw.Deadline, gw.Params)
	}
	// a comp chunks four frames a unit, so a unit's first estimate is four frames'
	if specs["sm0150-comp-v002"].Params["estimateSeconds"] != "80" {
		t.Errorf("comp estimate %s", specs["sm0150-comp-v002"].Params["estimateSeconds"])
	}
	tr := specs["style-train-v003"]
	if tr.Class != "ml" || tr.Params["steps"] != "1500" || tr.Params["checkpointEvery"] != "100" || clock.At(tr.Deadline.Time) != "Sat 04:10 EDT" {
		t.Errorf("training run %s %+v %v", tr.Class, tr.Params, tr.Deadline)
	}
	light := envOf(specs["sm0150-light-v004"])
	if light["S_FAIL_NODE"] != "worker2" || light["S_NAME"] != "sm0150" || light["S_OUT"] != "render" || light["S_PER_FRAME"] != "1" {
		t.Errorf("sm0150 light env %v", light)
	}
	if specs["sm0160-light-v001"].Deadline != nil {
		t.Error("sm0160 should take the farm's suggested deadline")
	}
	if e := envOf(specs["sm0170-review-v001"]); e["S_NAME"] != "sm0170_review" || e["S_EXT"] != "mov" || e["S_PER_FRAME"] != "" {
		t.Errorf("review env %v", e)
	}
}

func TestScenarioChecks(t *testing.T) {
	good := ScenarioJob{Name: "a", Project: "p", As: "artist", Type: "render", Shot: "a", Reads: []string{"a.usda"}, Frames: "1-2", FrameSeconds: 1}
	for name, j := range map[string]func(j *ScenarioJob){
		"no frames":      func(j *ScenarioJob) { j.Frames = "" },
		"no seconds":     func(j *ScenarioJob) { j.FrameSeconds = 0 },
		"clock due":      func(j *ScenarioJob) { j.Due = "18:00" },
		"half a failure": func(j *ScenarioJob) { j.Fail = &ScenarioFail{Node: "n"} },
		"no type":        func(j *ScenarioJob) { j.Type = "nuke" },
		"no reads":       func(j *ScenarioJob) { j.Reads = nil },
	} {
		bad := good
		j(&bad)
		if err := (Scenario{Name: "s", Title: "s", Jobs: []ScenarioJob{bad}}).check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	after := good
	after.Name, after.After = "b", []string{"c"}
	if err := (Scenario{Name: "s", Title: "s", Jobs: []ScenarioJob{good, after}}).check(); err == nil {
		t.Error("after a job that is not earlier: accepted")
	}
}

// fakeHUD answers the calls a scenario load makes, and keeps the jobs.
type fakeHUD struct {
	mu   sync.Mutex
	jobs []api.SubmitRequest
	as   []string
	puts int
	// lag is how many more submissions to each project the farm refuses
	// with "no project", as it does while a project just added reaches it
	lag map[string]int
}

func (f *fakeHUD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/missing"):
		var req api.MissingRequest
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(api.MissingResponse{Missing: req.IDs})
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/objects/"):
		io.Copy(io.Discard, r.Body)
		f.puts++
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/views/"):
		var req api.ViewRequest
		json.NewDecoder(r.Body).Decode(&req)
		root := c4view.NewNode("")
		for _, file := range req.Files {
			id, _ := c4.Parse(file.ID)
			dir, name := c4view.SplitPath(file.Path)
			root.Dir(dir).Files[name] = c4view.FileEntry(name, file.Size, id, file.Exec)
		}
		json.NewEncoder(w).Encode(api.ViewResponse{View: c4view.FromNode(root).ID.String(), Files: len(req.Files)})
	case r.Method == http.MethodPost && p == "/jobs":
		var req api.SubmitRequest
		json.NewDecoder(r.Body).Decode(&req)
		if f.lag[req.Project] != 0 {
			f.lag[req.Project]--
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "no project " + req.Project})
			return
		}
		f.jobs = append(f.jobs, req)
		f.as = append(f.as, r.URL.Query().Get("as"))
		json.NewEncoder(w).Encode(api.SubmitResponse{OK: true, Namespace: req.Project, Name: req.Name, Project: req.Project})
	default:
		http.Error(w, "no "+r.Method+" "+p, http.StatusNotFound)
	}
}

func TestLoadScenario(t *testing.T) {
	hud := &fakeHUD{}
	srv := httptest.NewServer(hud)
	defer srv.Close()
	sc, err := LookupScenario("evening")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	client := func(as string) (*Client, error) { return NewClient(Farm{HUD: srv.URL, As: as}) }
	if err := LoadScenario(context.Background(), Out{W: &out, Clock: eastern(t)}, sc, client); err != nil {
		t.Fatal(err)
	}
	if len(hud.jobs) != 6 || hud.puts == 0 {
		t.Fatalf("%d jobs, %d uploads", len(hud.jobs), hud.puts)
	}
	if hud.jobs[0].Name != "style-train-v003" || hud.as[0] != "artist" || hud.as[1] != "wrangler" {
		t.Errorf("order or owners: %s %v", hud.jobs[0].Name, hud.as)
	}
	comp := hud.jobs[4]
	if comp.Name != "sm0150-comp-v002" || comp.Type != "comp" || comp.Stage != "comp" || len(comp.Inputs) != 2 ||
		comp.Inputs[1] != (api.SubmitInput{FromJob: "sm0150-light-v004", MountPath: "/in", ReadOnly: true}) {
		t.Errorf("comp %+v", comp)
	}
	for _, want := range []string{
		"submitted saltmarsh/style-train-v003           LoRA fine-tune · due Sat 04:10 EDT",
		"          glassworks/gw010-light-v007          render · 60 frames · pool gpu · due 15:40 EDT",
		"          copperline/ep102-sh030-review-v001   transcode · due 15:10 EDT",
		"          saltmarsh/sm0150-comp-v002           comp · after sm0150-light-v004 · due 17:10 EDT",
		"          saltmarsh/sm0160-light-v001          render · 48 frames · deadline suggested",
	} {
		if !strings.Contains(out.String(), want+"\n") {
			t.Errorf("no line %q in\n%s", want, out.String())
		}
	}

	tight, _ := LookupScenario("tight-deadline")
	hud.jobs, hud.as = nil, nil
	if err := LoadScenario(context.Background(), Out{W: io.Discard, Clock: eastern(t)}, tight, client); err != nil {
		t.Fatal(err)
	}
	if c := hud.jobs[2]; len(c.Inputs) != 3 || c.Inputs[1].MountPath != "/in/sm0170-light-v002" || c.Inputs[2].MountPath != "/in/sm0170-depth-v001" {
		t.Errorf("a comp of two jobs: %+v", c.Inputs)
	}
	if ai := hud.jobs[1].AIUsage; ai["model"] != "Depth Anything V2 Small" || ai["oversight"] != "human_validated" {
		t.Errorf("depth AI use %v", ai)
	}
}

// standin runs the stand-in script with sh in a fresh mount.
func standin(t *testing.T, mount string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-euc", standIn)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "S_MOUNT=" + mount, "S_NAME=sm0150", "S_EXT=exr"}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestStandIn(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }

	// frames: one file each, and the progress
	m := t.TempDir()
	if out, err := standin(t, m, "S_OUT=render", "S_PER_FRAME=1", "S_SECONDS=0.01", "C4_FRAME_START=1001", "C4_FRAME_END=1003"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if read(filepath.Join(m, "render", "sm0150.1003.exr")) != "stand-in frame 1003 of sm0150" || read(filepath.Join(m, ".c4", "progress")) != "1.0000" {
		t.Errorf("frames: %v", read(filepath.Join(m, ".c4", "progress")))
	}

	// the bad node fails, and says why
	out, err := standin(t, t.TempDir(), "S_OUT=render", "S_SECONDS=0.01", "C4_FRAME_START=1", "C4_FRAME_END=1",
		"NODE_NAME=c4-local-worker2", "S_FAIL_NODE=c4-local-worker2", "S_FAIL_AFTER=0.01", "S_FAIL_MSG=read /show/geo/heron.usdc: input/output error")
	if err == nil || !strings.Contains(out, "input/output error") {
		t.Errorf("the bad node: %v %q", err, out)
	}

	// a training run yields at its first checkpoint when the farm says stop
	m, ctl := t.TempDir(), t.TempDir()
	train := []string{"S_OUT=out", "C4_STEPS=4", "C4_CHECKPOINT_EVERY=2", "S_STEP_SECONDS=0.01", "C4_CONTROL=" + ctl}
	os.WriteFile(filepath.Join(ctl, "safepoint.decision"), []byte(`{"seq": 1, "do": "stop", "why": "gw010 needs gpu"}`), 0o644)
	if out, err := standin(t, m, train...); err != nil || !strings.Contains(out, "yielded at step 2") {
		t.Fatalf("yield: %v %s", err, out)
	}
	if read(filepath.Join(m, "out", "checkpoint")) != "2" || !strings.Contains(read(filepath.Join(m, ".c4", "safepoint")), "checkpoint step-2") {
		t.Errorf("checkpoint %q", read(filepath.Join(m, "out", "checkpoint")))
	}
	// and the next attempt resumes there and finishes
	if out, err := standin(t, m, train...); err != nil || !strings.Contains(out, "resumed at step 2") {
		t.Fatalf("resume: %v %s", err, out)
	}
	if read(filepath.Join(m, "out", "sm0150.exr")) != "stand-in weights, 4 steps" || read(filepath.Join(m, ".c4", "progress")) != "1" {
		t.Errorf("finished run: %q", read(filepath.Join(m, "out", "sm0150.exr")))
	}
}

// A scenario's issues go to the stand-in GitHub through the HUD, and what
// each does on the farm is printed; a farm without one says how to add it.
func TestLoadIssues(t *testing.T) {
	sc, err := LookupScenario("tracker")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	var installer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		installer = r.Header.Get("X-C4-Installer")
		if r.Method != http.MethodPost || r.URL.Path != "/sandbox/issues" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "no", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"number":1,"message":"Issue #1 will become a job in saltmarsh for artist; watch it in the HUD."}`))
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := LoadIssues(context.Background(), Out{W: &out}, sc, srv.URL, "", ""); err != nil {
		t.Fatal(err)
	}
	if got["as"] != "supervisor" || got["assignee"] != "artist" || !strings.Contains(out.String(), "#1 Light sm0150 for Thursday's review (by supervisor)") ||
		!strings.Contains(out.String(), "will become a job in saltmarsh for artist") {
		t.Fatalf("sent %v, printed %q", got, out.String())
	}
	none := httptest.NewServer(http.NotFoundHandler())
	defer none.Close()
	// On a farm with an owner the issue is theirs, opened and assigned,
	// with the farm's installer token, as every request to its HUD has.
	if err := LoadIssues(context.Background(), Out{W: &out}, sc, srv.URL, "noor", "t0ken"); err != nil || got["as"] != "noor" || got["assignee"] != "noor" || installer != "t0ken" {
		t.Fatalf("an owner's farm sent %v with %q: %v", got, installer, err)
	}
	if err := LoadIssues(context.Background(), Out{W: &out}, sc, none.URL, "", ""); !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("no sandbox: %v", err)
	}
}

// A project a scenario has just added takes a moment to reach the farm: a
// job for it is sent again while the farm says it has no such project,
// and a project that never comes ends the load in the farm's words.
func TestLoadScenarioWaitsForItsProjects(t *testing.T) {
	defer func(w, r time.Duration) { ProjectWait, projectRetry = w, r }(ProjectWait, projectRetry)
	ProjectWait, projectRetry = time.Second, time.Millisecond
	hud := &fakeHUD{lag: map[string]int{"glassworks": 3, "copperline": 1}}
	srv := httptest.NewServer(hud)
	defer srv.Close()
	sc, err := LookupScenario("evening")
	if err != nil {
		t.Fatal(err)
	}
	client := func(as string) (*Client, error) { return NewClient(Farm{HUD: srv.URL, As: as}) }
	if err := LoadScenario(context.Background(), Out{W: io.Discard, Clock: eastern(t)}, sc, client); err != nil {
		t.Fatal(err)
	}
	if len(hud.jobs) != 6 {
		t.Fatalf("%d jobs", len(hud.jobs))
	}

	ProjectWait = 20 * time.Millisecond
	hud.jobs, hud.lag = nil, map[string]int{"glassworks": -1}
	err = LoadScenario(context.Background(), Out{W: io.Discard, Clock: eastern(t)}, sc, client)
	if err == nil || !strings.Contains(err.Error(), "glassworks/gw010-light-v007: no project glassworks") {
		t.Errorf("a project that never comes: %v", err)
	}
}

// A ci job's plan says it runs with no network, before a test that
// fetches its dependencies fails inside the job.
func TestCIPlanSaysNoNetwork(t *testing.T) {
	srv := httptest.NewServer(&fakeHUD{})
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "td"})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0o644)
	var out bytes.Buffer
	if _, err := Submit(context.Background(), c, Out{W: &out}, SubmitArgs{Type: "ci", Asset: dir, Project: "saltmarsh", DryRun: true}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "  note      "+ciNoNetwork) {
		t.Errorf("no network note in\n%s", out.String())
	}
}
