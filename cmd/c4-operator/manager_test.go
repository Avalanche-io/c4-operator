package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/work/policy"
)

// The farm's policy file sets values and limits; a malformed one stops
// the manager rather than running on half of it.
func TestLoadPolicy(t *testing.T) {
	defer func(s policy.Setting) { _ = policy.SetFarm(map[string]policy.Setting{policy.AsksMax: s}) }(policy.Farm(policy.AsksMax))
	dir := t.TempDir()
	good := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(good, []byte("asks.max: {value: \"4\", limit: \"6\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadPolicy(good); err != nil {
		t.Fatal(err)
	}
	if s := policy.Farm(policy.AsksMax); s.Value != "4" || s.Limit != "6" {
		t.Fatalf("farm %+v", s)
	}
	for name, body := range map[string]string{"unknown": "asks.many: {value: \"1\"}\n", "field": "asks.max: {valu: \"1\"}\n", "value": "asks.max: {value: lots}\n"} {
		p := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := loadPolicy(p); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if err := loadPolicy(""); err != nil {
		t.Fatal(err)
	}
}
