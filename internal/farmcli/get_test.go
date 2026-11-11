package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A view's links land under the folder; one that would reach past it is
// skipped, and a file in a link's place is an error.
func TestMakeLinks(t *testing.T) {
	dir := t.TempDir()
	links := []api.ViewLink{
		{Path: "README", Target: "README.md"},
		{Path: "frames/latest", Target: "../renders/v003"},
		{Path: "etc", Target: "/etc"},
		{Path: "up", Target: "../../outside"},
	}
	made, skipped, err := makeLinks(dir, links)
	if err != nil || made != 2 || len(skipped) != 2 || !strings.HasPrefix(skipped[0], "etc -> /etc: the link points outside") || !strings.HasPrefix(skipped[1], "up -> ") {
		t.Fatalf("made %d, skipped %q, %v", made, skipped, err)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "frames", "latest")); got != filepath.FromSlash("../renders/v003") {
		t.Fatalf("frames/latest -> %q", got)
	}
	// again: the links are already there
	if made, _, err := makeLinks(dir, links); err != nil || made != 0 {
		t.Fatalf("second run made %d, %v", made, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := makeLinks(dir, []api.ViewLink{{Path: "LICENSE", Target: "README.md"}}); err == nil {
		t.Fatal("a file was replaced by a link")
	}
}

// fakeView serves a view's listing and its objects as the HUD does.
func fakeView(t *testing.T, files map[string]string, links []api.ViewLink) *Client {
	t.Helper()
	return fakeViewDirs(t, files, links, nil)
}

// fakeViewDirs is fakeView with empty directories in the listing too.
func fakeViewDirs(t *testing.T, files map[string]string, links []api.ViewLink, dirs []string) *Client {
	t.Helper()
	ls := api.ViewListing{Links: links, Dirs: dirs}
	objs := map[string][]byte{}
	for p, body := range files {
		id := c4.Identify(strings.NewReader(body)).String()
		ls.Files = append(ls.Files, api.ViewFile{Path: p, ID: id, Size: int64(len(body))})
		objs[id] = []byte(body)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/files"):
			json.NewEncoder(w).Encode(ls)
		case strings.Contains(r.URL.Path, "/objects/"):
			body, ok := objs[path.Base(r.URL.Path)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, HTTP: srv.Client()}
}

// A name holding a backslash or a colon is a literal c4m name (Joshua,
// 2026-11-10). On Linux and macOS a get writes it with exactly that name.
// Under Windows' rules (git's) it is left out with its path and the
// reason, and everything else is written: nothing renamed, nothing mapped.
func TestGetWritesWhatThePlatformHolds(t *testing.T) {
	ctx := context.Background()
	files := map[string]string{`back\slash.txt`: "b\n", "shots/a:b.exr": "c\n", "shots/ok.exr": "o\n", "CON.txt": "d\n"}
	vid := c4.Identify(strings.NewReader("a view")).String()

	old := getOS
	t.Cleanup(func() { getOS = old })
	getOS = "linux"
	dir := t.TempDir()
	var out bytes.Buffer
	if err := Get(ctx, fakeView(t, files, nil), Out{W: &out}, GetArgs{Ref: vid, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	for p, body := range files {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p))); err != nil || string(b) != body {
			t.Errorf("%s: %q, %v", p, b, err)
		}
	}
	if ents, _ := os.ReadDir(dir); !hasName(ents, `back\slash.txt`) {
		t.Errorf("no file named exactly back\\slash.txt in %v", ents)
	}

	getOS = "windows"
	dir = t.TempDir()
	out.Reset()
	if err := Get(ctx, fakeViewDirs(t, files, []api.ViewLink{{Path: `l\ink`, Target: "shots/ok.exr"}}, []string{"empty:dir/", "fine/"}), Out{W: &out}, GetArgs{Ref: vid, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "shots", "ok.exr")); err != nil || string(b) != "o\n" {
		t.Errorf("the file Windows holds was not written: %q, %v", b, err)
	}
	for _, want := range []string{`skipped   back\slash.txt: this platform cannot hold the name`, "skipped   shots/a:b.exr:", "skipped   CON.txt:", `skipped   l\ink:`, "skipped   empty:dir:", "git v2.47.0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not say %q:\n%s", want, out.String())
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "fine")); err != nil || !fi.IsDir() {
		t.Errorf("the empty directory Windows holds was not made: %v", err)
	}
	for _, p := range []string{`back\slash.txt`, "back", "shots/a:b.exr", "CON.txt", `l\ink`, "empty:dir", "empty"} {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s was written", p)
		}
	}
}

func hasName(ents []os.DirEntry, name string) bool {
	for _, e := range ents {
		if e.Name() == name {
			return true
		}
	}
	return false
}
