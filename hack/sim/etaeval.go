package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
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
	// hour after submission) or "suggest" (forecast policy below).
	soft      string
	policy    forecast.Policy
	laterOnly bool // a revision may only move a suggestion later
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

type etaRunResult struct {
	Rows                  []etaRow
	Suggested, Revisions  int
	SuggestRuns, PolicyNs int64
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
		e.res.eta = &etaRunResult{}
	}
	return e.res.eta
}

// beforeDecide sets and revises suggested deadlines, so the decision
// plans on them.
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
		info := forecast.InfoPoint(j.done) && j.done > ej.lastInfo
		ev := forecast.Event{Now: now, Finished: j.done, Total: len(j.units), Candidate: ej.sugg.Deadline}
		if ej.pending || (info && ej.sugg.State != forecast.Settled) {
			c, b := a.candidate(e, *s, i)
			if c.IsZero() {
				continue
			}
			if a.laterOnly && !ej.pending && c.Before(ej.sugg.Deadline) {
				c = ej.sugg.Deadline
			}
			ev.Candidate, ev.Band, ev.Info = c, b, info && !ej.pending
			if info {
				ej.lastInfo = j.done
			}
			if ej.pending {
				e.etaResult().Suggested++
			}
			ej.pending = false
		}
		before := ej.sugg.Revisions
		ej.sugg.Observe(a.policy, ev)
		e.etaResult().Revisions += ej.sugg.Revisions - before
		if !ej.sugg.Deadline.IsZero() {
			j.deadline = ej.sugg.Deadline.Sub(weekStart).Seconds()
			s.Jobs[i].Deadline = ej.sugg.Deadline
		}
	}
}

// candidate is the policy's achievable deadline for job i of s, found
// as the queue does (internal/work/queue/suggest.go): the earliest
// deadline by which the job finishes at the policy's quantile without
// making any job that is on time now late.
func (a *etaArm) candidate(e *engine, s sched.Snapshot, i int) (time.Time, forecast.Band) {
	now := s.Now
	id := s.Jobs[i].ID
	sum := s.Jobs[i].Estimate
	t0 := time.Now()
	runs := int64(0)
	run := func(d time.Time) sched.Decisions {
		s2 := s
		s2.Jobs = append([]sched.Job(nil), s.Jobs...)
		s2.Jobs[i].Deadline = d
		var st sched.State
		runs++
		return e.pol.fn(s2, &st, e.pcfg)
	}
	defer func() {
		r := e.etaResult()
		r.SuggestRuns += runs
		r.PolicyNs += int64(time.Since(t0))
	}()
	hi := now.Add(7 * 24 * time.Hour)
	key := forecast.FinishKey{Class: s.Jobs[i].Class, Stage: forecast.StageOf(s.Jobs[i].UnitsDone, s.Jobs[i].UnitsDone+s.Jobs[i].RunnableTotal+s.Jobs[i].Blocked+len(s.Jobs[i].RunningUnits))}
	show := func(js sched.JobState) forecast.Band {
		return a.fcal.Apply(key, now, forecast.Display(now, sum, js.ETA50, js.ETA90))
	}
	base := run(hi)
	band := show(base.Jobs[id])
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
		r := run(d)
		b := show(r.Jobs[id])
		if b.IsZero() || b.At(a.policy.Quantile).After(d) {
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
		d = band.At(a.policy.Quantile)
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
		b := forecast.Display(now, sj.Estimate, js.ETA50, js.ETA90)
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

type etaResults struct {
	arms []*etaArm
	res  map[string][]*runResult
	cal  map[string]*estimate.Calibration
	took time.Duration
}

func runETAExperiment(p params, seeds []uint64) *etaResults {
	t0 := time.Now()
	pol := policyByName("objective+burst")
	// The calibration week: a farm's earlier week of finished jobs.
	cal := map[string]*estimate.Calibration{}
	fcal := forecast.NewFinishCalibration(0)
	w := generate(p, etaCalSeed)
	h := buildHistory(p, w.targets)
	// Twice over the same earlier week: first for the W scores, then,
	// with W calibrated, for the finish scores of the band it shows.
	e := newEngine(w, h, etaCalSeed, pol, mainFarm)
	e.eta = &etaArm{name: "calibration week", soft: "workload", collect: cal}
	e.run()
	e = newEngine(w, h, etaCalSeed, pol, mainFarm)
	e.eta = &etaArm{name: "calibration week, finish", soft: "workload", cal: cal, fcollect: fcal}
	e.run()

	sp := forecast.DefaultPolicy()
	once := sp
	once.MaxRevisions = 0
	arms := []*etaArm{
		{name: "workload deadlines, W as built", soft: "workload"},
		{name: "workload deadlines, W calibrated", soft: "workload", cal: cal},
		{name: "workload deadlines, W and finish calibrated", soft: "workload", cal: cal, fcal: fcal},
		{name: "guess: previews and ML due an hour after submission", soft: "asap", cal: cal, fcal: fcal},
		{name: "suggested, set at submission only", soft: "suggest", policy: once, cal: cal, fcal: fcal},
		{name: "suggested, up to 2 revisions when the band narrows", soft: "suggest", policy: sp, cal: cal, fcal: fcal},
		{name: "suggested, never earlier after submission", soft: "suggest", policy: sp, laterOnly: true, cal: cal, fcal: fcal},
	}
	type task struct {
		arm  *etaArm
		seed uint64
	}
	loads, hist := map[uint64]*workload{}, map[uint64]*history{}
	for _, s := range seeds {
		loads[s] = generate(p, s)
		hist[s] = buildHistory(p, loads[s].targets)
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
				fmt.Fprintf(os.Stderr, "eta %-55s seed %d: %s\n", t.arm.name, t.seed, time.Since(t1).Round(time.Millisecond))
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
	return &etaResults{arms: arms, res: res, cal: cal, took: time.Since(t0)}
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

func writeETA(dir string, x *etaResults, seeds []uint64) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# ETA bands and suggested deadlines\n\n")
	fmt.Fprintf(&b, "Generated by `go run ./hack/sim` (the ETA experiment, %s). Main farm, objective policy with burst, seeds %v, the same weeks as results.md. Synthetic workload.\n\n", x.took.Round(time.Second), seeds)
	fmt.Fprintf(&b, "The calibration arm's W percentiles come from `estimate.Calibration` built from a separate week (seed %#x) run the same way, per job kind, as the queue builds them live from finished jobs.\n\n", etaCalSeed)

	fmt.Fprintf(&b, "## 1. Does the shown band cover the actual finish?\n\n")
	fmt.Fprintf(&b, "A job's band is recorded when it is first planned, at every binary-order information point (1, 3, 5, 9, 17, 33 units finished) and every hour, and compared with when the job actually finished. P50 to P95 are `forecast.Display`, what `status.estimate.eta50`..`eta95` show; \"sched ETA50/90\" are the scheduler's own finishes, which decide but are not shown. Error is the median of |P50 - finish| over the time that was left.\n\n")
	head := []string{"kind", "records", "P50", "P80", "P90", "P95", "error", "sched ETA50", "sched ETA90"}
	for _, arm := range x.arms[:3] {
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
	fmt.Fprintf(&b, "### By how much of the job had finished (W and finish calibrated)\n\n")
	{
		var rows [][]string
		for _, st := range etaStages {
			var c etaCoverage
			for _, r := range x.res[x.arms[2].name] {
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
	fmt.Fprintf(&b, "Soft jobs are previews (the workload gives them 3 hours) and ML runs (36 hours). Each arm changes only how their deadlines are set; dailies (08:00), deliveries (Friday 18:00) and CI (15-minute SLO) keep theirs. Missed and late hours are against each job's own deadline in that arm; turnaround is submission to finish; padding is deadline minus finish for jobs that made it, over their run time. Means per week over the seeds.\n\n")
	head2 := []string{"arm", "previews on time", "previews missed", "preview late h", "preview turnaround (median h)", "preview padding (median)", "ML missed", "ML late h", "dailies missed", "dailies late h", "deliveries missed", "deliveries late h", "burst $", "revisions per suggestion", "policy runs per suggestion"}
	var rows [][]string
	for _, arm := range x.arms[2:] {
		rs := x.res[arm.name]
		var pm, pl, mm, ml, dm, dl, vm, vl, bc float64
		var turn, pad []float64
		var nPrev float64
		var sug, rev, runs int64
		for _, r := range rs {
			k := func(n string) *kindStat {
				if s := r.ByKind[n]; s != nil {
					return s
				}
				return &kindStat{}
			}
			pm += float64(k("preview").Missed)
			nPrev += float64(k("preview").Jobs)
			pl += k("preview").LateHours
			mm += float64(k("ml").Missed)
			ml += k("ml").LateHours
			dm += float64(k("dailies").Missed)
			dl += k("dailies").LateHours
			vm += float64(k("delivery").Missed)
			vl += k("delivery").LateHours
			bc += r.BurstCost
			for _, o := range r.Jobs {
				if o.Kind != "preview" {
					continue
				}
				turn = append(turn, o.Done-o.Submit)
				if o.Done <= o.Deadline && o.Done > o.Submit {
					pad = append(pad, (o.Deadline-o.Done)/(o.Done-o.Submit))
				}
			}
			if r.eta != nil {
				sug += int64(r.eta.Suggested)
				rev += int64(r.eta.Revisions)
				runs += r.eta.SuggestRuns
			}
		}
		n := float64(len(rs))
		sort.Float64s(turn)
		sort.Float64s(pad)
		medf := func(xs []float64, f string) string {
			if len(xs) == 0 {
				return ""
			}
			return fmt.Sprintf(f, xs[len(xs)/2])
		}
		rv, pr := "", ""
		if sug > 0 {
			rv = fmt.Sprintf("%.2f", float64(rev)/float64(sug))
			pr = fmt.Sprintf("%.0f", float64(runs)/float64(sug))
		}
		onTime := ""
		if nPrev > 0 {
			onTime = pct(1 - pm/nPrev)
		}
		rows = append(rows, []string{arm.name, onTime, fmt.Sprintf("%.1f", pm/n), fmt.Sprintf("%.1f", pl/n), medf(turn, "%.2f"),
			medf(pad, "%.2f"), fmt.Sprintf("%.1f", mm/n), fmt.Sprintf("%.1f", ml/n), fmt.Sprintf("%.1f", dm/n), fmt.Sprintf("%.1f", dl/n),
			fmt.Sprintf("%.1f", vm/n), fmt.Sprintf("%.1f", vl/n), fmt.Sprintf("%.0f", bc/n), rv, pr})
	}
	table(&b, head2, rows)
	b.WriteString("\n")
	if err := os.WriteFile(filepath.Join(dir, "eta.md"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	var csv strings.Builder
	csv.WriteString("arm,seed,kind,stage,taken_h,finish_h,p50_h,p80_h,p90_h,p95_h,sched50_h,sched90_h\n")
	for _, arm := range x.arms {
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
