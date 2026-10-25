package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// Lead entries: an observed Claude Code session drawn as the queue draws
// any job. Its turns are its frames, in order (the axis counts them from
// the first listed turn), and each turn is a generator: the workers it
// started (subagents, teammates) sit in its row. The next turn does not
// wait for them; a lead goes on while its workers run.
//
// An entry is built from the observed rows the viewer may see, so it has
// exactly their visibility: the runner's owners and no one else. It is
// marked observed and never reaches the queue: no rank, no slack, no
// deadline, and nothing the scheduler reads.

// observedNS prefixes a lead entry's namespace: observed:<node>. A colon
// is not allowed in a Kubernetes namespace, so no farm job shares the key.
const observedNS = "observed:"

// leadPlanned is the most turns to come an entry lists, however far off
// its turn limit is.
const leadPlanned = 500

// leadP90 stretches the time left to its p90: a sixth longer, as the HUD
// mock forecasts a lead.
const leadP90 = 7.0 / 6

// isWorker reports whether an observed row is a worker of a session (a
// subagent or a teammate) rather than a session of its own.
func isWorker(o api.ObservedSession) bool {
	return o.Kind == "subagent" || o.Kind == "teammate"
}

// lead is one session's entry: the summary that goes in the snapshot's
// jobs, and the unit rows only its job page reads.
type lead struct {
	sum    api.Job
	axis   map[string]any
	groups []map[string]any
	units  []map[string]any
	edges  []map[string]int
	prog   map[string]any
}

// observedLeads are the lead entries of the sessions in obs (the rows the
// viewer may see), one per session, in their order.
func (s *Server) observedLeads(obs []api.ObservedSession, v *api.Viewer, now time.Time) []*lead {
	var out []*lead
	for _, o := range obs {
		if isWorker(o) {
			continue
		}
		out = append(out, s.leadOf(o, workersOf(o, obs), v.User, now))
	}
	return out
}

// workersOf are the workers of session o among rows: each worker whose
// starter, or its starter's starter, is o, on the same node. A nested
// worker is placed by the turn of the session's own worker above it.
func workersOf(o api.ObservedSession, rows []api.ObservedSession) []api.ObservedSession {
	byID := map[string]api.ObservedSession{}
	for _, r := range rows {
		if r.Node == o.Node && isWorker(r) {
			byID[r.Session] = r
		}
	}
	var out []api.ObservedSession
	for _, r := range rows {
		if r.Node != o.Node || !isWorker(r) {
			continue
		}
		top, ok := r, false
		for range 16 { // a loop in the links ends the walk
			if top.Parent == o.Session {
				ok = true
				break
			}
			p, found := byID[top.Parent]
			if !found {
				break
			}
			top = p
		}
		if !ok {
			continue
		}
		r.Turn = top.Turn
		out = append(out, r)
	}
	return out
}

// leadOf builds session o's entry for owner at now, its workers given.
func (s *Server) leadOf(o api.ObservedSession, workers []api.ObservedSession, owner string, now time.Time) *lead {
	n := len(o.Turns)
	start := o.TurnsTotal + 1
	if n > 0 {
		start = o.Turns[0].Index
	}
	// the turns before the listed ones are done: counted, not listed, so
	// a session of 289 turns does not read as 199
	earlier := max(0, start-1)
	total := max(o.TurnsTotal, earlier+n)

	l := &lead{prog: map[string]any{}}
	created := now
	switch {
	case o.StartedAt != nil:
		created = *o.StartedAt
	case n > 0:
		created = o.Turns[0].Start
	}
	l.sum = api.Job{Namespace: observedNS + o.Node, Name: o.Session, Class: "lead", Driver: "lead", Owner: owner,
		Order: "sequential", Pools: []string{}, Inputs: []api.Input{}, DependsOn: []string{}, CreatedAt: created.UTC(),
		Nudge: api.Nudge{Boost: 1}, Objective: api.Objective{Type: "deadline"},
		Observed: true, Label: o.Name, Account: o.Account, TurnsTotal: total}
	l.axis = map[string]any{"kind": "turn", "start": start, "step": 1}

	// the measured time per turn, from the turns that ended
	var took time.Duration
	measured := 0
	for _, t := range o.Turns {
		if t.State == "done" && t.End.After(t.Start) {
			took += t.End.Sub(t.Start)
			measured++
		}
	}
	mean := 0.0
	if measured > 0 {
		mean = took.Seconds() / float64(measured)
	}

	sets := map[string][]int{}
	counts := &l.sum.Units
	add := func(e map[string]any) {
		i := len(l.units)
		e["i"] = i
		st := e["state"].(string)
		sets[st] = append(sets[st], i)
		l.units = append(l.units, e)
	}
	counts.Succeeded = earlier
	running := -1
	for k, t := range o.Turns {
		st := turnState(o, t)
		e := map[string]any{"pos": k, "group": "turn", "label": "turn " + strconv.Itoa(t.Index), "state": st,
			"generator": true, "attempts": 1, "started": t.Start.UTC(), "tokens": t.Tokens, "weighted": math.Round(t.Weighted)}
		spent := t.End.Sub(t.Start).Seconds()
		if st == "running" {
			running = k
			e["node"] = o.Node
			spent = now.Sub(t.Start).Seconds()
			if mean > 0 {
				l.prog[strconv.Itoa(k)] = map[string]any{"fraction": round3(math.Min(0.99, spent/mean)), "measured": false}
			}
		} else {
			e["ended"] = t.End.UTC()
			// a turn that ended has shown what it started
			e["expanded"] = true
		}
		e["work"] = api.UnitWork{Spent: math.Round(math.Max(0, spent)), Expected: math.Round(math.Max(spent, mean))}
		if k > 0 {
			l.edges = append(l.edges, map[string]int{"from": k - 1, "to": k})
		}
		add(e)
	}

	// The turns to come exist only under a turn limit: with none, no one
	// knows how many there will be, and the HUD reads "N turns so far".
	left := 0
	if o.TurnLimit > 0 {
		left = min(max(0, o.TurnLimit-total), leadPlanned)
	}
	resets := limitResets(o, now)
	base := now
	if resets != nil {
		base = *resets
	}
	runLeft := 0.0
	if running >= 0 && mean > 0 {
		runLeft = math.Max(0, mean-now.Sub(o.Turns[running].Start).Seconds())
	}
	at := base.Add(time.Duration(runLeft * float64(time.Second)))
	for k := range left {
		pos := n + k
		e := map[string]any{"pos": pos, "group": "turn", "label": "turn " + strconv.Itoa(start+pos), "state": "planned",
			"generator": true, "attempts": 0, "work": api.UnitWork{Expected: math.Round(mean)}}
		if mean > 0 {
			at = at.Add(time.Duration(mean * float64(time.Second)))
			e["eta"] = at.UTC()
		}
		if k == 0 {
			e["generates"] = map[string]any{"what": "turns", "low": left, "high": left, "from": "its turn limit"}
		}
		if pos > 0 {
			l.edges = append(l.edges, map[string]int{"from": pos - 1, "to": pos})
		}
		add(e)
	}
	turns := len(l.units)

	// each worker in the row of the turn that started it: one started
	// before the listed turns, or by a call no turn is known for, sits at
	// the nearest listed row (a session with no turn yet has no row)
	if turns == 0 {
		workers = nil
	}
	for _, w := range workers {
		pos := min(max(w.Turn-start, 0), turns-1)
		label := w.Name
		if label == "" {
			label = w.Description
		}
		e := map[string]any{"pos": pos, "group": "work", "label": label, "name": w.Kind, "state": workerState(w),
			"parent": pos, "attempts": 1, "session": w.Session}
		if w.WaitsOn != "" {
			e["waitsOn"] = w.WaitsOn
		}
		if w.AgentType != "" {
			e["agentType"] = w.AgentType
		}
		if w.StartedAt != nil {
			e["started"] = w.StartedAt.UTC()
		}
		add(e)
	}

	l.groups = []map[string]any{
		{"key": "turn", "label": "turns", "generator": true, "spans": []string{"work"}, "units": rangeOf(0, turns)},
		{"key": "work", "label": "what each turn started", "parent": "turn", "perBlock": true, "sparse": true,
			"units": rangeOf(turns, len(l.units))},
	}

	// counts in the queue's words; the earlier turns count as succeeded
	counts.Total = earlier + len(l.units)
	counts.Succeeded += len(sets["succeeded"])
	counts.Running = len(sets["running"])
	counts.Failed = len(sets["failed"])
	counts.Needs = len(sets["needs"])
	counts.Waits = len(sets["waits"])
	counts.Held = len(sets["held"])
	counts.Planned, counts.Unevaluated = len(sets["planned"]), len(sets["planned"])
	us := &l.sum.UnitStates
	us.Succeeded, us.Running, us.Failed = order.EncodeSet(sets["succeeded"]), order.EncodeSet(sets["running"]), order.EncodeSet(sets["failed"])
	us.Needs, us.Waits = order.EncodeSet(sets["needs"]), order.EncodeSet(sets["waits"])
	us.Planned = order.EncodeSet(sets["planned"])
	us.Unevaluated = us.Planned
	l.sum.Work = api.Work{Basis: api.BasisUnits, DoneSlotSeconds: float64(counts.Succeeded), RunningSlotSeconds: float64(counts.Running),
		RemainingSlotSeconds: float64(counts.Planned)}

	l.sum.Phase = leadPhase(o)
	l.sum.Estimate = s.leadEstimate(o, mean, runLeft, left, resets, now)
	return l
}

// turnState is a listed turn's state in the queue's words. A turn that
// ended on a call with no result waits: on its person at a prompt
// (needs), the one a session's own row means, or on its lead (waits);
// a session that stopped on an error failed there.
func turnState(o api.ObservedSession, t api.ObservedTurn) string {
	switch {
	case t.State == "running":
		return "running"
	case t.State != "waiting":
		return "succeeded"
	case o.Word == "failed":
		return "failed"
	case o.WaitsOn == "lead":
		return "waits"
	}
	return "needs"
}

// workerState is a worker's state in the queue's words, from its word.
func workerState(w api.ObservedSession) string {
	switch w.Word {
	case "done":
		return "succeeded"
	case "failed":
		return "failed"
	case "working":
		return "running"
	case "waiting":
		if w.WaitsOn == "person" {
			return "needs"
		}
		return "waits"
	case "idle":
		// a teammate with nothing under way waits for its lead
		return "waits"
	case "stopped":
		// its session, or the run that started it, ended first: stopped
		// by its person, not failed
		return "held"
	}
	return "running"
}

// leadPhase is the session's phase as a job's: running while it is up,
// queued while its account's usage limit holds it, then done or failed.
func leadPhase(o api.ObservedSession) string {
	switch {
	case o.Word == "at its limit":
		return "Queued"
	case o.Word == "failed":
		return "Failed"
	case o.Running:
		return "Running"
	}
	return "Succeeded"
}

// leadEstimate is the entry's ETA: only under a turn limit, the turns left
// at the measured time per turn, after the usage limit's reset when one
// holds it. Without a limit there is none (noEta), never a share of a
// whole no one knows.
func (s *Server) leadEstimate(o api.ObservedSession, mean, runLeft float64, left int, resets *time.Time, now time.Time) api.Estimate {
	e := api.Estimate{EtaP50: now, EtaP90: now, Feasible: true}
	if o.Running {
		e.RunningSlots = 1
	}
	wait := ""
	if o.Word == "at its limit" {
		wait = "Waits for its account's usage limit to reset"
		if resets != nil {
			wait += " at " + forecast.Clock(*resets, s.Loc)
		}
	}
	switch {
	case o.TurnLimit <= 0:
		e.NoETA = "no turn limit, so none projected"
		if wait != "" {
			e.Message = wait + "."
		}
		return e
	case mean <= 0:
		e.NoETA = "no turn has ended yet, so none projected"
		if wait != "" {
			e.Message = wait + "."
		}
		return e
	}
	rem := runLeft + float64(left)*mean
	base := now
	if resets != nil {
		base = *resets
	}
	e.RemainingSeconds, e.P90RemainingSeconds = math.Round(rem), math.Round(rem*leadP90)
	e.EtaP50 = base.Add(time.Duration(rem * float64(time.Second))).UTC()
	e.EtaP90 = base.Add(time.Duration(rem * leadP90 * float64(time.Second))).UTC()
	if wait != "" {
		e.Message = fmt.Sprintf("%s, then about %s of turns.", wait, aboutDur(time.Duration(rem*float64(time.Second))))
	}
	return e
}

// limitResets is when the usage limit holding session o resets, if it
// is at its limit and the row says when, still to come.
func limitResets(o api.ObservedSession, now time.Time) *time.Time {
	if o.Word != "at its limit" || len(o.Limit) == 0 {
		return nil
	}
	var l struct {
		Resets *time.Time `json:"resets"`
	}
	if json.Unmarshal(o.Limit, &l) != nil || l.Resets == nil || !l.Resets.After(now) {
		return nil
	}
	t := l.Resets.UTC()
	return &t
}

// aboutDur says a duration in hours and minutes: "52 min", "1 h 5 min".
func aboutDur(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	switch {
	case m < 1:
		return "a minute"
	case m < 60:
		return strconv.Itoa(m) + " min"
	case m%60 == 0:
		return strconv.Itoa(m/60) + " h"
	}
	return strconv.Itoa(m/60) + " h " + strconv.Itoa(m%60) + " min"
}

// rangeOf is the unit indexes from lo up to hi as a set.
func rangeOf(lo, hi int) string {
	if hi <= lo {
		return ""
	}
	idx := make([]int, 0, hi-lo)
	for i := lo; i < hi; i++ {
		idx = append(idx, i)
	}
	return order.EncodeSet(idx)
}

// detail is the entry's GET /jobs/{ns}/{name}: its summary with the job
// view's layout and unit rows, the shape a farm job's page has.
func (l *lead) detail() map[string]any {
	out, _ := jsonMap(l.sum)
	if out == nil {
		out = map[string]any{}
	}
	out["axis"] = l.axis
	out["groups"] = l.groups
	out["unitList"] = l.units
	out["progress"] = l.prog
	out["edges"] = l.edges
	if l.edges == nil {
		out["edges"] = []any{}
	}
	if l.units == nil {
		out["unitList"] = []any{}
	}
	out["expansions"] = []any{}
	out["riskUnits"] = ""
	out["classSummary"] = map[string]any{}
	return out
}

// handleObservedJob is GET /jobs/observed:<node>/<session>: a lead entry's
// page, for those who see the session; anyone else gets 404, as for a job
// in a project they do not see.
func (s *Server) handleObservedJob(w http.ResponseWriter, r *http.Request, v *api.Viewer) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	node := strings.TrimPrefix(ns, observedNS)
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	obs := s.observedSessions(m, v, time.Now())
	for _, l := range s.observedLeads(obs, v, m.at) {
		if l.sum.Namespace == observedNS+node && l.sum.Name == name {
			writeJSON(w, http.StatusOK, l.detail())
			return
		}
	}
	fail(w, http.StatusNotFound, "no job %s/%s", ns, name)
}
