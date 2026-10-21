package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Every command in the usage parses its flags: a flag a command defines
// twice (its own and one for every command) panics, and only running the
// command shows it.
func TestEveryCommandParsesItsFlags(t *testing.T) {
	for _, line := range strings.Split(usage, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		var words []string
		for _, w := range strings.Fields(line) {
			if w == strings.ToLower(w) && !strings.ContainsAny(w, "[]|-.") {
				words = append(words, w)
				continue
			}
			break
		}
		if len(words) == 0 || len(words) > 2 {
			continue
		}
		args := append(words, "-h")
		t.Run(strings.Join(words, " "), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("c4 farm %s: %v", strings.Join(args, " "), r)
				}
			}()
			var out bytes.Buffer
			run(args, &out, strings.NewReader(""))
		})
	}
}

// c4-farm with no arguments prints its help and succeeds: the c4 command
// runs it so for `c4 farm`.
func TestNoArgumentsIsHelp(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out, strings.NewReader("")); err != nil || !strings.HasPrefix(out.String(), "c4 farm: ") {
		t.Fatalf("err %v, out %q", err, out.String())
	}
}

// Signing in to a farm and submitting to it never run Docker or kubectl:
// only up needs Docker, and c4's README says so.
func TestLoginAndSubmitNeedNoDockerOrKubectl(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	for _, name := range []string{"docker", "kubectl", "kind"} {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho "+name+" \"$@\" >>"+calls+"\nexit 1\n"), 0o755)
	}
	t.Setenv("PATH", bin)
	t.Setenv("C4_FARM_CONFIG", filepath.Join(t.TempDir(), "farm.json"))
	tree := c4.Identify(strings.NewReader("a tree"))
	hud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/snapshot":
			json.NewEncoder(w).Encode(api.Snapshot{Viewer: &api.Viewer{User: "noor", Projects: []string{"saltmarsh"}}})
		case "/api/v1/submissions":
			json.NewEncoder(w).Encode(api.SubmissionResponse{OK: true, DryRun: true, Jobs: []api.SubmissionJob{{Key: "notes", Namespace: "saltmarsh", Name: "sm0140-notes-v001", What: "one unit"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer hud.Close()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("t0k3n"), 0o600)
	file := filepath.Join(t.TempDir(), "submit.yaml")
	os.WriteFile(file, []byte("c4: submit/1\nproject: saltmarsh\nshot: sm0140\ntree: "+tree.String()+"\njobs:\n  notes: {tool: notes}\n"), 0o644)
	var out bytes.Buffer
	if err := run([]string{"login", hud.URL, "--token-file", token}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("login: %v\n%s", err, out.String())
	}
	if err := run([]string{"submit", file, "--dry-run"}, &out, strings.NewReader("")); err != nil || !strings.Contains(out.String(), "sm0140-notes-v001") {
		t.Fatalf("submit: %v\n%s", err, out.String())
	}
	if b, err := os.ReadFile(calls); err == nil {
		t.Errorf("login and submit ran:\n%s", b)
	}
}

// An older c4 first on the PATH cannot run `c4 farm`: c4-farm says so,
// and names a later c4 on the PATH that can; a c4 that lists farm, or no
// c4 at all, gets no note.
func TestC4PathNote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts stand in for c4")
	}
	old, brew := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(old, "c4"), []byte("#!/bin/sh\necho 'Usage:'; echo '  c4 id <path>'\n"), 0o755)
	os.WriteFile(filepath.Join(brew, "c4"), []byte("#!/bin/sh\necho 'Usage:'; echo '  c4 farm    run work on a C4 farm'\n"), 0o755)
	ctx := context.Background()
	t.Setenv("PATH", old+string(os.PathListSeparator)+brew)
	if n := c4PathNote(ctx); !strings.Contains(n, "has no `c4 farm`; "+brew+"/c4 does: put "+brew+" ahead of "+old) {
		t.Errorf("old first: %q", n)
	}
	t.Setenv("PATH", brew+string(os.PathListSeparator)+old)
	if n := c4PathNote(ctx); n != "" {
		t.Errorf("brew first: %q", n)
	}
	t.Setenv("PATH", old)
	if n := c4PathNote(ctx); !strings.Contains(n, "has no `c4 farm` yet: brew upgrade mrjoshuak/tap/c4") {
		t.Errorf("only old: %q", n)
	}
	t.Setenv("PATH", t.TempDir())
	if n := c4PathNote(ctx); n != "" {
		t.Errorf("no c4: %q", n)
	}
}
