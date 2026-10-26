package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
)

// A scenario loads onto the local farm named, else the current farm when
// it is local, else the only local farm; a farm up made and down removed,
// or one only signed in to, is not a local farm to load onto.
func TestScenarioFarm(t *testing.T) {
	cfg := func(current string) *farmcli.Config {
		return &farmcli.Config{Current: current, Farms: map[string]farmcli.Farm{
			"c4-local": {HUD: "http://127.0.0.1:30080", Local: true},
			"c4-walk":  {HUD: "http://127.0.0.1:30280", Local: true},
			"c4-gone":  {HUD: "http://127.0.0.1:30380", Local: true},
			"studio":   {HUD: "https://farm.studio.example"},
		}}
	}
	both := []string{"c4-local", "c4-walk"}
	for _, tc := range []struct {
		why, current, name string
		clusters           []string
		want, err          string
	}{
		{why: "the current farm", current: "c4-walk", clusters: both, want: "c4-walk"},
		{why: "the one named", current: "c4-walk", name: "c4-local", clusters: both, want: "c4-local"},
		{why: "the only local farm", current: "studio", clusters: []string{"c4-walk"}, want: "c4-walk"},
		{why: "a current farm that is not local, of two", current: "studio", clusters: both, err: "name one with --name: c4-local, c4-walk"},
		{why: "a current farm down removed", current: "c4-gone", clusters: both, err: "name one with --name: c4-local, c4-walk"},
		{why: "a name with no farm", current: "c4-walk", name: "c4-gone", clusters: both, err: "there is no local farm c4-gone; the local farms here: c4-local, c4-walk"},
		{why: "no local farm", current: "studio", err: "`c4 farm up` makes one"},
	} {
		got, _, err := scenarioFarm(cfg(tc.current), tc.clusters, tc.name)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: %q, %v; want the error %q", tc.why, got, err, tc.err)
		case tc.err == "" && (err != nil || got != tc.want):
			t.Errorf("%s: %q, %v; want %q", tc.why, got, err, tc.want)
		}
	}
}

// status and down mean the current local farm too, never a guess at
// c4-local: with another person's farm on this machine and the current
// farm not local, they name the farms and ask for --name.
func TestLocalFarmForStatusAndDown(t *testing.T) {
	cfg := &farmcli.Config{Current: "c4-walk", Farms: map[string]farmcli.Farm{
		"c4-local": {Local: true}, "c4-walk": {Local: true}, "studio": {HUD: "https://farm.studio.example"}}}
	both := []string{"c4-local", "c4-walk"}
	if got, _, err := localFarm(cfg, both, "", "down deletes"); err != nil || got != "c4-walk" {
		t.Errorf("the current farm: %q %v", got, err)
	}
	cfg.Current = "studio"
	if _, _, err := localFarm(cfg, both, "", "down deletes"); err == nil || err.Error() != "down deletes a local farm, and the current farm is not one; name one with --name: c4-local, c4-walk" {
		t.Errorf("two, none current: %v", err)
	}
	if _, _, err := localFarm(cfg, nil, "", "status reports on"); !errors.Is(err, errNoLocalFarm) {
		t.Errorf("none: %v", err)
	}
}
