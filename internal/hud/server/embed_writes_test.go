package server

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/hud"
)

// The HUD the manager embeds marks every write it sends with HUDHeader.
// The installer rule refuses a write the browser's cookie authorizes
// without it, so a HUD synced from before the header (c4-hud eec7d6e)
// would fail every control in a browser opened with c4 farm open, while
// every read still worked. Writes go through web/js/api.js alone, and
// each one there carries the header.
func TestEmbeddedHUDMarksItsWrites(t *testing.T) {
	write := regexp.MustCompile(`method:\s*'(POST|PUT|PATCH|DELETE)'`)
	api, err := fs.ReadFile(hud.Web, "web/js/api.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(api)
	if !strings.Contains(src, "'"+HUDHeader+"': '1'") {
		t.Fatalf("web/js/api.js never sets %s: the embedded HUD predates the installer rule; sync c4-hud (hack/sync-hud.sh)", HUDHeader)
	}
	// each write's fetch spreads the header in
	for _, m := range write.FindAllStringIndex(src, -1) {
		end := min(len(src), m[1]+200)
		if !strings.Contains(src[m[0]:end], "...WRITE") {
			t.Errorf("web/js/api.js: a %s without the write header: %q", src[m[0]:m[1]], src[m[0]:end])
		}
	}
	// no other script writes on its own
	err = fs.WalkDir(hud.Web, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") || p == "web/js/api.js" {
			return err
		}
		b, err := fs.ReadFile(hud.Web, p)
		if err != nil {
			return err
		}
		if m := write.Find(b); m != nil {
			t.Errorf("%s sends %s itself: writes go through web/js/api.js, which marks them", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
