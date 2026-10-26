package server

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/hud"
)

// A tool's test job is the farm's setup, never a person's work: c4 farm ls
// leaves it out, and so must the HUD the manager embeds. The HUD knows one
// by the field the summary carries (api.Job.ToolTest), so a HUD synced from
// before c4-hud 4e95c06, or a renamed JSON tag, would put every tool test
// back in the person's lists and counts.
func TestEmbeddedHUDKeepsToolTestsOut(t *testing.T) {
	b, err := json.Marshal(api.Job{ToolTest: "comp"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"toolTest":"comp"`) {
		t.Fatalf("a job's summary no longer says toolTest: %s", b)
	}
	words, err := fs.ReadFile(hud.Web, "web/design/components/words.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(words), "farmCheck = (j) => !!(j && j.toolTest)") {
		t.Fatal("web/design/components/words.js has no farmCheck reading toolTest: the embedded HUD predates the Farm setup section; sync c4-hud (hack/sync-hud.sh)")
	}
	app, err := fs.ReadFile(hud.Web, "web/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(app), "all.filter((j) => !farmCheck(j))") {
		t.Fatal("web/js/app.js no longer filters tool tests out of the person's jobs")
	}
	if _, err := fs.Stat(hud.Web, "web/js/views/setup.js"); err != nil {
		t.Fatalf("the Farm setup section is not embedded: %v", err)
	}
}
