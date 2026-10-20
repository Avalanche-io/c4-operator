package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/schema"
)

type fakePins map[string]string

func (f fakePins) Pin(_ context.Context, ref string) (string, error) { return f[ref], nil }

// Each tool gets a lock pinning its image at the release, in the bytes the
// farm writes when it publishes.
func TestLocksPinAtTheRelease(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "render"), 0o755)
	os.WriteFile(filepath.Join(dir, "render", "tool.yaml"), []byte("c4: tool/1\nname: render\nkind: frames\nruns:\n  image: c4/job-hydra\n  command: [bash, x.sh]\n"), 0o644)
	pins := fakePins{"ghcr.io/avalanche-io/c4-job-hydra:v0.1.0": "ghcr.io/avalanche-io/c4-job-hydra@sha256:" + strings.Repeat("ab", 32)}
	if err := run(context.Background(), pins, dir, "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "render", "tool.lock"))
	if err != nil {
		t.Fatal(err)
	}
	lock, errs := schema.ParseLock(b)
	if len(errs) > 0 || lock.Images["c4/job-hydra"] != pins["ghcr.io/avalanche-io/c4-job-hydra:v0.1.0"] {
		t.Fatalf("lock %s %v", b, errs)
	}
	if !strings.HasPrefix(string(b), "{\n  \"c4\": \"lock/1\",") || !strings.HasSuffix(string(b), "}\n") {
		t.Errorf("not the farm's bytes:\n%s", b)
	}
}
