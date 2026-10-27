package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/work/forecast"
)

// The forecast of a lead's plan (DESIGN 10.6, A lead's plan; LEAD.md
// stage 3, "The forecast answers"). Every c4_plan call answers with what
// the scheduler forecasts for the lead's job with its edits applied: the
// queue's dry run of the job as its last tick left it, with the calls it
// has not applied yet and then the call's own (queue/planforecast.go),
// beside the same job without the call's, and what the call changes in
// plain words. The HUD adds the machines the job runs on, and their power
// where a machine says it. The answer does not wait for the queue to
// apply the call; when the queue cannot answer at once, it says the
// forecast follows on the next tick, and the job's detail carries the
// latest forecast (plan.forecast).

// PlanForecastFunc forecasts a lead's job with plan documents applied
// (queue.ForecastPlan): before with earlier, after with earlier and then
// edits.
type PlanForecastFunc func(ctx context.Context, job *c4v1.WorkJob, earlier, edits [][]byte) (before, after queue.PlanForecast, err error)

// planForecastTimeout bounds a forecast, store reads included.
const planForecastTimeout = 3 * time.Second

// planForecast is the forecast of lead's job j (rec in m) with the
// documents of the calls earlier, and then edits, applied. said is what
// the call's edits are, in words, for the change; a call already applied
// gives none of either, and is forecast from the job as it is. It
// returns nil, and why in a few words, when there is none to give.
func (s *Server) planForecast(ctx context.Context, m *model, rec *jobRec, j *c4v1.WorkJob, earlier []c4v1.WorkPlanRequest, edits [][]byte, said string) (*api.PlanForecast, string) {
	if s.PlanForecast == nil {
		return nil, "estimates are not available on this farm"
	}
	ctx, cancel := context.WithTimeout(ctx, planForecastTimeout)
	defer cancel()
	var docs [][]byte
	for _, r := range earlier {
		b, err := s.storedPlan(ctx, rec, r.Document)
		if err != nil {
			return nil, api.ForecastLater
		}
		docs = append(docs, b)
	}
	before, after, err := s.PlanForecast(ctx, j, docs, edits)
	switch {
	case errors.Is(err, queue.ErrBusy), errors.Is(err, queue.ErrNoPlan), errors.Is(err, context.DeadlineExceeded):
		return nil, api.ForecastLater
	case err != nil:
		return nil, "no forecast: " + err.Error()
	}
	owner := j.Annotations[c4v1.AnnotationOwner]
	out := &api.PlanForecast{AsOf: after.AsOf.UTC(), After: s.planEstimate(m, j, after, owner)}
	if len(edits) > 0 {
		b := s.planEstimate(m, j, before, owner)
		out.Before = &b
	}
	out.Change = planChange(said, out.Before, out.After, s.Loc)
	return out, ""
}

// storedPlan reads a plan document from the job's project's store.
func (s *Server) storedPlan(ctx context.Context, rec *jobRec, id string) ([]byte, error) {
	cid, err := c4.Parse(id)
	if err != nil {
		return nil, err
	}
	if s.Stores == nil {
		return nil, errors.New("no store")
	}
	st, err := s.Stores(ctx, rec.project)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, errors.New("no store")
	}
	return c4view.ReadObject(st, cid)
}

// planEstimate is one of the queue's forecasts as the API gives it, with
// the machines of its pools that run owner's work.
func (s *Server) planEstimate(m *model, j *c4v1.WorkJob, f queue.PlanForecast, owner string) api.PlanEstimate {
	e := api.PlanEstimate{Ready50: timePtr(f.Band.P50), Ready90: timePtr(f.Band.P90), Deadline: timePtr(f.Deadline),
		Feasible: f.Feasible, AtRisk: f.AtRisk, NoHistory: f.Estimate.NoHistory, Message: f.Message,
		CriticalPath: []api.PlanUnitEstimate{}, Capacity: api.PlanCapacity{Workers: f.Workers, Slots: f.Slots, Pools: f.Pools}}
	if e.NoHistory {
		// a guess: say so, in place of a slack
		e.Message = forecast.NoHistory(j.Spec.Class)
		if f.Agent {
			e.Message = forecast.NoWorkerHistory
		}
	} else if e.Deadline != nil && e.Ready90 != nil {
		e.SlackSeconds = int64(e.Deadline.Sub(*e.Ready90).Seconds())
	}
	unit := func(u queue.UnitEstimate) api.PlanUnitEstimate {
		out := api.PlanUnitEstimate{Name: u.Name, Seconds: u.Seconds, From: u.From}
		if !u.New {
			i := u.Index
			out.Index = &i
		}
		return out
	}
	for _, u := range f.Path {
		pu := unit(u)
		pu.From = ""
		e.CriticalPath = append(e.CriticalPath, pu)
	}
	for _, u := range f.Unmeasured {
		e.Unmeasured = append(e.Unmeasured, unit(u))
	}
	if m != nil {
		e.Capacity.Machines = planMachines(m, f.Pools, owner)
	}
	return e
}

// planMachines are the ready machines of pools that run owner's work: a
// host runner's only when owner is one of its owners (its node-owners,
// else its pool's one owner), with its slots and, when they are lowered,
// why.
func planMachines(m *model, pools []string, owner string) []api.PlanMachine {
	var out []api.PlanMachine
	for i := range m.nodes {
		n := &m.nodes[i]
		p := nodePool(m, n)
		if p == nil || !slices.Contains(pools, p.Name) || !nodeReady(n) {
			continue
		}
		if hostRunner(n) {
			owners := nodeOwnersOf(n)
			if _, ok := n.Annotations[c4v1.AnnotationNodeOwners]; !ok && len(p.Spec.Owners) == 1 {
				owners = p.Spec.Owners
			}
			if !slices.Contains(owners, owner) {
				continue
			}
		}
		pm := api.PlanMachine{Name: n.Name, Pool: p.Name, Slots: int(max(1, p.Spec.SlotsPerNode))}
		if v, ok := n.Labels[c4v1.LabelSlotLimit]; ok {
			if lim, err := strconv.Atoi(v); err == nil && lim >= 0 && lim < pm.Slots {
				pm.Slots, pm.Power = lim, n.Annotations[c4v1.AnnotationCapacityReason]
			}
		}
		out = append(out, pm)
	}
	return out
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func hostRunner(n *corev1.Node) bool {
	return slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.Key == hostTaint })
}

// planSaid is what a call's edits are, in words, for its change:
// "withdrawing docs", "adding parse and lexer, releasing parse". names
// are the units it adds, in order.
func planSaid(req api.PlanRequest, names []string) string {
	var adds, splits, merges, withdraws, releases, edges []string
	ordered := false
	for _, e := range req.Edits {
		switch {
		case e.Add != nil:
			if k := len(adds); k < len(names) {
				adds = append(adds, names[k])
			}
		case e.Split != nil:
			splits = append(splits, e.Split.Unit)
		case e.Merge != nil:
			merges = append(merges, list(e.Merge.Units, "units"))
		case e.Withdraw != nil:
			withdraws = append(withdraws, e.Withdraw.Unit)
		case e.Release != nil:
			releases = append(releases, e.Release...)
		case e.Order != nil:
			ordered = true
		case e.Edge != nil && e.Edge.Remove:
			edges = append(edges, "removing the edge from "+e.Edge.From+" to "+e.Edge.To)
		case e.Edge != nil:
			edges = append(edges, "an edge from "+e.Edge.From+" to "+e.Edge.To)
		}
	}
	var said []string
	for _, p := range []struct {
		verb  string
		units []string
	}{{"adding", adds}, {"splitting", splits}, {"merging", merges}, {"withdrawing", withdraws}, {"releasing", releases}} {
		if len(p.units) > 0 {
			said = append(said, p.verb+" "+list(p.units, "units"))
		}
	}
	said = append(said, edges...)
	if ordered {
		said = append(said, "the new order")
	}
	return list(said, "edits")
}

// list joins words as a sentence does ("a, b and c"); more than three
// are counted ("5 units").
func list(words []string, many string) string {
	switch n := len(words); {
	case n == 0:
		return ""
	case n == 1:
		return words[0]
	case n > 3:
		return strconv.Itoa(n) + " " + many
	default:
		return strings.Join(words[:n-1], ", ") + " and " + words[n-1]
	}
}

// planChange says what a call's edits (said) do to when the plan is
// ready, from before to after, against its deadline, local time with UTC
// beside it: "withdrawing docs brings ready to Mon 14:20 PDT (21:20
// UTC), 40 min earlier (p90 Mon 15:05 PDT (22:05 UTC)), 2 h 55 min
// before its deadline Mon 18:00 PDT (Tue 01:00 UTC)". A call already
// applied (no before) says when the plan is ready now, and no call (the
// job's detail) only when it is ready.
func planChange(said string, before *api.PlanEstimate, after api.PlanEstimate, loc *time.Location) string {
	if after.Ready50 == nil {
		return ""
	}
	at := forecast.Clock(*after.Ready50, loc)
	var b strings.Builder
	switch {
	case said == "":
		fmt.Fprintf(&b, "ready %s", at)
	case before == nil || before.Ready50 == nil:
		fmt.Fprintf(&b, "%s, applied: ready %s", said, at)
	default:
		switch d := after.Ready50.Sub(*before.Ready50); {
		case d > -time.Minute && d < time.Minute:
			fmt.Fprintf(&b, "%s leaves ready at %s", said, at)
		case d < 0:
			fmt.Fprintf(&b, "%s brings ready to %s, %s earlier", said, at, span(-d))
		default:
			fmt.Fprintf(&b, "%s moves ready to %s, %s later", said, at, span(d))
		}
	}
	if after.Ready90 != nil {
		fmt.Fprintf(&b, " (p90 %s)", forecast.Clock(*after.Ready90, loc))
	}
	switch {
	case after.NoHistory:
		b.WriteString("; " + after.Message)
	case after.Deadline != nil && after.Ready90 != nil:
		dl := forecast.Clock(*after.Deadline, loc)
		if slack := after.Deadline.Sub(*after.Ready90); slack >= 0 {
			fmt.Fprintf(&b, ", %s before its deadline %s", span(slack), dl)
		} else {
			fmt.Fprintf(&b, ", %s past its deadline %s", span(-slack), dl)
		}
	}
	return b.String()
}

// span is a duration as a change says it: "40 min", "2 h 5 min", "3
// days".
func span(d time.Duration) string {
	mins := int(d.Round(time.Minute).Minutes())
	switch {
	case mins < 1:
		return "under a minute"
	case mins < 90:
		return strconv.Itoa(mins) + " min"
	case mins < 48*60:
		if mins%60 == 0 {
			return strconv.Itoa(mins/60) + " h"
		}
		return strconv.Itoa(mins/60) + " h " + strconv.Itoa(mins%60) + " min"
	}
	return strconv.Itoa((mins+12*60)/(24*60)) + " days"
}
