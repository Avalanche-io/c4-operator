// Package plan turns a written development plan into a WorkJob's expansion
// document: one planned block per lane, with the edges between them, each
// edge citing the sentence it came from (agent-jobs section 10).
//
// The document is the queue's expansion document, version 1 (DESIGN 10.6;
// the shape agreed with work/sched): planned blocks are generators that a
// charter step expands later. Four optional fields carry what the plan
// knows and the queue may not use yet: the lens row (group), the paths a
// block writes (holds), plan facts (params) and the citation of every edge
// (cites).
//
// The importer only reads the plan directory. It never writes to it.
package plan

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Doc is an expansion document, version 1.
type Doc struct {
	Version int    `json:"version"`
	Units   []Unit `json:"units"`
	// Dependents says who inherits the generator's dependents: "all".
	Dependents string `json:"dependents,omitempty"`
}

// Unit is one block of the plan.
type Unit struct {
	Name  string   `json:"name"`
	Label string   `json:"label,omitempty"`
	Key   string   `json:"key,omitempty"`
	After []string `json:"after,omitempty"`
	// Work and WorkSD are a prior in slot-seconds, when the plan gives one.
	Work   float64 `json:"work,omitempty"`
	WorkSD float64 `json:"workSD,omitempty"`
	// Exists is the probability the block runs: 1 for a lane the plan lists.
	Exists  float64           `json:"exists"`
	Planned bool              `json:"planned,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Slots   int               `json:"slots,omitempty"`

	// Group is the row the block belongs to: a chapter or a phase.
	Group string `json:"group,omitempty"`
	// Holds are the paths the block writes (a lane's grants).
	Holds []string `json:"holds,omitempty"`
	// Params are facts from the plan: rules, model, refute, status, sha,
	// charter, filler, runOrder.
	Params map[string]string `json:"params,omitempty"`
	// Cites says where each edge in After came from.
	Cites []Cite `json:"cites,omitempty"`
}

// Cite is the sentence an edge came from.
type Cite struct {
	After string `json:"after"`
	File  string `json:"file"`
	Line  int    `json:"line"`
	Text  string `json:"text"`
}

// Note is something the importer read and could not turn into an edge, for
// a person to check: a sentence with ordering words and lane names but no
// pattern it knows, an edge to a lane the plan does not name, a cycle.
type Note struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
	Why  string `json:"why"`
}

// Plan is what the importer builds before it writes a document.
type Plan struct {
	units map[string]*Unit
	order []string // first-seen order, for a stable document
	Notes []Note
}

func newPlan() *Plan { return &Plan{units: map[string]*Unit{}} }

// unit returns the block named name, creating it in group if new.
func (p *Plan) unit(name, group string) *Unit {
	if u, ok := p.units[name]; ok {
		if u.Group == "" {
			u.Group = group
		}
		return u
	}
	u := &Unit{Name: name, Label: name, Key: keyOf(name), Exists: 1, Planned: true, Slots: 1, Group: group, Params: map[string]string{}}
	p.units[name] = u
	p.order = append(p.order, name)
	return u
}

// edge records that block runs after dep, citing where it was read.
func (p *Plan) edge(block, dep string, c Cite) {
	if block == dep {
		return
	}
	u := p.units[block]
	if u == nil || p.units[dep] == nil {
		return
	}
	c.After = dep
	if !slices.Contains(u.After, dep) {
		u.After = append(u.After, dep)
	}
	u.Cites = append(u.Cites, c)
}

func (p *Plan) note(file string, line int, text, why string) {
	p.Notes = append(p.Notes, Note{File: file, Line: line, Text: text, Why: why})
}

// Doc writes the plan as an expansion document, in the plan's order. It
// refuses a plan with a cycle, naming the blocks on it.
func (p *Plan) Doc() (*Doc, error) {
	if cyc := p.cycle(); cyc != nil {
		return nil, fmt.Errorf("the plan's edges form a cycle: %s", strings.Join(cyc, " → "))
	}
	d := &Doc{Version: 1, Dependents: "all"}
	for _, n := range p.order {
		u := *p.units[n]
		sort.Strings(u.After)
		u.Holds = slices.Compact(slices.Sorted(slices.Values(u.Holds)))
		if len(u.Params) == 0 {
			u.Params = nil
		}
		d.Units = append(d.Units, u)
	}
	return d, nil
}

// cycle returns one cycle in the edges, or nil.
func (p *Plan) cycle() []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var found []string
	var visit func(n string) bool
	visit = func(n string) bool {
		color[n] = grey
		stack = append(stack, n)
		u := p.units[n]
		deps := slices.Sorted(slices.Values(u.After))
		for _, d := range deps {
			switch color[d] {
			case grey:
				i := slices.Index(stack, d)
				found = append(slices.Clone(stack[i:]), d)
				return true
			case white:
				if visit(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}
	for _, n := range p.order {
		if color[n] == white && visit(n) {
			return found
		}
	}
	return nil
}

// JSON is the document, indented.
func (d *Doc) JSON() ([]byte, error) { return json.MarshalIndent(d, "", "  ") }

// keyOf is the estimator key for a block: blocks of one kind share
// history.
func keyOf(name string) string {
	switch {
	case strings.HasPrefix(name, "M2-"):
		return "metal-lane"
	case strings.HasPrefix(name, "C"):
		return "lane"
	case strings.HasPrefix(name, "A"):
		return "infra-lane"
	case strings.HasPrefix(name, "B"):
		return "frontend-lane"
	case strings.HasPrefix(name, "F"):
		return "foundation-lane"
	}
	return "lane"
}

// Extra is an edge a person adds where the plan states an order in words no
// pattern reads. It cites the plan the same way.
type Extra struct {
	Block string   `json:"block"`
	After []string `json:"after"`
	File  string   `json:"file"`
	Line  int      `json:"line"`
	Text  string   `json:"text"`
}

// AddExtras adds a person's edges, noting any that name a lane the plan
// does not list.
func (p *Plan) AddExtras(xs []Extra) {
	for _, x := range xs {
		c := Cite{File: x.File, Line: x.Line, Text: x.Text}
		for _, d := range x.After {
			switch {
			case p.units[x.Block] == nil:
				p.note(x.File, x.Line, x.Text, "an added edge names "+x.Block+", which the plan does not list")
			case p.units[d] == nil:
				p.note(x.File, x.Line, x.Text, "an added edge names "+d+", which the plan does not list")
			default:
				p.edge(x.Block, d, c)
			}
		}
	}
}

// Unit returns the block named name, or nil.
func (p *Plan) Unit(name string) *Unit { return p.units[name] }

// Len is the number of blocks.
func (p *Plan) Len() int { return len(p.order) }
