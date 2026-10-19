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
	if code := run([]string{"import", "-o", out, "--notes", notes, "../../internal/plan/testdata/lanes"}, &so, &se); code != 0 {
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
	if code := run([]string{"import", "--extra", extra, "-o", out, "../../internal/plan/testdata/lanes"}, &so, &se); code != 1 || !strings.Contains(se.String(), "cycle") {
		t.Fatalf("cycle: %d %s", code, se.String())
	}
}
