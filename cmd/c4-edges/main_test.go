package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "src", "b"), 0o755)
	os.WriteFile(filepath.Join(root, "src", "b", "b.go"), []byte("return Pending{Lane: \"A\"}\n"), 0o644)
	owners := filepath.Join(t.TempDir(), "owners.json")
	os.WriteFile(owners, []byte(`{"src/b/": "B"}`), 0o644)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-root", root, "-include", "**/*.go", "-pattern", `Pending\{[^}]*Lane: *"(?P<from>[^"]+)"`, "-owners", owners},
		&stdout, &stderr, func(k string) string { return map[string]string{"C4_OUTPUT": out}[k] })
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	b, _ := os.ReadFile(filepath.Join(out, ".c4", "expand.json"))
	if !strings.Contains(string(b), `"from": "A"`) || !strings.Contains(string(b), `"to": "B"`) || !strings.Contains(stderr.String(), "1 markers read as edges") {
		t.Fatalf("%s\n%s", b, stderr.String())
	}
}
