package farmcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A view's links land under the folder; one that would reach past it is
// skipped, and a file in a link's place is an error.
func TestMakeLinks(t *testing.T) {
	dir := t.TempDir()
	links := []api.ViewLink{
		{Path: "README", Target: "README.md"},
		{Path: "frames/latest", Target: "../renders/v003"},
		{Path: "etc", Target: "/etc"},
		{Path: "up", Target: "../../outside"},
	}
	made, skipped, err := makeLinks(dir, links)
	if err != nil || made != 2 || len(skipped) != 2 || !strings.HasPrefix(skipped[0], "etc -> /etc: the link points outside") || !strings.HasPrefix(skipped[1], "up -> ") {
		t.Fatalf("made %d, skipped %q, %v", made, skipped, err)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "frames", "latest")); got != filepath.FromSlash("../renders/v003") {
		t.Fatalf("frames/latest -> %q", got)
	}
	// again: the links are already there
	if made, _, err := makeLinks(dir, links); err != nil || made != 0 {
		t.Fatalf("second run made %d, %v", made, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := makeLinks(dir, []api.ViewLink{{Path: "LICENSE", Target: "README.md"}}); err == nil {
		t.Fatal("a file was replaced by a link")
	}
}
