package class

import (
	"slices"
	"strings"
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

func graphJob(params map[string]string) *c4v1.WorkJob {
	return &c4v1.WorkJob{Spec: c4v1.WorkJobSpec{Class: "graph", Params: params}}
}

// A generator's document becomes children after it, with sibling edges,
// fromUnit inputs (also edges), and the generator's dependents rewired.
func TestExpandDocument(t *testing.T) {
	d, err := Lookup("graph")
	if err != nil {
		t.Fatal(err)
	}
	job := graphJob(nil)
	p, err := d.Plan(job)
	if err != nil || len(p.Units) != 1 || !p.Units[0].Generator || p.Units[0].Name != "plan" {
		t.Fatalf("plan %+v %v", p, err)
	}
	// A unit planned after the root (as a class may), to see it rewired.
	p.Units = append(p.Units, Unit{Index: 1, Name: "report", After: []int{0}, Slots: 1})
	p.Order = append(p.Order, 1)
	doc := `{"version": 1, "dependents": "all", "units": [
		{"name": "deep", "work": 600, "group": "layers"},
		{"name": "beauty", "after": ["deep"], "inputs": [{"fromUnit": "deep", "mountPath": "/deep"}], "env": {"PASS": "beauty"}},
		{"name": "C3.2", "planned": true, "exists": 0.5, "key": "lane"}]}`
	x, err := Expand(d, job, p.Units[0], []byte(doc), p.Names(), len(p.Units))
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Units) != 3 || x.Units[0].Index != 2 || x.Units[1].Name != "beauty" {
		t.Fatalf("children %+v", x.Units)
	}
	beauty := x.Units[1]
	if !slices.Equal(beauty.After, []int{2}) || len(beauty.Inputs) != 1 || beauty.Inputs[0].From != 2 || beauty.Parent != 0 || beauty.Depth != 1 {
		t.Fatalf("beauty %+v", beauty)
	}
	if beauty.Env[0].Name != "C4_UNIT_NAME" || beauty.Env[0].Value != "beauty" || beauty.Env[1].Name != "PASS" {
		t.Fatalf("env %+v", beauty.Env)
	}
	if lane := x.Units[2]; !lane.Generator || lane.Exists != 0.5 || lane.Key != "lane" {
		t.Fatalf("planned %+v", lane)
	}
	if x.Units[0].Key != "graph/child" || x.Units[0].Exists != 1 || x.Units[0].Work != 600 || x.Units[0].Group != "layers" {
		t.Fatalf("defaults %+v", x.Units[0])
	}
	p.Apply(0, x)
	if len(p.Units) != 5 || !slices.Equal(p.Units[1].After, []int{0, 2, 3, 4}) {
		t.Fatalf("after apply: %d units, report after %v", len(p.Units), p.Units[1].After)
	}
	if !slices.Equal(p.Order, []int{0, 1, 4, 2, 3}) {
		t.Fatalf("order %v: generators first among the children", p.Order)
	}

	// dependents names a subset.
	q, _ := d.Plan(job)
	q.Units = append(q.Units, Unit{Index: 1, Name: "report", After: []int{0}})
	x, err = Expand(d, job, q.Units[0], []byte(`{"version":1,"units":[{"name":"a"},{"name":"b"}],"dependents":["b"]}`), q.Names(), 2)
	if err != nil {
		t.Fatal(err)
	}
	q.Apply(0, x)
	if !slices.Equal(q.Units[1].After, []int{0, 3}) {
		t.Fatalf("subset dependents: %v", q.Units[1].After)
	}
}

func TestExpandDocumentRefuses(t *testing.T) {
	d, _ := Lookup("graph")
	job := graphJob(map[string]string{"maxExpandDepth": "2"})
	p, _ := d.Plan(job)
	gen := p.Units[0]
	for doc, want := range map[string]string{
		`{"version": 2, "units": []}`:                                                             "version",
		`{"version": 1, "units": [{"label": "x"}]}`:                                               "no name",
		`{"version": 1, "units": [{"name": "a"}, {"name": "a"}]}`:                                 "twice",
		`{"version": 1, "units": [{"name": "plan"}]}`:                                             "already",
		`{"version": 1, "units": [{"name": "a", "after": ["zz"]}]}`:                               "not in the job",
		`{"version": 1, "units": [{"name": "a", "after": ["b"]}, {"name": "b", "after": ["a"]}]}`: "cycle",
		`{"version": 1, "units": [{"name": "a", "exists": 2}]}`:                                   "exists",
		`{"version": 1, "units": [{"name": "a", "inputs": [{"fromUnit": "plan"}]}]}`:              "mountPath",
		`{"version": 1, "units": [{"name": "a"}], "dependents": ["b"]}`:                           "dependents",
		`not json`: "expansion document",
		`{"version": 1, "units": [], "edges": [{"from": "zz", "to": "plan"}]}`:                                         "an edge from zz",
		`{"version": 1, "units": [], "edges": [{"from": "plan", "to": "plan"}]}`:                                       "itself",
		`{"version": 1, "units": [{"name": "a"}, {"name": "b", "after": ["a"]}], "edges": [{"from": "b", "to": "a"}]}`: "cycle",
	} {
		if _, err := Expand(d, job, gen, []byte(doc), p.Names(), 1); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", doc, err, want)
		}
	}
	deep := gen
	deep.Depth = 2
	if _, err := Expand(d, job, deep, []byte(`{"version":1,"units":[{"name":"a"}]}`), p.Names(), 1); err == nil || !strings.Contains(err.Error(), "nests") {
		t.Fatalf("depth: %v", err)
	}
}

// A unit's reasoning (does, model, effort, tools) comes from its
// document; does defaults to the class's (an agent member is a model unit)
// else a procedure. The graph root is a procedure unless rootDoes says it
// is a model (a generator reading a plan from prose).
func TestReasoning(t *testing.T) {
	d, _ := Lookup("graph")
	p, err := d.Plan(graphJob(map[string]string{"rootDoes": "model"}))
	if err != nil || DoesOf(d, p.Units[0]) != DoesModel {
		t.Fatalf("model root: %+v %v", p, err)
	}
	if p, _ := d.Plan(graphJob(nil)); DoesOf(d, p.Units[0]) != DoesProcedure {
		t.Fatal("the root is a procedure by default")
	}
	if _, err := d.Plan(graphJob(map[string]string{"rootDoes": "llm"})); err == nil {
		t.Fatal("rootDoes llm accepted")
	}
	root := Unit{Name: "plan", Key: "graph", Generator: true}
	doc := `{"version": 1, "units": [
	  {"name": "a", "reasoning": {"does": "model", "model": "claude-sonnet-5-5", "effort": "high", "tools": "repo-rw"}},
	  {"name": "b", "reasoning": {"does": "procedure"}}, {"name": "c"}]}`
	x, err := ExpandDocument(graphJob(nil), root, []byte(doc), map[string]int{"plan": 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if r := ReasoningOf(d, x.Units[0]); r != (Reasoning{Does: "model", Model: "claude-sonnet-5-5", Effort: "high", Tools: "repo-rw"}) {
		t.Fatalf("a: %+v", r)
	}
	if DoesOf(d, x.Units[1]) != DoesProcedure || DoesOf(d, x.Units[2]) != DoesProcedure {
		t.Fatal("b and c are procedures")
	}
	for _, bad := range []string{`{"name": "a", "reasoning": {"does": "human"}}`, `{"name": "a", "reasoning": {"effort": "turbo"}}`} {
		if _, err := ExpandDocument(graphJob(nil), root, []byte(`{"version": 1, "units": [`+bad+`]}`), map[string]int{"plan": 0}, 1); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	a, _ := Lookup("agent")
	if r := ReasoningOf(a, Unit{}); r.Does != DoesModel {
		t.Fatal("an agent member is a model unit")
	}
}

// Documents are read strictly: an unknown field is an error naming it,
// and a renamed one says where it went.
func TestStrictDocuments(t *testing.T) {
	root := Unit{Name: "plan", Key: "graph", Generator: true}
	for doc, want := range map[string]string{
		`{"version": 1, "units": [{"name": "a", "does": "model"}]}`:     `unknown field "does": it is now reasoning.does`,
		`{"version": 1, "units": [{"name": "a", "kind": "model"}]}`:     `unknown field "kind": it is now reasoning.does (model or procedure)`,
		`{"version": 1, "units": [{"name": "a", "afterr": ["b"]}]}`:     `unknown field "afterr"`,
		`{"version": 1, "units": [{"name": "a"}], "dependants": "all"}`: `unknown field "dependants"`,
	} {
		_, err := ExpandDocument(graphJob(nil), root, []byte(doc), map[string]int{"plan": 0}, 1)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", doc, err, want)
		}
	}
}
