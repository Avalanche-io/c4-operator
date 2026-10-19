package plan

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func load(t *testing.T) *Plan {
	t.Helper()
	p, err := ImportLanes("testdata/lanes", "M1")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func after(t *testing.T, p *Plan, name string) []string {
	t.Helper()
	u := p.Unit(name)
	if u == nil {
		t.Fatalf("no block %s", name)
	}
	a := slices.Clone(u.After)
	slices.Sort(a)
	return a
}

// Every ordering the synthetic plan states is an edge, each read from the
// sentence the test names.
func TestEdges(t *testing.T) {
	p := load(t)
	for name, want := range map[string][]string{
		"M2-1": {"C1.1", "C1.2"},                 // the M2 column
		"M2-2": {"C2.3"},                         // the M2 column
		"M2-3": {"C3.1", "C3.2", "C3.3"},         // a range: C3.1 … C3.3
		"C1.2": {"C1.0"},                         // "lands before C1.2 starts", past a clause boundary
		"C2.3": {"C2.1", "C2.2"},                 // families: "CNV + MIX come after both INT and FLT"
		"C4.1": {"C1.0", "C2.1", "C2.2", "C2.3"}, // precedes; cannot start before; starts when … landed
		"C4.2": {"C2.2", "C3.1", "C3.2", "C3.3", "C4.1"},
		"C3.3": {"C3.2"}, // "follows", in a clause of its own
		"F1":   {"B2"},   // "It starts after M1-B2 merges": the lane the previous sentence named
		"A2":   {"A1"},   // a wave's "(after A1, …)"
		"A3":   {"A0", "A1"},
		"B2":   {"B1"}, // "B1, then B2"
		"B3":   {"B1"}, // "once B1 lands"
		"C2.1": nil,
	} {
		if got := after(t, p, name); !slices.Equal(got, want) {
			t.Errorf("%s after %v, want %v", name, got, want)
		}
	}
	// every edge cites a sentence in a plan file
	for _, n := range p.order {
		u := p.units[n]
		for _, d := range u.After {
			i := slices.IndexFunc(u.Cites, func(c Cite) bool { return c.After == d })
			if i < 0 || u.Cites[i].File == "" || u.Cites[i].Line == 0 || u.Cites[i].Text == "" {
				t.Errorf("%s after %s: no citation", n, d)
			}
		}
	}
}

// Lanes carry their row, rule count, status, charter and run order; the
// charter gives the model, the refutation and the paths it holds.
func TestLaneFacts(t *testing.T) {
	p := load(t)
	c10 := p.Unit("C1.0")
	if c10.Group != "chapter 1" || c10.Label != "C1.0 modules" || c10.Params["status"] != "merged" || c10.Params["sha"] != "abc1234" {
		t.Errorf("C1.0 %+v", c10)
	}
	c12 := p.Unit("C1.2")
	if c12.Params["rules"] != "9" || c12.Params["charter"] != "charters/M1-C1.2-shapes.md" || c12.Params["refute"] != "full" ||
		!strings.Contains(c12.Params["model"], "large model") {
		t.Errorf("C1.2 params %v", c12.Params)
	}
	if !slices.Equal(c12.Holds, []string{"check/shape.go", "conformance/tables/shp.go"}) {
		t.Errorf("C1.2 holds %v", c12.Holds)
	}
	if a1 := p.Unit("A1"); a1.Params["refute"] != "none" || a1.Group != "phase A" || a1.Params["wave"] != "1" ||
		!slices.Equal(a1.Holds, []string{"tools/gate/gate.go"}) {
		t.Errorf("A1 %+v", a1)
	}
	for _, n := range []string{"C1.1", "C3.2"} {
		if p.Unit(n).Params["filler"] != "true" {
			t.Errorf("%s is a filler", n)
		}
	}
	for n, want := range map[string]string{"C2.1": "1", "C2.2": "2", "C2.3": "3", "C4.1": "4", "C3.1": "5", "C4.2": "6"} {
		if got := p.Unit(n).Params["runOrder"]; got != want {
			t.Errorf("%s runOrder %q, want %q", n, got, want)
		}
	}
	if m := p.Unit("M2-3"); m.Group != "chapter 3" || m.Params["target"] != "metal" || m.Key != "metal-lane" {
		t.Errorf("M2-3 %+v", m)
	}
	// a lane the plan defers is left out; one that defers a part stays
	if p.Unit("Z1") != nil {
		t.Error("Z1 is deferred to M2")
	}
	if a3 := p.Unit("A3"); a3 == nil || a3.Params["status"] != "merged" {
		t.Errorf("A3 %+v", a3)
	}
	if p.Unit("C9.9") != nil {
		t.Error("C9.9 is not in the plan")
	}
}

// What the importer could not read, or read with an assumption, is a note.
func TestNotes(t *testing.T) {
	p := load(t)
	want := []string{
		"names C9.9, which the plan does not list",
		"C3 read as chapter 3's lanes: C3.1, C3.2, C3.3",
		"deferred to M2",
	}
	for _, w := range want {
		if !slices.ContainsFunc(p.Notes, func(n Note) bool { return strings.Contains(n.Why, w) }) {
			t.Errorf("no note %q in %+v", w, p.Notes)
		}
	}
}

// The document is the queue's expansion document, version 1: planned
// blocks that exist, every edge named, in the plan's order.
func TestDoc(t *testing.T) {
	p := load(t)
	d, err := p.Doc()
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Version    int    `json:"version"`
		Dependents string `json:"dependents"`
		Units      []struct {
			Name    string   `json:"name"`
			After   []string `json:"after"`
			Exists  float64  `json:"exists"`
			Planned bool     `json:"planned"`
		} `json:"units"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Version != 1 || back.Dependents != "all" || len(back.Units) != p.Len() {
		t.Fatalf("doc %+v", back)
	}
	names := map[string]bool{}
	for _, u := range back.Units {
		names[u.Name] = true
		if u.Exists != 1 || !u.Planned {
			t.Errorf("%s exists %v planned %v", u.Name, u.Exists, u.Planned)
		}
	}
	for _, u := range back.Units {
		for _, a := range u.After {
			if !names[a] {
				t.Errorf("%s after %s, not in the document", u.Name, a)
			}
		}
	}
	if back.Units[0].Name != "C1.0" {
		t.Errorf("first block %s, want the plan's first lane", back.Units[0].Name)
	}
}

// A person's edges are added with their citation; one that closes a cycle
// makes the document an error naming the cycle.
func TestExtrasAndCycles(t *testing.T) {
	p := load(t)
	p.AddExtras([]Extra{
		{Block: "C3.1", After: []string{"F1"}, File: "LANES.md", Line: 9, Text: "no lane from C3.1 onward starts until F1"},
		{Block: "C8.8", After: []string{"F1"}, File: "LANES.md", Line: 9, Text: "a typo"},
	})
	if got := after(t, p, "C3.1"); !slices.Equal(got, []string{"F1"}) {
		t.Errorf("C3.1 after %v", got)
	}
	if !slices.ContainsFunc(p.Notes, func(n Note) bool { return strings.Contains(n.Why, "C8.8") }) {
		t.Error("no note for an added edge to an unknown lane")
	}
	if _, err := p.Doc(); err != nil {
		t.Fatal(err)
	}
	p.AddExtras([]Extra{{Block: "C2.1", After: []string{"C4.1"}, File: "x", Line: 1, Text: "wrong"}})
	_, err := p.Doc()
	if err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "C2.1") || !strings.Contains(err.Error(), "C4.1") {
		t.Fatalf("cycle: %v", err)
	}
}
