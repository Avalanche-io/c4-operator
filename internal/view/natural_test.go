package view

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A directory's view, as `c4-operator tree-id` scans it, lists its files in
// c4m's natural order: the conformance vectors every c4m implementation
// shares (c4m/testdata/natural_sort_vectors.json, in the c4 module this
// operator is built with), written to disk in reverse and read back in
// order. A tree ID is a hash of that listing, so an order that differs from
// the other implementations' is a different ID for the same files.
func TestScanFollowsTheSharedNaturalOrder(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/Avalanche-io/c4").Output()
	if err != nil {
		t.Skipf("the c4 module's directory: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "c4m", "testdata", "natural_sort_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vec struct {
		Sorted []string `json:"sorted"`
	}
	if err := json.Unmarshal(b, &vec); err != nil || len(vec.Sorted) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	dir := t.TempDir()
	for i := len(vec.Sorted) - 1; i >= 0; i-- {
		if err := os.WriteFile(filepath.Join(dir, vec.Sorted[i]), []byte(vec.Sorted[i]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	v, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range v.Manifest().Entries {
		got = append(got, e.Name)
	}
	if !slices.Equal(got, vec.Sorted) {
		t.Fatalf("the view's order is not c4m's natural order:\n got %q\nwant %q", got, vec.Sorted)
	}
}
