// Package images holds the job images' Dockerfiles; this test keeps them
// buildable as c4-unit grows.
package images

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every image builds c4-unit from the repository root with only what its
// .dockerignore admits, so each must copy, and admit, every package
// c4-unit imports from this module. A new import otherwise breaks every
// image build, and only an image build shows it.
func TestImagesCarryWhatC4UnitImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "../cmd/c4-unit").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	const module = "github.com/Avalanche-io/c4-operator/"
	var dirs []string
	for _, p := range strings.Fields(string(out)) {
		if dir, ok := strings.CutPrefix(p, module); ok && dir != "cmd/c4-unit" {
			dirs = append(dirs, dir)
		}
	}
	files, _ := filepath.Glob("*/Dockerfile*")
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, ".dockerignore") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "COPY cmd/c4-unit ./cmd/c4-unit") {
			continue
		}
		checked++
		ignore, err := os.ReadFile(f + ".dockerignore")
		if err != nil {
			t.Errorf("%s has no %s.dockerignore", f, filepath.Base(f))
			continue
		}
		for _, dir := range dirs {
			if !strings.Contains(string(b), "COPY "+dir+" ./"+dir+"\n") {
				t.Errorf("%s does not copy %s, which c4-unit imports", f, dir)
			}
			if !strings.Contains(string(ignore), "!"+dir+"/\n") {
				t.Errorf("%s.dockerignore does not admit %s/", f, dir)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Dockerfile builds c4-unit")
	}
}
