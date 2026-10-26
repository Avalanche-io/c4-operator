package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/localfarm"
)

// The arguments c4 farm up gives the manager start it, for an owner from
// a machine with no git email (a CI runner's login, runner): its viewers
// parse, the owner is the farm admin who sees every project and the farm's
// one person, and a readable token is taken. An unreadable token stops the
// manager in words naming the flag, the v0.2.0 failure.
func TestTheLocalFarmsManagerArgsStartIt(t *testing.T) {
	args := map[string]string{}
	for _, a := range localfarm.ManagerArgs(localfarm.Options{Owner: "runner", TZ: "Etc/UTC"}) {
		k, v, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		args[k] = v
	}
	viewers, err := parseViewers(args["hud-dev-viewers"])
	if err != nil {
		t.Fatal(err)
	}
	if v := viewers["runner"]; v == nil || v.Role != "farm-admin" || !v.All {
		t.Fatalf("the owner's viewer: %+v", v)
	}
	if len(viewers) < 7 {
		t.Errorf("viewers %v: the owner shadows a role", viewers)
	}
	if got := guardedSinglePerson(args["single-person-farm"], ":8090", args["hud-dev-viewers"], args["hud-installer-token-file"], false); got != "runner" {
		t.Errorf("the one person: %q", got)
	}

	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	os.WriteFile(tok, []byte(strings.Repeat("ab", 32)+"\n"), 0o440)
	if b, err := installerToken(args["hud-dev-viewers"], tok, false); err != nil || len(b) == 0 {
		t.Fatalf("a readable token: %v", err)
	}
	if os.Getuid() != 0 {
		os.Chmod(tok, 0o000)
		if _, err := installerToken(args["hud-dev-viewers"], tok, false); err == nil || !strings.Contains(err.Error(), "--hud-installer-token-file") {
			t.Errorf("an unreadable token: %v", err)
		}
	}
}
