package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/internal/keystore"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
)

// c4 farm open --as ROLE opens a local farm's role, and only a role it
// has; a studio's farm opens as the person, so --as is refused there. Both
// refusals come before any browser opens.
func TestOpenAsARole(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := farmcli.LoadConfig(farmcli.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Farms = map[string]farmcli.Farm{
		"c4-walk": {HUD: "http://127.0.0.1:1", As: "noor", Local: true},
		"studio":  {HUD: "https://farm.studio.example", As: "noor"},
	}
	cfg.Current = "c4-walk"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"open", "--as", "nobody"}, &out, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), `no role "nobody" on the local farm`) {
		t.Errorf("an unknown role: %v", err)
	}
	if err := run([]string{"open", "--as", "artist", "--farm", "studio"}, &out, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "--as opens a local farm's role") {
		t.Errorf("a studio's farm: %v", err)
	}
}

// login enrols this machine's key only for a person at a terminal: from
// an agent or a script it says how to finish, and on a farm with no
// sign-in provider it says nothing.
func TestLoginKeyOnlyAtATerminal(t *testing.T) {
	f := peopletest.Studio(t)
	c, err := farmcli.NewClient(farmcli.Farm{HUD: f.Server.URL})
	if err != nil {
		t.Fatal(err)
	}
	st := keystore.Store{Dir: filepath.Join(t.TempDir(), "keys"), FileOnly: true}
	var out bytes.Buffer
	loginKey(context.Background(), c, st, 0, false, &out)
	if !strings.Contains(out.String(), "run c4 farm login yourself, at a terminal") || st.Has(keystore.Device) {
		t.Errorf("not at a terminal: %q (a key made: %v)", out.String(), st.Has(keystore.Device))
	}
	one := peopletest.OnePerson(t)
	oc, err := farmcli.NewClient(farmcli.Farm{HUD: one.Server.URL})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	loginKey(context.Background(), oc, st, 0, true, &out)
	if out.Len() != 0 || st.Has(keystore.Device) {
		t.Errorf("a farm with no provider: %q", out.String())
	}
}

// c4 farm touch-key is a person's at a terminal: never from a script.
func TestTouchKeyOnlyAtATerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDECODE", "")
	var out bytes.Buffer
	if err := run([]string{"touch-key"}, &out, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "at a terminal") {
		t.Errorf("from a script: %v", err)
	}
}

// Inside an agent the CLI reaches no local farm on its installer token:
// the agent uses the farm's tools, whose server signs as it.
func TestNoInstallerTokenInsideAnAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDECODE", "1")
	cfg, err := farmcli.LoadConfig(farmcli.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Farms = map[string]farmcli.Farm{"c4-walk": {HUD: "http://127.0.0.1:1", As: "noor", Local: true, InstallerTokenFile: "~/.c4/c4-walk/installer-token"}}
	cfg.Current = "c4-walk"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"ls"}, &out, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "the farm is reached through its tools") {
		t.Errorf("ls inside an agent: %v", err)
	}
}
