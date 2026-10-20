package queue

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// Rounds (class/rounds.go): a generator's bounded loop, run as rounds of
// siblings. The queue's part:
//
//   - A round's generator whose previous round's until review passed (its
//     verdict's summary word is pass) does not run: the loop ends there,
//     and the generator counts as done (serveRounds).
//   - status.rounds lists each loop: its round, its bound, why it stopped,
//     its generators, and what the queue expects of the rest.
//   - Pricing: a loop's continuation (the chance a round is followed by
//     another) is learned per project, class and generator key, across
//     jobs, and saved with the project's calibrations, as rework is. Before
//     any history a loop expects max/2 rounds, or the generator's
//     expectRounds param (else the job's). The rounds after the pending one
//     are tentative units, round j weighted by the continuation to the
//     power j, each costing what a round of the key costs; they run one
//     after another, so their weighted durations add to the critical path.
//     The round in progress is priced from its own expansion, like any
//     generator's.

const (
	// roundsShrink is the prior's weight, in loops.
	roundsShrink = 2.0
	// roundsMaxP bounds a loop's continuation, and roundsMinWeight the
	// least weight of a tentative round priced.
	roundsMaxP      = 0.98
	roundsMinWeight = 0.01
)

// roundsStat is one generator key's loops in a project: rounds followed
// by another (Cont), and loops that stopped on their own (Stops: a loop
// ended by its bound or a failure says nothing of when it would have).
type roundsStat struct {
	Cont  float64 `json:"cont,omitempty"`
	Stops float64 `json:"stops,omitempty"`
}

// loop is one loop of a job: its generators in round order.
type loop struct {
	first int
	gens  []int
}

// loopsOf lists a plan's loops by their first generator.
func loopsOf(plan *class.Plan) []loop {
	by := map[int][]int{}
	for i, u := range plan.Units {
		if u.Rounds != nil {
			by[u.Rounds.First] = append(by[u.Rounds.First], i)
		}
	}
	out := make([]loop, 0, len(by))
	for first, gens := range by {
		sort.Slice(gens, func(a, b int) bool { return plan.Units[gens[a]].Rounds.Round < plan.Units[gens[b]].Rounds.Round })
		out = append(out, loop{first: first, gens: gens})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].first < out[b].first })
	return out
}

// stoppedBy says why loop l ended (until, generator, max or failed), and
// how many rounds ran; "" while it goes on.
func (l loop) stoppedBy(plan *class.Plan, m *jobMem) (string, int) {
	last := l.gens[len(l.gens)-1]
	r := plan.Units[last].Rounds
	switch {
	case m.succeeded[last] && !m.expanded(last):
		return "until", r.Round - 1
	case m.failed[last]:
		return "failed", r.Round
	case !m.expanded(last) && m.attempts[last] > 0:
		return "", r.Round
	case !m.expanded(last):
		return "", r.Round - 1 // its generator has not started
	case r.Round >= r.Max:
		return "max", r.Round
	}
	return "generator", r.Round
}

// roundPrefix is r<k>/ for a unit of round k of a loop, else "".
func roundPrefix(plan *class.Plan, i int) string {
	u := plan.Units[i]
	if u.Depth == 0 || u.Parent < 0 || u.Parent >= len(plan.Units) {
		return ""
	}
	if r := plan.Units[u.Parent].Rounds; r != nil {
		return class.RoundName(r.Round, "")
	}
	return ""
}

// lastVerdict is the summary word of review unit i's last verdict carried
// out, "" before one.
func (m *jobMem) lastVerdict(i int) string {
	for k := len(m.verdicts) - 1; k >= 0; k-- {
		if x := m.verdicts[k]; int(x.Unit) == i && x.Error == "" {
			return x.Verdict
		}
	}
	return ""
}

// serveRounds ends the loops whose until review passed: the next round's
// generator, runnable now, does not run.
func (q *Queue) serveRounds(ctx context.Context, v *view, now time.Time) {
	m, plan := v.mem, v.mem.plan
	var names map[string]int
	keep := v.runnable[:0]
	for _, i := range v.runnable {
		r := plan.Units[i].Rounds
		if r == nil || r.Round < 2 || r.Until == "" {
			keep = append(keep, i)
			continue
		}
		if names == nil {
			names = plan.Names()
		}
		rv, ok := names[r.UntilUnit(r.Round-1)]
		if !ok || m.lastVerdict(rv) != "pass" {
			keep = append(keep, i)
			continue
		}
		m.succeeded[i] = true
		why := fmt.Sprintf("%s passed round %d of %s", plan.Units[rv].Name, r.Round-1, r.Base)
		q.recordDecision(ctx, v, i, "rounds-stop", why, "", now, map[string]any{"round": r.Round - 1, "max": r.Max})
		q.Log.Info("rounds stop", "job", v.key, "loop", r.Base, "round", r.Round-1, "why", why)
	}
	v.runnable = keep
}

// noteLoops adds the job's loops that ended since the last tick to the
// project's history, each once.
func (q *Queue) noteLoops(v *view) {
	m, plan := v.mem, v.mem.plan
	for _, l := range loopsOf(plan) {
		by, ran := l.stoppedBy(plan, m)
		if by == "" || m.loopsNoted[l.first] {
			continue
		}
		if m.loopsNoted == nil {
			m.loopsNoted = map[int]bool{}
		}
		m.loopsNoted[l.first] = true
		s := q.roundsEntry(v.project, v.job.Spec.Class, plan.Units[l.first].Key)
		s.Cont += float64(max(ran-1, 0))
		if by == "until" || by == "generator" {
			s.Stops++
		}
		q.calp.touch(v.project)
	}
}

// continuation is the chance a round of generator gen's loop is followed
// by another: the project's history and this job's running loops of the
// key, shrunk toward the prior.
func (q *Queue) continuation(v *view, gen class.Unit) float64 {
	m, plan := v.mem, v.mem.plan
	m0 := float64(gen.Rounds.Max) / 2
	if s, ok := gen.Params["expectRounds"]; ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			m0 = f
		}
	} else if f, err := strconv.ParseFloat(v.job.Spec.Params["expectRounds"], 64); err == nil {
		m0 = f
	}
	m0 = max(m0, 1)
	h := q.roundsOf(v.project, v.job.Spec.Class, gen.Key)
	for _, l := range loopsOf(plan) {
		if by, ran := l.stoppedBy(plan, m); by == "" && plan.Units[l.first].Key == gen.Key {
			h.Cont += float64(ran - 1)
		}
	}
	p := (h.Cont + roundsShrink*(m0-1)) / (h.Cont + h.Stops + roundsShrink*m0)
	return min(max(p, 0), roundsMaxP)
}

// roundsExp is what the queue expects of a loop's remaining rounds: how
// many more, and their duration on the critical path.
type roundsExp struct {
	more, seconds float64
}

// expectedRounds prices the loops' rounds after the pending ones:
// tentative units, and each pending generator's addition to the critical
// path.
func (q *Queue) expectedRounds(v *view, e *estimate.Estimator) ([]estimate.Unit, map[int]float64, map[int]roundsExp) {
	m, plan := v.mem, v.mem.plan
	var extra []estimate.Unit
	path, exp := map[int]float64{}, map[int]roundsExp{}
	names := plan.Names()
	for _, l := range loopsOf(plan) {
		last := l.gens[len(l.gens)-1]
		g := plan.Units[last]
		r := g.Rounds
		if by, _ := l.stoppedBy(plan, m); by != "" || m.succeeded[last] {
			continue
		}
		p := q.continuation(v, g)
		// The pending round itself runs unless its until review, still
		// to judge, passes the round before it.
		q0 := 1.0
		if rv, ok := names[r.UntilUnit(r.Round-1)]; ok && r.Round > 1 && !m.done(rv) {
			q0 = p
		}
		round := e.Predict(g.Key, g.Pos).Mean + e.Generator(g.Key).Longest
		more, w := q0, q0
		for j := 1; j <= r.Max-r.Round; j++ {
			w *= p
			if w < roundsMinWeight {
				break
			}
			extra = append(extra, estimate.Unit{Key: g.Key, Pos: g.Pos, Slots: g.Slots, Gen: g.Key, Exists: w})
			path[last] += w * round
			more += w
		}
		exp[l.first] = roundsExp{more: more, seconds: more * round}
	}
	return extra, path, exp
}

// roundsStatus lists the job's loops for status.rounds.
func roundsStatus(plan *class.Plan, m *jobMem, exp map[int]roundsExp) []c4v1.WorkRounds {
	if plan == nil {
		return nil
	}
	var out []c4v1.WorkRounds
	for _, l := range loopsOf(plan) {
		r := plan.Units[l.first].Rounds
		by, ran := l.stoppedBy(plan, m)
		w := c4v1.WorkRounds{Unit: int32(l.first), Name: r.Base, Round: int32(max(ran, 1)), Max: int32(r.Max), Until: r.Until, StoppedBy: by}
		for _, g := range l.gens {
			w.Generators = append(w.Generators, int32(g))
		}
		if x, ok := exp[l.first]; ok && by == "" {
			w.ExpectMore = strconv.FormatFloat(math.Round(x.more*10)/10, 'f', -1, 64)
			w.ExpectSeconds = int64(math.Round(x.seconds))
		}
		out = append(out, w)
	}
	return out
}

// loadLoops marks the loops that had ended before a restart as noted.
func (m *jobMem) loadLoops(st *c4v1.WorkJobStatus) {
	for _, r := range st.Rounds {
		if r.StoppedBy == "" {
			continue
		}
		if m.loopsNoted == nil {
			m.loopsNoted = map[int]bool{}
		}
		m.loopsNoted[int(r.Unit)] = true
	}
}

func roundsKey(class, key string) string { return class + "/" + key }

func (q *Queue) roundsOf(project, class, key string) roundsStat {
	if s := q.rounds[project][roundsKey(class, key)]; s != nil {
		return *s
	}
	return roundsStat{}
}

func (q *Queue) roundsEntry(project, class, key string) *roundsStat {
	if q.rounds == nil {
		q.rounds = map[string]map[string]*roundsStat{}
	}
	if q.rounds[project] == nil {
		q.rounds[project] = map[string]*roundsStat{}
	}
	k := roundsKey(class, key)
	s := q.rounds[project][k]
	if s == nil {
		s = &roundsStat{}
		q.rounds[project][k] = s
	}
	return s
}

// restoreRounds adds a project's saved history to what was learned since
// the start.
func (q *Queue) restoreRounds(project string, saved map[string]roundsStat) {
	for k, s := range saved {
		if q.rounds == nil {
			q.rounds = map[string]map[string]*roundsStat{}
		}
		if q.rounds[project] == nil {
			q.rounds[project] = map[string]*roundsStat{}
		}
		cur := q.rounds[project][k]
		if cur == nil {
			cur = &roundsStat{}
			q.rounds[project][k] = cur
		}
		cur.Cont += s.Cont
		cur.Stops += s.Stops
	}
}

// savedRounds is a project's history as saved.
func (q *Queue) savedRounds(project string) map[string]roundsStat {
	if len(q.rounds[project]) == 0 {
		return nil
	}
	out := map[string]roundsStat{}
	for k, s := range q.rounds[project] {
		out[k] = *s
	}
	return out
}
