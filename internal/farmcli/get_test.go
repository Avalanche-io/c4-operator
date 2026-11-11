package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/view"
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
	links := []api.ViewLink{
		{Path: `l\ink`, Target: "shots/ok.exr"},
		{Path: "to-back", Target: `back\slash.txt`},
		{Path: "to-stream", Target: "x:stream"},
		{Path: "to-device", Target: "../CON"},
		{Path: "fine-link", Target: "shots/ok.exr"},
	}
	if err := Get(ctx, fakeViewDirs(t, files, links, []string{"empty:dir/", "fine/"}), Out{W: &out}, GetArgs{Ref: vid, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "shots", "ok.exr")); err != nil || string(b) != "o\n" {
		t.Errorf("the file Windows holds was not written: %q, %v", b, err)
	}
	for _, want := range []string{`skipped   back\slash.txt: this platform cannot hold the name`, "skipped   shots/a:b.exr:", "skipped   CON.txt:", `skipped   l\ink:`, "skipped   empty:dir:", "git v2.47.0",
		`skipped   to-back -> back\slash.txt: this platform cannot hold the name "back\\slash.txt" in the link's target`, "skipped   to-stream -> x:stream: this platform cannot hold", "skipped   to-device -> ../CON: this platform cannot hold"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not say %q:\n%s", want, out.String())
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "fine")); err != nil || !fi.IsDir() {
		t.Errorf("the empty directory Windows holds was not made: %v", err)
	}
	for _, p := range []string{`back\slash.txt`, "back", "shots/a:b.exr", "CON.txt", `l\ink`, "empty:dir", "empty", "to-back", "to-stream", "to-device"} {
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

// On macOS a link's target is kept as its bytes, whatever they are (APFS
// takes any target, measured); only on Windows is a target held to the
// name rule.
func TestALinkTargetIsHeldToTheNameRuleOnWindowsOnly(t *testing.T) {
	links := []api.ViewLink{{Path: "l", Target: "caf\xe9/x"}, {Path: "m", Target: `a\b`}}
	_, _, kept, left := writable("darwin", nil, nil, links)
	if len(kept) != 2 || len(left) != 0 {
		t.Fatalf("darwin: kept %v, left %q", kept, left)
	}
	_, _, kept, left = writable("windows", nil, nil, links)
	if len(kept) != 1 || kept[0].Path != "l" || len(left) != 1 {
		t.Fatalf("windows: kept %v, left %q", kept, left)
	}
}

// collides measures whether a folder like dir takes b for a: a is made,
// and b looked up.
func collides(t *testing.T, dir, a, b string) bool {
	t.Helper()
	d, err := os.MkdirTemp(dir, "probe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(d)
	if err := os.WriteFile(filepath.Join(d, a), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = os.Lstat(filepath.Join(d, b))
	return err == nil
}

// Names a folder takes for one: of each set, the last in byte order is
// written whole, its exact name and its bytes; the others are absent and
// reported with it. A scan of the folder afterwards reads the winner
// exactly and nothing of the losers. Which names collide is measured on
// the test's own folder (this Mac's APFS takes all of these pairs for one
// but the last; a case-sensitive volume, none).
func TestNamesTheFolderTakesForOne(t *testing.T) {
	ctx := context.Background()
	old := getOS
	t.Cleanup(func() { getOS = old })
	getOS = "linux"
	pairs := [][2]string{
		{"README.md", "readme.md"},
		{"\u00df", "ss"},
		{"\u03c3", "\u03c2"},
		{"\u00e9", "e\u0301"},
		{"I\u0307", "\u0130"},
		{"straße", "STRASSE"},
		{"i", "\u0130"},
	}
	files := map[string]string{}
	for i, pr := range pairs {
		files[fmt.Sprintf("p%d/%s", i, pr[0])] = fmt.Sprintf("first of %d\n", i)
		files[fmt.Sprintf("p%d/%s", i, pr[1])] = fmt.Sprintf("second of %d\n", i)
	}
	// Directories: Docs/a and docs/b.
	files["Docs/a"] = "a\n"
	files["docs/b"] = "b\n"
	dir := t.TempDir()
	var out bytes.Buffer
	if err := Get(ctx, fakeView(t, files, nil), Out{W: &out}, GetArgs{Ref: c4.Identify(strings.NewReader("v")).String(), Dir: dir}); err != nil {
		t.Fatal(err)
	}
	v, err := view.ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range v.Files {
		got[f.Path] = f.ID.String()
	}
	idOf := func(p string) string { return c4.Identify(strings.NewReader(files[p])).String() }
	expect := func(win, lose string, collide bool) {
		t.Helper()
		if !collide {
			for _, p := range []string{win, lose} {
				if got[p] != idOf(p) {
					t.Errorf("%q, held apart by this folder: scanned as %q", p, got[p])
				}
			}
			return
		}
		if got[win] != idOf(win) {
			t.Errorf("the winner %q is not scanned exactly: %q, want %s", win, got[win], idOf(win))
		}
		if _, ok := got[lose]; ok {
			t.Errorf("the loser %q is still there", lose)
		}
		if want := fmt.Sprintf("skipped   %s: this folder takes it for %s; %s, the last of them in byte order, is written", show(lose), show(win), show(win)); !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
	for i, pr := range pairs {
		a, b := fmt.Sprintf("p%d/%s", i, pr[0]), fmt.Sprintf("p%d/%s", i, pr[1])
		win, lose := a, b
		if b > a {
			win, lose = b, a
		}
		expect(win, lose, collides(t, dir, pr[0], pr[1]))
	}
	// Names that look alike are told apart: escaped, not as they print.
	if collides(t, dir, "\u00e9", "e\u0301") && !strings.Contains(out.String(), `"p3/e\u0301": this folder takes it for "p3/\u00e9"`) {
		t.Errorf("the report does not tell é from e + U+0301:\n%s", out.String())
	}
	// docs/ is later in byte order than Docs/: docs/b is written, Docs/a and
	// Docs/ are not.
	if collides(t, dir, "Docs", "docs") {
		if got["docs/b"] != idOf("docs/b") {
			t.Errorf("docs/b is not scanned exactly: %q", got["docs/b"])
		}
		if _, ok := got["Docs/a"]; ok {
			t.Error("Docs/a, under the losing directory, is still there")
		}
		if !strings.Contains(out.String(), "skipped   Docs: this folder takes it for docs") || !strings.Contains(out.String(), "skipped   Docs/a: under Docs, which this folder takes for docs") {
			t.Errorf("the report:\n%s", out.String())
		}
		ents, _ := os.ReadDir(dir)
		if hasName(ents, "Docs") || !hasName(ents, "docs") {
			t.Errorf("the folder lists %v, want docs and not Docs", ents)
		}
	} else if got["Docs/a"] != idOf("Docs/a") || got["docs/b"] != idOf("docs/b") {
		t.Errorf("on a case-sensitive folder both are written: %v", got)
	}
	t.Logf("the report:\n%s", out.String())
	collided := 0
	for _, pr := range pairs {
		if collides(t, dir, pr[0], pr[1]) {
			collided++
		}
	}
	t.Logf("%d of %d pairs collide on this folder", collided, len(pairs))
	// Again into the same folder: the same result, nothing more left out.
	var again bytes.Buffer
	if err := Get(ctx, fakeView(t, files, nil), Out{W: &again}, GetArgs{Ref: c4.Identify(strings.NewReader("v")).String(), Dir: dir}); err != nil {
		t.Fatal(err)
	}
	v2, _ := view.ScanDir(dir)
	if v2.ID != v.ID {
		t.Fatalf("a second get into the folder left %s, the first %s:\n%s", v2.ID, v.ID, again.String())
	}
}

// A file and a directory a folder takes for one, a link and a file, and an
// entry an earlier get left under another spelling: the same rule, and
// the folder ends holding the view's own spelling.
func TestCollisionsOfEveryKind(t *testing.T) {
	ctx := context.Background()
	old := getOS
	t.Cleanup(func() { getOS = old })
	getOS = "linux"
	dir := t.TempDir()
	if !collides(t, dir, "A", "a") {
		t.Skip("this folder tells names apart by case")
	}
	vid := c4.Identify(strings.NewReader("v")).String()
	files := map[string]string{"Docs/a": "a\n", "docs": "the file\n", "LATEST": "upper\n"}
	links := []api.ViewLink{{Path: "latest", Target: "docs"}}
	var out bytes.Buffer
	if err := Get(ctx, fakeView(t, files, links), Out{W: &out}, GetArgs{Ref: vid, Dir: dir}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	ents, _ := os.ReadDir(dir)
	if hasName(ents, "Docs") || hasName(ents, "LATEST") || !hasName(ents, "docs") || !hasName(ents, "latest") {
		t.Fatalf("the folder lists %v:\n%s", ents, out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "docs")); string(b) != "the file\n" {
		t.Fatalf("docs holds %q", b)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "latest")); got != "docs" {
		t.Fatalf("latest -> %q", got)
	}
	for _, want := range []string{"skipped   Docs: this folder takes it for docs", "skipped   Docs/a: under Docs", "skipped   LATEST: this folder takes it for latest"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}

	// An earlier get, or anything else, left README.MD; the view has
	// README.md. The folder ends listing the view's spelling.
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.MD"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Get(ctx, fakeView(t, map[string]string{"README.md": "new\n"}, nil), Out{W: &out}, GetArgs{Ref: vid, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	ents, _ = os.ReadDir(dir)
	if !hasName(ents, "README.md") || hasName(ents, "README.MD") {
		t.Fatalf("the folder lists %v", ents)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(b) != "new\n" {
		t.Fatalf("README.md holds %q", b)
	}
}

// What the folder held before a get, and what the user put there between
// gets, is never removed, whatever the view's names collide with. A
// re-run writes nothing. Three names in one set are reported with the one
// written. A link into a name that is not written is reported, not made.
func TestCollisionsKeepWhatIsThere(t *testing.T) {
	ctx := context.Background()
	old := getOS
	t.Cleanup(func() { getOS = old })
	getOS = "linux"
	dir := t.TempDir()
	if !collides(t, dir, "A", "a") {
		t.Skip("this folder tells names apart by case")
	}
	vid := c4.Identify(strings.NewReader("v")).String()
	// The user's own file, in a folder the view has under another spelling.
	if err := os.MkdirAll(filepath.Join(dir, "Docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "Docs", "mine.txt"), []byte("mine\n"), 0o644)
	files := map[string]string{"Docs/a": "a\n", "docs/b": "b\n", "README.md": "1\n", "Readme.md": "2\n", "readme.md": "3\n", "\u00e9/x": "nfc\n", "e\u0301/y": "nfd\n"}
	links := []api.ViewLink{{Path: "latest", Target: "Docs/a"}, {Path: "fine", Target: "docs/b"}}
	var out bytes.Buffer
	if err := Get(ctx, fakeView(t, files, links), Out{W: &out}, GetArgs{Ref: vid, Dir: dir}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	ents, _ := os.ReadDir(dir)
	if hasName(ents, "Docs") || !hasName(ents, "docs") {
		t.Fatalf("the folder lists %v", ents)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "docs", "mine.txt")); err != nil || string(b) != "mine\n" {
		t.Fatalf("the user's file: %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "docs", "a")); err == nil {
		t.Fatal("Docs/a, under the losing spelling, was written")
	}
	for _, want := range []string{
		"skipped   README.md: this folder takes it for readme.md; readme.md, the last of them in byte order, is written",
		"skipped   Readme.md: this folder takes it for readme.md; readme.md, the last of them in byte order, is written",
		"skipped   latest -> Docs/a: it points at Docs, which this folder takes for docs and is not written",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "latest")); err == nil {
		t.Error("the link into a name not written was made")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "readme.md")); string(b) != "3\n" || !hasName(ents, "readme.md") {
		t.Errorf("readme.md holds %q; the folder lists %v", b, ents)
	}
	// The user adds a file; the same get again writes nothing and removes
	// nothing.
	os.WriteFile(filepath.Join(dir, "docs", "mine2.txt"), []byte("mine2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "\u00e9", "mine3"), []byte("mine3\n"), 0o644)
	out.Reset()
	if err := Get(ctx, fakeView(t, files, links), Out{W: &out}, GetArgs{Ref: vid, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "0 files · 0 B · every ID checked · 3 already here") {
		t.Errorf("a re-run wrote something:\n%s", out.String())
	}
	for _, p := range []string{"docs/mine.txt", "docs/mine2.txt", "\u00e9/mine3"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s is gone after a re-run: %v", p, err)
		}
	}
}

// An empty executable file keeps its bit; hard links the folder already
// held are left as they are; a get writes nothing outside its folder,
// whatever links the listing has under it.
func TestGetEdgesAroundCollisions(t *testing.T) {
	ctx := context.Background()
	old := getOS
	t.Cleanup(func() { getOS = old })
	getOS = "linux"
	vid := c4.Identify(strings.NewReader("v")).String()

	dir := t.TempDir()
	empty := c4.Identify(strings.NewReader("")).String()
	exec := func() *Client {
		ls := api.ViewListing{Files: []api.ViewFile{{Path: "run.sh", ID: empty, Exec: true}, {Path: "other", ID: empty}}}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/files") {
				json.NewEncoder(w).Encode(ls)
			}
		}))
		t.Cleanup(srv.Close)
		return &Client{Base: srv.URL, HTTP: srv.Client()}
	}
	var out bytes.Buffer
	for range 2 {
		if err := Get(ctx, exec(), Out{W: &out}, GetArgs{Ref: vid, Dir: dir}); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil || fi.Mode().Perm()&0o111 == 0 {
			t.Fatalf("the empty executable: %v, %v", fi.Mode(), err)
		}
	}

	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("x\n"), 0o644)
	if err := os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Get(ctx, fakeView(t, map[string]string{"a": "x\n", "b": "x\n"}, nil), Out{W: &out}, GetArgs{Ref: vid, Dir: dir}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, n := range []string{"a", "b"} {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err != nil || string(b) != "x\n" {
			t.Errorf("%s: %q, %v", n, b, err)
		}
	}

	root := t.TempDir()
	dir = filepath.Join(root, "in")
	links := []api.ViewLink{{Path: "d/up", Target: ".."}, {Path: "e", Target: "d/up/.."}}
	_ = Get(ctx, fakeView(t, map[string]string{"e/pwn": "p\n", "d/k": "k\n"}, links), Out{W: &out}, GetArgs{Ref: vid, Dir: dir})
	if _, err := os.Lstat(filepath.Join(root, "pwn")); err == nil {
		t.Fatal("a file was written outside the folder")
	}
}
