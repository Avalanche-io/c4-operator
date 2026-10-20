package class

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

func envValue(e []corev1.EnvVar, name string) string {
	for _, v := range e {
		if v.Name == name {
			return v.Value
		}
	}
	return ""
}

// A graph job's root may be a loop's first round. A round's units are
// named for it; its document adds the next round's generator after them
// (a sibling, one level deep), unless it is the last round or says next:
// false; with until set, the document must hold that review.
func TestRounds(t *testing.T) {
	job := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Name: "refine"},
		Spec: c4v1.WorkJobSpec{Class: "graph", Params: map[string]string{"rounds": "3", "roundsUntil": "judge"}}}
	p, err := graph{}.Plan(job)
	if err != nil {
		t.Fatal(err)
	}
	root := p.Units[0]
	if r := root.Rounds; r == nil || r.Max != 3 || r.Until != "judge" || r.Round != 1 || r.First != 0 || r.Base != "plan" ||
		envValue(root.Env, "C4_ROUND") != "1" || envValue(root.Env, "C4_ROUND_MAX") != "3" {
		t.Fatalf("root %+v", root)
	}
	round := `{"version": 1, "units": [{"name": "work"}, {"name": "judge", "after": ["work"]}]}`
	x, err := ExpandDocument(job, root, []byte(round), p.Names(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Units) != 3 || x.Units[0].Name != "r1/work" || x.Units[1].Name != "r1/judge" || envValue(x.Units[0].Env, "C4_UNIT_NAME") != "r1/work" {
		t.Fatalf("round 1: %+v", x.Units)
	}
	next := x.Units[2]
	if next.Name != "r2/plan" || !next.Generator || next.Depth != root.Depth || !slices.Equal(next.After, []int{1, 2}) ||
		next.Rounds.Round != 2 || next.Rounds.First != 0 || envValue(next.Env, "C4_ROUND") != "2" || envValue(next.Env, "C4_UNIT_NAME") != "r2/plan" ||
		!slices.Contains(x.Dependents, 3) {
		t.Fatalf("round 2's generator %+v, dependents %v", next, x.Dependents)
	}
	p.Apply(0, x)
	// Round 2 refers to round 1's units by their full names.
	x2, err := ExpandDocument(job, p.Units[3], []byte(`{"version": 1, "units": [{"name": "work", "after": ["r1/judge"]}, {"name": "judge", "after": ["work"]}]}`), p.Names(), 4)
	if err != nil || x2.Units[0].Name != "r2/work" || !slices.Equal(x2.Units[0].After, []int{2}) || x2.Units[2].Name != "r3/plan" {
		t.Fatalf("round 2: %v %+v", err, x2)
	}
	// The last round adds none; nor does next: false.
	last := x2.Units[2]
	if x3, err := ExpandDocument(job, last, []byte(round), p.Names(), 7); err != nil || len(x3.Units) != 2 {
		t.Fatalf("round 3 of 3: %v %+v", err, x3)
	}
	if x, err := ExpandDocument(job, root, []byte(`{"version": 1, "next": false, "units": [{"name": "work"}]}`), map[string]int{"plan": 0}, 1); err != nil || len(x.Units) != 1 {
		t.Fatalf("next false: %v %+v", err, x)
	}
	for doc, want := range map[string]string{
		`{"version": 1, "units": [{"name": "work"}]}`:                                                       "has no unit judge",
		`{"version": 1, "units": [{"name": "g", "planned": true, "rounds": {"max": 0}}], "next": false}`:    "rounds.max",
		`{"version": 1, "units": [{"name": "g", "rounds": {"max": 2}}], "next": false}`:                     "not planned",
		`{"version": 1, "units": [{"name": "g", "planned": true, "rounds": {"max": 2000}}], "next": false}`: "rounds.max",
	} {
		if _, err := ExpandDocument(job, root, []byte(doc), map[string]int{"plan": 0}, 1); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", doc, err, want)
		}
	}
	plain := Unit{Index: 0, Name: "plan", Key: "graph", Generator: true}
	if _, err := ExpandDocument(job, plain, []byte(`{"version": 1, "next": true, "units": []}`), map[string]int{"plan": 0}, 1); err == nil || !strings.Contains(err.Error(), "not a round") {
		t.Errorf("next on a plain generator: %v", err)
	}
	// A planned unit of a document may be a loop: its own first round.
	y, err := ExpandDocument(job, plain, []byte(`{"version": 1, "units": [{"name": "search", "planned": true, "rounds": {"max": 50}}]}`), map[string]int{"plan": 0}, 1)
	if err != nil || y.Units[0].Rounds == nil || y.Units[0].Rounds.First != 1 || y.Units[0].Rounds.Base != "search" || envValue(y.Units[0].Env, "C4_ROUND_MAX") != "50" {
		t.Fatalf("a loop in a document: %v %+v", err, y)
	}
}
