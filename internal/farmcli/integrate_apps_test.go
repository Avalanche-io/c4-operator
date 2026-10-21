package farmcli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/integrations/dcc"
)

// c4 farm integrate, end to end on a made-up machine: a scratch home
// holding a Steam Blender and, on macOS, two Unreal Engines the Epic
// launcher lists. integrations lists each version; --show and a no change
// nothing; a yes installs one version and a second on says it's done; off
// leaves the home byte for byte as it was. Everything it writes is under
// the scratch home; nothing here reads or writes the real one.
func TestIntegrateAppsEndToEnd(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the made-up machine is a macOS or Linux one")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	if h, err := os.UserHomeDir(); err != nil || h != home {
		t.Fatalf("the home is %q (%v), not the scratch one; stopping before anything is read", h, err)
	}
	unreal := runtime.GOOS == "darwin" // the Epic launcher's list is only looked for in the home on macOS
	if unreal {
		app := filepath.Join(home, "Library", "Application Support", "Steam", "steamapps", "common", "Blender", "Blender.app")
		writeFile(t, filepath.Join(app, "Contents", "Info.plist"), `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>CFBundleShortVersionString</key>
	<string>4.5.3</string>
</dict>
</plist>
`)
		type install struct{ InstallLocation, AppName, AppVersion string }
		var list struct{ InstallationList []install }
		for _, v := range []string{"5.6", "5.7"} {
			dir := filepath.Join(home, "Epic Games", "UE_"+v)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			list.InstallationList = append(list.InstallationList, install{dir, "UE_" + v, v + ".0"})
		}
		b, _ := json.Marshal(list)
		writeFile(t, filepath.Join(home, "Library", "Application Support", "Epic", "UnrealEngineLauncher", "LauncherInstalled.dat"), string(b))
	} else if err := os.MkdirAll(filepath.Join(home, ".steam", "steam", "steamapps", "common", "Blender", "4.5", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := treeSums(t, home)
	var out bytes.Buffer
	o := Out{W: &out}
	asked := 0
	yes := func(string) bool { asked++; return true }
	no := func(string) bool { asked++; return false }

	// integrations lists each planted version, not yet in. This machine's
	// own applications (/Applications, /opt) may be listed too, so only
	// the planted ones are looked at, and only they are changed.
	Integrations(&Config{}, Detect(false), o)
	want := []string{"blender@4.5"}
	if unreal {
		want = append(want, "unreal@5.6", "unreal@5.7")
	}
	for _, w := range want {
		if !tableHas(out.String(), w, dcc.StatusAvailable) {
			t.Errorf("integrations has no %s %s:\n%s", w, dcc.StatusAvailable, out.String())
		}
	}

	// --show names every file, each in the scratch home, and writes none
	out.Reset()
	if err := App(o, "blender@4.5", true, true, yes); err != nil || asked != 1 || !strings.Contains(out.String(), "Blender 4.5  would: ") {
		t.Fatalf("--show: %v, asked %d\n%s", err, asked, out.String())
	}
	writes := 0
	for _, l := range strings.Split(out.String(), "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "writes") && !strings.HasPrefix(l, "removes") {
			continue
		}
		writes++
		if f := strings.Fields(l); !strings.HasPrefix(f[1], "~/") {
			t.Errorf("--show names a file outside the home: %s", l)
		}
	}
	if writes == 0 {
		t.Errorf("--show named no files:\n%s", out.String())
	}
	sameTree(t, "after --show", before, treeSums(t, home))

	// a no changes nothing
	out.Reset()
	if err := App(o, "blender@4.5", true, false, no); err != nil || asked != 2 || !strings.Contains(out.String(), "Blender 4.5  not changed\n") {
		t.Fatalf("no: %v, asked %d\n%s", err, asked, out.String())
	}
	sameTree(t, "after a no", before, treeSums(t, home))

	// a yes adds it, and integrations says so
	out.Reset()
	if err := App(o, "blender@4.5", true, false, yes); err != nil || asked != 3 || strings.Contains(out.String(), "not changed") {
		t.Fatalf("on: %v, asked %d\n%s", err, asked, out.String())
	}
	if status(t, "blender", "4.5") != dcc.StatusInstalled {
		t.Errorf("Blender 4.5 after on: %s", status(t, "blender", "4.5"))
	}
	out.Reset()
	Integrations(&Config{}, Detect(false), o)
	if !tableHas(out.String(), "blender@4.5", dcc.StatusInstalled) {
		t.Errorf("integrations after on:\n%s", out.String())
	}

	// a second on asks nothing and changes nothing
	installed := treeSums(t, home)
	out.Reset()
	if err := App(o, "blender", true, false, yes); err != nil || asked != 3 || out.String() != "Blender 4.5  has C4 already\n" {
		t.Errorf("on again: %v, asked %d\n%s", err, asked, out.String())
	}
	sameTree(t, "after a second on", installed, treeSums(t, home))

	if unreal {
		// NAME@VERSION adds one version and says what is left to do
		out.Reset()
		if err := App(o, "unreal@5.7", true, false, yes); err != nil || asked != 4 || !strings.Contains(out.String(), "then: ") {
			t.Fatalf("unreal@5.7 on: %v, asked %d\n%s", err, asked, out.String())
		}
		if s := status(t, "unreal", "5.7"); s != dcc.StatusInstalled {
			t.Errorf("Unreal 5.7 after on: %s", s)
		}
		if s := status(t, "unreal", "5.6"); s != dcc.StatusAvailable {
			t.Errorf("Unreal 5.6, never asked about: %s", s)
		}
	}

	// off takes out each version c4 recorded, and the home is as it was
	for _, name := range []string{"blender", "unreal"} {
		out.Reset()
		if err := App(o, name, false, false, yes); err != nil || strings.Contains(out.String(), "not changed") {
			t.Errorf("%s off: %v\n%s", name, err, out.String())
		}
	}
	if left := dcc.Installed(); len(left) != 0 {
		t.Errorf("installed after off: %+v", left)
	}
	out.Reset()
	if err := App(o, "blender", false, false, yes); err != nil || out.String() != "blender     c4 is in no version of it\n" {
		t.Errorf("off again: %v\n%s", err, out.String())
	}
	sameTree(t, "after off", before, treeSums(t, home))
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tableHas says some line of an integrations table starts with name and
// holds state.
func tableHas(table, name, state string) bool {
	for _, l := range strings.Split(table, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == name && strings.Contains(l, "  "+state+"  ") {
			return true
		}
	}
	return false
}

func status(t *testing.T, name, version string) string {
	t.Helper()
	for _, f := range dcc.Detect() {
		if f.Name == name && f.Version == version {
			return f.Status
		}
	}
	t.Fatalf("%s %s not found", name, version)
	return ""
}

// treeSums is every folder, file and link under root, each with its
// content's digest or link's target. The lock c4 keeps beside its records
// is left out; it stays by design.
func treeSums(t *testing.T, root string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == filepath.Join(".c4", "integrations", ".lock") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			sums[rel] = "dir " + info.Mode().Perm().String()
		case d.Type()&fs.ModeSymlink != 0:
			to, err := os.Readlink(p)
			if err != nil {
				return err
			}
			sums[rel] = "link " + to
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h := sha256.Sum256(b)
			sums[rel] = info.Mode().Perm().String() + " " + hex.EncodeToString(h[:])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sums
}

func sameTree(t *testing.T, when string, want, got map[string]string) {
	t.Helper()
	for p, w := range want {
		if g, ok := got[p]; !ok {
			t.Errorf("%s: %s is gone", when, p)
		} else if g != w {
			t.Errorf("%s: %s changed: %s, was %s", when, p, g, w)
		}
	}
	for p, g := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s: %s is new (%s)", when, p, g)
		}
	}
}
