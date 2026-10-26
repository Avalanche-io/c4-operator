package farmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// fakeWaitHUD serves a snapshot with two jobs and each job's wait route.
func fakeWaitHUD(t *testing.T, wait func(w http.ResponseWriter, r *http.Request, job string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/snapshot"):
			json.NewEncoder(w).Encode(api.Snapshot{Jobs: []api.Job{
				{Namespace: "show-a", Name: "lead-3f2a", Project: "show-a"},
				{Namespace: "show-a", Name: "lead-9c1d", Project: "show-a"}}})
		case strings.HasSuffix(r.URL.Path, "/wait"):
			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/wait"), "/")
			wait(w, r, parts[len(parts)-1])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := NewClient(Farm{HUD: srv.URL, As: "noor"})
	return c
}

func exitOf(t *testing.T, err error) int {
	t.Helper()
	var e *Exit
	if !errors.As(err, &e) {
		t.Fatalf("not an exit: %v", err)
	}
	return e.Code
}

// Waiting on all of a job's reviewers holds until each settles, across
// windows that pass with nothing settled, then prints each one's result,
// the unit's own words marked as such, and exits 1 when one failed.
func TestWaitForAll(t *testing.T) {
	var calls atomic.Int32
	c := fakeWaitHUD(t, func(w http.ResponseWriter, r *http.Request, job string) {
		if got := r.URL.Query()["unit"]; strings.Join(got, ",") != "review-1,review-2" || r.URL.Query().Get("any") != "" {
			t.Errorf("asked for %v any=%q", got, r.URL.Query().Get("any"))
		}
		if calls.Add(1) == 1 {
			json.NewEncoder(w).Encode(api.WaitAnswer{Units: []api.UnitResult{{Job: "show-a/" + job, Unit: 1, Name: "review-1", State: "running"}, {Job: "show-a/" + job, Unit: 2, Name: "review-2", State: "running"}}})
			return
		}
		json.NewEncoder(w).Encode(api.WaitAnswer{Done: true, Units: []api.UnitResult{
			{Job: "show-a/" + job, Unit: 1, Name: "review-1", State: "succeeded", Settled: "ended", DurationSeconds: 252,
				Summary: &api.UnitSummary{Text: "The draft holds up.\nTwo naming nits in section 3.", By: "unit"},
				Branch:  &api.UnitBranch{Branch: "c4/lead-3f2a/review-1", Base: "0123456789abcdef", Head: "fedcba9876543210", Commits: 1, Repo: "~/src/textkit"}},
			{Job: "show-a/" + job, Unit: 2, Name: "review-2", State: "failed", Settled: "ended",
				Ended: &api.UnitEnded{Phase: "Failed", Reason: "BlockGaveUp", Message: "the block ended without its summary in .c4/done"}}}})
	})
	var out bytes.Buffer
	err := Wait(context.Background(), c, Out{W: &out}, "show-a", WaitArgs{Units: []string{"lead-3f2a/review-1", "lead-3f2a/review-2"}})
	if code := exitOf(t, err); code != WaitFailed || calls.Load() != 2 {
		t.Fatalf("exit %d after %d calls\n%s", code, calls.Load(), out.String())
	}
	for _, want := range []string{"show-a/lead-3f2a/review-1  succeeded  4m12s", "summary   The draft holds up.", "            Two naming nits in section 3.",
		"(the unit's own words)", "branch    c4/lead-3f2a/review-1, 1 commit from 0123456789ab to fedcba987654 in ~/src/textkit",
		"show-a/lead-3f2a/review-2  failed", "ended     BlockGaveUp: the block ended without its summary in .c4/done"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
}

// With --any, the first unit to settle, in either job, ends the wait; the
// other job's request is dropped, not waited out.
func TestWaitForAny(t *testing.T) {
	c := fakeWaitHUD(t, func(w http.ResponseWriter, r *http.Request, job string) {
		if r.URL.Query().Get("any") != "true" {
			t.Errorf("any not asked")
		}
		if job == "lead-9c1d" {
			<-r.Context().Done() // a long window, never settled
			return
		}
		json.NewEncoder(w).Encode(api.WaitAnswer{Done: true, Units: []api.UnitResult{{Job: "show-a/lead-3f2a", Unit: 1, Name: "review-1", State: "succeeded", Settled: "ended"}}})
	})
	var out bytes.Buffer
	start := time.Now()
	err := Wait(context.Background(), c, Out{W: &out}, "show-a", WaitArgs{Units: []string{"lead-3f2a/review-1", "show-a/lead-9c1d/review-4"}, Any: true, JSON: true})
	if code := exitOf(t, err); code != WaitSucceeded || time.Since(start) > 5*time.Second {
		t.Fatalf("exit %d after %s", code, time.Since(start))
	}
	var ans api.WaitAnswer
	if err := json.Unmarshal(out.Bytes(), &ans); err != nil || !ans.Done || len(ans.Units) != 1 || ans.Units[0].Name != "review-1" {
		t.Fatalf("json %v: %s", err, out.String())
	}
}

// A unit stopped on a question ends the wait with exit 10, the question,
// and how to answer it; a HUD that did not answer is said once and waited
// through.
func TestWaitForAQuestion(t *testing.T) {
	var calls atomic.Int32
	c := fakeWaitHUD(t, func(w http.ResponseWriter, r *http.Request, job string) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(api.WaitAnswer{Done: true, Units: []api.UnitResult{{Job: "show-a/lead-3f2a", Unit: 3, Name: "converge", State: "needs", Settled: "asks",
			Question: &api.UnitQuestion{Kind: "needs", Text: "which plate version is final", Why: "v2 and v3 both exist"}}}})
	})
	var out, errs bytes.Buffer
	err := Wait(context.Background(), c, Out{W: &out}, "show-a", WaitArgs{Units: []string{"lead-3f2a/converge"}, Retry: 10 * time.Millisecond, Errs: &errs})
	if code := exitOf(t, err); code != WaitAsks {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	for _, want := range []string{"question  which plate version is final", "why       v2 and v3 both exist", "answer    with c4_supply (job show-a/lead-3f2a, unit converge)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Count(errs.String(), "did not answer") != 1 {
		t.Errorf("the HUD's silence: %q", errs.String())
	}
}

// A unit the job lacks is an error at once; a timeout prints what is
// still waiting and exits 2; a name that is not job/unit is refused.
func TestWaitRefusesAndTimesOut(t *testing.T) {
	c := fakeWaitHUD(t, func(w http.ResponseWriter, r *http.Request, job string) {
		if r.URL.Query().Get("unit") == "nope" {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "no unit nope in job show-a/lead-3f2a"})
			return
		}
		json.NewEncoder(w).Encode(api.WaitAnswer{Units: []api.UnitResult{{Job: "show-a/lead-3f2a", Unit: 1, Name: "review-1", State: "running"}}})
	})
	ctx := context.Background()
	var out bytes.Buffer
	if err := Wait(ctx, c, Out{W: &out}, "show-a", WaitArgs{Units: []string{"lead-3f2a/nope"}}); err == nil || !strings.Contains(err.Error(), "no unit nope") {
		t.Fatalf("a missing unit: %v", err)
	}
	err := Wait(ctx, c, Out{W: &out}, "show-a", WaitArgs{Units: []string{"lead-3f2a/review-1"}, Timeout: 300 * time.Millisecond})
	if code := exitOf(t, err); code != WaitTimedOut || !strings.Contains(out.String(), "still waiting:\nshow-a/lead-3f2a/review-1  running") {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if err := Wait(ctx, c, Out{W: &out}, "show-a", WaitArgs{Units: []string{"lead-3f2a"}}); err == nil || !strings.Contains(err.Error(), "names no unit") {
		t.Fatalf("no unit named: %v", err)
	}
}
