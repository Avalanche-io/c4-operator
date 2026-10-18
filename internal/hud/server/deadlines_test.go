package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/work/forecast"
)

// fakeQueue answers deadline options with a fixed readout, displacing
// one job in each project, and remembers what it was asked.
type fakeQueue struct {
	job string
	at  []time.Time
	err error
}

func (f *fakeQueue) Spent(string) float64 { return 0 }

func (f *fakeQueue) DeadlineOptions(job string, at []time.Time, sees func(string) bool) (*queue.Options, error) {
	f.job, f.at = job, at
	if f.err != nil {
		return nil, f.err
	}
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	b := forecast.Band{P50: now.Add(time.Hour), P80: now.Add(80 * time.Minute), P90: now.Add(90 * time.Minute), P95: now.Add(2 * time.Hour)}
	o := &queue.Options{Job: job, Now: now, Policy: forecast.Achievable, Quantile: 0.8, Band: b, Achievable: now.Add(105 * time.Minute),
		Floor: now.Add(40 * time.Minute), FloorWhy: "its longest remaining unit alone takes 40m"}
	for _, d := range at {
		x := queue.Option{At: d, Band: b, Meets: 50, Boost: 1.4, Displaces: 2,
			Burst: &queue.OptionBurst{Slots: map[string]int{"spot": 4}, Pools: []string{"spot"}, Cost: 12.5, Closes: true}}
		for _, id := range []string{"show-a/sh050-comp-v001", "show-b/ep104-crowd"} {
			p, _, _ := strings.Cut(id, "/")
			if sees(p) {
				x.Displaced = append(x.Displaced, id)
			}
		}
		x.Unnamed = x.Displaces - len(x.Displaced)
		o.Options = append(o.Options, x)
	}
	return o, nil
}

func TestDeadlineOptionsEndpoint(t *testing.T) {
	s := farm(t)
	path := "/api/v1/jobs/show-a/sh040-light-v007/deadline-options?as=artist&at=2026-10-23T11:00:00Z&at=2026-10-23T12:30:00-07:00"
	if code := get(t, s, path, nil); code != http.StatusNotImplemented {
		t.Fatalf("without a queue: %d", code)
	}
	q := &fakeQueue{}
	s.Queue = q
	var out api.DeadlineOptions
	if code := get(t, s, path, &out); code != http.StatusOK {
		t.Fatalf("options: %d", code)
	}
	if q.job != "show-a/sh040-light-v007" || len(q.at) != 2 || !q.at[1].Equal(time.Date(2026, 10, 23, 19, 30, 0, 0, time.UTC)) {
		t.Fatalf("asked %s %v", q.job, q.at)
	}
	if len(out.Options) != 2 || out.Achievable == nil || out.Band == nil || out.Policy != "achievable" || out.Quantile != 0.8 {
		t.Fatalf("readout %+v", out)
	}
	x := out.Options[0]
	if x.Boost != 1.4 || x.Burst == nil || x.Burst.Slots["spot"] != 4 || !x.Burst.Closes || x.Band == nil || x.Meets != 50 {
		t.Fatalf("option %+v", x)
	}
	// The artist sees show-a's displaced job named and show-b's counted.
	if x.Displaces != 2 || len(x.Displaced) != 1 || x.Displaced[0] != "show-a/sh050-comp-v001" || x.Unnamed != 1 {
		t.Fatalf("displacement for the artist %+v", x)
	}
	get(t, s, strings.Replace(path, "as=artist", "as=wrangler", 1), &out)
	if x := out.Options[0]; len(x.Displaced) != 2 || x.Unnamed != 0 {
		t.Fatalf("displacement for the wrangler %+v", x)
	}

	for p, want := range map[string]int{
		"/api/v1/jobs/show-b/ep104-crowd/deadline-options?as=artist&at=2026-10-23T11:00:00Z":                              http.StatusNotFound,
		"/api/v1/jobs/show-a/sh040-light-v007/deadline-options?as=artist&at=Friday":                                       http.StatusBadRequest,
		"/api/v1/jobs/show-a/sh040-light-v007/deadline-options?as=artist" + strings.Repeat("&at=2026-10-23T11:00:00Z", 9): http.StatusBadRequest,
	} {
		if code := get(t, s, p, nil); code != want {
			t.Errorf("%s: %d, want %d", p, code, want)
		}
	}
	q.err = queue.ErrNotPlanned
	if code := get(t, s, path, nil); code != http.StatusConflict {
		t.Fatalf("a job not in the plan: %d", code)
	}
}

// The job summary carries the whole band, the suggestion and who owns the
// deadline.
func TestSuggestionInSummary(t *testing.T) {
	s := farm(t)
	at := func(m int) *metav1.Time { return &metav1.Time{Time: time.Date(2026, 10, 23, 10, m, 0, 0, time.UTC)} }
	st := &s.m.byKey["show-a/sh040-light-v007"].job.Status
	st.Estimate.ETA80, st.Estimate.ETA95, st.Estimate.Calibrated = at(12), at(20), true
	st.Deadline, st.DeadlineSource = at(30), "suggested"
	st.Suggestion = &c4v1.WorkSuggestion{Deadline: *at(30), State: "Estimating", Policy: "sessions", Quantile: "0.8",
		MaxRevisions: 0, NextSession: at(45),
		History: []c4v1.WorkSuggestionRevision{{At: *at(0), Deadline: *at(30), UnitsMeasured: 1, Record: "c4suggest"}}}
	var j api.Job
	if code := get(t, s, "/api/v1/jobs/show-a/sh040-light-v007?as=artist", &j); code != 200 {
		t.Fatalf("job: %d", code)
	}
	e, o := j.Estimate, j.Objective
	if e.EtaP80 == nil || !e.EtaP80.Equal(at(12).Time) || e.EtaP95 == nil || !e.Calibrated {
		t.Fatalf("estimate %+v", e)
	}
	sg := o.Suggestion
	if o.Source != "suggested" || o.Mode != "Suggested" || sg == nil || sg.Quantile != 0.8 || sg.NextSession == nil ||
		len(sg.History) != 1 || sg.History[0].Record != "c4suggest" {
		t.Fatalf("objective %+v suggestion %+v", o, sg)
	}
}

// One click moves a job that will miss its session to the next one, and
// pins it; a deadline mode can be set on its own.
func TestNudgeDeadlineMode(t *testing.T) {
	s := farm(t)
	ctx := context.Background()
	rec := s.m.byKey["show-a/sh040-light-v007"]
	next := time.Date(2026, 10, 23, 14, 0, 0, 0, time.UTC)
	rec.job.Status.Deadline = &metav1.Time{Time: next.Add(-4 * time.Hour)}
	rec.job.Status.Suggestion = &c4v1.WorkSuggestion{Deadline: *rec.job.Status.Deadline, State: "Settled", Policy: "sessions",
		Quantile: "0.8", NextSession: &metav1.Time{Time: next}}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy()).Build()
	nudge := func(as, body string) (int, string) {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/nudge?as="+as, strings.NewReader(body)))
		var out struct {
			Message string `json:"message"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Message
	}
	live := func() *c4v1.WorkJob {
		var j c4v1.WorkJob
		if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "sh040-light-v007"}, &j); err != nil {
			t.Fatal(err)
		}
		return &j
	}

	if code, msg := nudge("artist", `{"nextSession": true}`); code != 200 || !strings.Contains(msg, "next session") {
		t.Fatalf("next session: %d %q", code, msg)
	}
	if j := live(); j.Spec.Deadline == nil || !j.Spec.Deadline.Time.Equal(next) || j.Spec.DeadlineMode != "Pinned" {
		t.Fatalf("after next session: %v %q", j.Spec.Deadline, j.Spec.DeadlineMode)
	}
	if code, _ := nudge("artist", `{"deadlineMode": "Suggested"}`); code != 200 || live().Spec.DeadlineMode != "Suggested" {
		t.Fatalf("suggested: %d %q", code, live().Spec.DeadlineMode)
	}
	if code, _ := nudge("artist", `{"deadlineMode": "Soon"}`); code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d", code)
	}
	// A deadline set by a person pins it.
	if code, _ := nudge("artist", `{"deadline": "2026-10-23T18:00:00Z"}`); code != 200 || live().Spec.DeadlineMode != "Pinned" {
		t.Fatalf("deadline: %d %q", code, live().Spec.DeadlineMode)
	}
	// Without a next session to offer, the control says so.
	rec.job.Status.Suggestion = nil
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy()).Build()
	if code, _ := nudge("artist", `{"nextSession": true}`); code != http.StatusConflict {
		t.Fatalf("no next session: %d", code)
	}
	if code, _ := nudge("artist", `{"deadlineMode": "Pinned"}`); code != 200 || live().Spec.Deadline == nil ||
		!live().Spec.Deadline.Time.Equal(next.Add(-4*time.Hour)) {
		t.Fatalf("pin the deadline it has: %d %v", code, live().Spec.Deadline)
	}
}
