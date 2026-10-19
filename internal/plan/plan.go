// Package plan builds a WorkJob's expansion document from a plan: one block
// per piece of work, the edges between them, and where each edge came from
// (agent-jobs section 10).
//
// The output is the queue's own format, expansion v1 (class.Doc; DESIGN
// 10.6), and Doc checks it the way the queue reads it. Nothing here knows
// any one project's layout. A plan reaches the builder through an adapter:
// Read takes a plan written as data (a list of blocks), and lanemap reads a
// plan written as prose in the lane-map layout. An adapter that reads prose
// records what it could not read as Notes; their count is how much of the
// plan is still only prose.
package plan

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// DefaultKey is the estimator key of a block whose plan names none.
const DefaultKey = "block"

// Unit is one block of the plan, as the builder holds it.
type Unit struct {
	Name  string
	Label string
	// Key is the estimator key: blocks of one kind share history.
	Key   string
	After []string
	// Work and WorkSD are a prior in slot-seconds, when the plan gives one.
	Work, WorkSD float64
	// Exists is the probability the block runs: 1 for one the plan lists.
	Exists float64
	// Planned makes the block a generator, expanded when it runs.
	Planned bool
	Env     map[string]string
	Inputs  []class.DocInput
	Slots   int
	// Group is the row the block belongs to.
	Group string
	// Holds are the paths or keys the block writes (10.4).
	Holds []string
	// Params are facts from the plan, carried to the queue as they are.
	Params map[string]string
	// Cites says where each edge in After came from.
	Cites []Cite
	// Wait makes the block a wait for a person, a party or a clock: no
	// slot, completed by a decision. Planned is false for it.
	Wait *class.DocWait
}

// Cite is where an edge came from: a sentence of the plan, or a line of a
// plan file.
type Cite struct {
	After string `json:"after"`
	File  string `json:"file"`
	Line  int    `json:"line"`
	Text  string `json:"text"`
}

// Note is something an adapter read and could not turn into a block or an
// edge, for a person to check: a sentence with ordering words but no
// pattern it knows, an edge to a block the plan does not name.
type Note struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
	Why  string `json:"why"`
}

// Plan is the builder an adapter fills before Doc writes the document.
type Plan struct {
	units map[string]*Unit
	order []string // first-seen order, for a stable document
	Notes []Note
}

// New returns an empty plan.
func New() *Plan { return &Plan{units: map[string]*Unit{}} }

// Block returns the block named name, creating it in group if new: a
// planned block with one slot that exists for certain. A block created
// without a group takes the first one it is given later.
func (p *Plan) Block(name, group string) *Unit {
	if u, ok := p.units[name]; ok {
		if u.Group == "" {
			u.Group = group
		}
		return u
	}
	u := &Unit{Name: name, Label: name, Key: DefaultKey, Exists: 1, Planned: true, Slots: 1, Group: group, Params: map[string]string{}}
	p.units[name] = u
	p.order = append(p.order, name)
	return u
}

// Edge records that block runs after dep, citing where it was read. An
// edge naming a block the plan does not hold is ignored; adapters note it.
func (p *Plan) Edge(block, dep string, c Cite) {
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

// Note records something the adapter could not read.
func (p *Plan) Note(file string, line int, text, why string) {
	p.Notes = append(p.Notes, Note{File: file, Line: line, Text: text, Why: why})
}

// Unit returns the block named name, or nil.
func (p *Plan) Unit(name string) *Unit { return p.units[name] }

// Names are the blocks in the order they were first seen.
func (p *Plan) Names() []string { return slices.Clone(p.order) }

// Len is the number of blocks.
func (p *Plan) Len() int { return len(p.order) }

// Doc writes the plan as an expansion document, in the plan's order, and
// reads it back as the queue would. It refuses a plan with a cycle, naming
// the blocks on it, and one the queue would refuse.
func (p *Plan) Doc() (*class.Doc, error) {
	if cyc := p.cycle(); cyc != nil {
		return nil, fmt.Errorf("the plan's edges form a cycle: %s", strings.Join(cyc, " → "))
	}
	d := &class.Doc{Version: 1, Dependents: json.RawMessage(`"all"`)}
	for _, n := range p.order {
		u := p.units[n]
		exists := u.Exists
		du := class.DocUnit{Name: u.Name, Label: u.Label, Key: u.Key, After: slices.Sorted(slices.Values(u.After)),
			Work: u.Work, WorkSD: u.WorkSD, Exists: &exists, Planned: u.Planned, Env: u.Env, Inputs: u.Inputs,
			Slots: u.Slots, Group: u.Group, Holds: slices.Compact(slices.Sorted(slices.Values(u.Holds))), Wait: u.Wait}
		if len(du.Holds) == 0 {
			du.Holds = nil
		}
		if len(u.Params) > 0 {
			du.Params = u.Params
		}
		if len(u.Cites) > 0 {
			b, err := json.Marshal(u.Cites)
			if err != nil {
				return nil, err
			}
			du.Cites = b
		}
		d.Units = append(d.Units, du)
	}
	b, err := JSON(d)
	if err != nil {
		return nil, err
	}
	if _, err := class.ExpandDocument(&c4v1.WorkJob{}, class.Unit{Name: rootName, Key: "graph", Generator: true}, b, map[string]int{rootName: 0}, 1); err != nil {
		return nil, fmt.Errorf("the queue would refuse the document: %w", err)
	}
	return d, nil
}

// rootName stands in for the generator that would emit the document.
const rootName = "plan-root"

// JSON is the document, indented.
func JSON(d *class.Doc) ([]byte, error) { return json.MarshalIndent(d, "", "  ") }

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

// Extra is an edge a person adds where a plan states an order in words no
// adapter reads. It cites the plan the same way.
type Extra struct {
	Block string   `json:"block"`
	After []string `json:"after"`
	File  string   `json:"file"`
	Line  int      `json:"line"`
	Text  string   `json:"text"`
	// Wait, with Before, adds a wait block named Block (a ruling, a
	// review): the blocks in Before run after it. It takes the row and the
	// origin of the first of them.
	Wait   *class.DocWait `json:"wait,omitempty"`
	Before []string       `json:"before,omitempty"`
}

// AddExtras adds a person's edges and waits, noting any that name a block
// the plan does not list.
func (p *Plan) AddExtras(xs []Extra) {
	for _, x := range xs {
		c := Cite{File: x.File, Line: x.Line, Text: x.Text}
		if x.Wait != nil {
			p.addWait(x, c)
			continue
		}
		for _, d := range x.After {
			switch {
			case p.units[x.Block] == nil:
				p.Note(x.File, x.Line, x.Text, "an added edge names "+x.Block+", which the plan does not list")
			case p.units[d] == nil:
				p.Note(x.File, x.Line, x.Text, "an added edge names "+d+", which the plan does not list")
			default:
				p.Edge(x.Block, d, c)
			}
		}
	}
}

// addWait adds a wait block before the blocks it holds up.
func (p *Plan) addWait(x Extra, c Cite) {
	var first *Unit
	for _, b := range x.Before {
		if u := p.units[b]; u != nil && first == nil {
			first = u
		}
	}
	switch {
	case x.Block == "" || p.units[x.Block] != nil:
		p.Note(x.File, x.Line, x.Text, "a wait needs a name of its own: "+x.Block)
		return
	case first == nil:
		p.Note(x.File, x.Line, x.Text, "a wait holds up no block the plan lists: "+x.Block)
		return
	}
	u := p.Block(x.Block, first.Group)
	u.Planned, u.Key, u.Wait = false, "wait", x.Wait
	if o := first.Params["origin"]; o != "" {
		u.Params["origin"] = o
	}
	if x.Wait.Ask != "" {
		u.Label = x.Block + ": " + x.Wait.Ask
	}
	for _, d := range x.After {
		if p.units[d] != nil {
			p.Edge(x.Block, d, c)
		}
	}
	for _, b := range x.Before {
		if p.units[b] == nil {
			p.Note(x.File, x.Line, x.Text, "a wait holds up "+b+", which the plan does not list")
			continue
		}
		p.Edge(b, x.Block, c)
	}
}

// Groups are the plan's rows in first-seen order, with their block counts.
func (p *Plan) Groups() (names []string, n map[string]int) {
	n = map[string]int{}
	for _, b := range p.order {
		g := p.units[b].Group
		if _, ok := n[g]; !ok {
			names = append(names, g)
		}
		n[g]++
	}
	return names, n
}
