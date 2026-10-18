package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/work/forecast"
)

// handleDeadlineOptions is GET /jobs/{ns}/{name}/deadline-options: for
// each candidate time (?at=RFC 3339, repeated; none: the tighter choices
// the queue offers), what the deadline would take. Jobs it would make late
// are named only in the viewer's projects.
func (s *Server) handleDeadlineOptions(w http.ResponseWriter, r *http.Request) {
	_, rec, v := s.job(w, r)
	if rec == nil {
		return
	}
	if s.Queue == nil {
		fail(w, http.StatusNotImplemented, "deadline options need the work queue (the manager runs without --work)")
		return
	}
	var at []time.Time
	for _, a := range r.URL.Query()["at"] {
		t, err := time.Parse(time.RFC3339, a)
		if err != nil {
			fail(w, http.StatusBadRequest, "at %q: want an RFC 3339 time, for example 2026-10-23T18:00:00-07:00", a)
			return
		}
		at = append(at, t)
	}
	if len(at) > queue.MaxDeadlineOptions {
		fail(w, http.StatusBadRequest, "give at most %d candidate times as ?at=", queue.MaxDeadlineOptions)
		return
	}
	o, err := s.Queue.DeadlineOptions(rec.key, at, v.Sees)
	switch {
	case errors.Is(err, queue.ErrNoPlan):
		fail(w, http.StatusServiceUnavailable, "%v", err)
		return
	case errors.Is(err, queue.ErrNotPlanned):
		fail(w, http.StatusConflict, "%v", err)
		return
	case err != nil:
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, s.deadlineOptions(o))
}

// deadlineOptions is the queue's readout as the API reports it.
func (s *Server) deadlineOptions(o *queue.Options) api.DeadlineOptions {
	out := api.DeadlineOptions{Job: o.Job, Now: o.Now.In(s.Loc), Policy: string(o.Policy), Quantile: o.Quantile,
		Deadline: s.optTime(o.Deadline), Band: s.band(o.Band), Achievable: s.optTime(o.Achievable),
		Floor: o.Floor.In(s.Loc), FloorWhy: o.FloorWhy, Options: []api.DeadlineOption{}}
	for _, x := range o.Options {
		ao := api.DeadlineOption{At: x.At.In(s.Loc), Label: x.Label, Band: s.band(x.Band), Meets: x.Meets, OnTime: x.OnTime, Boost: x.Boost,
			Displaces: x.Displaces, Displaced: x.Displaced, Unnamed: x.Unnamed, Floor: s.optTime(x.Floor), FloorWhy: x.FloorWhy}
		if ao.Displaced == nil {
			ao.Displaced = []string{}
		}
		if b := x.Burst; b != nil {
			ao.Burst = &api.Burst{Slots: b.Slots, Pools: b.Pools, Cost: b.Cost, Closes: b.Closes, Automatic: b.Auto, Why: b.Why}
		}
		out.Options = append(out.Options, ao)
	}
	return out
}

func (s *Server) band(b forecast.Band) *api.Band {
	if b.IsZero() {
		return nil
	}
	return &api.Band{P50: b.P50.In(s.Loc), P80: b.P80.In(s.Loc), P90: b.P90.In(s.Loc), P95: b.P95.In(s.Loc)}
}

func (s *Server) optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.In(s.Loc)
	return &t
}
