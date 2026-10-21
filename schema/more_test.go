package schema

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
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

// TestSlice1 keeps fixtures/submit/slice1.yaml whole: every submission it
// refuses is a passing fixture, every refusal has a path, a message and a
// fix, and at least one passing submission is left for slice 1 to run.
func TestSlice1(t *testing.T) {
	b, err := fs.ReadFile(Fixtures(), "submit/slice1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Refusals map[string][]Error `json:"refusals"`
	}
	if err := yaml.UnmarshalStrict(b, &f); err != nil {
		t.Fatal(err)
	}
	pass := map[string]bool{}
	for _, c := range cases(t) {
		if c.Format == "submit" && c.Pass {
			pass[c.Name] = true
		}
	}
	for name, errs := range f.Refusals {
		if !pass[name] {
			t.Errorf("slice1.yaml refuses %q, which is not a passing submission", name)
		}
		for _, e := range errs {
			if e.Path == "" || e.Message == "" || e.Fix == "" {
				t.Errorf("%s: a refusal needs a path, a message and a fix: %+v", name, e)
			}
		}
	}
	runs := 0
	for name := range pass {
		if f.Refusals[name] == nil {
			runs++
		}
	}
	if runs == 0 {
		t.Error("slice 1 runs none of the passing submissions")
	}
}

// TestOneErrorPerSlip: a STRING drawn with a control no parameter has gives
// one error, with the STRING's controls as its fix.
func TestOneErrorPerSlip(t *testing.T) {
	_, errs := ParseTool([]byte("c4: tool/1\nname: a\nkind: once\nruns: {image: c4/runtime-base, command: [\"true\"]}\nparams:\n- {name: task, type: STRING, userInterface: {control: MULTILINE}}\n"))
	if len(errs) != 1 {
		t.Fatalf("want one error, got %+v", errs)
	}
	if want := "a STRING is drawn with LINE_EDIT or MULTILINE_EDIT or DROPDOWN_LIST or CHECK_BOX"; errs[0].Fix != want {
		t.Errorf("fix %q, want %q", errs[0].Fix, want)
	}
}
