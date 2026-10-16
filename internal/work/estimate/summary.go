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
}

// Summary is a job's remaining work. W, Sigma and W90 are slot-seconds;
// TPath is seconds and is filled in by the caller from CriticalPath,
// because only the class knows the unit graph.
type Summary struct {
	W, Sigma, W90 float64
	TPath         float64
	Queued        int
	Running       int
	Unevaluated   int
}

// Plus combines two independent summaries.
func (s Summary) Plus(o Summary) Summary {
	r := Summary{
		W:           s.W + o.W,
		Sigma:       math.Hypot(s.Sigma, o.Sigma),
		TPath:       math.Max(s.TPath, o.TPath),
		Queued:      s.Queued + o.Queued,
		Running:     s.Running + o.Running,
		Unevaluated: s.Unevaluated + o.Unevaluated,
	}
	r.W90 = r.W + 1.28*r.Sigma
	return r
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

// Summarize computes W, its spread and W90 over queued and running units,
// including the expected work of unexpanded generators.
//
// Variance: queued units use the region model in the package comment.
// Running units are independent of each other and of the regions. For G
// unexpanded generators of one key, the variance is G*PerSD^2 (each
// generator's own spread) plus (G*MeanSD)^2 (the shared uncertainty of
// the fan-out mean), so many generators of an unknown key are treated as
// one large correlated unknown until the job's own expansions narrow it.
func (e *Estimator) Summarize(queued []Unit, run []RunningUnit) Summary {
	var s Summary
	corr := map[RegionID]float64{}
	var indVar float64
	gens := map[string]int{}
	for _, u := range queued {
		p := e.Predict(u.Key, u.Pos)
		rem := p.Mean - u.Done
		if rem < 0.1*p.Mean {
			rem = 0.1 * p.Mean
		}
		f := rem / p.Mean * u.slots()
		s.W += p.Mean * f
		corr[p.Region] += p.SDCorr * f
		indVar += (p.SDInd * f) * (p.SDInd * f)
		s.Queued++
		if u.Gen != "" {
			gens[u.Gen]++
		}
	}
	for _, r := range run {
		p := e.Predict(r.Key, r.Pos)
		rem, sd := e.Remainder(p, r.Done, r.Elapsed)
		s.W += rem * r.slots()
		indVar += (sd * r.slots()) * (sd * r.slots())
		s.Running++
		if r.Gen != "" {
			gens[r.Gen]++
		}
	}
	for k, n := range gens {
		g := e.Generator(k)
		G := float64(n)
		s.W += G * g.Mean
		indVar += G*g.PerSD*g.PerSD + (G*g.MeanSD)*(G*g.MeanSD)
		s.Unevaluated += n
	}
	v := indVar
	for _, c := range corr {
		v += c * c
	}
	s.Sigma = math.Sqrt(v)
	s.W90 = s.W + e.z()*s.Sigma
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
// one completed unit (which, being inside the run, was itself measured
// above the threshold). It needs at least three completed units.
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
	oi := 0
	for pos := range n {
		for oi < len(t.obs) && t.obs[oi].pos < pos {
			oi++
		}
		if oi < len(t.obs) && t.obs[oi].pos == pos {
			measured[pos] = true
			cost[pos] = t.obs[oi].dur
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
