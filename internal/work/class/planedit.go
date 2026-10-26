package class

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A lead's plan (LEAD.md stage 3, c4_plan). A lead edits its job's plan
// from outside the job with one call of edits, applied together or not at
// all. The HUD writes the call as one plan document: the expansion
// document a generator writes (units, after, work, edges), extended with
// what a lead does that a generator does not:
//
//	{ "version": 1,
//	  "units": [ {"name": "parse", "held": true, "work": 1800},   // add: held until released
//	             {"name": "lexer"}, {"name": "grammar"}, {"name": "docs"} ],
//	  "split":    [{"unit": "parse-all", "into": ["lexer", "grammar"], "why": "..."}],
//	  "merge":    [{"units": ["docs-a", "docs-b"], "into": "docs"}],
//	  "withdraw": [{"unit": "bench", "why": "the benchmark has a job of its own"}],
//	  "edges":    [{"from": "lexer", "to": "grammar"}, {"from": "a", "to": "b", "remove": true}],
//	  "release":  ["lexer", "parse"],
//	  "order":    ["lexer", "parse"] }
//
// A unit is named by its name, or by its index. Identities never move: a
// unit is its index everywhere, so split and merge add their replacements
// as units of the document (into names them) and put the units they
// replace into a final state, withdrawn, naming the replacements; withdraw
// does the same with none. Whatever followed a replaced unit now follows
// its replacements too, and a replacement waits on what the unit it
// replaces waited on, is held if it was, and takes its order key. Nothing
// is renumbered or removed. Only a unit that has not started is split,
// merged or withdrawn, ordered or given an edge.
//
// The edits apply in one order, whatever order the call gave them: the
// units are added, then the splits, merges and withdrawals, then the
// edges are added and removed, the units released, and the order given.
// An order lists the units its lead wants first, first: each takes the
// next key from 1, and every other unit's key is cleared.

// PlanEdits is a plan document read against a job's table: the units it
// adds and the edges it adds (the Expansion, with no dependents: the
// units hold up nothing they were not put before), and its other edits by
// index.
type PlanEdits struct {
	*Expansion
	// Withdraw are the units withdrawn, by index.
	Withdraw map[int]Withdrawal
	// Remove are the edges taken away: [from, to].
	Remove [][2]int
	// Release are the units released.
	Release []int
	// Order is the order given, first first, when Ordered.
	Order   []int
	Ordered bool
}

// Started says why unit i can no longer be changed by a plan, in the
// farm's words ("is running", "has succeeded"), or "" when it has not
// started. A unit between attempts is running.
type Started func(i int) string

// maxPlanWhy bounds a withdrawal's reason, in bytes.
const maxPlanWhy = 1000

// ReadPlan reads a lead's plan document against plan p of job, under gen
// (the unit its units are added under, as an append's are), for the call
// with key. started says which units have started; nil reads the document
// as it was applied before (a replay), when they had not. It refuses the
// document whole, in the farm's words: a unit it cannot find, a unit that
// has started, a unit another unit still reads, the job's review.
func ReadPlan(job *c4v1.WorkJob, gen Unit, doc []byte, p *Plan, key string, started Started) (*PlanEdits, error) {
	d, err := readDoc(doc)
	if err != nil {
		return nil, err
	}
	if deps := bytes.TrimSpace(d.Dependents); len(deps) > 0 && string(deps) != "[]" {
		return nil, fmt.Errorf("dependents: a plan's units hold up nothing they were not put before; give edges instead")
	}
	first := len(p.Units)
	// A unit is named by its name or its index; a name wins.
	names := p.Names()
	for i := range p.Units {
		if _, taken := names[strconv.Itoa(i)]; !taken {
			names[strconv.Itoa(i)] = i
		}
	}
	review, hasReview := p.Review()
	for _, e := range d.Edges {
		for _, n := range []string{e.From, e.To} {
			if i, ok := names[n]; ok && hasReview && i == review {
				return nil, fmt.Errorf("%s is the job's review, which comes after everything: no plan changes it", n)
			}
		}
	}
	ex, err := expandDoc(job, gen, d, names, first)
	if err != nil {
		return nil, err
	}
	ex.Dependents = nil
	if len(ex.Units) == 0 && d.planOnly() == "" && len(d.Edges) == 0 {
		return nil, fmt.Errorf("the plan makes no edit: give units to add, or units to split, merge, withdraw, release or order, or edges")
	}
	for _, u := range ex.Units {
		names[u.Name] = u.Index
	}
	unit := func(i int) *Unit {
		if i >= first {
			return &ex.Units[i-first]
		}
		return &p.Units[i]
	}
	ref := func(i int) string {
		r := job.Name + "/" + strconv.Itoa(i)
		if n := unit(i).Name; n != "" {
			r += " (" + n + ")"
		}
		return r
	}
	resolve := func(n string) (int, error) {
		i, ok := names[n]
		switch {
		case n == ReviewName || ok && hasReview && i == review:
			return 0, fmt.Errorf("%s is the job's review, which comes after everything: no plan changes it", n)
		case !ok:
			return 0, fmt.Errorf("%s is not in the job", n)
		}
		return i, nil
	}
	e := &PlanEdits{Expansion: ex, Withdraw: map[int]Withdrawal{}}
	withdrawn := func(i int) bool {
		_, now := e.Withdraw[i]
		return now || unit(i).Withdrawn != nil
	}
	// notStarted refuses a unit that has started, for an edit whose rule
	// is rule.
	notStarted := func(i int, rule string) error {
		if i >= first || started == nil {
			return nil
		}
		if why := started(i); why != "" {
			return fmt.Errorf("%s %s: %s", ref(i), why, rule)
		}
		return nil
	}
	// pending refuses a unit verb (split, merge, withdraw) cannot retire.
	pending := func(n, verb string) (int, error) {
		i, err := resolve(n)
		switch {
		case err != nil:
			return 0, fmt.Errorf("%s %w", verb, err)
		case i >= first:
			return 0, fmt.Errorf("%s %s: it is added by this plan; give it as it should be instead", verb, ref(i))
		case withdrawn(i):
			return 0, fmt.Errorf("%s %s: it is withdrawn already", verb, ref(i))
		}
		if started != nil {
			switch why := started(i); {
			case why == "is running":
				return 0, fmt.Errorf("%s is running; %s it after it ends, or steer it", ref(i), verb)
			case why != "":
				return 0, fmt.Errorf("%s %s: only a unit that has not started is split, merged or withdrawn", ref(i), why)
			}
		}
		return i, nil
	}
	// replaces is what each replacement replaces.
	replaces := map[int][]int{}
	into := func(n, verb string, old []int) ([]int, error) {
		i, ok := names[n]
		if !ok || i < first {
			return nil, fmt.Errorf("%s into %s: name a unit this plan adds (in its units)", verb, n)
		}
		if _, taken := replaces[i]; taken {
			return nil, fmt.Errorf("%s into %s: it replaces other units already in this plan", verb, n)
		}
		replaces[i] = old
		return []int{i}, nil
	}
	for _, s := range d.Split {
		i, err := pending(s.Unit, "split")
		if err != nil {
			return nil, err
		}
		if len(s.Into) == 0 {
			return nil, fmt.Errorf("split %s: into names the units that replace it", ref(i))
		}
		var by []int
		for _, n := range s.Into {
			b, err := into(n, "split "+ref(i), []int{i})
			if err != nil {
				return nil, err
			}
			by = append(by, b...)
		}
		e.Withdraw[i] = Withdrawal{Why: whyOr(s.Why, "split into "+strings.Join(s.Into, ", ")), By: by, Edit: key}
	}
	for _, mg := range d.Merge {
		if len(mg.Units) < 2 {
			return nil, fmt.Errorf("merge into %s: units names at least two units to merge", mg.Into)
		}
		var old []int
		for _, n := range mg.Units {
			i, err := pending(n, "merge")
			if err != nil {
				return nil, err
			}
			if slices.Contains(old, i) {
				return nil, fmt.Errorf("merge %s: it is named twice", ref(i))
			}
			old = append(old, i)
		}
		by, err := into(mg.Into, "merge", old)
		if err != nil {
			return nil, err
		}
		for _, i := range old {
			e.Withdraw[i] = Withdrawal{Why: whyOr(mg.Why, "merged into "+mg.Into), By: by, Edit: key}
		}
	}
	for _, w := range d.Withdraw {
		i, err := pending(w.Unit, "withdraw")
		if err != nil {
			return nil, err
		}
		switch why := strings.TrimSpace(w.Why); {
		case why == "":
			return nil, fmt.Errorf("withdraw %s: say why, for its record", ref(i))
		case len(why) > maxPlanWhy:
			return nil, fmt.Errorf("withdraw %s: why is %d bytes, over %d", ref(i), len(why), maxPlanWhy)
		}
		e.Withdraw[i] = Withdrawal{Why: strings.TrimSpace(w.Why), Edit: key}
	}
	for k, old := range replaces {
		// A replacement takes the place of what it replaces.
		u := unit(k)
		for _, r := range old {
			o := p.Units[r]
			for _, a := range o.After {
				if _, gone := e.Withdraw[a]; !gone && !slices.Contains(u.After, a) {
					u.After = append(u.After, a)
				}
			}
			u.Held = u.Held || o.Held
			if o.OrderKey > 0 && (u.OrderKey == 0 || o.OrderKey < u.OrderKey) {
				u.OrderKey = o.OrderKey
			}
		}
		sort.Ints(u.After)
	}
	// A withdrawn unit has no output: nothing that runs may read it.
	for k := range first + len(ex.Units) {
		if withdrawn(k) {
			continue
		}
		for _, in := range unit(k).Inputs {
			if _, gone := e.Withdraw[in.From]; gone {
				return nil, fmt.Errorf("%s reads %s's output: withdraw, split or merge it in the same plan, or keep %s",
					ref(k), ref(in.From), ref(in.From))
			}
		}
	}
	const edgeRule = "an edge goes to a unit that has not started"
	for _, x := range ex.Edges {
		for _, i := range x {
			if withdrawn(i) {
				return nil, fmt.Errorf("an edge from %s to %s: %s is withdrawn", ref(x[0]), ref(x[1]), ref(i))
			}
		}
		if err := notStarted(x[1], edgeRule); err != nil {
			return nil, err
		}
	}
	for _, de := range d.Edges {
		if !de.Remove {
			continue
		}
		f, err := resolve(de.From)
		if err != nil {
			return nil, fmt.Errorf("an edge from %w", err)
		}
		t, err := resolve(de.To)
		if err != nil {
			return nil, fmt.Errorf("an edge to %w", err)
		}
		switch to := unit(t); {
		case !slices.Contains(to.After, f):
			return nil, fmt.Errorf("%s does not run after %s: there is no edge to remove", ref(t), ref(f))
		case slices.ContainsFunc(to.Inputs, func(in UnitInput) bool { return in.From == f }):
			return nil, fmt.Errorf("%s reads %s's output: that edge stays while it does", ref(t), ref(f))
		case withdrawn(t):
			return nil, fmt.Errorf("an edge to %s: it is withdrawn", ref(t))
		}
		if err := notStarted(t, edgeRule); err != nil {
			return nil, err
		}
		e.Remove = append(e.Remove, [2]int{f, t})
	}
	for _, n := range d.Release {
		i, err := resolve(n)
		if err != nil {
			return nil, fmt.Errorf("release %w", err)
		}
		if withdrawn(i) {
			return nil, fmt.Errorf("release %s: it is withdrawn, and a withdrawn unit never runs", ref(i))
		}
		e.Release = append(e.Release, i)
	}
	if len(d.Order) > 0 {
		e.Ordered = true
		for _, n := range d.Order {
			i, err := resolve(n)
			switch {
			case err != nil:
				return nil, fmt.Errorf("order: %w", err)
			case slices.Contains(e.Order, i):
				return nil, fmt.Errorf("order: %s is named twice", ref(i))
			case withdrawn(i):
				return nil, fmt.Errorf("order: %s is withdrawn, and a withdrawn unit never runs", ref(i))
			}
			if err := notStarted(i, "order names only units that have not started"); err != nil {
				return nil, err
			}
			e.Order = append(e.Order, i)
		}
	}
	return e, nil
}

func whyOr(why, alt string) string {
	if w := strings.TrimSpace(why); w != "" {
		return w
	}
	return alt
}

// ApplyPlan makes a plan's edits to the table: its units at the end, as
// an expansion's under unit gen; each unit withdrawn put in its final
// state, and whatever followed it now following its replacements too; the
// edges removed; the units released; and the order given.
func (p *Plan) ApplyPlan(gen int, e *PlanEdits) {
	p.Apply(gen, e.Expansion)
	ws := make([]int, 0, len(e.Withdraw))
	for i := range e.Withdraw {
		ws = append(ws, i)
	}
	sort.Ints(ws)
	for _, i := range ws {
		w := e.Withdraw[i]
		w.By = slices.Clone(w.By)
		p.Units[i].Withdrawn = &w
		for k := range p.Units {
			u := &p.Units[k]
			if !slices.Contains(u.After, i) || slices.Contains(w.By, k) {
				continue
			}
			for _, b := range w.By {
				if !slices.Contains(u.After, b) {
					u.After = append(u.After, b)
				}
			}
			sort.Ints(u.After)
		}
	}
	for _, r := range e.Remove {
		u := &p.Units[r[1]]
		u.After = slices.DeleteFunc(slices.Clone(u.After), func(a int) bool { return a == r[0] })
	}
	for _, i := range e.Release {
		p.Units[i].Held = false
	}
	if e.Ordered {
		for k := range p.Units {
			p.Units[k].OrderKey = 0
		}
		for n, i := range e.Order {
			p.Units[i].OrderKey = n + 1
		}
	}
}

// DocSplit replaces a unit with the document's units into names.
type DocSplit struct {
	Unit string   `json:"unit"`
	Into []string `json:"into"`
	Why  string   `json:"why,omitempty"`
}

// DocMerge replaces units with the one document unit into names.
type DocMerge struct {
	Units []string `json:"units"`
	Into  string   `json:"into"`
	Why   string   `json:"why,omitempty"`
}

// DocWithdraw retires a unit, with why.
type DocWithdraw struct {
	Unit string `json:"unit"`
	Why  string `json:"why"`
}
