package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/Avalanche-io/c4-operator/hud/api"
)

// testingHUD is a new farm whose render tool is published and still in
// its test: no version of it is offered yet, so a typed submit runs the
// built-in render.
type testingHUD struct {
	toolsHUD
	state string
	jobs  []api.SubmitRequest
}

func (h *testingHUD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case p == "/tools/saltmarsh/render" && r.URL.Query().Get("version") == "":
		http.Error(w, "tool render has no version offered yet", http.StatusNotFound)
	case p == "/tools/saltmarsh/render":
		json.NewEncoder(w).Encode(api.ToolDetail{ToolSummary: api.ToolSummary{Name: "render", Version: "v1", Kind: "frames"},
			Versions: []api.ToolVersionInfo{{Version: "v1", Test: api.ToolTestState{Job: "saltmarsh/render-farmtest-1a2b3c4d", State: h.state}}}})
	case p == "/jobs":
		var req api.SubmitRequest
		json.NewDecoder(r.Body).Decode(&req)
		h.jobs = append(h.jobs, req)
		json.NewEncoder(w).Encode(api.SubmitResponse{OK: true, DryRun: req.DryRun, Namespace: "saltmarsh", Project: "saltmarsh",
			Name: req.Shot + "-" + req.Stage + "-v001"})
	default:
		h.toolsHUD.ServeHTTP(w, r)
	}
}

// A job submitted while its tool is still testing is named as the tool
// names it, so the name a guide shows does not depend on when a newcomer
// submits; the plan says why the built-in job ran.
func TestSubmitWhileTheToolTestsIsNamedAsTheTool(t *testing.T) {
	for state, want := range map[string]string{
		"running": "tool      render is still testing on the farm, so this runs the built-in render; the job's name is the same\n",
		"failed":  "tool      render's test failed on the farm, so this runs the built-in render (c4 farm ls --tool-tests shows the test)\n",
	} {
		hud := &testingHUD{state: state}
		srv := httptest.NewServer(hud)
		c, _ := NewClient(Farm{HUD: srv.URL, As: "artist"})
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "sm0140"), 0o755)
		os.WriteFile(filepath.Join(dir, "sm0140", "sm0140.usda"), []byte("#usda 1.0\n"), 0o644)
		os.WriteFile(filepath.Join(dir, ".c4project"), nil, 0o644)
		var out bytes.Buffer
		a := SubmitArgs{Type: "render", Asset: filepath.Join(dir, "sm0140", "sm0140.usda"), Project: "saltmarsh",
			Options: Options{Frames: "1001-1048"}, Yes: true}
		done, err := Submit(context.Background(), c, Out{W: &out}, a)
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v\n%s", state, err, out.String())
		}
		if len(hud.jobs) == 0 || hud.jobs[0].Stage != "render" || done.Name != "sm0140-render-v001" {
			t.Errorf("%s: jobs %+v, queued %+v", state, hud.jobs, done)
		}
		if !strings.HasPrefix(out.String(), want) || !strings.Contains(out.String(), "sm0140-render-v001   render · 48 frames") {
			t.Errorf("%s: plan:\n%s", state, out.String())
		}
	}
}
