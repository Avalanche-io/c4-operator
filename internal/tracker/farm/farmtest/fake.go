// Package farmtest is an in-memory farm.Farm for testing c4-sync. Jobs
// are hud/api Jobs the test moves between states; decisions are checked
// as the HUD checks them (internal/hud/server/controls.go, handleDecide).
package farmtest

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm"
)

// Farm is the fake.
type Farm struct {
	mu         sync.Mutex
	Now        func() time.Time
	jobs       map[string]*api.Job
	Submitted  []farm.JobRequest
	Relays     []farm.Relay
	Deadlines  []time.Time
	Milestones []farm.Milestone
	// MilestonesBuilt makes EnsureMilestone succeed; by default it answers
	// ErrNotBuilt, as the operator does today.
	MilestonesBuilt bool
}

// New makes an empty farm.
func New() *Farm { return &Farm{jobs: map[string]*api.Job{}, Now: time.Now} }

func k(j farm.JobRef) string { return j.Namespace + "/" + j.Name }

func (f *Farm) Submit(_ context.Context, r farm.JobRequest) (farm.JobRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := farm.JobRef{Namespace: r.Namespace, Name: r.Name}
	if _, ok := f.jobs[k(ref)]; ok {
		return ref, nil
	}
	f.Submitted = append(f.Submitted, r)
	j := &api.Job{Namespace: r.Namespace, Name: r.Name, Project: "proj", Class: r.Spec.Class, Phase: "Queued",
		CreatedAt: f.Now()}
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

// Decide carries out a decision on an open wait, as the HUD would: a
// wait unit takes approve, one of its choices, or fail; an escalated unit
// accept, rework or fail.
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
	case !w.Escalated && len(w.Choices) > 0 && r.Do != "fail" && r.Do != "approve" && !slices.Contains(w.Choices, r.Do):
		return fmt.Errorf("do: one of %v, or fail", w.Choices)
	}
	now := f.Now()
	w.Do, w.Note, w.Actor, w.At = r.Do, farm.RelayNote(r, ""), "agent/c4-sync", &now
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

func (f *Farm) EnsureMilestone(_ context.Context, m farm.Milestone) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.MilestonesBuilt {
		return farm.ErrNotBuilt
	}
	f.Milestones = append(f.Milestones, m)
	return nil
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
