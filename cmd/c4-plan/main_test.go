package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The command writes the document and the notes, and prints a summary; an
// added edge that closes a cycle fails it.
func TestImport(t *testing.T) {
	dir := t.TempDir()
	out, notes := filepath.Join(dir, "plan.json"), filepath.Join(dir, "notes.json")
	var so, se bytes.Buffer
	if code := run([]string{"import", "-o", out, "--notes", notes, "../../internal/plan/lanemap/testdata"}, &so, &se); code != 0 {
		t.Fatalf("import: %d %s", code, se.String())
	}
	if !strings.Contains(so.String(), "blocks,") || !strings.Contains(so.String(), "chapter 1") {
		t.Errorf("summary %q", so.String())
	}
	var doc struct {
		Version int                     `json:"version"`
		Units   []struct{ Name string } `json:"units"`
	}
	b, _ := os.ReadFile(out)
	if err := json.Unmarshal(b, &doc); err != nil || doc.Version != 1 || len(doc.Units) == 0 {
		t.Fatalf("doc %v %s", err, b)
	}
	if b, _ := os.ReadFile(notes); !strings.Contains(string(b), "C9.9") {
		t.Errorf("notes %s", b)
	}
	extra := filepath.Join(dir, "extra.yaml")
	if err := os.WriteFile(extra, []byte("- {block: C2.1, after: [C4.1], file: LANES.md, line: 1, text: wrong}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	so.Reset()
	se.Reset()
	if code := run([]string{"import", "--extra", extra, "-o", out, "../../internal/plan/lanemap/testdata"}, &so, &se); code != 1 || !strings.Contains(se.String(), "cycle") {
		t.Fatalf("cycle: %d %s", code, se.String())
	}
}

// A plan written as data goes through the same command, chosen because the
// argument is a file.
func TestImportFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "plan.json")
	var so, se bytes.Buffer
	if code := run([]string{"import", "-o", out, "../../internal/plan/testdata/port.yaml"}, &so, &se); code != 0 {
		t.Fatalf("import: %d %s", code, se.String())
	}
	if !strings.HasPrefix(so.String(), "11 blocks, ") || !strings.Contains(so.String(), "0 notes") || !strings.Contains(so.String(), "leaves") {
		t.Errorf("summary %q", so.String())
	}
	if code := run([]string{"import", "--format", "lanemap", "-o", out, "../../internal/plan/testdata/port.yaml"}, &so, &se); code != 1 {
		t.Errorf("a file read as a lane map: %d", code)
	}
}

// edges after a merge: the plan file gains an ordering, and the command
// writes it alone against the job's document; folded in with a second
// --since, nothing is new. import --owners writes the held paths.
func TestEdgesCommand(t *testing.T) {
	dir := t.TempDir()
	prev, owners := filepath.Join(dir, "prev.json"), filepath.Join(dir, "owners.json")
	var so, se bytes.Buffer
	if code := run([]string{"import", "-o", prev, "--owners", owners, "../../internal/plan/testdata/port.yaml"}, &so, &se); code != 0 {
		t.Fatalf("import: %d %s", code, se.String())
	}
	if b, _ := os.ReadFile(owners); !strings.Contains(string(b), `"text/scan": "text/scan"`) {
		t.Errorf("owners %s", b)
	}
	src, _ := os.ReadFile("../../internal/plan/testdata/port.yaml")
	next := filepath.Join(dir, "next.yaml")
	if err := os.WriteFile(next, []byte(strings.Replace(string(src), "{name: io/files, group: core, key: module, after: [io/paths]", "{name: io/files, group: core, key: module, after: [io/paths, text/units]", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	edges := filepath.Join(dir, "edges.json")
	so.Reset()
	se.Reset()
	if code := run([]string{"edges", "--since", prev, "-o", edges, next}, &so, &se); code != 0 {
		t.Fatalf("edges: %d %s", code, se.String())
	}
	if !strings.HasPrefix(so.String(), "1 new edges") {
		t.Errorf("summary %q", so.String())
	}
	b, _ := os.ReadFile(edges)
	if !strings.Contains(string(b), `"from": "text/units"`) || !strings.Contains(string(b), `"to": "io/files"`) || strings.Contains(string(b), `"name"`) {
		t.Errorf("edges %s", b)
	}
	so.Reset()
	if code := run([]string{"edges", "--since", prev, "--since", edges, "-o", filepath.Join(dir, "again.json"), next}, &so, &se); code != 0 || !strings.HasPrefix(so.String(), "0 new edges") {
		t.Errorf("again: %d %q %s", code, so.String(), se.String())
	}
	if code := run([]string{"edges", next}, &so, &se); code != 2 {
		t.Errorf("edges without --since: %d", code)
	}
}
