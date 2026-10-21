package class

import (
	"fmt"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// TableParam names a graph job's unit table: an expansion document stored
// in the project's store when the job was submitted, by its C4 ID. A job
// with one has no root generator: the document's units are the job's
// first units, and its planned units expand as any generator's do. A plan
// submitted with a job and a single agent task are both graph jobs of
// this kind (DESIGN 10.6).
const TableParam = "table"

// NeedsTable is what a graph job's Plan returns when its units are in a
// stored table: the caller reads the document (from the store, or the
// one a dry run holds) and plans with TablePlan.
type NeedsTable struct{ ID string }

func (e *NeedsTable) Error() string {
	return fmt.Sprintf("the job's units are the stored table %s, which the queue reads", e.ID)
}

// TableOf is the stored table a job names, or "".
func TableOf(job *c4v1.WorkJob) string { return param(job, TableParam, "") }

// TablePlan is a graph job's plan from its stored table: the document's
// units at depth 0, in its order, generators first, with its edges. Units
// that name no key take the job's (param key, default graph).
func TablePlan(job *c4v1.WorkJob, doc []byte) (*Plan, error) {
	prior, err := priorParam(job)
	if err != nil {
		return nil, err
	}
	key := param(job, "key", "graph")
	// A virtual generator one level up, so the table's units sit at depth
	// 0, where the class plans units and their parent means nothing.
	gen := Unit{Index: 0, Key: key, Depth: -1}
	x, err := ExpandDocument(job, gen, doc, map[string]int{}, 0)
	if err != nil {
		return nil, fmt.Errorf("the job's unit table: %w", err)
	}
	if len(x.Units) == 0 {
		return nil, fmt.Errorf("the job's unit table has no units")
	}
	cfg := estimate.DefaultConfig()
	cfg.NoLocality = true
	cfg.Default = estimate.Prior{Mean: 600, SD: 300}
	p := &Plan{OrderName: string(order.Sequential), Estimate: cfg, Prior: prior, Axis: Axis{Kind: "step"}}
	for _, u := range x.Units {
		if u.Key == key+"/child" {
			u.Key = key
		}
		p.Units = append(p.Units, u)
	}
	for _, u := range p.Units {
		if u.Generator {
			p.Order = append(p.Order, u.Index)
		}
	}
	for _, u := range p.Units {
		if !u.Generator {
			p.Order = append(p.Order, u.Index)
		}
	}
	return p, nil
}
