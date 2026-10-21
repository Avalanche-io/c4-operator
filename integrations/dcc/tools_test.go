// Package dcc_test checks the tool folders the DCC submitters ship
// (integrations/dcc/<application>/tools/<tool>/): each reads as `c4 farm`
// reads a tool, is named for its folder, passes the farm's publish-time
// check, has a test whose tree it carries, and asks for nothing the farm
// refuses at submit today, so a studio that
// points its own image at one can publish it and submit from the panel.
package dcc_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/authoring"
	"github.com/Avalanche-io/c4-operator/internal/farmcli"
)

func TestShippedToolsRead(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("*", "tools", "*", "tool.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, y := range dirs {
		dir := filepath.Dir(y)
		name := filepath.Base(dir)
		tool, err := farmcli.ReadTool(dir)
		if err != nil {
			t.Errorf("%s: %v", dir, err)
			continue
		}
		if tool.Name != name {
			t.Errorf("%s: tool.yaml names it %q", dir, tool.Name)
		}
		for _, e := range authoring.CheckTool(tool, "", nil) {
			t.Errorf("%s: %s: %s (%s)", dir, e.Path, e.Message, e.Fix)
		}
		for _, e := range authoring.ToolRefusals(tool) {
			t.Errorf("%s: the farm would refuse it at submit: %s (%s)", dir, e.Message, e.Fix)
		}
		if tool.Test == nil {
			t.Errorf("%s: no test", dir)
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(tool.Test.Tree))); err != nil {
			t.Errorf("%s: test.tree %s: %v", dir, tool.Test.Tree, err)
		}
	}
}
