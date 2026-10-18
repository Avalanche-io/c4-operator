// Package estimate predicts the remaining work of a job from the durations
// of its completed units (DESIGN.md section 9.8).
//
// Durations are kept per class key (a render pass, a CI target, an ML
// step) and indexed by position on the job's unit axis. An unfinished
// unit's duration is interpolated between the nearest completed units on
// each side. When that is not possible the estimate falls back, in order,
// to the median of the nearest completed units on the one side that has
// them, to a prior from history for the (project, class key), to the job's
// median over all keys, and to the class default.
//
// DESIGN.md lists the job median before the history prior. That order is
// changed here on purpose: a key with no completed unit in the job (a
// beauty pass just revealed by a generator) would otherwise be predicted
// from other keys' durations (45-second generator evaluations), which can
// be off by two orders of magnitude, while history for the same key is
// not.
//
// Error model, stated exactly:
//
//   - Every prediction carries a correlated part (sdCorr) and an
//     independent part (sdInd), and belongs to one region.
//   - A region is the stretch between two consecutive completed units of a
//     key (a bracket), one of the two stretches beyond the outermost
//     completed units (an edge), or, for keys with no completed unit, the
//     whole key.
//   - Within a region the correlated parts add linearly (they are treated
//     as perfectly correlated: if the interpolation is wrong for one unit
//     of a bracket it is wrong the same way for its neighbors). Independent
//     parts and different regions add in quadrature.
//   - For an interpolated unit, sdCorr combines half the difference between
//     the two neighbors with the relative leave-one-out residual of the
//     interpolation (each interior completed unit predicted from its own
//     neighbors), shrunk toward a prior relative spread while few residuals
//     exist. sdInd is zero.
//
// This is conservative: when samples are sparse the whole bracket is
// assumed to share one error. As sampling densifies, brackets shrink to a
// unit or two and the model approaches independent per-unit errors.
package estimate

import (
	"math"
	"sort"
)

// Prior is a duration distribution in seconds.
type Prior struct {
	Mean, SD float64
}

func (p Prior) ok() bool { return p.Mean > 0 }

// GenPrior is the history for a generator key: per generator, the number
// of children it emits, the total work of those children in slot-seconds,
// and the duration of its longest child (for the critical path).
type GenPrior struct {
	Children, Work, Longest Prior
}

// Config holds a class's estimator settings.
type Config struct {
	// HeavyFactor flags a region whose cost exceeds this multiple of the
	// key's median (3 in DESIGN.md).
	HeavyFactor float64
	// Overrun is the fraction of its elapsed time a unit already past its
	// estimate is assumed to need again (0.25).
	Overrun float64
	// Z is the quantile multiplier for W90 (1.28).
	Z float64
	// PriorRelSD is the relative spread assumed before leave-one-out
	// residuals exist, and ShrinkK the pseudo-count with which it is
	// blended with measured residuals and with which history priors are
	// blended with a job's own observed fan-out.
	PriorRelSD float64
	ShrinkK    float64
	// NoLocality marks axes where neighboring units are unrelated (CI
	// shards). Interpolation is skipped; units are predicted from the
	// key's median with independent errors.
	NoLocality bool
	// Default is the class default per unit.
	Default Prior
}

// DefaultConfig returns the settings in DESIGN.md section 9.8.
func DefaultConfig() Config {
	return Config{
		HeavyFactor: 3,
		Overrun:     0.25,
		Z:           1.28,
		PriorRelSD:  0.25,
		ShrinkK:     3,
		Default:     Prior{Mean: 600, SD: 600},
	}
}

// Source says which rule produced a prediction.
type Source int

const (
	Measured Source = iota
	Interpolated
	OneSided
	JobMedian
	History
	ClassDefault
)

func (s Source) String() string {
	return [...]string{"measured", "interpolated", "one-sided", "job-median", "history", "class-default"}[s]
}

// Pred is one unit's predicted duration in seconds.
type Pred struct {
	Mean   float64
	SDCorr float64 // shared with the rest of the region
	SDInd  float64 // independent
	Source Source
	Region RegionID
}

// SD is the unit's own standard deviation.
func (p Pred) SD() float64 { return math.Hypot(p.SDCorr, p.SDInd) }

// RegionID identifies a correlation region.
type RegionID struct {
	Key string
	// Lo is the position of the completed unit that opens a bracket;
	// edgeLeft or edgeRight for the edges; wholeKey when the key has no
	// completed unit (or has no locality).
	Lo int
}

const (
	edgeLeft  = math.MinInt + 1
	edgeRight = math.MaxInt
	wholeKey  = math.MinInt
)

type obs struct {
	pos int
	dur float64
	// lb is set on provisional observations (running units past their
	// prediction): the elapsed time, a lower bound on the duration.
	lb   float64
	prov bool
}

type track struct {
	obs    []obs // completed units, sorted by pos
	prov   []obs // provisional observations from running units
	merged []obs // obs and prov, sorted by pos; nil when prov is empty
	looOK  bool
	looRMS float64
	looN   int
	sorted []float64 // durations, sorted, for medians
	sortOK bool
}

func (t *track) add(pos int, dur float64) {
	i := sort.Search(len(t.obs), func(i int) bool { return t.obs[i].pos > pos })
	t.obs = append(t.obs, obs{})
	copy(t.obs[i+1:], t.obs[i:])
	t.obs[i] = obs{pos: pos, dur: dur}
	t.looOK, t.sortOK = false, false
	t.merge()
}

// view is what interpolation uses: completed units plus provisional ones.
func (t *track) view() []obs {
	if t.merged != nil {
		return t.merged
	}
	return t.obs
}

func (t *track) merge() {
	if len(t.prov) == 0 {
		t.merged = nil
		return
	}
	t.merged = append(append(t.merged[:0:0], t.obs...), t.prov...)
	sort.SliceStable(t.merged, func(i, j int) bool { return t.merged[i].pos < t.merged[j].pos })
}

func (t *track) median() float64 {
	if !t.sortOK {
		t.sorted = t.sorted[:0]
		for _, o := range t.obs {
			t.sorted = append(t.sorted, o.dur)
		}
		sort.Float64s(t.sorted)
		t.sortOK = true
	}
	return median(t.sorted)
}

// loo returns the RMS relative leave-one-out residual of linear
// interpolation over the key's completed units, and how many residuals it
// is based on.
func (t *track) loo() (float64, int) {
	if t.looOK {
		return t.looRMS, t.looN
	}
	var ss float64
	n := 0
	for i := 1; i+1 < len(t.obs); i++ {
		l, r := t.obs[i-1], t.obs[i+1]
		if r.pos == l.pos {
			continue
		}
		f := float64(t.obs[i].pos-l.pos) / float64(r.pos-l.pos)
		pred := l.dur + (r.dur-l.dur)*f
		if pred <= 0 {
			continue
		}
		e := (t.obs[i].dur - pred) / pred
		ss += e * e
		n++
	}
	t.looN = n
	t.looRMS = 0
	if n > 0 {
		t.looRMS = math.Sqrt(ss / float64(n))
	}
	t.looOK = true
	return t.looRMS, t.looN
}

type running struct{ n, mean, m2 float64 }

func (r *running) add(x float64) {
	r.n++
	d := x - r.mean
	r.mean += d / r.n
	r.m2 += d * (x - r.mean)
}

func (r running) variance() float64 {
	if r.n < 2 {
		return 0
	}
	return r.m2 / (r.n - 1)
}

type fanout struct {
	children running // per expanded generator
	work     running // per generator whose children all finished
	longest  running
}

// Estimator holds one job's observations.
type Estimator struct {
	cfg       Config
	tracks    map[string]*track
	priors    map[string]Prior
	genPriors map[string]GenPrior
	fan       map[string]*fanout
	all       []float64 // every observed duration, sorted lazily
	allOK     bool
}

// New returns an estimator for one job.
func New(cfg Config) *Estimator {
	return &Estimator{
		cfg:       cfg,
		tracks:    map[string]*track{},
		priors:    map[string]Prior{},
		genPriors: map[string]GenPrior{},
		fan:       map[string]*fanout{},
	}
}

// SetPrior records the history prior for a class key.
func (e *Estimator) SetPrior(key string, p Prior) { e.priors[key] = p }

// SetGenPrior records the history prior for a generator key.
func (e *Estimator) SetGenPrior(genKey string, p GenPrior) { e.genPriors[genKey] = p }

// Observe records a completed unit's duration in seconds of wall time.
func (e *Estimator) Observe(key string, pos int, dur float64) {
	t := e.tracks[key]
	if t == nil {
		t = &track{}
		e.tracks[key] = t
	}
	t.add(pos, dur)
	e.all = append(e.all, dur)
	e.allOK = false
}

// Count returns how many completed units of key have been observed.
func (e *Estimator) Count(key string) int {
	if t := e.tracks[key]; t != nil {
		return len(t.obs)
	}
	return 0
}

// Median returns the median observed duration of key, or 0.
func (e *Estimator) Median(key string) float64 {
	t := e.tracks[key]
	if t == nil || len(t.obs) == 0 {
		return 0
	}
	return t.median()
}

func (e *Estimator) jobMedian() float64 {
	if !e.allOK {
		sort.Float64s(e.all)
		e.allOK = true
	}
	return median(e.all)
}

func median(s []float64) float64 {
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// relSD is the relative spread of interpolation for a key: measured
// leave-one-out RMS blended with the prior spread by pseudo-count.
func (e *Estimator) relSD(t *track) float64 {
	rms, n := t.loo()
	k := e.cfg.ShrinkK
	p := e.cfg.PriorRelSD
	return math.Sqrt((float64(n)*rms*rms + k*p*p) / (float64(n) + k))
}

// Predict returns the predicted duration of the unit of key at pos.
func (e *Estimator) Predict(key string, pos int) Pred {
	t := e.tracks[key]
	if t == nil || len(t.view()) == 0 {
		return e.fallback(key)
	}
	if e.cfg.NoLocality {
		if len(t.obs) == 0 {
			return e.fallback(key)
		}
		return e.noLocality(key, t)
	}
	rel := e.relSD(t)
	v := t.view()
	i := sort.Search(len(v), func(i int) bool { return v[i].pos >= pos })
	if i < len(v) && v[i].pos == pos {
		// A position with a completed unit (a retry, or a second unit of
		// the same key at the same place) or a provisional one: use what
		// was measured there.
		sum, n := 0.0, 0
		for j := i; j < len(v) && v[j].pos == pos; j++ {
			sum += v[j].dur
			n++
		}
		m := sum / float64(n)
		return Pred{Mean: m, SDCorr: rel * m, Source: Measured, Region: RegionID{key, pos}}
	}
	switch {
	case i > 0 && i < len(v):
		l, r := v[i-1], v[i]
		f := float64(pos-l.pos) / float64(r.pos-l.pos)
		m := l.dur + (r.dur-l.dur)*f
		sd := math.Hypot(math.Abs(r.dur-l.dur)/2, rel*m)
		return Pred{Mean: m, SDCorr: sd, Source: Interpolated, Region: RegionID{key, l.pos}}
	case i == 0:
		return e.oneSided(key, v, 0, rel, edgeLeft)
	default:
		return e.oneSided(key, v, len(v)-1, rel, edgeRight)
	}
}

// SetRunning replaces the provisional observations with those implied by
// the given running units. A running unit that has already run longer
// than its prediction from completed units is censored data: its duration
// is at least its elapsed time. It is entered at its position as a
// provisional observation of (Done+Elapsed)*(1+Overrun), the same overrun
// rule its own remainder uses, so interpolation around it reflects what
// is already known before the unit finishes. Leave-one-out residuals and
// medians use completed units only. Keys without locality are skipped.
func (e *Estimator) SetRunning(run []RunningUnit) {
	for _, t := range e.tracks {
		if len(t.prov) > 0 {
			t.prov = t.prov[:0]
			t.merge()
		}
	}
	if e.cfg.NoLocality {
		return
	}
	type add struct {
		key string
		o   obs
	}
	var adds []add
	for _, r := range run {
		p := e.Predict(r.Key, r.Pos)
		took := r.Done + r.Elapsed
		if took <= p.Mean {
			continue
		}
		adds = append(adds, add{r.Key, obs{pos: r.Pos, dur: took * (1 + e.cfg.Overrun), lb: took, prov: true}})
	}
	touched := map[*track]bool{}
	for _, a := range adds {
		t := e.tracks[a.key]
		if t == nil {
			t = &track{}
			e.tracks[a.key] = t
		}
		t.prov = append(t.prov, a.o)
		touched[t] = true
	}
	for t := range touched {
		t.merge()
	}
}

// oneSided predicts from the median of the (up to) three observed units
// nearest the edge at index from of v.
func (e *Estimator) oneSided(key string, v []obs, from int, rel float64, edge int) Pred {
	var near []float64
	for k := 0; k < 3 && k < len(v); k++ {
		j := from + k
		if edge == edgeRight {
			j = from - k
		}
		near = append(near, v[j].dur)
	}
	sort.Float64s(near)
	m := median(near)
	spread := (near[len(near)-1] - near[0]) / 2
	return Pred{Mean: m, SDCorr: math.Hypot(spread, rel*m), Source: OneSided, Region: RegionID{key, edge}}
}

func (e *Estimator) noLocality(key string, t *track) Pred {
	m := t.median()
	var mean, ss float64
	for _, o := range t.obs {
		mean += o.dur
	}
	mean /= float64(len(t.obs))
	for _, o := range t.obs {
		ss += (o.dur - mean) * (o.dur - mean)
	}
	n := float64(len(t.obs))
	k := e.cfg.ShrinkK
	prior := e.cfg.PriorRelSD * m
	if p, ok := e.priors[key]; ok && p.ok() {
		prior = p.SD
	}
	// Per-unit spread: sample variance blended with the prior's.
	v := (ss + k*prior*prior) / (n - 1 + k)
	if n < 2 {
		v = prior * prior
	}
	sd := math.Sqrt(v)
	// The median's own uncertainty is shared by every unit of the key.
	return Pred{Mean: m, SDCorr: sd / math.Sqrt(n), SDInd: sd, Source: Interpolated, Region: RegionID{key, wholeKey}}
}

func (e *Estimator) fallback(key string) Pred {
	r := RegionID{key, wholeKey}
	if p, ok := e.priors[key]; ok && p.ok() {
		return Pred{Mean: p.Mean, SDCorr: p.SD, Source: History, Region: r}
	}
	if len(e.all) > 0 {
		m := e.jobMedian()
		return Pred{Mean: m, SDCorr: 0.5 * m, Source: JobMedian, Region: r}
	}
	return Pred{Mean: e.cfg.Default.Mean, SDCorr: e.cfg.Default.SD, Source: ClassDefault, Region: r}
}

// Expanded records that a generator of genKey emitted children units.
func (e *Estimator) Expanded(genKey string, children int) {
	e.fanFor(genKey).children.add(float64(children))
}

// ExpansionDone records the total work (slot-seconds) and longest child
// duration (seconds) of a generator whose children have all finished.
func (e *Estimator) ExpansionDone(genKey string, work, longest float64) {
	f := e.fanFor(genKey)
	f.work.add(work)
	f.longest.add(longest)
}

func (e *Estimator) fanFor(k string) *fanout {
	f := e.fan[k]
	if f == nil {
		f = &fanout{}
		e.fan[k] = f
	}
	return f
}

// GenEstimate is the expected eventual child work of one unexpanded
// generator: Mean and PerSD in slot-seconds per generator, MeanSD the
// uncertainty of Mean itself (shared by all of the job's generators of the
// key), and Longest the expected longest child in seconds.
type GenEstimate struct {
	Mean, PerSD, MeanSD, Longest float64
	Children                     float64
}

// Generator returns the fan-out estimate for genKey.
//
// Before any generator of the key has been evaluated in this job, it is
// the history prior. Once generators have expanded, their observed child
// counts scale the prior's work per child. Once expanded generators have
// finished all their children, their observed work replaces the prior by
// a normal-normal update with pseudo-count ShrinkK: the mean moves to the
// observed running mean, the per-generator spread to the observed spread,
// and the shared uncertainty of the mean falls as 1/sqrt(ShrinkK+n).
func (e *Estimator) Generator(genKey string) GenEstimate {
	p, ok := e.genPriors[genKey]
	if !ok {
		p = GenPrior{
			Children: Prior{Mean: 1, SD: 1},
			Work:     e.cfg.Default,
			Longest:  e.cfg.Default,
		}
	}
	k := e.cfg.ShrinkK
	f := e.fan[genKey]
	g := GenEstimate{Mean: p.Work.Mean, PerSD: p.Work.SD, Longest: p.Longest.Mean, Children: p.Children.Mean}
	g.MeanSD = p.Work.SD / math.Sqrt(k)
	if f == nil {
		return g
	}
	if f.children.n > 0 {
		g.Children = f.children.mean
		if p.Children.Mean > 0 {
			scale := f.children.mean / p.Children.Mean
			g.Mean = p.Work.Mean * scale
			g.PerSD = p.Work.SD * scale
			g.MeanSD = g.PerSD / math.Sqrt(k+f.children.n)
		}
	}
	if f.work.n > 0 {
		n := f.work.n
		g.Mean = (k*g.Mean + n*f.work.mean) / (k + n)
		v := (k*g.PerSD*g.PerSD + (n-1)*f.work.variance()) / (k + n - 1)
		g.PerSD = math.Sqrt(v)
		g.MeanSD = g.PerSD / math.Sqrt(k+n)
		g.Longest = (k*g.Longest + n*f.longest.mean) / (k + n)
	}
	return g
}
