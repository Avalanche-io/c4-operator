package localfarm

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The installer token is made once, the person's alone (0600 in a 0700
// folder), kept across ups, and goes with the farm; a folder already there
// keeps its own mode.
func TestInstallerToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tok, err := InstallerToken("c4-walk")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(tok) {
		t.Fatalf("token %q", tok)
	}
	p := filepath.Join(home, ".c4", "c4-walk", "installer-token")
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Dir(p)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("folder %v %v", fi.Mode(), err)
	}
	if again, _ := InstallerToken("c4-walk"); again != tok {
		t.Error("a second up made a new token")
	}
	if other, _ := InstallerToken("c4-local"); other == tok {
		t.Error("two farms share a token")
	}
	if err := RemoveInstallerToken("c4-walk"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
		t.Errorf("the farm's folder is left: %v", err)
	}

	// a farm named like a folder ~/.c4 already has
	shared := filepath.Join(home, ".c4", "agents")
	os.MkdirAll(shared, 0o755)
	os.WriteFile(filepath.Join(shared, "keep"), []byte("x"), 0o644)
	if _, err := InstallerToken("agents"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(shared); fi.Mode().Perm() != 0o755 {
		t.Errorf("an existing folder's mode changed to %v", fi.Mode().Perm())
	}
	RemoveInstallerToken("agents")
	if _, err := os.Stat(filepath.Join(shared, "keep")); err != nil {
		t.Errorf("removing the token took the folder's other files: %v", err)
	}
}

// The manager's pod alone gets the token: a Secret, mounted read-only in
// the manager, the flag naming the file; patching again leaves one flag. A
// plan shows where the token goes and never a token.
func TestInstallerTokenReachesTheManager(t *testing.T) {
	o := Options{Owner: "noor", InstallerToken: strings.Repeat("ab", 32)}
	if !slices.Contains(ManagerArgs(o), "--hud-installer-token-file=/etc/c4/installer/token") {
		t.Fatalf("args %v", ManagerArgs(o))
	}
	p := string(ManagerPatch(o, []string{"manager", "--hud-installer-token-file=/old"}))
	for _, want := range []string{`"mountPath":"/etc/c4/installer"`, `"secretName":"c4-installer-token"`, `"defaultMode":256`} {
		if !strings.Contains(p, want) {
			t.Errorf("no %s in the patch %s", want, p)
		}
	}
	if strings.Contains(p, "/old") || strings.Count(p, "--hud-installer-token-file=") != 1 {
		t.Errorf("patch %s", p)
	}
	if strings.Contains(p, o.InstallerToken) {
		t.Error("the token is in the manager's patch")
	}
	y := string(YAML(Farm(o)))
	if !strings.Contains(y, "name: "+InstallerSecret) || !strings.Contains(y, "token: "+o.InstallerToken) {
		t.Errorf("no Secret with the token in\n%s", y)
	}
	plan := string(YAML(Farm(Options{Owner: "noor", Name: "c4-walk"})))
	if !strings.Contains(plan, "made by up, kept in ~/.c4/c4-walk/installer-token") || regexp.MustCompile(`token: [0-9a-f]{64}`).MatchString(plan) {
		t.Errorf("the plan's Secret:\n%s", plan)
	}
}
