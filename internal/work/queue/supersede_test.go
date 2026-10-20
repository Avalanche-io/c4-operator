package queue

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// writeChangeset stores a changeset that writes one file into the empty
// view, as a unit's capture would, and returns its ID.
func writeChangeset(t *testing.T, st *storeurl.Sharded, name, content string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := st.PutBytes(ctx, nil); err != nil { // the empty directory's record
		t.Fatal(err)
	}
	id, err := st.PutBytes(ctx, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	a, err := c4view.Apply(ctx, c4view.EmptyID(), []c4view.Edit{{Path: name, Entry: c4view.FileEntry(name, int64(len(content)), id, false)}}, c4view.StoreRecords(st))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range a.Records {
		if _, err := st.PutBytes(ctx, r.Data); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := st.PutBytes(ctx, a.Changeset)
	if err != nil {
		t.Fatal(err)
	}
	return cs.String()
}

// outputFiles lists the files of a job's output view.
func outputFiles(t *testing.T, st *storeurl.Sharded, j *c4v1.WorkJob) []string {
	t.Helper()
	if j.Status.Output == nil {
		t.Fatal("no output")
	}
	id, err := c4.Parse(j.Status.Output.View)
	if err != nil {
		t.Fatal(err)
	}
	v, err := c4view.Load(context.Background(), id, c4view.StoreRecords(st), 1)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for name := range v.Root.Files {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Best of N: a review scores three attempts and keeps the top one. The
// other two are superseded: done, not accepted, out of the job's output,
// which is refolded without their changesets; the job succeeds. A later
// verdict may score a unit again, and the last score stands.
func TestSelectSupersedes(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "bon", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a1"}, {"name": "a2"}, {"name": "a3"},
		{"name": "pick", "after": ["a1", "a2", "a3"]}]}`)
	finishUnit(t, now, ns, "bon", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	// units: 0 root, 1-3 attempts, 4 pick
	for i := 1; i <= 3; i++ {
		finishUnit(t, now, ns, "bon", i, map[string]string{c4v1.AnnotationOutputChangeset: writeChangeset(t, st, fmt.Sprintf("a%d.txt", i), fmt.Sprintf("attempt %d", i))})
	}
	tick(t, q)
	if got := get(t, ns, "bon"); got.Status.Output == nil || got.Status.Output.Changesets != 3 || len(outputFiles(t, st, got)) != 3 {
		t.Fatalf("before the pick: %+v", got.Status.Output)
	}
	v := putDoc(t, st, `{"version": 1, "verdict": "pass", "select": {"top": 1}, "items": [
		{"units": ["a1"], "do": "pass", "score": 0.4},
		{"units": ["a2"], "do": "pass", "score": 0.9},
		{"units": ["a3"], "do": "pass", "score": 0.7}]}`)
	finishUnit(t, now, ns, "bon", 4, map[string]string{c4v1.AnnotationOutputVerdict: v})
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "bon")
	if !slices.Equal(states(t, got, "superseded"), []int{1, 3}) || got.Status.Units.Superseded != 2 {
		t.Fatalf("states %v, units %+v", got.Status.UnitStates, got.Status.Units)
	}
	vd := got.Status.Verdicts[len(got.Status.Verdicts)-1]
	if vd.Passed != 1 || vd.Superseded != 2 || vd.Select != "top 1" {
		t.Fatalf("verdict %+v", vd)
	}
	if r := got.Status.Reviews["2"]; r.Score != "0.9" || r.Scored != 1 {
		t.Fatalf("a2's review %+v", r)
	}
	if r := got.Status.Reviews["1"]; r.Do != "supersede" || !strings.Contains(r.Why, "top 1") || r.Score != "0.4" {
		t.Fatalf("a1's review %+v", r)
	}
	if files := outputFiles(t, st, got); !slices.Equal(files, []string{"a2.txt"}) || got.Status.Output.Changesets != 1 {
		t.Fatalf("output %v, %d changesets", files, got.Status.Output.Changesets)
	}
	if got.Status.Phase != "Succeeded" {
		t.Fatalf("phase %s", got.Status.Phase)
	}
	// A restart keeps the superseded units, and the same output.
	view := got.Status.Output.View
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	got = get(t, ns, "bon")
	if !slices.Equal(states(t, got, "superseded"), []int{1, 3}) || got.Status.Output.View != view {
		t.Fatalf("after a restart: states %v, output %s, was %s", got.Status.UnitStates, got.Status.Output.View, view)
	}
}

// A review may stop alternatives still running: a sibling it does not run
// after is superseded, its pod stopped, with no retry spent and nothing
// failed. A unit that runs after the review cannot be superseded by it.
func TestSupersedeRunning(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "race", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "fast"}, {"name": "slow"}, {"name": "early", "after": ["fast"]},
		{"name": "ship", "after": ["early"]}]}`)
	finishUnit(t, now, ns, "race", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	// units: 0 root, 1 fast, 2 slow, 3 early, 4 ship
	finishUnit(t, now, ns, "race", 1, nil)
	tick(t, q)
	bad := putDoc(t, st, `{"version": 1, "verdict": "pass", "items": [{"units": ["ship"], "do": "supersede"}]}`)
	finishUnit(t, now, ns, "race", 3, map[string]string{c4v1.AnnotationOutputVerdict: bad})
	tick(t, q)
	if got := get(t, ns, "race"); got.Status.Verdicts[0].Error == "" || !slices.Contains(states(t, got, "failed"), 3) {
		t.Fatalf("superseding a unit after the review: %+v", got.Status.Verdicts)
	}
	// The person requeues the review; it supersedes the slow attempt.
	j := get(t, ns, "race")
	j.Annotations = map[string]string{c4v1.AnnotationRequeue: "r1:3"}
	if err := k8s.Update(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	ok := putDoc(t, st, `{"version": 1, "verdict": "pass", "items": [{"units": ["slow"], "do": "supersede", "why": "fast passed"}]}`)
	finishUnit(t, now, ns, "race", 3, map[string]string{c4v1.AnnotationOutputVerdict: ok})
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "race")
	if !slices.Equal(states(t, got, "superseded"), []int{2}) || slices.Contains(states(t, got, "failed"), 2) {
		t.Fatalf("states %v", got.Status.UnitStates)
	}
	for _, p := range unitPods(t, ns, "race") {
		if unitOf(p) != 2 {
			continue
		}
		var cur corev1.Pod
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(&p), &cur); err == nil && cur.DeletionTimestamp == nil {
			t.Fatal("the superseded unit's pod was not stopped")
		}
	}
	if !hasPod(t, ns, "race", 4) {
		t.Fatal("ship does not run")
	}
}

// Selection rules: top k, a threshold (lower is better with scoreOrder
// asc), and UCB, which favors units scored less often.
func TestSelectAmong(t *testing.T) {
	f := func(x float64) *float64 { return &x }
	k := func(n int) *int { return &n }
	acts := func() []action {
		return []action{{unit: 1, do: "pass", score: f(0.5)}, {unit: 2, do: "pass", score: f(0.8)}, {unit: 3, do: "pass", score: f(0.6)},
			{unit: 4, do: "rework"}, {unit: 5, do: "pass"}}
	}
	kept := func(as []action) []int {
		var out []int
		for _, a := range as {
			if a.do == "pass" {
				out = append(out, a.unit)
			}
		}
		return out
	}
	m := &jobMem{reviews: map[int]*c4v1.WorkReview{}}
	for _, c := range []struct {
		rule *selectRule
		asc  bool
		want []int
	}{
		{&selectRule{Top: k(2)}, false, []int{2, 3, 5}},
		{&selectRule{Threshold: f(0.55)}, false, []int{2, 3, 5}},
		{&selectRule{Threshold: f(0.55)}, true, []int{1, 5}},
		{&selectRule{Top: k(1)}, true, []int{1, 5}},
	} {
		as := acts()
		selectAmong(m, as, c.rule, c.asc)
		if got := kept(as); !slices.Equal(got, c.want) {
			t.Errorf("%s asc=%v: kept %v, want %v", c.rule, c.asc, got, c.want)
		}
	}
	// UCB: unit 2 is best but scored nine times; unit 1, scored once, is
	// explored.
	m.reviews[2] = &c4v1.WorkReview{Score: "0.8", Scored: 9}
	m.reviews[3] = &c4v1.WorkReview{Score: "0.6", Scored: 9}
	as := []action{{unit: 1, do: "pass", score: f(0.5)}, {unit: 2, do: "pass"}, {unit: 3, do: "pass"}}
	selectAmong(m, as, &selectRule{UCB: f(1)}, false)
	if got := kept(as); !slices.Equal(got, []int{1}) {
		t.Errorf("ucb kept %v, want [1]", got)
	}
	for rule, want := range map[*selectRule]string{
		{}:                             "want top",
		{Top: k(0)}:                    "top 0",
		{Threshold: f(1), Top: k(1)}:   "stands alone",
		{UCB: f(-1)}:                   "ucb",
		{Threshold: f(0.5), UCB: f(1)}: "stands alone",
	} {
		if err := rule.check(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", rule, err, want)
		}
	}
}
