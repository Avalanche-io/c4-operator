package farmcli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitRepo is a checkout the way a person's looks: node_modules and a build
// folder ignored at the top, a nested .gitignore with a negation, a file
// that is tracked though its folder is ignored, and .git.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// the person's own git settings are not the test's
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	for name, body := range map[string]string{
		".gitignore":              "node_modules/\nbuild/\n",
		"main.go":                 "package main\n",
		"node_modules/a/index.js": "x",
		"node_modules/b/index.js": "y",
		"build/out.bin":           "built",
		"build/keep.bin":          "tracked anyway",
		"logs/.gitignore":         "*.log\n!keep.log\n",
		"logs/run.log":            "noise",
		"logs/keep.log":           "kept",
		"docs/notes.md":           "notes",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-f", "build/keep.bin"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func scanned(t *testing.T, dir string, all bool) ([]string, uploadScan) {
	t.Helper()
	s, err := scanUpload(context.Background(), dir, all)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range s.v.Files {
		paths = append(paths, f.Path)
	}
	slices.Sort(paths)
	return paths, s
}

// A checkout sends what git would keep, by the repository's own .gitignore
// files: never .git, never what they leave out, a negation kept, and a
// tracked file kept though its folder is ignored.
func TestUploadKeepsWhatGitKeeps(t *testing.T) {
	dir := gitRepo(t)
	paths, s := scanned(t, dir, false)
	want := []string{".gitignore", "build/keep.bin", "docs/notes.md", "logs/.gitignore", "logs/keep.log", "main.go"}
	if !slices.Equal(paths, want) {
		t.Errorf("sent %v, want %v", paths, want)
	}
	// node_modules' two, build/out.bin and logs/run.log
	if s.left != 4 {
		t.Errorf("left out %d, want 4", s.left)
	}
	// a folder of the repository is sent by the same rule
	if paths, s := scanned(t, filepath.Join(dir, "logs"), false); !slices.Equal(paths, []string{".gitignore", "keep.log"}) || s.left != 1 {
		t.Errorf("logs: sent %v, left %d", paths, s.left)
	}
	// --all sends everything but .git
	if paths, s := scanned(t, dir, true); len(paths) != 10 || s.left != 0 || slices.ContainsFunc(paths, func(p string) bool { return strings.HasPrefix(p, ".git/") }) {
		t.Errorf("--all: sent %v, left %d", paths, s.left)
	}
}

// A folder handed over with its .git is only sent: a core.fsmonitor its
// config sets is never run.
func TestUploadRunsNoCommandTheFolderNames(t *testing.T) {
	dir := gitRepo(t)
	ran := filepath.Join(t.TempDir(), "ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor.sh")
	os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+ran+"\n"), 0o755)
	if out, err := exec.Command("git", "-C", dir, "config", "core.fsmonitor", hook).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	scanned(t, dir, false)
	if _, err := os.Stat(ran); err == nil {
		t.Error("the folder's core.fsmonitor ran")
	}
}

// A folder that is no git working tree is sent whole.
func TestUploadOfAFolderOutsideGitIsWhole(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".gitignore", "node_modules/a/index.js", "run.log", "shot.usda"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(name), 0o644)
	}
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n*.log\n"), 0o644)
	if paths, s := scanned(t, dir, false); len(paths) != 4 || s.left != 0 {
		t.Errorf("sent %v, left %d", paths, s.left)
	}
}

// One checkout is one tree on every machine: the person's global excludes
// and .git/info/exclude do not change what is sent, or its ID.
func TestUploadIgnoresThePersonsOwnExcludes(t *testing.T) {
	dir := gitRepo(t)
	_, before := scanned(t, dir, false)
	excludes := filepath.Join(t.TempDir(), "excludes")
	os.WriteFile(excludes, []byte("docs/\n*.md\n"), 0o644)
	if err := os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), []byte("[core]\n\texcludesFile = "+excludes+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("main.go\n"), 0o644)
	if _, after := scanned(t, dir, false); after.v.ID != before.v.ID || after.left != before.left {
		t.Errorf("ID %s then %s, left %d then %d", before.v.ID, after.v.ID, before.left, after.left)
	}
}

// --all is for a person at a terminal: an agent, or no terminal, is refused.
func TestUploadAllIsForAPerson(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	if err := allowAll(true, true); err != nil {
		t.Errorf("at a terminal: %v", err)
	}
	if err := allowAll(true, false); err == nil {
		t.Error("--all with no terminal was taken")
	}
	if err := allowAll(false, false); err != nil {
		t.Errorf("no --all: %v", err)
	}
	t.Setenv("CLAUDECODE", "1")
	if err := allowAll(true, true); err == nil || !strings.Contains(err.Error(), "an agent") {
		t.Errorf("inside an agent: %v", err)
	}
	if _, err := Submit(context.Background(), nil, Out{}, SubmitArgs{All: true, Terminal: true}); err == nil {
		t.Error("submit --all inside an agent was taken")
	}
}

// The files line says what .gitignore left out.
func TestFilesLineSaysWhatWasLeftOut(t *testing.T) {
	dir := gitRepo(t)
	srv := httptest.NewServer(&toolsHUD{})
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "td"})
	tree, err := scanTree(context.Background(), c, "saltmarsh", dir, false)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	Out{W: &out}.printFiles(tree)
	if !strings.Contains(out.String(), "6 files in "+tildePath(dir)+" · 4 left out by .gitignore · ") {
		t.Errorf("files line: %q", out.String())
	}
}
