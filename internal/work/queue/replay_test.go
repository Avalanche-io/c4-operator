package queue

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// The library-port run (agent-jobs, 2026-10-19: proposals/agent-jobs/
// RUN-2026-10-19-second-round.md), replayed through the queue on one
// slot: its expansion documents and verdicts (testdata/library-port, the
// block prompts left out) and each block's attempts as they ran. The
// queue's ETA is sampled every minute.
var portAttempts = map[string][]float64{ // seconds
	"plan": {22}, "scaffold": {32}, "survey": {53},
	"port/bisect": {101, 69, 53}, "port/colorsys": {164, 95}, "port/fnmatch": {284, 90},
	"port/shlex": {237}, "port/textwrap": {732},
	"review/api": {63, 27}, "review/diff": {320, 27}, "docs": {37},
}

var portDocs = map[string][]string{ // per attempt
	"plan": {"root.json"}, "survey": {"survey.json"},
	"review/api": {"review-api-1.json", "review-api-2.json"}, "review/diff": {"review-diff-1.json", "review-diff-2.json"},
}

type etaSample struct {
	at, p50, p90 time.Time
}

// replayPort runs the port job once, its reviews marked in the survey's
// document when mark is set, and returns its ETA curve and finish.
func replayPort(t *testing.T, ns string, q *Queue, now *time.Time, put func(string) string, name string, mark bool) ([]etaSample, time.Time) {
	t.Helper()
	ctx := context.Background()
	docs := map[string]string{}
	for _, files := range portDocs {
		for _, f := range files {
			b, err := os.ReadFile(filepath.Join("testdata", "library-port", f))
			if err != nil {
				t.Fatal(err)
			}
			s := string(b)
			if f == "survey.json" && mark {
				s = strings.ReplaceAll(s, `"key": "reviewer",`, `"key": "reviewer", "params": {"review": "true"},`)
			}
			docs[f] = put(s)
		}
	}
	j := job(ns, name, "graph", "cpu-"+ns, map[string]string{"root": "plan", "estimateSeconds": "600", "maxExpandDepth": "3"}, time.Time{})
	j.Spec.Deadline = nil
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	started := map[string]time.Time{} // pod -> start
	attempt := map[string]int{}       // unit name -> attempts finished
	var curve []etaSample
	next := *now
	for end := now.Add(3 * time.Hour); now.Before(end); *now = now.Add(10 * time.Second) {
		tick(t, q)
		got := get(t, ns, name)
		if got.Status.Phase == "Succeeded" {
			return curve, *now
		}
		if !now.Before(next) {
			if e := got.Status.Estimate; e != nil && e.ETA50 != nil && e.ETA90 != nil {
				curve = append(curve, etaSample{*now, e.ETA50.Time, e.ETA90.Time})
			}
			next = next.Add(time.Minute)
		}
		plan, _ := q.PlanOf(ns + "/" + name)
		for _, p := range unitPods(t, ns, name) {
			if p.Status.Phase != "" && p.Status.Phase != corev1.PodPending && p.Status.Phase != corev1.PodRunning {
				continue
			}
			unit := plan.Units[unitOf(p)].Name
			s, ok := started[p.Name]
			if !ok {
				run(t, &p, *now)
				started[p.Name] = *now
				continue
			}
			k := attempt[unit]
			ds := portAttempts[unit]
			if k >= len(ds) {
				t.Fatalf("%s: attempt %d not in the trace", unit, k+1)
			}
			d := time.Duration(ds[k] * float64(time.Second))
			if now.Sub(s) < d {
				continue
			}
			ann := map[string]string{c4v1.AnnotationOutputTree: viewID(fmt.Sprintf("%s/%s/%d", name, unit, k))}
			if files := portDocs[unit]; k < len(files) {
				if strings.HasPrefix(unit, "review/") {
					ann[c4v1.AnnotationOutputVerdict] = docs[files[k]]
				} else {
					ann[c4v1.AnnotationOutputExpand] = docs[files[k]]
				}
			}
			finish(t, &p, corev1.PodSucceeded, s.Add(d), d, ann)
			attempt[unit]++
		}
	}
	t.Fatalf("%s did not finish: %s", name, get(t, ns, name).Status.Phase)
	return nil, time.Time{}
}

// Replayed, the first job's p90 carries the reviews' expected rework from
// the survey's expansion on, rather than sitting below the finish until
// the first rework verdict; a second job of the kind, its reviews not
// marked, starts from what the first one's reviews did. REPLAY_OUT names
// a directory for the curves (CSV).
func TestReplayLibraryPort(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 1) // the run's one slot
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	q.Stores = func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
	*now = time.Date(2026, 10, 19, 8, 28, 56, 0, time.UTC)
	put := func(s string) string { return putDoc(t, st, s) }
	first, end1 := replayPort(t, ns, q, now, put, "port1", true)
	*now = now.Add(time.Minute)
	second, end2 := replayPort(t, ns, q, now, put, "port2", false)
	if dir := os.Getenv("REPLAY_OUT"); dir != "" {
		for name, c := range map[string][]etaSample{"port1": first, "port2": second} {
			var b strings.Builder
			fmt.Fprintln(&b, "at,p50,p90")
			for _, s := range c {
				fmt.Fprintf(&b, "%s,%s,%s\n", s.at.UTC().Format(time.RFC3339), s.p50.UTC().Format(time.RFC3339), s.p90.UTC().Format(time.RFC3339))
			}
			if err := os.WriteFile(filepath.Join(dir, name+".csv"), []byte(b.String()), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("port1 ended %s, port2 %s", end1.Format(time.RFC3339), end2.Format(time.RFC3339))
	}
	// From five minutes in (the survey has sized the ports) to the finish,
	// the p90's error stays within what the replay measured with rework
	// priced: on main the p90 sat up to 20 minutes below the finish
	// before the first verdict and jumped 89 minutes above it at the
	// verdict, a mean error of 16 minutes; priced, 7 and under 20.
	check := func(name string, c []etaSample, end time.Time) {
		t.Helper()
		var sum, worstAbove, worstBelow float64
		n := 0
		for _, s := range c[min(5, len(c)):] {
			d := s.p90.Sub(end).Minutes()
			sum += math.Abs(d)
			worstAbove, worstBelow = math.Max(worstAbove, d), math.Max(worstBelow, -d)
			n++
		}
		if n == 0 || sum/float64(n) > 9 || worstAbove > 25 || worstBelow > 15 {
			t.Errorf("%s: p90 mean error %.1f min, up to %.1f above and %.1f below the finish", name, sum/float64(max(n, 1)), worstAbove, worstBelow)
		}
	}
	check("port1", first, end1)
	check("port2", second, end2)
}
