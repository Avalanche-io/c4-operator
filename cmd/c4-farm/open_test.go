package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
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
