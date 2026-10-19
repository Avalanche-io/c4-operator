package class

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// Dynamic expansion (DESIGN 10.6). A generator unit writes an expansion
// document, .c4/expand.json, as it ends; the node daemon stores it at
// capture and the queue adds its units to the job's unit table under the
// generator. The document, version 1:
//
//	{ "version": 1,
//	  "units": [
//	    { "name": "C3.2/phase-1", "label": "C3.2 phase 1", "key": "builder",
//	      "after": ["C3.2/charter"],        // siblings or earlier units, by name
//	      "work": 7200, "workSD": 3600,      // a prior, slot-seconds
//	      "exists": 1,                        // the probability it runs
//	      "planned": false,                   // true: itself a generator
//	      "env": {"C4_BLOCK": "C3.2/1"},
//	      "inputs": [{"fromUnit": "C3.2/charter", "mountPath": "/charter"}],
//	      "slots": 1,
//	      "group": "chapter 3", "holds": ["src/net/"],
//	      "params": {"model": "opus"}, "cites": [...] } ],
//	  "dependents": "all" }                   // or a list of names
//
// Children inherit the generator's dependents: whatever waited on the
// generator now waits on its children, all of them or the subset
// dependents names. group, holds, params and cites are carried for the
// HUD, for claims (a later primitive) and for the record; cites are kept
// in the stored document, which the expansion record names.

// MaxExpandDepth is how deep expansion may nest unless a job says
// otherwise (param maxExpandDepth).
const MaxExpandDepth = 4

// MaxUnits bounds a job's unit table, expansions included (DESIGN 10.5).
const MaxUnits = 50000

// MaxExpandDoc bounds an expansion document's size, in bytes.
const MaxExpandDoc = 16 << 20

// UnitInput mounts another unit's captured output view.
type UnitInput struct {
	From      int
	MountPath string
}

// Expander is implemented by classes whose expansion differs from the
// document's plain reading (ExpandDocument).
type Expander interface {
	Expand(job *c4v1.WorkJob, gen Unit, doc []byte, names map[string]int, first int) (*Expansion, error)
}

// Expansion is a generator's evaluation: its children, indexes first
// onward, and which of them the generator's dependents now wait on.
type Expansion struct {
	Units      []Unit
	Dependents []int
}

// Doc is an expansion document, version 1.
type Doc struct {
	Version    int             `json:"version"`
	Units      []DocUnit       `json:"units"`
	Dependents json.RawMessage `json:"dependents,omitempty"`
}

// DocUnit is one unit of an expansion document.
type DocUnit struct {
	Name    string            `json:"name"`
	Label   string            `json:"label,omitempty"`
	Key     string            `json:"key,omitempty"`
	After   []string          `json:"after,omitempty"`
	Work    float64           `json:"work,omitempty"`
	WorkSD  float64           `json:"workSD,omitempty"`
	Exists  *float64          `json:"exists,omitempty"`
	Planned bool              `json:"planned,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Inputs  []DocInput        `json:"inputs,omitempty"`
	Slots   int               `json:"slots,omitempty"`
	Group   string            `json:"group,omitempty"`
	Holds   []string          `json:"holds,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	Cites   json.RawMessage   `json:"cites,omitempty"`
}

// DocInput is a fromUnit input.
type DocInput struct {
	FromUnit  string `json:"fromUnit"`
	MountPath string `json:"mountPath"`
}

// Expand evaluates a generator's document with the class's Expander, or
// ExpandDocument when it has none.
func Expand(d Driver, job *c4v1.WorkJob, gen Unit, doc []byte, names map[string]int, first int) (*Expansion, error) {
	if x, ok := d.(Expander); ok {
		return x.Expand(job, gen, doc, names, first)
	}
	return ExpandDocument(job, gen, doc, names, first)
}

// ExpandDocument reads an expansion document as written. names maps the
// job's named units to their indexes, for edges and inputs to units
// outside the document; first is the index of the first child. It refuses
// a malformed document whole: a bad version, a unit without a name or
// with one already taken, an edge or input to a name it cannot find, a
// cycle, an exists outside 0 to 1, or nesting deeper than the job allows.
func ExpandDocument(job *c4v1.WorkJob, gen Unit, doc []byte, names map[string]int, first int) (*Expansion, error) {
	if len(doc) > MaxExpandDoc {
		return nil, fmt.Errorf("expansion document of %d bytes, over %d", len(doc), MaxExpandDoc)
	}
	var d Doc
	dec := json.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("expansion document: %w", err)
	}
	if d.Version != 1 {
		return nil, fmt.Errorf("expansion document version %d, want 1", d.Version)
	}
	maxDepth, err := intParam(job, "maxExpandDepth", MaxExpandDepth, 1, 64)
	if err != nil {
		return nil, err
	}
	if gen.Depth+1 > maxDepth {
		return nil, fmt.Errorf("expansion of %s nests %d deep, over the job's %d", gen.Name, gen.Depth+1, maxDepth)
	}
	at := map[string]int{}
	for i, u := range d.Units {
		if u.Name == "" {
			return nil, fmt.Errorf("unit %d has no name", i)
		}
		if _, dup := at[u.Name]; dup {
			return nil, fmt.Errorf("unit %s is named twice", u.Name)
		}
		if _, taken := names[u.Name]; taken {
			return nil, fmt.Errorf("unit %s is already in the job", u.Name)
		}
		at[u.Name] = first + i
	}
	resolve := func(name string) (int, bool) {
		if i, ok := at[name]; ok {
			return i, true
		}
		i, ok := names[name]
		return i, ok
	}
	x := &Expansion{}
	for i, du := range d.Units {
		u := Unit{Index: first + i, Name: du.Name, Label: du.Label, Key: du.Key, Pos: i, Slots: du.Slots,
			Generator: du.Planned, Exists: 1, Parent: gen.Index, Depth: gen.Depth + 1,
			Work: du.Work, WorkSD: du.WorkSD, Group: du.Group, Holds: du.Holds, Params: du.Params}
		if u.Label == "" {
			u.Label = du.Name
		}
		if u.Key == "" {
			u.Key = gen.Key + "/child"
		}
		if u.Slots <= 0 {
			u.Slots = 1
		}
		if du.Exists != nil {
			if *du.Exists < 0 || *du.Exists > 1 {
				return nil, fmt.Errorf("unit %s: exists %v, want 0 to 1", du.Name, *du.Exists)
			}
			u.Exists = *du.Exists
		}
		if du.Work < 0 || du.WorkSD < 0 {
			return nil, fmt.Errorf("unit %s: negative work", du.Name)
		}
		for _, a := range du.After {
			j, ok := resolve(a)
			switch {
			case !ok:
				return nil, fmt.Errorf("unit %s runs after %s, which is not in the job", du.Name, a)
			case j == u.Index:
				return nil, fmt.Errorf("unit %s runs after itself", du.Name)
			}
			if !slices.Contains(u.After, j) {
				u.After = append(u.After, j)
			}
		}
		for _, in := range du.Inputs {
			j, ok := resolve(in.FromUnit)
			switch {
			case !ok:
				return nil, fmt.Errorf("unit %s reads %s, which is not in the job", du.Name, in.FromUnit)
			case j == u.Index:
				return nil, fmt.Errorf("unit %s reads itself", du.Name)
			case in.MountPath == "":
				return nil, fmt.Errorf("unit %s reads %s at no mountPath", du.Name, in.FromUnit)
			}
			u.Inputs = append(u.Inputs, UnitInput{From: j, MountPath: in.MountPath})
			// A unit reads what another wrote only once it is written.
			if !slices.Contains(u.After, j) {
				u.After = append(u.After, j)
			}
		}
		sort.Ints(u.After)
		keys := make([]string, 0, len(du.Env))
		for k := range du.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		u.Env = append(u.Env, env("C4_UNIT_NAME", du.Name))
		for _, k := range keys {
			u.Env = append(u.Env, env(k, du.Env[k]))
		}
		x.Units = append(x.Units, u)
	}
	if cyc := cycle(x.Units, first); cyc != nil {
		return nil, fmt.Errorf("the document's edges form a cycle: %v", cyc)
	}
	switch deps := bytes.TrimSpace(d.Dependents); {
	case len(deps) == 0 || string(deps) == `"all"`:
		for _, u := range x.Units {
			x.Dependents = append(x.Dependents, u.Index)
		}
	default:
		var list []string
		if err := json.Unmarshal(deps, &list); err != nil {
			return nil, fmt.Errorf("dependents: want \"all\" or a list of names")
		}
		for _, n := range list {
			i, ok := at[n]
			if !ok {
				return nil, fmt.Errorf("dependents names %s, which the document does not", n)
			}
			x.Dependents = append(x.Dependents, i)
		}
	}
	return x, nil
}

// cycle returns the names on one cycle among units (indexes from first),
// or nil. Edges to earlier units cannot close one.
func cycle(units []Unit, first int) []string {
	const (
		white = iota
		grey
		black
	)
	color := make([]int, len(units))
	var stack []int
	var found []string
	var visit func(i int) bool
	visit = func(i int) bool {
		color[i] = grey
		stack = append(stack, i)
		for _, a := range units[i].After {
			j := a - first
			if j < 0 || j >= len(units) {
				continue
			}
			switch color[j] {
			case grey:
				for _, k := range stack[slices.Index(stack, j):] {
					found = append(found, units[k].Name)
				}
				found = append(found, units[j].Name)
				return true
			case white:
				if visit(j) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[i] = black
		return false
	}
	for i := range units {
		if color[i] == white && visit(i) {
			return found
		}
	}
	return nil
}

// graph: a job whose units are written while it runs. Its plan is one
// generator unit, root, that runs the job's template and writes
// .c4/expand.json; its children run the same template, told apart by
// their environment ($C4_UNIT_NAME and the document's env), and planned
// children expand in turn. A plan importer, a CI step discovering its
// shards and a sweep planner are graph jobs.
//
//	params: root "plan" (the root unit's name), key "graph",
//	        turnaround "24h", estimateSeconds, maxExpandDepth "4"
type graph struct{}

func init() { register(graph{}) }

func (graph) Name() string      { return "graph" }
func (graph) Pools() []string   { return []string{"cpu"} }
func (graph) Preemptible() bool { return true }
func (graph) Checkpoints() bool { return false }
func (graph) Lens() string      { return "graph" }

func (graph) Plan(job *c4v1.WorkJob) (*Plan, error) {
	prior, err := priorParam(job)
	if err != nil {
		return nil, err
	}
	if _, err := intParam(job, "maxExpandDepth", MaxExpandDepth, 1, 64); err != nil {
		return nil, err
	}
	name, key := param(job, "root", "plan"), param(job, "key", "graph")
	cfg := estimate.DefaultConfig()
	cfg.NoLocality = true
	cfg.Default = estimate.Prior{Mean: 600, SD: 300}
	p := &Plan{OrderName: string(order.Sequential), Estimate: cfg, Prior: prior, Axis: Axis{Kind: "step"},
		Units: []Unit{{Index: 0, Name: name, Label: name, Key: key, Slots: 1, Generator: true, Exists: 1,
			Env: []corev1.EnvVar{env("C4_UNIT_NAME", name)}}},
		Order: []int{0}}
	return p, nil
}

func (graph) Objective(job *c4v1.WorkJob, ready time.Time) Objective {
	if job.Spec.Deadline != nil {
		return Objective{Type: "deadline", Deadline: job.Spec.Deadline.Time}
	}
	t, err := durationParam(job, "turnaround", 24*time.Hour)
	if err != nil {
		t = 24 * time.Hour
	}
	return Objective{Type: "deadline", Deadline: job.CreationTimestamp.Add(t)}
}

// Names maps a plan's named units to their indexes.
func (p *Plan) Names() map[string]int {
	out := map[string]int{}
	for _, u := range p.Units {
		if u.Name != "" {
			out[u.Name] = u.Index
		}
	}
	return out
}

// Apply adds an expansion's children to the plan: at the end of the unit
// table, generators first in the dispatch order (their evaluation tells
// the scheduler what the rest costs, DESIGN 10.6), and every unit that
// waited on the generator now also waits on the children dependents
// names.
func (p *Plan) Apply(gen int, x *Expansion) {
	p.Units = append(p.Units, x.Units...)
	for _, u := range x.Units {
		if u.Generator {
			p.Order = append(p.Order, u.Index)
		}
	}
	for _, u := range x.Units {
		if !u.Generator {
			p.Order = append(p.Order, u.Index)
		}
	}
	first := len(p.Units) - len(x.Units)
	for i := range p.Units[:first] {
		u := &p.Units[i]
		if !slices.Contains(u.After, gen) {
			continue
		}
		for _, c := range x.Dependents {
			if !slices.Contains(u.After, c) {
				u.After = append(u.After, c)
			}
		}
		sort.Ints(u.After)
	}
}

// Clone copies a plan deeply enough for Apply.
func (p *Plan) Clone() *Plan {
	c := *p
	c.Units = make([]Unit, len(p.Units))
	for i, u := range p.Units {
		u.After = slices.Clone(u.After)
		c.Units[i] = u
	}
	c.Order = slices.Clone(p.Order)
	return &c
}
