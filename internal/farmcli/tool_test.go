package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/schema"
)

func TestNewToolPassesItsOwnChecks(t *testing.T) {
	for _, kind := range []string{"frames", "once"} {
		dir := filepath.Join(t.TempDir(), "proxies")
		var out bytes.Buffer
		if err := NewTool(Out{W: &out}, dir, kind); err != nil {
			t.Fatal(err)
		}
		tool, err := ReadTool(dir)
		if err != nil || tool.Name != "proxies" || tool.Kind != kind || tool.Test == nil {
			t.Fatalf("%s: %+v %v", kind, tool, err)
		}
		if info, err := os.Stat(filepath.Join(dir, "proxies.sh")); err != nil || info.Mode()&0o100 == 0 {
			t.Errorf("%s: the script is not executable: %v", kind, err)
		}
		if err := NewTool(Out{W: io.Discard}, dir, kind); err == nil {
			t.Errorf("%s: a second new over the same folder was allowed", kind)
		}
	}
	if err := NewTool(Out{W: io.Discard}, filepath.Join(t.TempDir(), "Bad_Name"), "frames"); err == nil {
		t.Error("a folder that is not a tool name was accepted")
	}
}

func writeTool(t *testing.T, yamlText string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proxies")
	if err := os.MkdirAll(filepath.Join(dir, "test", "plates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(yamlText), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "test", "plates", "test.1001.exr"), []byte("plate"), 0o644)
	return dir
}

const proxiesYAML = `c4: tool/1
name: proxies
title: Proxies and previews
kind: frames
runs:
  image: c4/runtime-oiio
  command: [bash, "{{Tool.Dir}}/proxies.sh", "{{Param.plates}}", "{{Param.width}}"]
frames: {chunk: 8, order: binary}
params:
- {name: plates, type: PATH, objectType: FILE, dataFlow: IN}
- {name: width, type: INT, default: 960}
test:
  tree: test/
  frames: 1001-1002
  with: {plates: plates/test.$F4.exr}
  expect: [proxy/test.1001.jpg, proxy/test.1002.jpg]
`

func TestReadToolReportsEveryProblem(t *testing.T) {
	dir := writeTool(t, `c4: tool/2
name: Proxies
kind: batch
runs: {}
params:
- {name: width, type: INT}
- {name: width, type: INT}
test:
  tree: test/
  with: {height: 2}
`)
	_, err := ReadTool(dir)
	var te *ToolError
	if !asToolError(err, &te) || len(te.Errors) == 0 {
		t.Fatalf("not a ToolError: %v", err)
	}
	paths := map[string]bool{}
	for _, e := range te.Errors {
		paths[e.Path] = true
		if e.Message == "" {
			t.Errorf("%s: no message", e.Path)
		}
	}
	for _, want := range []string{"c4", "name", "kind"} {
		if !paths[want] {
			t.Errorf("no error at %s in %v", want, te.Errors)
		}
	}
	// a test that sets a parameter the tool lacks, in a file otherwise fine
	dir = writeTool(t, strings.Replace(proxiesYAML, "with: {plates: plates/test.$F4.exr}", "with: {plates: plates/test.$F4.exr, height: 2}", 1))
	if _, err := ReadTool(dir); !asToolError(err, &te) || te.Errors[0].Path != "test.with.height" {
		t.Errorf("test.with.height: %v", err)
	}
}

func asToolError(err error, te **ToolError) bool {
	e, ok := err.(*ToolError)
	*te = e
	return ok
}

func TestToolUnitsAndParams(t *testing.T) {
	tool, err := ReadTool(writeTool(t, proxiesYAML))
	if err != nil {
		t.Fatal(err)
	}
	units, err := testUnits(tool, "1001-1010,1020")
	if err != nil || len(units) != 3 || units[0] != (testUnit{"0", 1001, 1008}) || units[1] != (testUnit{"1", 1009, 1010}) || units[2] != (testUnit{"2", 1020, 1020}) {
		t.Fatalf("units %+v %v", units, err)
	}
	params, err := paramValues(tool, "/show", tool.Test.With, map[string]any{"width": "480"})
	if err != nil || params["plates"] != "/show/plates/test.$F4.exr" || params["width"] != "480" {
		t.Fatalf("params %v %v", params, err)
	}
	if _, err := paramValues(tool, "/show", map[string]any{"colour": 1}); err == nil {
		t.Error("a parameter the tool lacks was accepted")
	}
	env, _ := paramEnv(map[string]any{"plates": "/show/p.exr", "width": 960})
	if strings.Join(env, " ") != `C4_PARAMS={"plates":"/show/p.exr","width":960}` {
		t.Errorf("env %q", env)
	}
	if ResolveImage("c4/runtime-oiio") != "ghcr.io/avalanche-io/c4-runtime-oiio:v0.1.0" || ResolveImage("studio/nuke:16") != "studio/nuke:16" {
		t.Errorf("images %s", ResolveImage("c4/runtime-oiio"))
	}
}

// A fake docker: `version` succeeds; `run` writes, into the /show mount,
// the file for each frame of the unit (as a tool would), and records its
// arguments.
const fakeDocker = `#!/bin/bash
[ "$1" = version ] && exit 0
echo "$@" >>"$FAKE_DOCKER_LOG"
show=""; start=""; end=""
while [ $# -gt 0 ]; do
  case "$1" in
    -v) case "$2" in *:/show) show="${2%:/show}";; esac; shift 2 ;;
    -e) case "$2" in C4_FRAME_START=*) start="${2#*=}";; C4_FRAME_END=*) end="${2#*=}";; esac; shift 2 ;;
    *) shift ;;
  esac
done
mkdir -p "$show/proxy"
for f in $(seq "$start" "$end"); do [ "$f" = "$FAKE_SKIP" ] || touch "$show/proxy/test.$f.jpg"; echo "c4_frame: $f"; done
`

func TestTestToolRunsEachUnitInDocker(t *testing.T) {
	dir := writeTool(t, proxiesYAML)
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	if err := os.WriteFile(docker, []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(bin, "log")
	t.Setenv("FAKE_DOCKER_LOG", log)
	var out bytes.Buffer
	if err := TestTool(context.Background(), Out{W: &out}, TestArgs{Dir: dir, Docker: docker}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{"tool.yaml   ok · frames · 2 parameters · image c4/runtime-oiio", "test        2 frames in 1 unit, on this machine",
		"            c4_frame: 1001", "expect: 2 of 2 files", "ok"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}
	args, _ := os.ReadFile(log)
	for _, want := range []string{"--user 65532:65532", "--cap-drop ALL", "--read-only", "--network none", dir + ":/etc/c4/tools:ro",
		`C4_PARAMS={"plates":"/show/plates/test.$F4.exr"`, "C4_FRAME_START=1001", "C4_FRAME_END=1002", "ghcr.io/avalanche-io/c4-runtime-oiio:v0.1.0 c4-unit run"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("docker run lacks %q:\n%s", want, args)
		}
	}
	// a frame the tool did not write fails the test
	t.Setenv("FAKE_SKIP", "1002")
	out.Reset()
	if err := TestTool(context.Background(), Out{W: &out}, TestArgs{Dir: dir, Docker: docker}); err == nil || !strings.Contains(out.String(), "missing  proxy/test.1002.jpg") {
		t.Errorf("a missing file passed: %v\n%s", err, out.String())
	}
}

// toolsHUD answers the tool and submission calls, and the uploads before them.
type toolsHUD struct {
	mu          sync.Mutex
	publishes   []map[string]any
	submissions []map[string]any
	polls       int
}

func (h *toolsHUD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case strings.HasSuffix(p, "/missing"):
		var req api.MissingRequest
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(api.MissingResponse{Missing: req.IDs})
	case r.Method == http.MethodPut:
		io.Copy(io.Discard, r.Body)
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case strings.HasPrefix(p, "/views/"):
		var req api.ViewRequest
		json.NewDecoder(r.Body).Decode(&req)
		root := c4view.NewNode("")
		for _, f := range req.Files {
			id, _ := c4.Parse(f.ID)
			dir, name := c4view.SplitPath(f.Path)
			root.Dir(dir).Files[name] = c4view.FileEntry(name, f.Size, id, f.Exec)
		}
		for _, d := range req.Dirs {
			root.Dir(d)
		}
		json.NewEncoder(w).Encode(api.ViewResponse{View: c4view.FromNode(root).ID.String()})
	case r.Method == http.MethodGet && p == "/tools/saltmarsh":
		json.NewEncoder(w).Encode(api.ToolListResponse{Tools: []api.ToolSummary{
			{Name: "render", Kind: "frames", Version: "v1", Title: "Render a USD stage", Scope: "farm"},
			{Name: "proxies", Kind: "frames", Version: "v3", Title: "Proxies and previews", Scope: "project"}}})
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/tools/saltmarsh/proxies"):
		h.polls++
		state := "running"
		if h.polls >= 2 {
			state = "ok"
		}
		json.NewEncoder(w).Encode(api.ToolDetail{ToolSummary: api.ToolSummary{Name: "proxies", Version: "v4", Kind: "frames", Title: "Proxies and previews"},
			Params: json.RawMessage(`[{"name": "plates", "type": "PATH", "dataFlow": "IN"}, {"name": "width", "type": "INT", "default": 960}]`),
			Test:   &api.ToolTestState{Job: "saltmarsh/proxies-test-v4", State: state, Seconds: 6}})
	case r.Method == http.MethodPost && p == "/tools/saltmarsh":
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		h.publishes = append(h.publishes, req)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "tool": "proxies", "version": "v4", "view": req["view"],
			"image": "ghcr.io/avalanche-io/c4-runtime-oiio@sha256:91f2", "test": api.ToolTestState{Job: "saltmarsh/proxies-test-v4", State: "running"}})
	case r.Method == http.MethodPost && p == "/submissions":
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		h.submissions = append(h.submissions, req)
		doc := req["submission"].(map[string]any)
		if doc["shot"] == "bad" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "errors": []api.FieldError{{Path: "jobs.movie.after",
				Message: `no job "comp-v4" in this submission`, Fix: `jobs follow jobs by their key in the file; did you mean "comp"?`}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "submission": "c4sub", "jobs": []api.SubmissionJob{
			{Key: "proxies", Namespace: "saltmarsh", Name: "sm0140-proxies-v001", Tool: "proxies", Version: "v4", What: "Proxies and previews · 48 frames"}}})
	default:
		http.Error(w, "no "+r.Method+" "+p, http.StatusNotFound)
	}
}

func TestToolsPublishAndSubmitAgainstTheHUD(t *testing.T) {
	hud := &toolsHUD{}
	srv := httptest.NewServer(hud)
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "td"})
	ctx := context.Background()

	var out bytes.Buffer
	if err := ListTools(ctx, c, Out{W: &out}, "saltmarsh"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "proxies  frames  v3    Proxies and previews\n") || !strings.Contains(out.String(), "render   frames  v1    Render a USD stage · farm-wide") {
		t.Errorf("tools:\n%s", out.String())
	}

	dir := writeTool(t, proxiesYAML)
	out.Reset()
	if err := PublishTool(ctx, c, Out{W: &out}, PublishArgs{Dir: dir, Project: "saltmarsh", Poll: 1}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(hud.publishes) != 1 || !strings.HasPrefix(hud.publishes[0]["view"].(string), "c4") || hud.polls < 2 {
		t.Fatalf("publish %v, %d polls", hud.publishes, hud.polls)
	}
	for _, want := range []string{"proxies v4", "image ghcr.io/avalanche-io/c4-runtime-oiio@sha256:91f2", "ok in 6s", `published to saltmarsh: artists see "Proxies and previews" in Submit`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}

	// a submission whose tree is a folder beside it: uploaded, then named by its ID
	sub := t.TempDir()
	os.MkdirAll(filepath.Join(sub, "shot", "plates"), 0o755)
	os.WriteFile(filepath.Join(sub, "shot", "plates", "sm0140.1001.exr"), []byte("plate"), 0o644)
	file := filepath.Join(sub, "sm0140-proxies.yaml")
	os.WriteFile(file, []byte("c4: submit/1\nproject: saltmarsh\nshot: sm0140\ntree: shot\njobs:\n  proxies:\n    tool: proxies\n    frames: 1001-1048\n"), 0o644)
	out.Reset()
	if _, err := SubmitFile(ctx, c, Out{W: &out}, SubmitFileArgs{Path: file, Yes: true}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(hud.submissions) != 2 || hud.submissions[0]["dryRun"] != true || hud.submissions[1]["dryRun"] != nil {
		t.Fatalf("submissions %v", hud.submissions)
	}
	if tree, _ := hud.submissions[0]["submission"].(map[string]any)["tree"].(string); !strings.HasPrefix(tree, "c4") {
		t.Errorf("the folder was not named by its ID: %q", tree)
	}
	if !strings.Contains(out.String(), "sm0140-proxies-v001   Proxies and previews · 48 frames") || !strings.Contains(out.String(), "queued    saltmarsh/sm0140-proxies-v001") {
		t.Errorf("submit:\n%s", out.String())
	}

	// the farm's field errors, with their fixes
	os.WriteFile(file, []byte("c4: submit/1\nproject: saltmarsh\nshot: bad\njobs: {}\n"), 0o644)
	out.Reset()
	if _, err := SubmitFile(ctx, c, Out{W: &out}, SubmitFileArgs{Path: file, Yes: true}); err == nil ||
		!strings.Contains(out.String(), `  jobs.movie.after: no job "comp-v4" in this submission`) || !strings.Contains(out.String(), `did you mean "comp"?`) {
		t.Errorf("refusal: %v\n%s", err, out.String())
	}
	if _, err := SubmitFile(ctx, c, Out{W: &out}, SubmitFileArgs{Path: writeTool(t, proxiesYAML) + "/tool.yaml"}); err == nil {
		t.Error("a tool.yaml was sent as a submission")
	}
}

func TestSubmitTypedOverAPublishedTool(t *testing.T) {
	hud := &toolsHUD{}
	srv := httptest.NewServer(hud)
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "plates"), 0o755)
	os.WriteFile(filepath.Join(dir, "plates", "sm0140.1001.exr"), []byte("plate"), 0o644)
	os.WriteFile(filepath.Join(dir, ".c4project"), nil, 0o644)
	var out bytes.Buffer
	a := SubmitArgs{Type: "proxies", Asset: filepath.Join(dir, "plates", "sm0140.$F4.exr"), Project: "saltmarsh", Deadline: "+3h",
		Options: Options{Frames: "1001-1048", Set: map[string]string{"width": "480"}}, Yes: true}
	if _, err := Submit(context.Background(), c, Out{W: &out}, a); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(hud.submissions) != 2 {
		t.Fatalf("submissions %v", hud.submissions)
	}
	doc := hud.submissions[0]["submission"].(map[string]any)
	job := doc["jobs"].(map[string]any)["proxies"].(map[string]any)
	with := job["with"].(map[string]any)
	if doc["shot"] != "sm0140" || doc["due"] != "+3h" || job["tool"] != "proxies" || job["frames"] != "1001-1048" ||
		with["plates"] != "plates/sm0140.$F4.exr" || with["width"] != float64(480) || !strings.HasPrefix(doc["tree"].(string), "c4") {
		t.Errorf("submission %v", doc)
	}
	for _, bad := range []SubmitArgs{{Type: "proxies", Asset: a.Asset, Project: "saltmarsh", Options: Options{Set: map[string]string{"colour": "x"}}},
		{Type: "proxies", Asset: a.Asset, Project: "saltmarsh", Options: Options{Set: map[string]string{"width": "wide"}}},
		{Type: "proxies", Asset: a.Asset, Project: "saltmarsh", Options: Options{Image: "mine:dev"}}} {
		if _, err := Submit(context.Background(), c, Out{W: io.Discard}, bad); err == nil {
			t.Errorf("%+v accepted", bad.Options)
		}
	}
}

// A submission goes to the farm in the order written, which is the order
// its jobs are created in, after the CLI names its project and tree.
func TestSubmissionKeepsItsOrder(t *testing.T) {
	raw, err := schema.ToJSON([]byte("c4: submit/1\ntree: shots\njobs:\n  proxies: {tool: proxies}\n  comp: {tool: comp, after: proxies}\n  movie: {tool: transcode, after: comp}\n"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := readDoc(raw)
	if err != nil {
		t.Fatal(err)
	}
	d.setString("tree", "c45xZ")
	d.setString("project", "saltmarsh")
	b, _ := d.MarshalJSON()
	want := `{"c4":"submit/1","tree":"c45xZ","jobs":{"proxies":{"tool":"proxies"},"comp":{"tool":"comp","after":"proxies"},"movie":{"tool":"transcode","after":"comp"}},"project":"saltmarsh"}`
	if string(b) != want {
		t.Errorf("sent\n%s\nwant\n%s", b, want)
	}
}

// A tool's job takes its shot from the folder its asset is in at the top
// of the project, not from the asset's own name.
func TestShotOfAnAsset(t *testing.T) {
	for rel, want := range map[string]string{"saltmarsh/sm0140/geo": "sm0140", "saltmarsh/sm0140/sm0140.usda": "sm0140",
		"sm0140/plates/x.$F4.exr": "sm0140", "saltmarsh/sm0140.$F4.exr": "sm0140", "saltmarsh/sm0220_fx": "sm0220_fx", "work/notes": "notes"} {
		root, rel, _ := strings.Cut(rel, "/")
		if got := shotOf("/home/td/"+root, rel); got != want {
			t.Errorf("shotOf(%q) = %q, want %q", rel, got, want)
		}
	}
}

// trace prints a file's signed record: whether it verifies, the unit and
// node that made it, and what it read; a record that does not verify
// fails the command.
func TestTraceAFile(t *testing.T) {
	verified := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/snapshot":
			json.NewEncoder(w).Encode(api.Snapshot{Jobs: []api.Job{{Namespace: "saltmarsh", Name: "sm0140-render-v001"}}})
		case "/api/v1/trace/saltmarsh/sm0140-render-v001":
			if r.URL.Query().Get("path") != "render/sm0140.1024.exr" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"verified":%v,"signer":"https://cccc.io/id/node/c4-local-worker2","signedAt":"2026-10-21T00:11:19Z","record":"c449jt8DzAk2GmnQ",
				"file":{"id":"c44TARH9igh6Xrx5","path":"render/sm0140.1024.exr"},"lineage":[{"view":"c42JkXENgggchnoL"},{"view":"c4aaaabbbbccccdd","job":"sm0140-light-v001"}],
				"statement":{"predicate":{"invocation":{"job":"sm0140-render-v001","unit":"23"},"runtime":{"node":"c4-local-worker2","images":[{"digest":"sha256:de81"}]}}}}`, verified)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
	var out bytes.Buffer
	o := Out{W: &out, Clock: Clock{UTC: true}}
	if err := Trace(context.Background(), c, o, "", "sm0140-render-v001", "render/sm0140.1024.exr"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"render/sm0140.1024.exr   c44T…Xrx5   verifies\n",
		"  made by   saltmarsh/sm0140-render-v001 · unit 23 · node c4-local-worker2 · signed by the node\n",
		"  read      view c42J…hnoL, as uploaded\n", "  read      the output of sm0140-light-v001 · view c4aa…ccdd\n", "  image     sha256:de81\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}
	verified = false
	if err := Trace(context.Background(), c, Out{W: io.Discard}, "", "sm0140-render-v001", "render/sm0140.1024.exr"); err == nil {
		t.Error("a record that does not verify passed")
	}
}
