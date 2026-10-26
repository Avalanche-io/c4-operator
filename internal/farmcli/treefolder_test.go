package farmcli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tree outside the submission's folder (by .., or by a link inside it
// that leads out) is sent only on the yes of a person at a terminal who
// has seen it named: refused with --yes or no terminal, and refused inside
// an agent even with both; a tree inside goes as before. Nothing reaches
// the farm when it is refused.
func TestSubmitTreeOutsideItsFolder(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	hud := &toolsHUD{}
	srv := httptest.NewServer(hud)
	defer srv.Close()
	c, _ := NewClient(Farm{HUD: srv.URL, As: "td"})
	ctx := context.Background()

	root := t.TempDir()
	sub := filepath.Join(root, "repo")
	for _, d := range []string{filepath.Join(sub, "shot"), filepath.Join(root, "elsewhere")} {
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "sm0140.1001.exr"), []byte("plate"), 0o644)
	}
	os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(sub, "shots"))
	file := filepath.Join(sub, "sm0140-proxies.yaml")
	write := func(tree string) {
		os.WriteFile(file, []byte("c4: submit/1\nproject: saltmarsh\nshot: sm0140\ntree: "+tree+"\njobs:\n  proxies:\n    tool: proxies\n    frames: 1001-1048\n"), 0o644)
	}
	submit := func(a SubmitFileArgs, in string) (string, error) {
		var out bytes.Buffer
		o := Out{W: &out}
		if in != "" {
			o.In = strings.NewReader(in)
		}
		a.Path = file
		_, err := SubmitFile(ctx, c, o, a)
		return out.String(), err
	}

	for _, tree := range []string{"../elsewhere", "shots", filepath.Join(root, "elsewhere")} {
		write(tree)
		hud.submissions = nil
		for name, a := range map[string]SubmitFileArgs{"--yes": {Yes: true, Terminal: true}, "no terminal": {}} {
			if _, err := submit(a, "y\n"); err == nil || !strings.Contains(err.Error(), "is outside the submission's folder: run it at a terminal to confirm the folder") {
				t.Errorf("%s, %s: %v", tree, name, err)
			}
		}
		t.Setenv("CLAUDECODE", "1")
		if _, err := submit(SubmitFileArgs{Yes: true, Terminal: true}, ""); err == nil || !strings.Contains(err.Error(), "an agent cannot send a folder outside the submission") {
			t.Errorf("%s, inside an agent: %v", tree, err)
		}
		t.Setenv("CLAUDECODE", "")
		if len(hud.submissions) != 0 {
			t.Errorf("%s: refused, yet the farm was asked %d times", tree, len(hud.submissions))
		}
	}

	// at a terminal, the person's yes after the folder is named sends it
	write("../elsewhere")
	if out, err := submit(SubmitFileArgs{Terminal: true}, "y\n"); err != nil || !strings.Contains(out, "1 file in "+tildePath(filepath.Join(root, "elsewhere"))) {
		t.Errorf("at a terminal: %v\n%s", err, out)
	}
	// a tree inside goes with --yes and no terminal, as before
	write("shot")
	if _, err := submit(SubmitFileArgs{Yes: true}, ""); err != nil {
		t.Errorf("inside, --yes: %v", err)
	}
}
