package tools_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/tools"
)

// Every prebuilt tool reads as the CLI reads a tool, its folder is named
// for it, and its test has the tree it names.
func TestPrebuiltToolsRead(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	dirs := 0
	for _, e := range entries {
		if e.IsDir() {
			dirs++
		}
	}
	if dirs != len(tools.Names) {
		t.Fatalf("%d folders, %d names: add the new folder to Names and the embed", dirs, len(tools.Names))
	}
	for _, name := range tools.Names {
		f, err := tools.Folder(name)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), name)
		if err := os.CopyFS(dir, f); err != nil {
			t.Fatal(err)
		}
		tool, err := farmcli.ReadTool(dir)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if tool.Name != name {
			t.Errorf("%s: tool.yaml names it %q", name, tool.Name)
		}
		if tool.Test == nil {
			t.Errorf("%s: no test", name)
			continue
		}
		if _, err := fs.Stat(f, filepath.ToSlash(filepath.Clean(tool.Test.Tree))); err != nil {
			t.Errorf("%s: test.tree %s: %v", name, tool.Test.Tree, err)
		}
	}
}
