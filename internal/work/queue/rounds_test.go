package queue

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A loop runs as rounds of siblings: each round's document adds the next
// round's generator after its units, which runs once they are done,
// unless the round's until review passed; then the loop ends there, the
// generator counted done without running, and the job finishes. The
// remaining rounds are priced while the loop goes on; a restart keeps the
// loop; its end is in the book and the project's history.
func TestRoundsRunAsSiblings(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	j := job(ns, "refine", "graph", "cpu-"+ns, map[string]string{"rounds": "5", "roundsUntil": "judge"}, now.Add(48*time.Hour))
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	round := func(k int) string {
		after := ""
		if k > 1 {
			after = `, "after": ["r` + strconv.Itoa(k-1) + `/judge"]`
		}
		return putDoc(t, st, `{"version": 1, "units": [{"name": "work"`+after+`}, {"name": "judge", "after": ["work"]}]}`)
	}
	finishUnit(t, now, ns, "refine", 0, map[string]string{c4v1.AnnotationOutputExpand: round(1)})
	tick(t, q)
	// units: 0 plan, 1 r1/work, 2 r1/judge, 3 r2/plan
	plan, _ := q.PlanOf(ns + "/refine")
	if plan == nil || len(plan.Units) != 4 || plan.Units[1].Name != "r1/work" || plan.Units[3].Name != "r2/plan" || plan.Units[3].Depth != 0 {
		t.Fatalf("after round 1's expansion: %+v", plan)
	}
	got := get(t, ns, "refine")
	if len(got.Status.Rounds) != 1 {
		t.Fatalf("rounds %+v", got.Status.Rounds)
	}
	r := got.Status.Rounds[0]
	more, _ := strconv.ParseFloat(r.ExpectMore, 64)
	if r.Unit != 0 || r.Name != "plan" || r.Round != 1 || r.Max != 5 || r.Until != "judge" || r.StoppedBy != "" ||
		!slices.Equal(r.Generators, []int32{0, 3}) || more < 1 || r.ExpectSeconds <= 0 {
		t.Fatalf("rounds %+v", r)
	}

	// Round 1's review does not pass: round 2 runs.
	finishUnit(t, now, ns, "refine", 1, nil)
	tick(t, q)
	finishUnit(t, now, ns, "refine", 2, map[string]string{c4v1.AnnotationOutputVerdict: putDoc(t, st, `{"version": 1, "verdict": "again"}`)})
	tick(t, q)
	finishUnit(t, now, ns, "refine", 3, map[string]string{c4v1.AnnotationOutputExpand: round(2)})
	tick(t, q)
	// 4 r2/work, 5 r2/judge, 6 r3/plan

	// A restart rebuilds the loop from status and the store.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores, Signer: q.Signer}
	tick(t, q)
	plan, _ = q.PlanOf(ns + "/refine")
	if plan == nil || len(plan.Units) != 7 || plan.Units[4].Name != "r2/work" || plan.Units[6].Name != "r3/plan" || plan.Units[6].Rounds.Round != 3 {
		t.Fatalf("after a restart: %+v", plan)
	}
	if r := get(t, ns, "refine").Status.Rounds[0]; r.Round != 2 || !slices.Equal(r.Generators, []int32{0, 3, 6}) {
		t.Fatalf("after a restart: %+v", r)
	}

	// Round 2's review passes: round 3's generator does not run.
	finishUnit(t, now, ns, "refine", 4, nil)
	tick(t, q)
	finishUnit(t, now, ns, "refine", 5, map[string]string{c4v1.AnnotationOutputVerdict: putDoc(t, st, `{"version": 1, "verdict": "pass", "pass": "none", "items": [{"units": ["work"], "do": "pass"}]}`)})
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "refine")
	if hasPod(t, ns, "refine", 6) || !slices.Contains(states(t, got, "succeeded"), 6) {
		t.Fatalf("round 3's generator: pod %v, states %v", hasPod(t, ns, "refine", 6), got.Status.UnitStates)
	}
	if r := got.Status.Rounds[0]; r.StoppedBy != "until" || r.Round != 2 || r.ExpectMore != "" {
		t.Fatalf("the loop's end: %+v", r)
	}
	if got.Status.Phase != "Succeeded" {
		t.Fatalf("phase %s, states %v", got.Status.Phase, got.Status.UnitStates)
	}
	mem := q.jobs[got.UID]
	if n := len(mem.decisions); n == 0 || mem.decisions[n-1].Kind != "rounds-stop" {
		t.Fatalf("the stop was not recorded: %+v", mem.decisions)
	}
	var h roundsStat
	for _, byKey := range q.rounds {
		if s := byKey["graph/graph"]; s != nil {
			h = *s
		}
	}
	if h.Cont != 1 || h.Stops != 1 {
		t.Fatalf("history %+v", h)
	}
	// Noted once.
	tick(t, q)
	for _, byKey := range q.rounds {
		if s := byKey["graph/graph"]; s != nil && (s.Cont != 1 || s.Stops != 1) {
			t.Fatalf("history after another tick %+v", *s)
		}
	}
}
