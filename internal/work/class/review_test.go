package class

import (
	"slices"
	"strings"
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A job's review is a wait after every unit its class planned, and after
// every unit an expansion adds, whatever the document's dependents say;
// no document names it or waits on it.
func TestReviewIsLast(t *testing.T) {
	render, err := Lookup("render")
	if err != nil {
		t.Fatal(err)
	}
	job := &c4v1.WorkJob{Spec: c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-4", "chunk": "2"},
		Review: &c4v1.WorkReviewSpec{Server: "person:dana", Ask: "approve the shot?", Choices: []string{"approve", "rework"}, ExpectSeconds: 3600}}}
	p, err := render.Plan(job)
	if err != nil {
		t.Fatal(err)
	}
	n := len(p.Units)
	if p, err = WithReview(job, p); err != nil {
		t.Fatal(err)
	}
	r, ok := p.Review()
	u := p.Units[len(p.Units)-1]
	if !ok || r != n || u.Name != ReviewName || u.Wait == nil || u.Wait.Server != "person:dana" || u.Wait.Expect != 3600 ||
		len(u.After) != n || !slices.Contains(p.Order, r) {
		t.Fatalf("review %d %v: %+v order %v", r, ok, u, p.Order)
	}
	if again, _ := WithReview(job, p); len(again.Units) != n+1 {
		t.Fatal("a second WithReview added another review")
	}
	if c := p.Clone(); func() bool { i, ok := c.Review(); return !ok || i != r }() {
		t.Fatal("the clone lost the review")
	}

	graph, _ := Lookup("graph")
	gj := graphJob(nil)
	gj.Spec.Review = &c4v1.WorkReviewSpec{Server: "party:client"}
	q, err := graph.Plan(gj)
	if err != nil {
		t.Fatal(err)
	}
	if q, err = WithReview(gj, q); err != nil {
		t.Fatal(err)
	}
	r, _ = q.Review()
	x, err := Expand(graph, gj, q.Units[0], []byte(`{"version":1,"units":[{"name":"a"},{"name":"b"}],"dependents":["b"]}`), q.Names(), len(q.Units))
	if err != nil {
		t.Fatal(err)
	}
	q.Apply(0, x)
	for _, c := range x.Units {
		if !slices.Contains(q.Units[r].After, c.Index) {
			t.Fatalf("the review does not follow %s: after %v", c.Name, q.Units[r].After)
		}
	}

	for doc, want := range map[string]string{
		`{"version":1,"units":[{"name":"@review"}]}`:                                 "the job's review",
		`{"version":1,"units":[{"name":"x", "after": ["@review"]}]}`:                 "@review",
		`{"version":1,"units":[{"name":"x"}],"edges":[{"from":"@review","to":"x"}]}`: "@review",
	} {
		if _, err := Expand(graph, gj, q.Units[0], []byte(doc), q.Names(), len(q.Units)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", doc, err, want)
		}
	}

	for _, bad := range []*c4v1.WorkReviewSpec{{Server: "clock:2026-10-23T18:00:00Z"}, {Server: "unit:x"}, {Server: "person:dana", Choices: []string{"fail"}}} {
		job.Spec.Review = bad
		pp, _ := render.Plan(job)
		if _, err := WithReview(job, pp); err == nil {
			t.Errorf("review %+v accepted", bad)
		}
	}
}
