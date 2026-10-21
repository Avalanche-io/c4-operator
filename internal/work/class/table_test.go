package class

import (
	"errors"
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A graph job with a stored table plans from it: no root generator, the
// document's units at depth 0 with their edges and env, planned units
// first in the order, and the job's key on units that name none.
func TestTablePlan(t *testing.T) {
	job := &c4v1.WorkJob{Spec: c4v1.WorkJobSpec{Class: "graph", Params: map[string]string{"table": "c4xyz", "key": "agent"}}}
	d, _ := Lookup("graph")
	_, err := d.Plan(job)
	var nt *NeedsTable
	if !errors.As(err, &nt) || nt.ID != "c4xyz" {
		t.Fatalf("a job with a table planned without it: %v", err)
	}
	doc := []byte(`{"version": 1, "units": [
	  {"name": "scan", "planned": true, "env": {"C4_BLOCK_PROMPT": "port scan"}},
	  {"name": "format", "after": ["scan"], "key": "review", "reasoning": {"does": "model", "model": "m", "effort": "high"}}]}`)
	p, err := TablePlan(job, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Units) != 2 {
		t.Fatalf("units: %+v", p.Units)
	}
	scan, format := p.Units[0], p.Units[1]
	if scan.Depth != 0 || format.Depth != 0 || !scan.Generator || scan.Key != "agent" || format.Key != "review" {
		t.Fatalf("depths, generator or keys: %+v %+v", scan, format)
	}
	if len(format.After) != 1 || format.After[0] != 0 || format.Reasoning.Does != "model" || format.Reasoning.Effort != "high" {
		t.Fatalf("format: %+v", format)
	}
	found := false
	for _, e := range scan.Env {
		found = found || e.Name == "C4_BLOCK_PROMPT" && e.Value == "port scan"
	}
	if !found {
		t.Fatalf("scan's env: %+v", scan.Env)
	}
	if len(p.Order) != 2 || p.Order[0] != 0 {
		t.Fatalf("order: %v", p.Order)
	}
	// A planned unit of the table expands at depth 1 under it, as any
	// generator's children do.
	x, err := Expand(d, job, scan, []byte(`{"version": 1, "units": [{"name": "scan/translate"}]}`), p.Names(), len(p.Units))
	if err != nil || len(x.Units) != 1 || x.Units[0].Depth != 1 || x.Units[0].Parent != 0 {
		t.Fatalf("expanding a table unit: %+v %v", x, err)
	}
	if _, err := TablePlan(job, []byte(`{"version": 1, "units": []}`)); err == nil {
		t.Fatal("an empty table planned")
	}
	if _, err := TablePlan(job, []byte(`{"version": 1, "units": [{"name": "a", "after": ["b"]}, {"name": "b", "after": ["a"]}]}`)); err == nil {
		t.Fatal("a table with a cycle planned")
	}
}
