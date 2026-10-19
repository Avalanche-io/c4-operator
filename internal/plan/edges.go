package plan

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// EdgesSince is the plan re-read after a merge, as the job already running
// it needs it: an expansion document of edges alone (DESIGN 10.6, edge
// sources) holding every ordering between blocks of prev that prev does
// not have, each citing where it was read. prev is the document the job
// was created from, with any edges documents since folded in (Fold).
//
// Edges are only added. A block the plan now lists that prev does not, and
// an ordering prev has that the plan no longer states, are notes: the
// first needs an expansion of its own (a new plan version), the second a
// person. A plan whose edges form a cycle is an error.
func (p *Plan) EdgesSince(prev *class.Doc) (*class.Doc, []Note, error) {
	if cyc := p.cycle(); cyc != nil {
		return nil, nil, fmt.Errorf("the plan's edges form a cycle: %s", strings.Join(cyc, " → "))
	}
	had := map[string][]string{}
	for _, u := range prev.Units {
		had[u.Name] = u.After
	}
	for _, e := range prev.Edges {
		had[e.To] = append(had[e.To], e.From)
	}
	d := &class.Doc{Version: 1}
	var notes []Note
	for _, n := range p.order {
		u := p.units[n]
		before, ok := had[n]
		if !ok {
			notes = append(notes, Note{Text: n, Why: "a block the running job does not have: it needs an expansion of its own (a new plan version)"})
			continue
		}
		for _, dep := range slices.Sorted(slices.Values(u.After)) {
			if _, ok := had[dep]; !ok || slices.Contains(before, dep) {
				continue
			}
			e := class.DocEdge{From: dep, To: n}
			if i := slices.IndexFunc(u.Cites, func(c Cite) bool { return c.After == dep }); i >= 0 {
				b, err := json.Marshal(u.Cites[i])
				if err != nil {
					return nil, nil, err
				}
				e.Cite = b
			}
			d.Edges = append(d.Edges, e)
		}
	}
	for _, u := range prev.Units {
		now := p.units[u.Name]
		for _, dep := range u.After {
			if now != nil && p.units[dep] != nil && !slices.Contains(now.After, dep) {
				notes = append(notes, Note{Text: u.Name + " after " + dep, Why: "the running job has this ordering and the plan no longer states it; edges are only added, so a person decides"})
			}
		}
	}
	// the queue takes it against the job's names
	names := map[string]int{rootName: 0}
	for i, u := range prev.Units {
		names[u.Name] = i + 1
	}
	b, err := JSON(d)
	if err != nil {
		return nil, nil, err
	}
	if _, err := class.ExpandDocument(&c4v1.WorkJob{}, class.Unit{Name: rootName, Key: "graph", Generator: true}, b, names, len(names)); err != nil {
		return nil, nil, fmt.Errorf("the queue would refuse the edges: %w", err)
	}
	return d, notes, nil
}

// Fold adds an edges document to a running job's document, so the next
// EdgesSince compares against both.
func Fold(prev, edges *class.Doc) {
	prev.Edges = append(prev.Edges, edges.Edges...)
}

// Owners maps each path a block holds to the block, for an edge source
// that names a marker's waiting unit by the file it is in (c4-edges
// -owners). A path two blocks hold has no single owner and is a note.
func (p *Plan) Owners() (map[string]string, []Note) {
	owners := map[string]string{}
	shared := map[string]bool{}
	var notes []Note
	for _, n := range p.order {
		for _, h := range p.units[n].Holds {
			switch o, ok := owners[h]; {
			case shared[h]:
			case ok && o != n:
				shared[h] = true
				delete(owners, h)
				notes = append(notes, Note{Text: h, Why: "held by " + o + " and " + n + ": no single owner for a marker in it"})
			default:
				owners[h] = n
			}
		}
	}
	return owners, notes
}
