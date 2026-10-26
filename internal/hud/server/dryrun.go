package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// QueueDryRun plans drafts with the work queue (queue.DryRun), for
// Server.DryRun.
func QueueDryRun(q *queue.Queue) DryRunFunc {
	return func(ctx context.Context, job *c4v1.WorkJob, project string) (api.SubmitEstimate, error) {
		d, err := q.DryRun(ctx, job, project)
		switch {
		case errors.Is(err, queue.ErrNoPlan):
			return api.SubmitEstimate{}, fmt.Errorf("%w: %v", ErrNotPlanned, err)
		case err != nil:
			return api.SubmitEstimate{}, err
		}
		return estimateOf(d), nil
	}
}

// QueueDryRunAll plans drafts together with the work queue
// (queue.DryRunAll), for Server.DryRunAll.
func QueueDryRunAll(q *queue.Queue) DryRunAllFunc {
	return func(ctx context.Context, jobs []*c4v1.WorkJob, project string, views map[string]string, tables map[string][]byte) ([]api.SubmitEstimate, error) {
		ds, err := q.DryRunTables(ctx, jobs, project, views, tables)
		switch {
		case errors.Is(err, queue.ErrNoPlan):
			return nil, fmt.Errorf("%w: %v", ErrNotPlanned, err)
		case err != nil:
			return nil, err
		}
		out := make([]api.SubmitEstimate, len(ds))
		for i, d := range ds {
			out[i] = estimateOf(d)
		}
		return out, nil
	}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// estimateOf turns the queue's dry run into the API's estimate. Slack is
// the deadline less the shown finish, as the job view shows it; an
// estimate that rests on the class default (NoHistory) has none.
func estimateOf(d queue.DryRun) api.SubmitEstimate {
	w50 := d.Estimate.W50
	if w50 == 0 {
		w50 = d.Estimate.W
	}
	e := api.SubmitEstimate{
		Units: d.Units, Work50: w50, Work90: d.Estimate.W90,
		ETA50: timePtr(d.Band.P50), ETA80: timePtr(d.Band.P80), ETA90: timePtr(d.Band.P90), ETA95: timePtr(d.Band.P95),
		Deadline: timePtr(d.Deadline), DeadlineSource: d.DeadlineSource,
		Feasible: d.Feasible, AtRisk: d.AtRisk, Calibrated: d.Estimate.Calibrated, NoHistory: d.Estimate.NoHistory,
		Message: d.Message, Reused: d.Reused,
	}
	// slack against the shown p90, as a running job's (queue report.go;
	// p95 for a suggested deadline)
	if f := d.Band.P90; !d.Deadline.IsZero() && !f.IsZero() && !e.NoHistory {
		if d.DeadlineSource == "suggested" && !d.Band.P95.IsZero() {
			f = d.Band.P95
		}
		e.SlackSeconds = int64(d.Deadline.Sub(f).Seconds())
	}
	if s := d.Suggestion; s != nil {
		e.Suggestion = &api.SubmitSuggestion{Deadline: s.Deadline.Time, State: s.State, Policy: s.Policy, Quantile: s.Quantile}
		if s.NextSession != nil {
			e.Suggestion.NextSession = timePtr(s.NextSession.Time)
		}
	}
	return e
}
