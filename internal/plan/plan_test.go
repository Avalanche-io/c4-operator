package plan

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

func read(t *testing.T) *Plan {
	t.Helper()
	p, err := Read("testdata/port.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A plan written as data becomes the queue's document with the defaults
// filled in: blocks are generators that exist for certain on one slot,
// waits are not, and every edge cites the file (with its reason, when the
// file gives one).
func TestReadFile(t *testing.T) {
	p := read(t)
	if p.Len() != 11 || len(p.Notes) != 0 {
		t.Fatalf("%d blocks, notes %v", p.Len(), p.Notes)
	}
	tok := p.Unit("text/tokens")
	if !slices.Equal(slices.Sorted(slices.Values(tok.After)), []string{"text/scan", "text/units"}) || tok.Key != "module" || !tok.Planned || tok.Slots != 1 || tok.Exists != 1 {
		t.Errorf("text/tokens %+v", tok)
	}
	if i := slices.IndexFunc(tok.Cites, func(c Cite) bool { return c.After == "text/scan" }); i < 0 || tok.Cites[i].Text != "tokens imports scan" {
		t.Errorf("text/tokens cites %+v", tok.Cites)
	}
	brk := p.Unit("api-break")
	if brk.Wait == nil || brk.Planned || brk.Key != "wait" || !strings.Contains(brk.Label, "open()") || brk.Group != "api" {
		t.Errorf("api-break %+v", brk)
	}
	if !slices.Contains(p.Unit("api/document").After, "api-break") || !slices.Contains(p.Unit("cli/main").After, "upstream-review") {
		t.Error("a wait's before is not its blocks' after")
	}
	if g, n := p.Groups(); !slices.Equal(g, []string{"leaves", "core", "api", "cli"}) || n["leaves"] != 3 {
		t.Errorf("groups %v %v", g, n)
	}
	d, err := p.Doc()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := JSON(d)
	x, err := class.ExpandDocument(&c4v1.WorkJob{}, class.Unit{Name: "root", Key: "graph", Generator: true}, b, map[string]int{"root": 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Units) != 11 {
		t.Fatalf("the queue reads %d units", len(x.Units))
	}
}

// A file the builder cannot take: an unknown field, a name used twice, the
// wrong version. An edge to a block the file does not list is a note, and
// one that closes a cycle makes the document an error naming the cycle.
func TestFileErrorsAndCycles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field": "version: 1\nblocks: [{name: a, afterr: [b]}]\n",
		"twice":         "version: 1\nblocks: [{name: a}, {name: a}]\n",
		"version":       "version: 2\nblocks: [{name: a}]\n",
	} {
		f := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".yaml")
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(f); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	p := read(t)
	p.AddExtras([]Extra{{Block: "text/scan", After: []string{"text/zzz"}, File: "x", Line: 1, Text: "a typo"}})
	if !slices.ContainsFunc(p.Notes, func(n Note) bool { return strings.Contains(n.Why, "text/zzz") }) {
		t.Error("no note for an added edge to an unknown block")
	}
	p.AddExtras([]Extra{{Block: "text/scan", After: []string{"cli/main"}, File: "x", Line: 2, Text: "wrong"}})
	if _, err := p.Doc(); err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "cli/main") {
		t.Fatalf("cycle: %v", err)
	}
}

// Doc refuses what the queue would refuse: here a wait that is also a
// generator.
func TestDocIsWhatTheQueueTakes(t *testing.T) {
	p := read(t)
	p.Unit("api-break").Planned = true
	if _, err := p.Doc(); err == nil || !strings.Contains(err.Error(), "refuse") {
		t.Fatalf("doc: %v", err)
	}
}

// A plan file's claims and gate reach the queue as written, and a claim the
// queue would refuse is refused here.
func TestClaimsAndGate(t *testing.T) {
	f := &File{Version: 1, Blocks: []FileBlock{
		{Name: "a", Holds: []string{"src/a"}, Claims: []class.DocClaim{{On: "db/test", Mode: "shared"}}},
		{Name: "a-merge", After: []string{"a"}, Planned: new(bool), Gate: "main"},
	}}
	p, err := FromFile(f, "x.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d, err := p.Doc()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := JSON(d)
	x, err := class.ExpandDocument(&c4v1.WorkJob{}, class.Unit{Name: "root", Key: "graph", Generator: true}, b, map[string]int{"root": 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Units[0].Claims) != 2 || x.Units[1].Gate != "main" {
		t.Fatalf("the queue reads claims %+v, gate %q", x.Units[0].Claims, x.Units[1].Gate)
	}
	f.Blocks[0].Claims[0].Mode = "sometimes"
	if p, err = FromFile(f, "x.yaml"); err == nil {
		_, err = p.Doc()
	}
	if err == nil || !strings.Contains(err.Error(), "refuse") {
		t.Fatalf("a bad claim mode: %v", err)
	}
}

// After a merge the plan states a new ordering between blocks the job
// already runs: EdgesSince gives it as a document of edges alone, with its
// citation, and the queue takes it; a block the job lacks and an ordering
// the plan dropped are notes. Run again with that document folded in, it
// gives nothing new.
func TestEdgesSince(t *testing.T) {
	p := read(t)
	prev, err := p.Doc()
	if err != nil {
		t.Fatal(err)
	}
	next := read(t)
	next.AddExtras([]Extra{{Block: "io/files", After: []string{"text/units"}, File: "port.yaml", Line: 9, Text: "files sizes buffers in units"}})
	next.Unit("api/render").After = slices.DeleteFunc(next.Unit("api/render").After, func(s string) bool { return s == "text/format" })
	next.Block("text/hyphen", "core")
	d, notes, err := next.EdgesSince(prev)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Units) != 0 || len(d.Edges) != 1 || d.Edges[0].From != "text/units" || d.Edges[0].To != "io/files" || !strings.Contains(string(d.Edges[0].Cite), "buffers") {
		t.Fatalf("edges %+v", d)
	}
	if !slices.ContainsFunc(notes, func(n Note) bool { return n.Text == "text/hyphen" }) ||
		!slices.ContainsFunc(notes, func(n Note) bool { return n.Text == "api/render after text/format" }) {
		t.Errorf("notes %+v", notes)
	}
	Fold(prev, d)
	again, _, err := next.EdgesSince(prev)
	if err != nil || len(again.Edges) != 0 {
		t.Fatalf("again: %v %+v", err, again)
	}
	// an ordering that closes a cycle is an error, not a document
	next.AddExtras([]Extra{{Block: "text/scan", After: []string{"cli/main"}, File: "x", Line: 1, Text: "wrong"}})
	if _, _, err := next.EdgesSince(prev); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle: %v", err)
	}
}

// Owners maps each held path to its block; a path two blocks hold is a
// note, not an owner.
func TestOwners(t *testing.T) {
	p := read(t)
	p.Unit("text/format").Holds = append(p.Unit("text/format").Holds, "text/scan")
	o, notes := p.Owners()
	if o["text/tokens"] != "text/tokens" || o["io/paths"] != "io/paths" {
		t.Errorf("owners %v", o)
	}
	if _, ok := o["text/scan"]; ok || len(notes) != 1 {
		t.Errorf("a shared path: %v %v", o["text/scan"], notes)
	}
}
