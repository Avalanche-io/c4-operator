package schema

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChunkAll(t *testing.T) {
	s, errs := ParseSubmission([]byte("c4: submit/1\njobs:\n  bake: {tool: sim, chunk: all}\n  light: {tool: karma, chunk: 4}\n"))
	if errs != nil {
		t.Fatal(errs)
	}
	if c := s.Jobs.Job("bake").Chunk; c == nil || !c.All {
		t.Errorf("bake chunk %+v", c)
	}
	if c := s.Jobs.Job("light").Chunk; c == nil || c.All || c.N != 4 {
		t.Errorf("light chunk %+v", c)
	}
	if _, errs := ParseSubmission([]byte("c4: submit/1\njobs:\n  bake: {tool: sim, chunk: half}\n")); errs == nil {
		t.Error("chunk: half was accepted")
	}
}

func TestImageRef(t *testing.T) {
	for _, c := range [][3]string{
		{"c4/job-media", "v0.2.0", "ghcr.io/avalanche-io/c4-job-media:v0.2.0"},
		{"c4/runtime-oiio:1", "v0.2.0", "ghcr.io/avalanche-io/c4-runtime-oiio:1"},
		{"registry.petrelfinch.example/nuke:16.0", "v0.2.0", "registry.petrelfinch.example/nuke:16.0"},
		{"c4/", "v0.2.0", "c4/"},
	} {
		if got := ImageRef(c[0], c[1]); got != c[2] {
			t.Errorf("ImageRef(%q) = %q, want %q", c[0], got, c[2])
		}
	}
}

// TestRepositoryTools parses every tool folder the repository ships
// (tools/<name>/tool.yaml), so the farm's own tools are fixtures too.
func TestRepositoryTools(t *testing.T) {
	dirs, _ := filepath.Glob(filepath.Join("..", "tools", "*", "tool.yaml"))
	if len(dirs) == 0 {
		t.Skip("no tools/ folders in this checkout")
	}
	for _, f := range dirs {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		tl, errs := ParseTool(b)
		if errs != nil {
			t.Errorf("%s: %+v", f, errs)
			continue
		}
		if want := filepath.Base(filepath.Dir(f)); tl.Name != want {
			t.Errorf("%s names itself %q; a tool's folder is its name", f, tl.Name)
		}
	}
}
