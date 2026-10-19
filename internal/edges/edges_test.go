package edges

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Markers in code become edges: the marker names the unit waited on, and
// the file's owner (or the marker's own to group, or the default) waits.
// Fixtures are excluded; a marker naming its own owner is no edge; what
// cannot be read is noted.
func TestMarkerEdges(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/net/conn.go", "x := 1\nreturn Pending{Lane: \"C1.1\", Why: \"needs 7.2\"}\nPending{Lane: \"C3.2\"}\n")
	write(t, root, "src/net/dial.go", "Pending{Lane: \"C1.1\"}\n")
	write(t, root, "src/eval/eval.go", "Pending{Lane: \"C2.2\"} // waits\nPending{Lane: \"C3.2\"}\n")
	write(t, root, "src/misc/x.go", "Pending{Lane: \"C9\"}\n")
	write(t, root, "policy/rulecoverage_test.go", "Pending{Lane: \"C2.2\"}\n")
	write(t, root, "README.md", "Pending{Lane: \"C5\"}\n")
	src, err := Open("marker", Config{Root: root, Pattern: `Pending\{[^}]*Lane: *"(?P<from>[^"]+)"`,
		Include: []string{"**/*.go"}, Exclude: []string{"**/*_test.go"},
		Owners: map[string]string{"src/net/": "C3.2", "src/eval/": "C4.1"}})
	if err != nil {
		t.Fatal(err)
	}
	es, notes, err := src.Edges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Document(es)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Version int
		Edges   []struct {
			From, To string
			Cite     struct {
				File string
				Line int
			}
		}
	}
	if err := json.Unmarshal(doc, &d); err != nil || d.Version != 1 {
		t.Fatalf("%s %v", doc, err)
	}
	var got []string
	for _, e := range d.Edges {
		got = append(got, e.From+">"+e.To)
	}
	// C1.1 > C3.2 once (two markers), cited at its first; C3.2's own
	// marker is no edge; eval's markers wait as C4.1; the fixture and the
	// README are not read; src/misc has no owner.
	if strings.Join(got, " ") != "C1.1>C3.2 C2.2>C4.1 C3.2>C4.1" {
		t.Fatalf("edges %v", got)
	}
	if d.Edges[0].Cite.File != "src/net/conn.go" || d.Edges[0].Cite.Line != 2 {
		t.Fatalf("cite %+v", d.Edges[0].Cite)
	}
	if len(notes) != 1 || notes[0].File != "src/misc/x.go" {
		t.Fatalf("notes %+v", notes)
	}
	if _, err := Open("marker", Config{Pattern: `Pending`}); err == nil {
		t.Fatal("a pattern without a from group")
	}
	if _, err := Open("nope", Config{}); err == nil {
		t.Fatal("an unknown source")
	}
}

func TestJSONEdges(t *testing.T) {
	f := filepath.Join(t.TempDir(), "edges.json")
	os.WriteFile(f, []byte(`[{"from": "a", "to": "b", "file": "plan.md", "line": 3}, {"from": "c"}]`), 0o644)
	src, _ := Open("json", Config{File: f})
	es, notes, err := src.Edges(context.Background())
	if err != nil || len(es) != 1 || es[0].To != "b" || len(notes) != 1 {
		t.Fatalf("%+v %+v %v", es, notes, err)
	}
}
