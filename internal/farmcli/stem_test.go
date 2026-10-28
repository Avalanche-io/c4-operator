package farmcli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// checkout is a folder name, as git init with origin url (none for "")
// leaves it.
func checkout(t *testing.T, name, origin string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	dir := filepath.Join(t.TempDir(), name)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/x\n"), 0o644)
	args := [][]string{{"init", "-q"}}
	if origin != "" {
		args = append(args, []string{"remote", "add", "origin", origin})
	}
	for _, a := range args {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	return dir
}

// Work on no shot is named for its tree: the repository's name from its
// origin remote, so two checkouts share a series, else the folder's.
func TestTreeStemIsTheRepositorysName(t *testing.T) {
	for _, c := range []struct{ folder, origin, want string }{
		{"checkout-2", "https://git.studio.example/tools/TextKit.git", "textkit"},
		{"wa", "git@git.studio.example:web/webapp.git", "webapp"},
		{"pipeline", "/srv/git/pipeline-tools/", "pipeline-tools"},
		{"My Repo", "", "my-repo"},
	} {
		if got := treeStem(context.Background(), checkout(t, c.folder, c.origin)); got != c.want {
			t.Errorf("%s (%s): %q, want %q", c.folder, c.origin, got, c.want)
		}
	}
	plain := filepath.Join(t.TempDir(), "notes_2026")
	os.MkdirAll(plain, 0o755)
	if got := treeStem(context.Background(), plain); got != "notes-2026" {
		t.Errorf("a folder outside git: %q", got)
	}
}

// A typed submit of a repository, as the built-in job, sends the stem;
// over a published tool, the submission's name is the stem.
func TestSubmitOfARepositoryIsNamedForIt(t *testing.T) {
	repo := checkout(t, "checkout-2", "https://git.studio.example/tools/textkit.git")
	hud := &testingHUD{state: "running"}
	srv := httptest.NewServer(hud)
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "td"})
	if _, err := Submit(context.Background(), c, Out{W: io.Discard}, SubmitArgs{Type: "ci", Asset: repo, Project: "saltmarsh", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if j := hud.jobs[len(hud.jobs)-1]; j.Stem != "textkit" || j.Shot != "" || j.Name != "" {
		t.Errorf("built-in: %+v", j)
	}
	// --name wins
	if _, err := Submit(context.Background(), c, Out{W: io.Discard}, SubmitArgs{Type: "ci", Asset: repo, Project: "saltmarsh", Name: "nightly", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if j := hud.jobs[len(hud.jobs)-1]; j.Stem != "" || j.Name != "nightly" {
		t.Errorf("--name: %+v", j)
	}

	tools := &toolsHUD{}
	tsrv := httptest.NewServer(tools)
	defer tsrv.Close()
	tc, _ := NewClient(Farm{HUD: tsrv.URL, As: "td"})
	if _, err := Submit(context.Background(), tc, Out{W: io.Discard}, SubmitArgs{Type: "proxies", Asset: repo, Project: "saltmarsh", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if doc := tools.submissions[0]["submission"].(map[string]any); doc["name"] != "textkit" || doc["shot"] != nil {
		t.Errorf("over a tool: %v", doc)
	}
}

// A farm from before the stem refuses the field; the job is sent again
// without it, and named as that farm names it.
func TestPostJobToAFarmWithoutTheStem(t *testing.T) {
	var sent []api.SubmitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"stem"`) {
			http.Error(w, `body: json: unknown field "stem"`, http.StatusBadRequest)
			return
		}
		var req api.SubmitRequest
		json.Unmarshal(raw, &req)
		sent = append(sent, req)
		json.NewEncoder(w).Encode(api.SubmitResponse{OK: true, Namespace: "saltmarsh", Name: "ci-v001"})
	}))
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "td"})
	var out api.SubmitResponse
	if err := postJob(context.Background(), c, &api.SubmitRequest{Project: "saltmarsh", Stage: "ci", Stem: "textkit"}, &out); err != nil || out.Name != "ci-v001" || len(sent) != 1 {
		t.Errorf("%v %+v %+v", err, out, sent)
	}
}

// A submission file whose tree is a repository, with no shot or name, is
// named for the repository; one with a name keeps it.
func TestSubmissionOfARepositoryIsNamedForIt(t *testing.T) {
	repo := checkout(t, "checkout-2", "https://git.studio.example/tools/textkit.git")
	tools := &toolsHUD{}
	srv := httptest.NewServer(tools)
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "td"})
	for i, file := range []string{"c4: submit/1\nproject: saltmarsh\ntree: .\njobs:\n  proxies: {tool: proxies}\n",
		"c4: submit/1\nproject: saltmarsh\nname: nightly\ntree: .\njobs:\n  proxies: {tool: proxies}\n"} {
		p := filepath.Join(repo, "submit.yaml")
		os.WriteFile(p, []byte(file), 0o644)
		if _, err := SubmitFile(context.Background(), c, Out{W: io.Discard}, SubmitFileArgs{Path: p, Yes: true}); err != nil {
			t.Fatal(err)
		}
		want := []string{"textkit", "nightly"}[i]
		if doc := tools.submissions[len(tools.submissions)-1]["submission"].(map[string]any); doc["name"] != want {
			t.Errorf("%d: name %v, want %s", i, doc["name"], want)
		}
	}
}
