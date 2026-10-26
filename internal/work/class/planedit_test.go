package class

import (
	"slices"
	"strings"
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// leadPlan is a lead's job's table: its root (plan), a parser worker, a
// docs worker after it reading its output, and a report after both.
func leadPlan(t *testing.T) (*c4v1.WorkJob, *Plan) {
	t.Helper()
	d, err := Lookup("graph")
	if err != nil {
		t.Fatal(err)
	}
	job := graphJob(nil)
	job.Name = "comp"
	p, err := d.Plan(job)
	if err != nil {
		t.Fatal(err)
	}
	x, err := Expand(d, job, p.Units[0], []byte(`{"version": 1, "dependents": [], "units": [
		{"name": "parser"},
		{"name": "docs", "after": ["parser"], "inputs": [{"fromUnit": "parser", "mountPath": "/parser"}]},
		{"name": "report", "after": ["parser", "docs"]}]}`), p.Names(), len(p.Units))
	if err != nil {
		t.Fatal(err)
	}
	x.Dependents = nil
	p.Apply(0, x)
	return job, p
}

func readPlan(t *testing.T, job *c4v1.WorkJob, p *Plan, doc string, started Started) *PlanEdits {
	t.Helper()
	root := p.Units[0]
	e, err := ReadPlan(job, root, []byte(doc), p, "k1", started)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// A plan's units are added held, after the table's; a split appends its
// replacements and withdraws the unit it replaces, naming them, and what
// followed it follows them; a withdrawal names none. No index moves: a
// replacement waits on what the unit it replaces waited on, and is held
// if it was. A release lets units run, and an order gives keys from 1.
func TestPlanEdits(t *testing.T) {
	job, p := leadPlan(t)
	e := readPlan(t, job, p, `{"version": 1, "units": [
		{"name": "bench", "held": true, "work": 1800, "after": ["1"]},
		{"name": "lint", "held": true}]}`, nil)
	p.ApplyPlan(0, e)
	if len(p.Units) != 6 || !p.Units[4].Held || !p.Units[5].Held || p.Units[4].Work != 1800 || !slices.Equal(p.Units[4].After, []int{1}) {
		t.Fatalf("added %+v", p.Units[4:])
	}

	// report (3) waits on docs (2); docs is split in two, and bench (4),
	// still held, is split into its own one replacement, held too.
	e = readPlan(t, job, p, `{"version": 1, "units": [
		{"name": "docs-api"}, {"name": "docs-guide", "after": ["lint"]}, {"name": "bench-2"}],
		"split": [{"unit": "docs", "into": ["docs-api", "docs-guide"], "why": "two readers"}, {"unit": "4", "into": ["bench-2"]}],
		"withdraw": [{"unit": "lint", "why": "the linter runs in CI"}],
		"order": ["bench-2", "docs-guide"]}`, nil)
	if len(e.Units) != 3 || e.Units[0].Index != 6 {
		t.Fatalf("units %+v", e.Units)
	}
	p.ApplyPlan(0, e)
	for i, w := range map[int]Withdrawal{2: {Why: "two readers", By: []int{6, 7}, Edit: "k1"},
		4: {Why: "split into bench-2", By: []int{8}, Edit: "k1"}, 5: {Why: "the linter runs in CI", Edit: "k1"}} {
		got := p.Units[i].Withdrawn
		if got == nil || got.Why != w.Why || !slices.Equal(got.By, w.By) || got.Edit != w.Edit {
			t.Fatalf("unit %d withdrawn %+v, want %+v", i, got, w)
		}
	}
	if !slices.Equal(p.Units[3].After, []int{1, 2, 6, 7}) {
		t.Fatalf("report after %v: what followed docs follows its replacements", p.Units[3].After)
	}
	if !slices.Equal(p.Units[6].After, []int{1}) || !slices.Equal(p.Units[7].After, []int{1, 5}) || p.Units[6].Held {
		t.Fatalf("docs' replacements %+v %+v: after what docs was, released as it was", p.Units[6], p.Units[7])
	}
	if b := p.Units[8]; !b.Held || !slices.Equal(b.After, []int{1}) || b.OrderKey != 1 {
		t.Fatalf("bench-2 %+v: held as bench was, after what it was, ordered first", b)
	}
	if p.Units[7].OrderKey != 2 || p.Units[1].OrderKey != 0 {
		t.Fatalf("order keys %d %d", p.Units[7].OrderKey, p.Units[1].OrderKey)
	}
	if r, ok := p.Review(); ok {
		t.Fatalf("a review at %d", r)
	}

	// Merging, an edge added and one removed, and a release.
	e = readPlan(t, job, p, `{"version": 1, "units": [{"name": "docs-all"}],
		"merge": [{"units": ["docs-api", "docs-guide"], "into": "docs-all"}],
		"edges": [{"from": "parser", "to": "bench-2", "remove": true}, {"from": "docs-all", "to": "bench-2"}],
		"release": ["bench-2"]}`, nil)
	p.ApplyPlan(0, e)
	if w := p.Units[6].Withdrawn; w == nil || w.Why != "merged into docs-all" || !slices.Equal(w.By, []int{9}) || p.Units[7].Withdrawn == nil {
		t.Fatalf("merged %+v %+v", p.Units[6].Withdrawn, p.Units[7].Withdrawn)
	}
	if !slices.Equal(p.Units[9].After, []int{1, 5}) || !slices.Equal(p.Units[3].After, []int{1, 2, 6, 7, 9}) {
		t.Fatalf("docs-all after %v, report after %v", p.Units[9].After, p.Units[3].After)
	}
	if b := p.Units[8]; b.Held || !slices.Equal(b.After, []int{9}) {
		t.Fatalf("bench-2 %+v: released, after docs-all and no longer the parser", b)
	}
	if c := p.Clone(); c.Cycle() != nil {
		t.Fatalf("cycle %v", c.Cycle())
	}
}

// A plan is refused whole, in the farm's words: a running unit (steer it
// instead), one that has ended, one another unit still reads, the job's
// review, a name not in the job, and a withdrawal without why. A
// generator's document, and an append's, may make none of a lead's edits.
func TestPlanEditsRefused(t *testing.T) {
	job, p := leadPlan(t)
	started := func(i int) string {
		switch i {
		case 1:
			return "is running"
		case 3:
			return "has succeeded"
		}
		return ""
	}
	for _, tc := range []struct{ doc, want string }{
		{`{"version": 1, "withdraw": [{"unit": "parser", "why": "done by hand"}]}`,
			"comp/1 (parser) is running; withdraw it after it ends, or steer it"},
		{`{"version": 1, "units": [{"name": "a"}, {"name": "b"}], "split": [{"unit": "1", "into": ["a", "b"]}]}`,
			"comp/1 (parser) is running; split it after it ends, or steer it"},
		{`{"version": 1, "withdraw": [{"unit": "report", "why": "not needed"}]}`,
			"comp/3 (report) has succeeded: only a unit that has not started is split, merged or withdrawn"},
		{`{"version": 1, "order": ["report"]}`,
			"comp/3 (report) has succeeded: order names only units that have not started"},
		{`{"version": 1, "edges": [{"from": "docs", "to": "parser"}]}`,
			"comp/1 (parser) is running: an edge goes to a unit that has not started"},
		{`{"version": 1, "units": [{"name": "x"}], "merge": [{"units": ["docs", "report"], "into": "x"}]}`,
			"comp/3 (report) has succeeded: only a unit that has not started is split, merged or withdrawn"},
		{`{"version": 1, "units": [{"name": "x"}], "merge": [{"units": ["docs", "x"], "into": "x"}]}`,
			"merge comp/4 (x): it is added by this plan; give it as it should be instead"},
		{`{"version": 1, "withdraw": [{"unit": "docs"}]}`, "withdraw comp/2 (docs): say why, for its record"},
		{`{"version": 1, "withdraw": [{"unit": "nope", "why": "x"}]}`, "withdraw nope is not in the job"},
		{`{"version": 1, "release": ["@review"]}`, "@review is the job's review, which comes after everything: no plan changes it"},
		{`{"version": 1, "edges": [{"from": "docs", "to": "report", "remove": true}, {"from": "parser", "to": "docs", "remove": true}]}`,
			"comp/3 (report) has succeeded: an edge goes to a unit that has not started"},
		{`{"version": 1, "edges": [{"from": "parser", "to": "docs", "remove": true}]}`,
			"comp/2 (docs) reads comp/1 (parser)'s output: that edge stays while it does"},
		{`{"version": 1, "edges": [{"from": "docs", "to": "plan", "remove": true}]}`,
			"comp/0 (plan) does not run after comp/2 (docs): there is no edge to remove"},
		{`{"version": 1}`, "the plan makes no edit"},
		{`{"version": 1, "units": [{"name": "a"}], "dependents": "all"}`, "dependents: a plan's units hold up nothing"},
	} {
		_, err := ReadPlan(job, p.Units[0], []byte(tc.doc), p, "k1", started)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v, want %q", tc.doc, err, tc.want)
		}
	}
	// docs reads parser's output, so parser (were it not running) is not
	// withdrawn unless docs is too.
	_, err := ReadPlan(job, p.Units[0], []byte(`{"version": 1, "withdraw": [{"unit": "parser", "why": "x"}]}`), p, "k1", nil)
	if err == nil || err.Error() != "comp/2 (docs) reads comp/1 (parser)'s output: withdraw, split or merge it in the same plan, or keep comp/1 (parser)" {
		t.Fatalf("a unit still read: %v", err)
	}
	if _, err := ReadPlan(job, p.Units[0], []byte(`{"version": 1, "withdraw": [{"unit": "parser", "why": "x"}, {"unit": "docs", "why": "x"}]}`), p, "k1", nil); err != nil {
		t.Fatalf("its reader withdrawn with it: %v", err)
	}

	for _, doc := range []string{
		`{"version": 1, "units": [{"name": "a", "held": true}]}`,
		`{"version": 1, "withdraw": [{"unit": "docs", "why": "x"}]}`,
		`{"version": 1, "release": ["docs"]}`,
		`{"version": 1, "edges": [{"from": "parser", "to": "report", "remove": true}]}`,
	} {
		_, err := ExpandDocument(job, p.Units[0], []byte(doc), p.Names(), len(p.Units))
		if err == nil || !strings.Contains(err.Error(), "only a lead's plan (c4_plan) does that, not a generator's document or an append") {
			t.Fatalf("a generator's %s: %v", doc, err)
		}
	}
}
