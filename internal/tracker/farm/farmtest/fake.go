// Package farmtest is an in-memory farm.Farm for testing c4-sync. Jobs
// are hud/api Jobs the test moves between states; decisions are checked
// as the HUD checks them (internal/hud/server/controls.go, handleDecide).
package farmtest

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm"
)

// Farm is the fake.
type Farm struct {
	mu        sync.Mutex
	Now       func() time.Time
	jobs      map[string]*api.Job
	Submitted []farm.JobRequest
	Relays    []farm.Relay
	Deadlines []time.Time
	Resized   []api.ForecastChange
	// Withdrawn are the forecasts withdrawn: namespace/name, and why.
	Withdrawn []string
	Views     map[string]map[string][]byte
	// Previews are jobs' output stills, by namespace/name; PreviewCalls
	// counts the reads, and PreviewFails fails that many next.
	Previews     map[string][]byte
	PreviewCalls int
	PreviewFails int
	// Grants are the people who let the tracker submit for them; a
	// submission for anyone else is refused 403, as the HUD refuses it.
	Grants map[string]bool
	// Refusal, when set, is how the HUD refuses every submission for a
	// person (a revoked grant, a sync it does not know).
	Refusal *farm.APIError
	// Log is the farm's transition log; Oldest, when set, is where it
	// begins (an older cursor is gone).
	Log    []api.Transition
	Oldest int64
}

func (f *Farm) Book(_ context.Context, since int64, limit int) (api.Transitions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Oldest > 0 && since+1 < f.Oldest {
		return api.Transitions{}, &farm.ErrGone{Oldest: f.Oldest}
	}
	out := api.Transitions{Entries: []api.Transition{}, Next: since}
	for _, t := range f.Log {
		out.Head = max(out.Head, t.Seq)
		if t.Seq <= since {
			continue
		}
		if len(out.Entries) == limit {
			out.More = true
			break
		}
		out.Entries = append(out.Entries, t)
		out.Next = t.Seq
	}
	if !out.More {
		out.Next = max(since, out.Head)
	}
	return out, nil
}

// New makes an empty farm.
func New() *Farm { return &Farm{jobs: map[string]*api.Job{}, Now: time.Now} }

func k(j farm.JobRef) string { return j.Namespace + "/" + j.Name }

func (f *Farm) Submit(_ context.Context, r farm.JobRequest) (farm.JobRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := farm.JobRef{Namespace: r.Namespace, Name: r.Name}
	if j, ok := f.jobs[k(ref)]; ok {
		// A taken name is adopted only when it is this submission's own,
		// as the HUD client checks (farm.HUD.Submit).
		owner := "c4-sync"
		if r.For != "" {
			owner = r.For
		}
		if j.TrackerRef != r.TrackerRef || j.Owner != owner {
			return farm.JobRef{}, fmt.Errorf("%w: %s is %s's, serving %q", farm.ErrTaken, k(ref), j.Owner, j.TrackerRef)
		}
		return ref, nil
	}
	if r.For != "" && f.Refusal != nil {
		return farm.JobRef{}, f.Refusal
	}
	if r.For != "" && !f.Grants[r.For] {
		return farm.JobRef{}, &farm.APIError{Status: http.StatusForbidden, Reason: api.RefusedNoGrant, Msg: "no grant from " + r.For + " for tracker " + r.TrackerRef}
	}
	f.Submitted = append(f.Submitted, r)
	owner := "c4-sync"
	if r.For != "" {
		owner = r.For
	}
	j := &api.Job{Namespace: r.Namespace, Name: r.Name, Project: "proj", Class: r.Spec.Class, Phase: "Queued",
		Owner: owner, TrackerRef: r.TrackerRef, CreatedAt: f.Now()}
	if fc := r.Spec.Forecast; fc != nil {
		j.Phase = "Forecast"
		j.Forecast = &api.JobForecast{SlotHours: fc.SlotHours, SD: fc.SD, UnitHours: fc.UnitHours, Exists: fc.Exists}
	}
	j.Objective.Type = "Deadline"
	j.Objective.Mode = r.Spec.DeadlineMode
	if r.Spec.Deadline != nil {
		t := r.Spec.Deadline.Time
		j.Objective.Deadline, j.Objective.Source = &t, "spec"
	} else {
		j.Objective.Mode, j.Objective.Source = "Suggested", "suggested"
	}
	f.jobs[k(ref)] = j
	return ref, nil
}

func (f *Farm) Job(_ context.Context, j farm.JobRef) (*api.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[k(j)]
	if !ok {
		return nil, farm.ErrNotFound
	}
	c := *job
	c.Waits = slices.Clone(job.Waits)
	return &c, nil
}

func (f *Farm) SetDeadline(_ context.Context, j farm.JobRef, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[k(j)]
	if !ok {
		return farm.ErrNotFound
	}
	job.Objective.Deadline = &at
	if job.Objective.Mode == "Suggested" {
		job.Objective.Mode = "Pinned"
	}
	f.Deadlines = append(f.Deadlines, at)
	return nil
}

// Resize changes a forecast job's size, as the HUD does; a real job is
// refused 409.
func (f *Farm) Resize(_ context.Context, j farm.JobRef, c api.ForecastChange) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[k(j)]
	if !ok {
		return farm.ErrNotFound
	}
	if job.Forecast == nil {
		return &farm.APIError{Status: http.StatusConflict, Msg: j.Name + " is a real job: only a forecast is resized"}
	}
	fc := *job.Forecast
	for _, x := range []struct{ to, field *string }{{c.SlotHours, &fc.SlotHours}, {c.SD, &fc.SD}, {c.UnitHours, &fc.UnitHours}, {c.Exists, &fc.Exists}} {
		if x.to != nil {
			*x.field = *x.to
		}
	}
	job.Forecast = &fc
	f.Resized = append(f.Resized, c)
	return nil
}

// Withdraw deletes a forecast, as the HUD does; a real job is refused 409.
func (f *Farm) Withdraw(_ context.Context, j farm.JobRef, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[k(j)]
	if !ok {
		return farm.ErrNotFound
	}
	if job.Forecast == nil {
		return &farm.APIError{Status: http.StatusConflict, Msg: j.Name + " is a real job: only a forecast is withdrawn"}
	}
	delete(f.jobs, k(j))
	f.Withdrawn = append(f.Withdrawn, k(j)+": "+reason)
	return nil
}

// Decide carries out a decision on an open wait, as the HUD and the queue
// do (internal/hud/server/controls.go, internal/work/queue/wait.go): a
// wait with choices takes one of them or fail, one without takes any;
// an escalated unit takes accept, rework or fail.
func (f *Farm) Decide(_ context.Context, j farm.JobRef, r farm.Relay) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[k(j)]
	if !ok {
		return farm.ErrNotFound
	}
	var w *api.Wait
	for i := range job.Waits {
		ww := &job.Waits[i]
		if ww.Do == "" && ww.Since != nil && (ww.Name == r.Unit || strconv.Itoa(ww.Unit) == r.Unit) {
			w = ww
		}
	}
	if w == nil {
		return fmt.Errorf("%w: unit %q", farm.ErrNotOpen, r.Unit)
	}
	switch {
	case w.Escalated && r.Do != "accept" && r.Do != "rework" && r.Do != "fail":
		return fmt.Errorf("do: accept, rework or fail, for a unit a review escalated")
	case !w.Escalated && len(w.Choices) > 0 && r.Do != "fail" && !slices.Contains(w.Choices, r.Do):
		return fmt.Errorf("do: one of %v, or fail", w.Choices)
	}
	now := f.Now()
	w.Do, w.Note, w.Actor, w.At = r.Do, r.Note, r.Person+" via tracker "+r.Tracker+" ("+r.By+")", &now
	f.Relays = append(f.Relays, r)
	// As the queue does: with no open wait left, nobody is needed.
	open := false
	for _, ww := range job.Waits {
		open = open || (ww.Do == "" && ww.Since != nil)
	}
	if !open && job.NeedsPerson != nil && job.NeedsPerson.Reason == "decision" {
		job.NeedsPerson = nil
	}
	return nil
}

// StoreView records the files and returns a made-up view ID.
func (f *Farm) StoreView(_ context.Context, project string, files map[string][]byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Views == nil {
		f.Views = map[string]map[string][]byte{}
	}
	id := fmt.Sprintf("c4view%03d", len(f.Views)+1)
	f.Views[id] = files
	return id, nil
}

// Preview returns the job's still from Previews, keyed namespace/name;
// the next PreviewFails calls fail as a replica that is not the leader
// answers (503).
func (f *Farm) Preview(_ context.Context, j farm.JobRef) (string, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PreviewCalls++
	if f.PreviewFails > 0 {
		f.PreviewFails--
		return "", nil, &farm.APIError{Status: http.StatusServiceUnavailable, Msg: "not the queue's leader"}
	}
	b, ok := f.Previews[j.Namespace+"/"+j.Name]
	if !ok {
		return "", nil, farm.ErrNotFound
	}
	return j.Name + ".png", b, nil
}

func (f *Farm) JobURL(j farm.JobRef) string {
	return "https://hud.example/#/job/" + j.Namespace + "/" + j.Name
}

// Update changes a job as the queue would.
func (f *Farm) Update(ns, name string, fn func(*api.Job)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.jobs[ns+"/"+name])
}

// OpenWait opens a person's wait named name on the job.
func (f *Farm) OpenWait(ns, name, wait, server string, choices ...string) {
	f.Update(ns, name, func(j *api.Job) {
		now := f.Now()
		j.Waits = append(j.Waits, api.Wait{Unit: 100 + len(j.Waits), Name: wait, WaitsFor: server, Choices: choices,
			Since: &now, Blocking: true, NeedsYou: true})
		j.NeedsPerson = &api.NeedsPerson{Reason: "decision", Message: "a decision holds up work", Since: now}
	})
}

// Count returns how many jobs exist.
func (f *Farm) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs)
}

// Seed puts a job on the farm that no submission made: someone else's.
func (f *Farm) Seed(j api.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[j.Namespace+"/"+j.Name] = &j
}
