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
	for _, rel := range trees {
		if rel != "v0.2.0" {
			t.Fatalf("release %q", rel)
		}
	}
}
