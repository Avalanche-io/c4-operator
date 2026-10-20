package main

import (
	"bytes"
	"os"
	"testing"
)

// docs/agents/tools.md is what c4-operator mcp --tools-md prints: the
// reference lists every tool the server has.
func TestToolsDocIsCurrent(t *testing.T) {
	var b bytes.Buffer
	if err := toolsMarkdown(&b); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../docs/agents/tools.md")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc, b.Bytes()) {
		t.Fatal("docs/agents/tools.md is stale: run c4-operator mcp --tools-md > docs/agents/tools.md")
	}
}
