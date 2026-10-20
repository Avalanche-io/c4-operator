package main

import (
	"bytes"
	"os"
	"regexp"
	"testing"
)

// docs/agents/tools.md is what c4-operator mcp --tools-md prints, but for
// the padding of its tables (commits align markdown tables): the
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
	if !bytes.Equal(unpadTables(doc), unpadTables(b.Bytes())) {
		t.Fatal("docs/agents/tools.md is stale: run c4-operator mcp --tools-md > docs/agents/tools.md")
	}
}

var (
	cellPad = regexp.MustCompile(` *\| *`)
	rule    = regexp.MustCompile(`-{3,}`)
)

// unpadTables drops the padding around table cells and the length of
// separator rules.
func unpadTables(md []byte) []byte {
	lines := bytes.Split(md, []byte("\n"))
	for i, l := range lines {
		if bytes.HasPrefix(l, []byte("|")) {
			lines[i] = rule.ReplaceAll(cellPad.ReplaceAll(bytes.TrimRight(l, " "), []byte("|")), []byte("---"))
		}
	}
	return bytes.Join(lines, []byte("\n"))
}
