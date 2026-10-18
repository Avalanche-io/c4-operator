package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// The ETA experiment measures, on the main farm under the objective
// policy with burst, (1) how well the finish band a person is shown
// (forecast.Display) covers the actual finish under contention, with and
// without a W calibration built from an earlier week, and (2) what
// happens to soft jobs (previews and ML runs) and everyone else when
// their deadlines come from the farm's suggestion instead of a guess.

// softKind marks the jobs whose deadline a person would otherwise guess.
func softKind(k string) bool { return k == "preview" || k == "ml" }

// etaArm is one arm of the experiment.
type etaArm struct {
	name string
	// soft says how soft jobs get their deadline: "workload" (the
	// workload's: previews due 3h after submission, ML 36h), "asap" (one
	// hour after submission) or "suggest" (the policy below).
	soft   string
	policy forecast.Policy
	// tighten models a person who, right after the suggestion, moves
	// it once to the next session the band supports (see beforeDecide).
	tighten bool
	// bands reports the arm in section 1 (band coverage, eta.csv);
	// deadlines reports it in the deadline comparisons.
	bands, deadlines bool
	// cal, by job kind, calibrates W; nil leaves it as built. collect,
	// when set, receives this run's calibration scores instead.
	cal     map[string]*estimate.Calibration
	collect map[string]*estimate.Calibration
	// fcal, when set, calibrates the shown band's finish
	// (forecast.FinishCalibration); fcollect receives finish scores.
	fcal, fcollect *forecast.FinishCalibration
}

// etaJob is a job's experiment state.
type etaJob struct {
	pending  bool
	sugg     forecast.Suggestion
	lastInfo int
	lastRec  float64
	lastDone int
	recs     []bandRec
	pts      []calPt
}

type bandRec struct {
	t, frac float64
	stage   int8
	rawP50  float64    // the uncalibrated median, which finish scores are taken against
	band    [4]float64 // P50, P80, P90, P95, seconds from the week's start
	s50     float64    // the scheduler's own ETA50 and ETA90
	s90     float64
}

type calPt struct {
	at   float64
	done int
	sum  estimate.Summary
}

// etaRow is one recorded band of a finished job.
type etaRow struct {
	Kind          string
	Stage         string
	Band          [4]float64
	S50, S90      float64
	Finish, Taken float64
}

// etaCount is one soft kind's suggestion tally in a run.
type etaCount struct {
	Suggested int
	AtRisk    int   // forecast.Suggestion's at-risk moves
	Tightened int   // moved to the next session (tighten arms)
	Runs      int64 // policy runs spent on suggestions and checks
}

type etaRunResult struct {
	Rows     []etaRow
	ByKind   map[string]*etaCount
	PolicyNs int64
}

const etaRecordEvery = hour

func (a *etaArm) arrive(e *engine, j *job) {
	if c := a.cal[j.spec.kind]; c != nil {
		j.est = e.hist.estimatorWith(j.spec, c)
	}
	j.eta = &etaJob{lastRec: -1, lastDone: -1}
	if !softKind(j.spec.kind) {
		return
	}
	switch a.soft {
	case "asap":
		j.deadline = j.spec.submit + hour
	case "suggest":
		j.eta.pending = true
	}
}

func (e *engine) etaResult() *etaRunResult {
	if e.res.eta == nil {
		e.res.eta = &etaRunResult{ByKind: map[string]*etaCount{}}
	}
	return e.res.eta
}

func (e *engine) etaCount(kind string) *etaCount {
	r := e.etaResult()
	if r.ByKind[kind] == nil {
		r.ByKind[kind] = &etaCount{}
	}
	return r.ByKind[kind]
}

// beforeDecide sets suggested deadlines and moves them, so the decision
// plans on them. As the queue does (internal/work/queue/suggest.go), the
// suggestion is searched at submission, and again only at an
// information point where the band shown under the current deadline is
// at risk at the policy quantile; forecast.Suggestion decides whether to
// move (later only, by more than the minimum move, at most MaxRevisions
// times). A tighten arm then models a person who, right after the first
// suggestion, moves the deadline once to the earliest dailies session
// (08:00 on a weekday) at or after the band's P80, if that is earlier.
func (a *etaArm) beforeDecide(e *engine, s *sched.Snapshot) {
	if a.soft != "suggest" {
		return
	}
	now := e.at(e.now)
	byID := map[string]*job{}
	for _, j := range e.active {
		byID[j.spec.id] = j
	}
	for i := range s.Jobs {
		j := byID[s.Jobs[i].ID]
		if j == nil || j.eta == nil || !softKind(j.spec.kind) {
			continue
		}
		ej := j.eta
		cnt := e.etaCount(j.spec.kind)
		info := forecast.InfoPoint(j.done) && j.done > ej.lastInfo
		ev := forecast.Event{Now: now, Finished: j.done, Total: len(j.units), Candidate: ej.sugg.Deadline}
		switch {
		case ej.pending:
			c, b := a.candidate(e, *s, i)
			if c.IsZero() {
				continue
			}
			ev.Candidate, ev.Band = c, b
			ej.pending = false
			cnt.Suggested++
		case info && ej.sugg.State != forecast.Settled:
			b := a.shown(e, *s, i)
			ev.Band, ev.Info = b, true
			if ej.sugg.AtRisk(a.policy.Quantile, b) {
				if c, _ := a.candidate(e, *s, i); !c.IsZero() {
					ev.Candidate = c
				}
			}
		}
		if info {
			ej.lastInfo = j.done
		}
		first := ej.sugg.Deadline.IsZero()
		before := ej.sugg.Revisions
		ej.sugg.Observe(a.policy, ev)
		cnt.AtRisk += ej.sugg.Revisions - before
		if ej.sugg.Deadline.IsZero() {
			continue
		}
		setDeadline(j, s, i, ej.sugg.Deadline)
		if !first || !a.tighten {
			continue
		}
		b := a.shown(e, *s, i)
		if b.IsZero() {
			continue
		}
		if t := nextSession(maxT(bandAt(now, b, 0.8), now.Add(a.policy.MinLead))); t.Before(ej.sugg.Deadline) {
			// The person's act: the system's suggestion is replaced.
			ej.sugg.Deadline = t
			cnt.Tightened++
			setDeadline(j, s, i, t)
		}
	}
}

func setDeadline(j *job, s *sched.Snapshot, i int, d time.Time) {
	j.deadline = d.Sub(weekStart).Seconds()
	s.Jobs[i].Deadline = d
}

func maxT(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// nextSession is the first dailies session, 08:00 on a weekday, at or
// after t.
func nextSession(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 8, 0, 0, 0, t.Location())
	for d.Before(t) || d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// bandAt is the band's quantile q, seen at now. forecast.Band stops at
// P95; past it, the tail is extrapolated from P90 and P95 in log
// time-left as a normal tail would continue, an assumption the band
// does not measure.
func bandAt(now time.Time, b forecast.Band, q float64) time.Time {
	if q <= 0.95 {
		return b.At(q)
	}
	z := func(p float64) float64 { return math.Sqrt2 * math.Erfinv(2*p-1) }
	k := (z(q) - z(0.95)) / (z(0.95) - z(0.9))
	l90, l95 := b.P90.Sub(now).Seconds(), b.P95.Sub(now).Seconds()
	if l90 <= 0 || l95 <= l90 {
		return b.P95.Add(time.Duration(k * (l95 - l90) * float64(time.Second)))
	}
	l := math.Log(l95) + (math.Log(l95)-math.Log(l90))*k
	return now.Add(time.Duration(math.Exp(l) * float64(time.Second)))
}

// policyRun runs the policy on s with job i's deadline set to d.
func (a *etaArm) policyRun(e *engine, s sched.Snapshot, i int, d time.Time) sched.Decisions {
	s2 := s
	s2.Jobs = append([]sched.Job(nil), s.Jobs...)
	s2.Jobs[i].Deadline = d
	var st sched.State
	t0 := time.Now()
	out := e.pol.fn(s2, &st, e.pcfg)
	e.etaResult().PolicyNs += int64(time.Since(t0))
	e.etaCount(s.Jobs[i].Class).Runs++
	return out
}

// show is job i's band as a person sees it under decisions d.
func (a *etaArm) show(s sched.Snapshot, i int, d sched.Decisions) forecast.Band {
	sj := s.Jobs[i]
	key := forecast.FinishKey{Class: sj.Class, Stage: forecast.StageOf(sj.UnitsDone, sj.UnitsDone+sj.RunnableTotal+sj.Blocked+len(sj.RunningUnits))}
	js := d.Jobs[sj.ID]
	return a.fcal.Apply(key, s.Now, forecast.Display(s.Now, sj.Estimate, js.ETA50, js.ETA90, forecast.Tail(sj.SlotsPerUnit, js.GroupSlots)))
}

// shown is job i's band under its current deadline: one policy run.
func (a *etaArm) shown(e *engine, s sched.Snapshot, i int) forecast.Band {
	return a.show(s, i, a.policyRun(e, s, i, s.Jobs[i].Deadline))
}

// candidate is the policy's achievable deadline for job i of s, found
// as the queue does (internal/work/queue/suggest.go): the earliest
// deadline by which the job finishes at the policy's quantile without
// making any job that is on time now late.
func (a *etaArm) candidate(e *engine, s sched.Snapshot, i int) (time.Time, forecast.Band) {
	now := s.Now
	id := s.Jobs[i].ID
	hi := now.Add(7 * 24 * time.Hour)
	base := a.policyRun(e, s, i, hi)
	band := a.show(s, i, base)
	if band.IsZero() {
		return time.Time{}, band
	}
	var onTime []string
	for k, js := range base.Jobs {
		if k != id && js.Feasible && !js.AtRisk {
			onTime = append(onTime, k)
		}
	}
	sort.Strings(onTime)
	ok := func(d time.Time) bool {
		r := a.policyRun(e, s, i, d)
		b := a.show(s, i, r)
		if b.IsZero() || bandAt(now, b, a.policy.Quantile).After(d) {
			return false
		}
		for _, k := range onTime {
			if r.Jobs[k].AtRisk {
				return false
			}
		}
		return true
	}
	d := forecast.FirstAchievable(now.Add(a.policy.MinLead), hi, 5*time.Minute, ok)
	if d.Equal(hi) {
		d = bandAt(now, band, a.policy.Quantile)
	}
	return a.policy.Earliest(d, now, now, now), band
}

// afterDecide records the band each job is shown, and calibration
// points.
func (a *etaArm) afterDecide(e *engine, s sched.Snapshot, d sched.Decisions) {
	now := s.Now
	byID := map[string]*job{}
	for _, j := range e.active {
		byID[j.spec.id] = j
	}
	for _, sj := range s.Jobs {
		j := byID[sj.ID]
		if j == nil || j.eta == nil {
			continue
		}
		ej := j.eta
		total := len(j.units)
		if a.collect != nil && j.done > ej.lastDone && j.done < total && len(ej.pts) < 80 && keepPoint(j.done, total) {
			ej.pts = append(ej.pts, calPt{at: e.now, done: j.done, sum: sj.Estimate})
		}
		info := forecast.InfoPoint(j.done) && j.done > ej.lastDone
		due := ej.lastRec < 0 || info || e.now-ej.lastRec >= etaRecordEvery
		ej.lastDone = max(ej.lastDone, j.done)
		if !due || len(ej.recs) >= 64 {
			continue
		}
		js := d.Jobs[sj.ID]
		b := forecast.Display(now, sj.Estimate, js.ETA50, js.ETA90, forecast.Tail(sj.SlotsPerUnit, js.GroupSlots))
		if b.IsZero() {
			continue
		}
		stage := forecast.StageOf(j.done, total)
		raw := b.P50
		b = a.fcal.Apply(forecast.FinishKey{Class: j.spec.kind, Stage: stage}, now, b)
		sec := func(t time.Time) float64 { return t.Sub(weekStart).Seconds() }
		ej.recs = append(ej.recs, bandRec{t: e.now, frac: float64(j.done) / float64(total), stage: stage, rawP50: sec(raw),
			band: [4]float64{sec(b.P50), sec(b.P80), sec(b.P90), sec(b.P95)}, s50: sec(js.ETA50), s90: sec(js.ETA90)})
		ej.lastRec = e.now
	}
}

// keepPoint mirrors the queue's rule (internal/work/queue/calibrate.go).
func keepPoint(finished, total int) bool {
	if finished <= 33 {
		return true
	}
	return finished%max(1, total/32) == 0
}

func stageOf(r bandRec) string {
	switch {
	case r.frac == 0:
		return "nothing finished"
	case r.frac < 0.25:
		return "under 1/4"
	case r.frac < 0.75:
		return "1/4 to 3/4"
	}
	return "3/4 on"
}

// done turns a finished job's records into rows and calibration scores.
func (a *etaArm) done(e *engine, j *job) {
	ej := j.eta
	if ej == nil {
		return
	}
	r := e.etaResult()
	if a.fcollect != nil {
		for _, rec := range ej.recs {
			a.fcollect.Add(forecast.FinishKey{Class: j.spec.kind, Stage: rec.stage},
				forecast.FinishScore(e.at(rec.t), e.at(rec.rawP50), e.at(j.finish)))
		}
	}
	for _, rec := range ej.recs {
		r.Rows = append(r.Rows, etaRow{Kind: j.spec.kind, Stage: stageOf(rec), Band: rec.band, S50: rec.s50, S90: rec.s90,
			Finish: j.finish, Taken: rec.t})
	}
	if a.collect == nil {
		return
	}
	c := a.collect[j.spec.kind]
	if c == nil {
		c = estimate.NewCalibration(0)
		a.collect[j.spec.kind] = c
	}
	for _, p := range ej.pts {
		actual := 0.0
		for _, u := range j.units {
			if u.finEnd <= p.at {
				continue
			}
			actual += (u.finEnd - math.Max(p.at, u.finStart)) * float64(u.spec.slots)
		}
		c.Add(p.sum.Stratum, estimate.Score(p.sum, actual))
	}
}

// etaCalSeed draws the calibration week; no experiment seed uses it.
const etaCalSeed = 0x0E7A

// etaVariant is a workload the ETA experiment runs on: the parameters,
// plus previews submitted in the evening, which the workload generator
// does not make (it submits previews 09:00 to 17:00).
type etaVariant struct {
	name string
	p    params
	// evening is previews per hour per show, Mon to Fri 17:00 to 22:00,
	// on top of the daytime ones, each due PreviewTurn after submission.
	evening map[string]float64
}

// eveningSeed separates the evening previews' draws from the workload's.
const eveningSeed = 0xE7E

// workload is the variant's week for seed: the generator's week, plus
// the evening previews drawn from their own stream.
func (v etaVariant) workload(seed uint64) *workload {
	w := generate(v.p, seed)
	if len(v.evening) == 0 {
		return w
	}
	r := rand.New(rand.NewPCG(seed, eveningSeed))
	n := 0
	for d := 0; d < 5; d++ {
		for _, show := range showNames {
			for h := 17; h < 22; h++ {
				for range poisson(r, v.evening[show]) {
					at := float64(d)*day + (float64(h)+r.Float64())*hour
					js := renderJob(r, v.p, "preview", show, r.Float64() < v.p.GPUShare, at, at+v.p.PreviewTurn*hour)
					n++
					js.id = fmt.Sprintf("%s-preview-e%04d", show, n)
					w.jobs = append(w.jobs, js)
				}
			}
		}
	}
	sort.SliceStable(w.jobs, func(i, j int) bool { return w.jobs[i].submit < w.jobs[j].submit })
	return w
}

// contentionVariant makes previews compete with deadline work: each is
// about three times the work, they come half again as often in the day,
// and more arrive in the evening, while dailies are submitted.
func contentionVariant(p params) etaVariant {
	p.PreviewCost = 1.0
	p.PreviewPerHour = map[string]float64{"showA": 3, "showB": 2.25, "showC": 1.5}
	return etaVariant{name: "contention", p: p, evening: map[string]float64{"showA": 1.5, "showB": 1, "showC": 0.75}}
}

// changes lists the variant's parameters that differ from base, and its
// evening previews.
func (v etaVariant) changes(base params) []string {
	var out []string
	bv, vv := reflect.ValueOf(base), reflect.ValueOf(v.p)
	for i := 0; i < bv.NumField(); i++ {
		if !reflect.DeepEqual(bv.Field(i).Interface(), vv.Field(i).Interface()) {
			out = append(out, fmt.Sprintf("%s: %v (main workload %v)", bv.Type().Field(i).Name, vv.Field(i).Interface(), bv.Field(i).Interface()))
		}
	}
	if len(v.evening) > 0 {
		out = append(out, fmt.Sprintf("evening previews per hour, Mon to Fri 17:00 to 22:00: %v (main workload none)", v.evening))
	}
	return out
}

type etaResults struct {
	variant etaVariant
	arms    []*etaArm
	res     map[string][]*runResult
	cal     map[string]*estimate.Calibration
	took    time.Duration
}

// etaArmSet builds a variant's arms given its calibrations.
type etaArmSet func(cal map[string]*estimate.Calibration, fcal *forecast.FinishCalibration) []*etaArm

// mainETAArms are the main workload's arms: the three band arms of
// section 1, then the deadline arms (deadlineArms).
func mainETAArms(cal map[string]*estimate.Calibration, fcal *forecast.FinishCalibration) []*etaArm {
	arms := []*etaArm{
		{name: "workload deadlines, W as built", soft: "workload", bands: true},
		{name: "workload deadlines, W calibrated", soft: "workload", cal: cal, bands: true},
	}
	d := deadlineArms(cal, fcal)
	d[0].bands = true
	return append(arms, d...)
}

// deadlineArms compare how soft jobs get their deadlines: the workload's
// and a guess as references, the suggestion at 0.8 set once (the earlier
// default), and generous suggestions at 0.95 and 0.98: set once, moved
// later when at risk, or tightened by a person to the next session.
// Every quantile is set here, not taken from forecast.DefaultPolicy.
func deadlineArms(cal map[string]*estimate.Calibration, fcal *forecast.FinishCalibration) []*etaArm {
	pol := func(q float64, revisions int) forecast.Policy {
		p := forecast.DefaultPolicy()
		p.Quantile, p.MaxRevisions = q, revisions
		return p
	}
	arms := []*etaArm{
		{name: "workload deadlines, W and finish calibrated", soft: "workload"},
		{name: "guess: previews and ML due an hour after submission", soft: "asap"},
		{name: "suggested at 0.8, set once", soft: "suggest", policy: pol(0.8, 0)},
		{name: "generous at 0.95, set once", soft: "suggest", policy: pol(0.95, 0)},
		{name: "generous at 0.98, set once", soft: "suggest", policy: pol(0.98, 0)},
		{name: "generous at 0.95, moved later only when at risk", soft: "suggest", policy: pol(0.95, 2)},
		{name: "generous at 0.95, user tightens to the next session", soft: "suggest", policy: pol(0.95, 0), tighten: true},
	}
	for _, a := range arms {
		a.cal, a.fcal, a.deadlines = cal, fcal, true
	}
	return arms
}

func runETAExperiment(v etaVariant, seeds []uint64, armSet etaArmSet) *etaResults {
	t0 := time.Now()
	pol := policyByName("objective+burst")
	// The calibration week: a farm's earlier week of finished jobs.
	cal := map[string]*estimate.Calibration{}
	fcal := forecast.NewFinishCalibration(0)
	w := v.workload(etaCalSeed)
	h := buildHistory(v.p, w.targets)
	// Twice over the same earlier week: first for the W scores, then,
	// with W calibrated, for the finish scores of the band it shows.
	e := newEngine(w, h, etaCalSeed, pol, mainFarm)
	e.eta = &etaArm{name: "calibration week", soft: "workload", collect: cal}
	e.run()
	e = newEngine(w, h, etaCalSeed, pol, mainFarm)
	e.eta = &etaArm{name: "calibration week, finish", soft: "workload", cal: cal, fcollect: fcal}
	e.run()

	arms := armSet(cal, fcal)
	type task struct {
		arm  *etaArm
		seed uint64
	}
	loads, hist := map[uint64]*workload{}, map[uint64]*history{}
	for _, s := range seeds {
		loads[s] = v.workload(s)
		hist[s] = buildHistory(v.p, loads[s].targets)
	}
	res := map[string][]*runResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	tasks := make(chan task)
	for range max(1, min(len(seeds)*len(arms), runtime.GOMAXPROCS(0))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range tasks {
				t1 := time.Now()
				en := newEngine(loads[t.seed], hist[t.seed], t.seed, pol, mainFarm)
				arm := *t.arm
				en.eta = &arm
				r := en.run()
				fmt.Fprintf(os.Stderr, "eta %s %-58s seed %d: %s\n", v.name, t.arm.name, t.seed, time.Since(t1).Round(time.Millisecond))
				mu.Lock()
				res[t.arm.name] = append(res[t.arm.name], r)
				mu.Unlock()
			}
		}()
	}
	for _, a := range arms {
		for _, s := range seeds {
			tasks <- task{a, s}
		}
	}
	close(tasks)
	wg.Wait()
	for _, rs := range res {
		sortBySeed(rs)
	}
	return &etaResults{variant: v, arms: arms, res: res, cal: cal, took: time.Since(t0)}
}

// etaCoverage tallies rows: the share finished by each band percentile and
// by the scheduler's own ETA50 and ETA90.
type etaCoverage struct {
	n    int
	band [4]int
	s50  int
	s90  int
	err  []float64 // |P50 - actual| over the time remaining then
}

func (c *etaCoverage) add(r etaRow) {
	c.n++
	for i, t := range r.Band {
		if r.Finish <= t+1 {
			c.band[i]++
		}
	}
	if r.Finish <= r.S50+1 {
		c.s50++
	}
	if r.Finish <= r.S90+1 {
		c.s90++
	}
	if left := r.Finish - r.Taken; left > 0 {
		c.err = append(c.err, math.Abs(r.Band[0]-r.Finish)/left)
	}
}

func (c etaCoverage) cells() []string {
	if c.n == 0 {
		return []string{"0", "", "", "", "", "", "", ""}
	}
	f := func(k int) string { return pct(float64(k) / float64(c.n)) }
	sort.Float64s(c.err)
	med := ""
	if len(c.err) > 0 {
		med = pct(c.err[len(c.err)/2])
	}
	return []string{fmt.Sprint(c.n), f(c.band[0]), f(c.band[1]), f(c.band[2]), f(c.band[3]), med, f(c.s50), f(c.s90)}
}

var etaKinds = []string{"dailies", "preview", "delivery", "ml", "ci"}
var etaStages = []string{"nothing finished", "under 1/4", "1/4 to 3/4", "3/4 on"}

func writeETA(dir string, x, cont *etaResults, seeds []uint64) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# ETA bands and suggested deadlines\n\n")
	fmt.Fprintf(&b, "Generated by `go run ./hack/sim` (the ETA experiment, %s). Main farm, objective policy with burst, seeds %v, the same weeks as results.md. Synthetic workload.\n\n", (x.took + cont.took).Round(time.Second), seeds)
	fmt.Fprintf(&b, "The calibration arm's W percentiles come from `estimate.Calibration` built from a separate week (seed %#x) run the same way, per job kind, as the queue builds them live from finished jobs.\n\n", etaCalSeed)

	fmt.Fprintf(&b, "## 1. Does the shown band cover the actual finish?\n\n")
	fmt.Fprintf(&b, "A job's band is recorded when it is first planned, at every binary-order information point (1, 3, 5, 9, 17, 33 units finished) and every hour, and compared with when the job actually finished. P50 to P95 are `forecast.Display`, what `status.estimate.eta50`..`eta95` show; \"sched ETA50/90\" are the scheduler's own finishes, which decide but are not shown. Error is the median of |P50 - finish| over the time that was left.\n\n")
	head := []string{"kind", "records", "P50", "P80", "P90", "P95", "error", "sched ETA50", "sched ETA90"}
	var last *etaArm
	for _, arm := range x.arms {
		if !arm.bands {
			continue
		}
		last = arm
		fmt.Fprintf(&b, "### %s\n\n", arm.name)
		var rows [][]string
		var all etaCoverage
		for _, k := range etaKinds {
			var c etaCoverage
			for _, r := range x.res[arm.name] {
				for _, row := range r.eta.rowsOf(k) {
					c.add(row)
					all.add(row)
				}
			}
			rows = append(rows, append([]string{k}, c.cells()...))
		}
		rows = append(rows, append([]string{"**all**"}, all.cells()...))
		table(&b, head, rows)
		b.WriteString("\n")
	}
	if last != nil {
		fmt.Fprintf(&b, "### By how much of the job had finished (%s)\n\n", strings.TrimPrefix(last.name, "workload deadlines, "))
		var rows [][]string
		for _, st := range etaStages {
			var c etaCoverage
			for _, r := range x.res[last.name] {
				for _, row := range r.eta.Rows {
					if row.Stage == st {
						c.add(row)
					}
				}
			}
			rows = append(rows, append([]string{st}, c.cells()...))
		}
		h := append([]string{"stage"}, head[1:]...)
		table(&b, h, rows)
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## 2. Suggested deadlines against guessed ones\n\n")
	fmt.Fprintf(&b, "Soft jobs are previews (the workload gives them 3 hours) and ML runs (36 hours). Each arm changes only how their deadlines are set; dailies (08:00), deliveries (Friday 18:00) and CI (15-minute SLO) keep theirs.\n\n")
	fmt.Fprintf(&b, "- \"Suggested at q\" and \"generous at q\" are the farm's suggestion at submission: the earliest deadline the job meets at quantile q of its shown band without making any job that is on time now late (`forecast.Policy` achievable, rounded up to 15 minutes, at least 15 minutes out). The shown band stops at P95, so q = 0.98 extrapolates the band's tail past P95 from P90 and P95 as a normal tail in log time-left; that tail is assumed, not measured.\n")
	fmt.Fprintf(&b, "- \"Set once\" never moves it (`MaxRevisions` 0). \"Moved later only when at risk\" is `forecast.Suggestion` with `MaxRevisions` 2, as the queue runs it: at an information point (1, 3, 5, 9, 17, 33 units finished), if the band shown under the current deadline puts P95 after the deadline, the suggestion is searched again and moves to it when that is later by more than max(a tenth of the time left, 15 minutes); never earlier, and settled after two moves, 33 units, or a quarter of the time to the deadline (\"at-risk moves\").\n")
	fmt.Fprintf(&b, "- The earlier rule, revising up to twice either way when the band narrowed by a fifth, is no longer in `forecast`; its five-seed result on the main workload was 79%% of previews on time at a median padding of 1.71 (commit 17e6ab1).\n")
	fmt.Fprintf(&b, "- \"User tightens to the next session\" models a person: right after the 0.95 suggestion, the deadline moves once to the earliest dailies session (08:00 on a weekday) at or after the band's P80 (80%% on-time odds), if that is earlier than the suggestion (\"tightened\"); otherwise the suggestion stands.\n\n")
	fmt.Fprintf(&b, "Missed and late hours are against each job's own deadline in that arm; turnaround is submission to finish; padding is deadline minus finish for jobs that made it, over their run time (submission to finish). Missed, late hours and burst are means per week over the seeds; on time and medians pool the seeds. Moves and policy runs (the policy runs spent finding the suggestion and checking it) are per suggestion of that kind.\n\n")
	deadlineTables(&b, x)

	fmt.Fprintf(&b, "## 3. The same arms when previews compete with deadline work\n\n")
	fmt.Fprintf(&b, "A workload variant, `%s`, run the same way: its own calibration week (the same seed, the variant's parameters), then seeds %v. It differs from the main workload only in:\n\n", cont.variant.name, seeds)
	for _, c := range cont.variant.changes(x.variant.p) {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	np, nm := meanJobs(cont, "preview"), meanJobs(x, "preview")
	fmt.Fprintf(&b, "\nPreviews per week: %.0f (main workload %.0f), each about three times the work. Evening previews are submitted while dailies are (dailies arrive around 19:00, due 08:00), so a preview's deadline decides whether it runs ahead of a daily. Columns as in section 2.\n\n", np, nm)
	deadlineTables(&b, cont)

	if err := os.WriteFile(filepath.Join(dir, "eta.md"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	var sum strings.Builder
	sum.WriteString("workload,arm,seed,kind,jobs,missed,late_h,suggested,at_risk_moves,tightened,policy_runs,dailies_missed,dailies_late_h,delivery_missed,delivery_late_h,burst_cost\n")
	for _, xr := range []*etaResults{x, cont} {
		for _, arm := range xr.arms {
			if !arm.deadlines {
				continue
			}
			for _, r := range xr.res[arm.name] {
				for _, kind := range []string{"preview", "ml"} {
					k, c := r.kind(kind), r.eta.count(kind)
					fmt.Fprintf(&sum, "%q,%q,%d,%s,%d,%d,%.3f,%d,%d,%d,%d,%d,%.3f,%d,%.3f,%.2f\n", xr.variant.name, arm.name, r.Seed, kind,
						k.Jobs, k.Missed, k.LateHours, c.Suggested, c.AtRisk, c.Tightened, c.Runs,
						r.kind("dailies").Missed, r.kind("dailies").LateHours, r.kind("delivery").Missed, r.kind("delivery").LateHours, r.BurstCost)
				}
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "eta_arms.csv"), []byte(sum.String()), 0o644); err != nil {
		return err
	}
	var csv strings.Builder
	csv.WriteString("arm,seed,kind,stage,taken_h,finish_h,p50_h,p80_h,p90_h,p95_h,sched50_h,sched90_h\n")
	for _, arm := range x.arms {
		if !arm.bands {
			continue
		}
		for _, r := range x.res[arm.name] {
			if r.eta == nil {
				continue
			}
			for _, row := range r.eta.Rows {
				fmt.Fprintf(&csv, "%q,%d,%s,%q,%.3f,%.3f,%.3f,%.3f,%.3f,%.3f,%.3f,%.3f\n", arm.name, r.Seed, row.Kind, row.Stage,
					row.Taken/hour, row.Finish/hour, row.Band[0]/hour, row.Band[1]/hour, row.Band[2]/hour, row.Band[3]/hour, row.S50/hour, row.S90/hour)
			}
		}
	}
	return os.WriteFile(filepath.Join(dir, "eta.csv"), []byte(csv.String()), 0o644)
}

// count is a run's suggestion tally for kind, empty when it had none.
func (r *etaRunResult) count(kind string) etaCount {
	if r == nil || r.ByKind[kind] == nil {
		return etaCount{}
	}
	return *r.ByKind[kind]
}

// kind is r's tally for a job kind, empty when it had none.
func (r *runResult) kind(n string) *kindStat {
	if s := r.ByKind[n]; s != nil {
		return s
	}
	return &kindStat{}
}

// meanJobs is the mean count of a kind's jobs per week over the first
// deadline arm's seeds.
func meanJobs(x *etaResults, kind string) float64 {
	for _, arm := range x.arms {
		if !arm.deadlines {
			continue
		}
		rs := x.res[arm.name]
		n := 0
		for _, r := range rs {
			n += r.kind(kind).Jobs
		}
		return float64(n) / float64(max(1, len(rs)))
	}
	return 0
}

// softStat is one soft kind's deadline outcome over an arm's seeds.
type softStat struct {
	jobs, missed, late float64
	turn, pad          []float64
	c                  etaCount
}

func (s *softStat) add(r *runResult, kind string) {
	k := r.kind(kind)
	s.jobs += float64(k.Jobs)
	s.missed += float64(k.Missed)
	s.late += k.LateHours
	for _, o := range r.Jobs {
		if o.Kind != kind {
			continue
		}
		s.turn = append(s.turn, o.Done-o.Submit)
		if o.Done <= o.Deadline && o.Done > o.Submit {
			s.pad = append(s.pad, (o.Deadline-o.Done)/(o.Done-o.Submit))
		}
	}
	c := r.eta.count(kind)
	s.c.Suggested += c.Suggested
	s.c.AtRisk += c.AtRisk
	s.c.Tightened += c.Tightened
	s.c.Runs += c.Runs
}

func (s softStat) onTime() float64 {
	if s.jobs == 0 {
		return math.NaN()
	}
	return 1 - s.missed/s.jobs
}

func median(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2], true
}

// cells are on time, missed and late hours per week, median turnaround
// and padding, then moves and policy runs per suggestion.
func (s softStat) cells(n float64) []string {
	onTime := ""
	if s.jobs > 0 {
		onTime = pct(s.onTime())
	}
	med := func(xs []float64) string {
		if m, ok := median(xs); ok {
			return fmt.Sprintf("%.2f", m)
		}
		return ""
	}
	per := func(k int64, f string) string {
		if s.c.Suggested == 0 {
			return ""
		}
		return fmt.Sprintf(f, float64(k)/float64(s.c.Suggested))
	}
	return []string{onTime, fmt.Sprintf("%.1f", s.missed/n), fmt.Sprintf("%.1f", s.late/n), med(s.turn), med(s.pad),
		per(int64(s.c.AtRisk), "%.2f"), per(int64(s.c.Tightened), "%.2f"), per(s.c.Runs, "%.0f")}
}

// deadlineTables writes the deadline arms' outcomes for previews and ML
// runs, and their collateral on everyone else.
func deadlineTables(b *strings.Builder, x *etaResults) {
	kinds := []struct{ kind, title string }{{"preview", "Previews"}, {"ml", "ML runs"}}
	stats := map[string][]softStat{}
	var arms []*etaArm
	var coll [][]string
	for _, arm := range x.arms {
		if !arm.deadlines {
			continue
		}
		arms = append(arms, arm)
		rs := x.res[arm.name]
		var dm, dl, vm, vl, bc float64
		for _, k := range kinds {
			var s softStat
			for _, r := range rs {
				s.add(r, k.kind)
			}
			stats[k.kind] = append(stats[k.kind], s)
		}
		for _, r := range rs {
			dm += float64(r.kind("dailies").Missed)
			dl += r.kind("dailies").LateHours
			vm += float64(r.kind("delivery").Missed)
			vl += r.kind("delivery").LateHours
			bc += r.BurstCost
		}
		n := float64(max(1, len(rs)))
		coll = append(coll, []string{arm.name, fmt.Sprintf("%.1f", dm/n), fmt.Sprintf("%.1f", dl/n), fmt.Sprintf("%.1f", vm/n), fmt.Sprintf("%.1f", vl/n), fmt.Sprintf("%.0f", bc/n)})
	}
	for _, k := range kinds {
		fmt.Fprintf(b, "%s:\n\n", k.title)
		var rows [][]string
		best := ""
		for i, arm := range arms {
			s := stats[k.kind][i]
			rows = append(rows, append([]string{arm.name}, s.cells(float64(max(1, len(x.res[arm.name]))))...))
			if best == "" && !arm.tighten && arm.policy.MaxRevisions == 0 && arm.policy.Quantile >= 0.95 && s.onTime() >= 0.95 {
				best = fmt.Sprintf("%.2f (%s on time)", arm.policy.Quantile, pct(s.onTime()))
			}
		}
		table(b, []string{"arm", "on time", "missed", "late h", "turnaround (median h)", "padding (median)",
			"at-risk moves per suggestion", "tightened per suggestion", "policy runs per suggestion"}, rows)
		if best == "" {
			best = "none of 0.95 and 0.98"
		}
		fmt.Fprintf(b, "\nLowest generous set-once quantile with %s on time at least 95%%: %s.\n\n", strings.ToLower(k.title), best)
	}
	b.WriteString("Everyone else:\n\n")
	table(b, []string{"arm", "dailies missed", "dailies late h", "deliveries missed", "deliveries late h", "burst $"}, coll)
	b.WriteString("\n")
}

func (r *etaRunResult) rowsOf(kind string) []etaRow {
	if r == nil {
		return nil
	}
	var out []etaRow
	for _, row := range r.Rows {
		if row.Kind == kind {
			out = append(out, row)
		}
	}
	return out
}
