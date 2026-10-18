package estimate

import (
	"fmt"
	"math"
	"sort"
)

// Unit is an unfinished unit as the estimator sees it.
type Unit struct {
	Key   string
	Pos   int
	Slots int // slots the unit occupies while running (a gang's size); 0 means 1
	// Done is work of this unit already retained, in seconds (a
	// checkpointed run that was preempted and requeued).
	Done float64
	// Gen, when set, marks an unevaluated generator of that generator key.
	// Its own evaluation is predicted from Key and Pos like any unit; its
	// eventual children are predicted from the fan-out model.
	Gen string
}

func (u Unit) slots() float64 {
	if u.Slots < 1 {
		return 1
	}
	return float64(u.Slots)
}

// RunningUnit is a unit that is running now.
type RunningUnit struct {
	Unit
	Elapsed float64 // seconds since this attempt started
	// Progress is the share of the unit done that the unit itself reports
	// (.c4/progress), 0 to 1; 0 when it reports none.
	Progress float64
}

// Summary is a job's remaining work. W, Sigma and the percentiles W50 to
// W95 are slot-seconds; TPath is seconds and is filled in by the caller
// from CriticalPath, because only the class knows the unit graph.
//
// W is the region model's expectation. The percentiles are calibrated
// (see Calibration) when the estimator's Config has a Calibration with
// enough scores for the job's Stratum, and Calibrated says so; otherwise
// they are W plus the normal quantile times Sigma, with W90 using Config.Z.
type Summary struct {
	W, Sigma, W90 float64
	W50, W80, W95 float64
	TPath         float64
	Queued        int
	Running       int
	Unevaluated   int
	Stratum       Stratum
	Calibrated    bool
}

// Plus combines two independent summaries. Calibrated percentiles add
// (a conservative combination: it assumes the two are comonotone);
// uncalibrated ones are recomputed from the combined Sigma.
func (s Summary) Plus(o Summary) Summary {
	r := Summary{
		W:           s.W + o.W,
		Sigma:       math.Hypot(s.Sigma, o.Sigma),
		TPath:       math.Max(s.TPath, o.TPath),
		Queued:      s.Queued + o.Queued,
		Running:     s.Running + o.Running,
		Unevaluated: s.Unevaluated + o.Unevaluated,
	}
	if s.Calibrated && o.Calibrated {
		r.W50, r.W80, r.W90, r.W95 = s.W50+o.W50, s.W80+o.W80, s.W90+o.W90, s.W95+o.W95
		r.Calibrated = true
		return r
	}
	r.normal(1.28)
	return r
}

// normal sets the percentiles from W and Sigma as a normal distribution.
func (s *Summary) normal(z90 float64) {
	s.W50 = s.W
	s.W80 = s.W + 0.8416*s.Sigma
	s.W90 = s.W + z90*s.Sigma
	s.W95 = s.W + 1.6449*s.Sigma
}

// calibrate sets the percentiles from c's scores for the summary's
// stratum, and reports whether it could.
func (s *Summary) calibrate(c *Calibration) bool {
	if c == nil {
		return false
	}
	sc := Scale(*s)
	var w [4]float64
	for i, q := range []float64{0.5, 0.8, 0.9, 0.95} {
		x, ok := c.Quantile(s.Stratum, q)
		if !ok {
			return false
		}
		w[i] = math.Max(0, s.W+x*sc)
	}
	s.W50, s.W80, s.W90, s.W95 = w[0], w[1], w[2], w[3]
	s.Calibrated = true
	return true
}

// Remainder is the expected remaining duration (seconds) of a running
// unit with prediction p: the prediction less work done, or, once the unit
// has run past its prediction, Overrun times its elapsed time. The second
// value is its standard deviation.
func (e *Estimator) Remainder(p Pred, done, elapsed float64) (float64, float64) {
	rem := p.Mean - done - elapsed
	if rem <= 0 {
		r := e.cfg.Overrun * elapsed
		return r, r
	}
	return rem, p.SD() * rem / p.Mean
}

// ProgressMin is the least reported progress the estimator uses: below
// it, elapsed*(1-f)/f says too little.
const ProgressMin = 0.02

// RemainderAt is Remainder for a unit that reports progress f (the share
// of it done). A unit that reports none, or too little, gets Remainder.
//
// Progress says what a single long unit cannot otherwise say: it has no
// siblings to sample, so without it the estimator knows only its prior
// and how long it has run, and a 4.9-hour unit with a 0.6-hour prior
// looks nearly done for hours. From progress, the unit's total is
// (done+elapsed)/f, so the remainder is (done+elapsed)*(1-f)/f, with a
// relative spread of ProgressRelSD*sqrt((1-f)/f): wide early, where a
// unit's progress is least linear, narrow late. While the unit is within
// its prediction, the two remainders are combined by precision, so
// progress takes over as f grows. Once it has run past its prediction,
// the prediction is disproved and the overrun rule is a guess with no
// evidence behind it, so progress alone decides.
func (e *Estimator) RemainderAt(p Pred, done, elapsed, progress float64) (float64, float64) {
	rem, sd := e.Remainder(p, done, elapsed)
	if progress < ProgressMin {
		return rem, sd
	}
	if progress >= 1 {
		return 0, 0
	}
	took := done + elapsed
	overrun := p.Mean-took <= 0
	rp := took * (1 - progress) / progress
	rel := e.cfg.ProgressRelSD
	if rel <= 0 {
		rel = 0.2
	}
	sp := math.Max(rp*rel*math.Sqrt((1-progress)/progress), 1)
	if overrun {
		return rp, sp
	}
	sd = math.Max(sd, 0.05*rem+1)
	w1, w2 := 1/(sd*sd), 1/(sp*sp)
	return (rem*w1 + rp*w2) / (w1 + w2), 1 / math.Sqrt(w1+w2)
}

// Summarize computes W, its spread and its percentiles over queued and
// running units, including the expected work of unexpanded generators.
// It first calls SetRunning with the running units. The percentiles are
// calibrated when Config.Calibration covers the job's stratum.
//
// Variance: queued units use the region model in the package comment.
// Running units are independent of each other and of the regions. For G
// unexpanded generators of one key, the variance is G*PerSD^2 (each
// generator's own spread) plus (G*MeanSD)^2 (the shared uncertainty of
// the fan-out mean), so many generators of an unknown key are treated as
// one large correlated unknown until the job's own expansions narrow it.
func (e *Estimator) Summarize(queued []Unit, run []RunningUnit) Summary {
	e.SetRunning(run)
	var s Summary
	// Maps are summed in first-seen order, so the result does not depend
	// on map iteration order (floating-point addition is not associative).
	corr := map[RegionID]float64{}
	var regions []RegionID
	var indVar float64
	gens := map[string]int{}
	var genKeys []string
	for _, u := range queued {
		p := e.Predict(u.Key, u.Pos)
		rem := p.Mean - u.Done
		if rem < 0.1*p.Mean {
			rem = 0.1 * p.Mean
		}
		f := rem / p.Mean * u.slots()
		s.W += p.Mean * f
		if _, ok := corr[p.Region]; !ok {
			regions = append(regions, p.Region)
		}
		corr[p.Region] += p.SDCorr * f
		indVar += (p.SDInd * f) * (p.SDInd * f)
		s.Queued++
		if u.Gen != "" {
			if gens[u.Gen] == 0 {
				genKeys = append(genKeys, u.Gen)
			}
			gens[u.Gen]++
		}
	}
	for _, r := range run {
		p := e.Predict(r.Key, r.Pos)
		rem, sd := e.RemainderAt(p, r.Done, r.Elapsed, r.Progress)
		s.W += rem * r.slots()
		indVar += (sd * r.slots()) * (sd * r.slots())
		s.Running++
		if r.Gen != "" {
			if gens[r.Gen] == 0 {
				genKeys = append(genKeys, r.Gen)
			}
			gens[r.Gen]++
		}
	}
	for _, k := range genKeys {
		n := gens[k]
		g := e.Generator(k)
		G := float64(n)
		s.W += G * g.Mean
		indVar += G*g.PerSD*g.PerSD + (G*g.MeanSD)*(G*g.MeanSD)
		s.Unevaluated += n
	}
	v := indVar
	for _, r := range regions {
		v += corr[r] * corr[r]
	}
	s.Sigma = math.Sqrt(v)
	s.Stratum = e.stratum(queued, run)
	if !s.calibrate(e.cfg.Calibration) {
		s.normal(e.z())
	}
	return s
}

func (e *Estimator) z() float64 {
	if e.cfg.Z > 0 {
		return e.cfg.Z
	}
	return 1.28
}

// Region is a stretch of the unit axis flagged as heavy.
type Region struct {
	Key    string
	Lo, Hi int     // inclusive unit positions
	Factor float64 // peak cost over the key's median
	// Measured is how many completed units lie inside.
	Measured int
}

// Heavy returns the heavy regions of key on an axis of n units: maximal
// runs of positions whose measured or interpolated cost exceeds
// HeavyFactor times the key's reference cost, each containing at least
// one measured unit (which, being inside the run, was itself measured
// above the threshold). A running unit counts as measured once its elapsed
// time alone is above the threshold (see SetRunning); its cost there is
// its elapsed time, a lower bound. It needs at least three completed
// units.
//
// The reference cost is the lower of two medians: the median of the
// completed units, and the median over every position of the axis of the
// measured or interpolated cost. The first is biased upward when samples
// cluster inside a heavy region (refinement there); the second when one
// heavy sample's interpolation ramps cover much of a coarsely sampled
// axis. Taking the lower avoids both biases at some risk of false flags,
// which the simulation measures.
func (e *Estimator) Heavy(key string, n int) []Region {
	t := e.tracks[key]
	if t == nil || len(t.obs) < 3 || e.cfg.NoLocality || n <= 0 {
		return nil
	}
	cost := make([]float64, n)
	measured := make([]bool, n)
	v := t.view()
	oi := 0
	for pos := range n {
		for oi < len(v) && v[oi].pos < pos {
			oi++
		}
		if oi < len(v) && v[oi].pos == pos {
			measured[pos] = true
			cost[pos] = v[oi].dur
			if v[oi].prov {
				cost[pos] = v[oi].lb // what is known for certain
			}
			continue
		}
		cost[pos] = e.Predict(key, pos).Mean
	}
	axis := append([]float64(nil), cost...)
	sort.Float64s(axis)
	ref := math.Min(t.median(), median(axis))
	hf := e.cfg.HeavyFactor
	if hf <= 0 {
		hf = 3
	}
	thr := hf * ref
	var out []Region
	var cur *Region
	for pos := range n {
		if cost[pos] <= thr {
			if cur != nil && cur.Measured > 0 {
				out = append(out, *cur)
			}
			cur = nil
			continue
		}
		if cur == nil {
			cur = &Region{Key: key, Lo: pos}
		}
		cur.Hi = pos
		cur.Factor = math.Max(cur.Factor, cost[pos]/ref)
		if measured[pos] {
			cur.Measured++
		}
	}
	if cur != nil && cur.Measured > 0 {
		out = append(out, *cur)
	}
	return out
}

// CriticalPath returns the longest chain of durations through a DAG:
// dur[i] is node i's duration and deps[i] the nodes it waits on. It
// returns an error if the graph has a cycle or an out-of-range edge.
func CriticalPath(dur []float64, deps [][]int) (float64, error) {
	n := len(dur)
	finish := make([]float64, n)
	state := make([]uint8, n) // 0 new, 1 on stack, 2 done
	var visit func(i int) error
	visit = func(i int) error {
		switch state[i] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("cycle through node %d", i)
		}
		state[i] = 1
		start := 0.0
		if i < len(deps) {
			for _, d := range deps[i] {
				if d < 0 || d >= n {
					return fmt.Errorf("node %d depends on %d, out of range", i, d)
				}
				if err := visit(d); err != nil {
					return err
				}
				start = math.Max(start, finish[d])
			}
		}
		finish[i] = start + dur[i]
		state[i] = 2
		return nil
	}
	best := 0.0
	for i := range n {
		if err := visit(i); err != nil {
			return 0, err
		}
		best = math.Max(best, finish[i])
	}
	return best, nil
}
