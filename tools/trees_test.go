package tools

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// One tool without its tool.lock leaves the others release trees.
func TestReleaseTreesEachOnItsOwn(t *testing.T) {
	folders := map[string]fs.FS{
		"locked": fstest.MapFS{"tool.yaml": {Data: []byte("name: locked\n")}, "tool.lock": {Data: []byte("{}\n")}},
		"bare":   fstest.MapFS{"tool.yaml": {Data: []byte("name: bare\n")}},
	}
	trees, err := releaseTrees("v0.2.0", folders)
	if err != nil || len(trees) != 1 {
		t.Fatalf("trees %v, err %v", trees, err)
	}
	for _, tr := range trees {
		if tr != (ReleaseTree{Name: "locked", Release: "v0.2.0"}) {
			t.Fatalf("tree %+v", tr)
		}
	}
}

// A tree is tested only when the release names its tool among those whose
// tests it ran.
func TestTreesSayWhichTheReleaseTested(t *testing.T) {
	defer func(was string) { tested = was }(tested)
	tested = "comp, render"
	folders := map[string]fs.FS{
		"comp":  fstest.MapFS{"tool.yaml": {Data: []byte("name: comp\n")}, "tool.lock": {Data: []byte("{}\n")}},
		"infer": fstest.MapFS{"tool.yaml": {Data: []byte("name: infer\n")}, "tool.lock": {Data: []byte("{}\n")}},
	}
	trees, err := releaseTrees("v0.2.0", folders)
	if err != nil || len(trees) != 2 {
		t.Fatalf("trees %v, err %v", trees, err)
	}
	for _, tr := range trees {
		if tr.Tested != (tr.Name == "comp") {
			t.Errorf("%s: tested %v", tr.Name, tr.Tested)
		}
	}
	if Tested("rend") || !Tested("render") {
		t.Error("Tested matches whole names")
	}
}
