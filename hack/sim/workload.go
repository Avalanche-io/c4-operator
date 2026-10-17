package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/Avalanche-io/c4-operator/internal/work/order"
)

// params is every workload parameter. results.md prints them all.
type params struct {
	Days float64 // length of the submission window (a week)

	// Dailies: render jobs submitted in the evening, due 08:00 next day.
	DailiesPerEvening map[string]float64 // Poisson mean per show, Mon to Fri
	DailiesSubmitMean float64            // hours after midnight
	DailiesSubmitSD   float64            // hours
	DailiesSubmitLo   float64
	DailiesSubmitHi   float64
	// Previews: working-hours WIP renders, every second frame, cheaper
	// settings, due 3 hours after submission.
	PreviewPerHour map[string]float64 // Poisson rate per show, Mon to Fri 09:00 to 17:00
	PreviewCost    float64            // frame cost relative to dailies
	PreviewTurn    float64            // hours
	// Deliveries: final-quality renders submitted Wed 10:00 to Thu 20:00,
	// due Fri 18:00, rendered in sequential order.
	DeliveriesPerShow map[string]int
	DeliveryCost      float64
	DeliveryFrames    float64 // median frames

	FramesMedian, FramesSigma    float64
	FramesMin, FramesMax         int
	CPUFrameMin, GPUFrameMin     float64 // median minutes per frame, dailies quality
	JobSigma, FrameSigma         float64 // lognormal sigma across jobs and per frame
	TrendAmp                     float64 // amplitude of the smooth cost drift along a shot
	HeavyProb                    float64 // probability a shot has a heavy region
	HeavyLenLo, HeavyLenHi       float64 // region length as a fraction of the shot
	HeavyMults                   []float64
	GPUShare, DeliveryGPUShare   float64
	ExpandShare                  float64 // jobs whose frames are generators expanding into passes
	DeliveryExpandShare          float64
	EnvCost, VolumesCost         float64 // deep layer cost relative to beauty
	PassSigma                    float64
	VolumesProbLo, VolumesProbHi float64 // per job, probability a frame has a volumes layer
	UsdExportSec, UsdExportSigma float64 // per-frame USD export (Solaris) before the render
	RenderFailProb               float64 // per attempt; the 4th attempt always succeeds

	// CI: pipelines on weekdays 08:00 to 20:00 (rate shaped by hour), a
	// build that discovers test shards, p95 SLO 15 minutes.
	CIRatePerHour    float64
	CIBuildMin       float64
	CIBuildSigma     float64
	CIShardsBase     int
	CIShardsPoisson  float64
	CITargets        int
	CITargetMin      float64
	CITargetSigma    float64
	CIRunSigma       float64
	CIFailProb       float64
	CISLO            float64 // minutes
	CIMergeDayFactor float64 // rate factor on Thursdays (release cut)

	// ML fine-tuning runs.
	MLWeekdayRuns, MLWeekendRuns float64 // Poisson mean per day
	MLGPUs                       []int
	MLGPUWeights                 []float64
	MLHoursMedian, MLHoursSigma  float64
	MLCkptMinLo, MLCkptMinHi     float64
	MLRestoreMin                 float64
	MLHazardPerHour              float64
	MLDeadlineHours              float64
	MLDeclaredSigma              float64
}

func defaultParams() params {
	return params{
		Days:              7,
		DailiesPerEvening: map[string]float64{"showA": 9, "showB": 7, "showC": 5},
		DailiesSubmitMean: 19, DailiesSubmitSD: 1.3, DailiesSubmitLo: 15, DailiesSubmitHi: 23.5,
		PreviewPerHour:    map[string]float64{"showA": 2, "showB": 1.5, "showC": 1},
		PreviewCost:       0.35,
		PreviewTurn:       3,
		DeliveriesPerShow: map[string]int{"showA": 6, "showB": 4, "showC": 3},
		DeliveryCost:      1.6,
		DeliveryFrames:    150,
		FramesMedian:      100, FramesSigma: 0.5, FramesMin: 24, FramesMax: 400,
		CPUFrameMin: 45, GPUFrameMin: 12,
		JobSigma: 0.6, FrameSigma: 0.2, TrendAmp: 0.15,
		HeavyProb: 0.4, HeavyLenLo: 0.08, HeavyLenHi: 0.25,
		HeavyMults:       []float64{2, 3, 5, 8},
		GPUShare:         0.25,
		DeliveryGPUShare: 0.2,
		ExpandShare:      0.3, DeliveryExpandShare: 0.4,
		EnvCost: 0.2, VolumesCost: 0.3, PassSigma: 0.1,
		VolumesProbLo: 0.3, VolumesProbHi: 0.9,
		UsdExportSec: 45, UsdExportSigma: 0.3,
		RenderFailProb: 0.02,

		CIRatePerHour: 30, CIBuildMin: 3, CIBuildSigma: 0.3,
		CIShardsBase: 4, CIShardsPoisson: 10,
		CITargets: 400, CITargetMin: 1.5, CITargetSigma: 0.6, CIRunSigma: 0.15,
		CIFailProb: 0.02, CISLO: 15, CIMergeDayFactor: 1.3,

		MLWeekdayRuns: 4, MLWeekendRuns: 2,
		MLGPUs: []int{1, 2, 4, 8}, MLGPUWeights: []float64{0.2, 0.35, 0.35, 0.1},
		MLHoursMedian: 5, MLHoursSigma: 0.5,
		MLCkptMinLo: 5, MLCkptMinHi: 15, MLRestoreMin: 3,
		MLHazardPerHour: 0.005, MLDeadlineHours: 36, MLDeclaredSigma: 0.15,
	}
}

// unitSpec is one unit of work as generated. Children of generators start
// hidden and are revealed when their generator finishes.
type unitSpec struct {
	key      string
	pos      int
	sub      int // pass order within a position
	slots    int
	dur      float64 // true seconds of work
	deps     []int
	genKey   string // set on generators
	children []int
	parent   int
	hidden   bool
}

type mlSpec struct {
	ckpt, restore, hazard, declared float64 // seconds, seconds, per second, seconds
}

type jobSpec struct {
	id, project, kind string
	gpu               bool
	expand            bool
	submit, deadline  float64 // seconds from the start of the week
	ord               order.Kind
	nPos              int
	rank              []int // dispatch rank of each position
	coarse            int   // ranks below this are information-bearing
	units             []unitSpec
	priority          int
	failProb          float64
	ml                *mlSpec
	priorKey          string // which history the estimator starts from
	heavyLo, heavyHi  int    // true heavy region (-1 if none)
	heavyMult         float64
}

const hour = 3600.0
const day = 24 * hour

func lognorm(r *rand.Rand, median, sigma float64) float64 {
	return median * math.Exp(sigma*r.NormFloat64())
}

func poisson(r *rand.Rand, mean float64) int {
	if mean <= 0 {
		return 0
	}
	if mean > 30 {
		return max(0, int(math.Round(mean+math.Sqrt(mean)*r.NormFloat64())))
	}
	l := math.Exp(-mean)
	k, p := 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func clampF(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }

// ciTarget is one test target of the monorepo.
type ciTarget struct {
	base, failRate float64
}

type workload struct {
	p       params
	jobs    []*jobSpec
	targets []ciTarget
}

var showNames = []string{"showA", "showB", "showC"}

// generate builds a week of work from a seed. Every random draw comes from
// r in a fixed order, so a seed always yields the same week.
func generate(p params, seed uint64) *workload {
	r := rand.New(rand.NewPCG(seed, 0x5eed))
	w := &workload{p: p, targets: makeTargets(p)}
	n := 0
	id := func(kind string) string {
		n++
		return fmt.Sprintf("%s-%04d", kind, n)
	}
	for d := 0; d < 5; d++ {
		for _, show := range showNames {
			k := poisson(r, p.DailiesPerEvening[show])
			for i := 0; i < k; i++ {
				at := float64(d)*day + clampF(p.DailiesSubmitMean+p.DailiesSubmitSD*r.NormFloat64(), p.DailiesSubmitLo, p.DailiesSubmitHi)*hour
				due := float64(d+1)*day + 8*hour
				if d == 4 {
					due = 7*day + 8*hour // Friday's dailies are watched Monday
				}
				js := renderJob(r, p, "dailies", show, r.Float64() < p.GPUShare, at, due)
				js.id = id(show + "-dailies")
				w.jobs = append(w.jobs, js)
			}
			for h := 9; h < 17; h++ {
				k := poisson(r, p.PreviewPerHour[show])
				for i := 0; i < k; i++ {
					at := float64(d)*day + (float64(h)+r.Float64())*hour
					js := renderJob(r, p, "preview", show, r.Float64() < p.GPUShare, at, at+p.PreviewTurn*hour)
					js.id = id(show + "-preview")
					w.jobs = append(w.jobs, js)
				}
			}
		}
	}
	for _, show := range showNames {
		for i := 0; i < p.DeliveriesPerShow[show]; i++ {
			at := 2*day + 10*hour + r.Float64()*34*hour // Wed 10:00 to Thu 20:00
			js := renderJob(r, p, "delivery", show, r.Float64() < p.DeliveryGPUShare, at, 4*day+18*hour)
			js.id = id(show + "-delivery")
			w.jobs = append(w.jobs, js)
		}
	}
	for d := 0; d < 5; d++ {
		f := 1.0
		if d == 3 {
			f = p.CIMergeDayFactor
		}
		for h := 8; h < 20; h++ {
			k := poisson(r, p.CIRatePerHour*f*ciShape(h))
			for i := 0; i < k; i++ {
				at := float64(d)*day + (float64(h)+r.Float64())*hour
				js := ciJob(r, p, w.targets, at)
				js.id = id("ci")
				w.jobs = append(w.jobs, js)
			}
		}
	}
	for d := 0; d < 7; d++ {
		mean, lo, span := p.MLWeekdayRuns, 9.0, 9.0
		if d >= 5 {
			mean, lo, span = p.MLWeekendRuns, 0, 24
		}
		k := poisson(r, mean)
		for i := 0; i < k; i++ {
			at := float64(d)*day + (lo+r.Float64()*span)*hour
			js := mlJob(r, p, at)
			js.id = id("ml")
			w.jobs = append(w.jobs, js)
		}
	}
	sort.SliceStable(w.jobs, func(i, j int) bool { return w.jobs[i].submit < w.jobs[j].submit })
	return w
}

// ciShape is the weekday arrival profile by hour, mean 1 over 08:00 to
// 20:00, peaking late morning and mid afternoon.
func ciShape(h int) float64 {
	shape := map[int]float64{8: 0.5, 9: 0.9, 10: 1.3, 11: 1.6, 12: 0.9, 13: 1.0, 14: 1.4, 15: 1.6, 16: 1.3, 17: 0.8, 18: 0.4, 19: 0.3}
	return shape[h]
}

func makeTargets(p params) []ciTarget {
	r := rand.New(rand.NewPCG(7, 7)) // the monorepo is the same in every run
	t := make([]ciTarget, p.CITargets)
	for i := range t {
		t[i] = ciTarget{
			base:     lognorm(r, p.CITargetMin*60, p.CITargetSigma),
			failRate: math.Pow(r.Float64(), 3) * 0.1,
		}
	}
	return t
}

func renderJob(r *rand.Rand, p params, kind, show string, gpu bool, at, due float64) *jobSpec {
	frames := int(clampF(lognorm(r, p.FramesMedian, p.FramesSigma), float64(p.FramesMin), float64(p.FramesMax)))
	cost := p.CPUFrameMin * 60
	if gpu {
		cost = p.GPUFrameMin * 60
	}
	ord := order.Binary
	expandShare := p.ExpandShare
	switch kind {
	case "preview":
		frames = max(12, frames/2) // every second frame
		cost *= p.PreviewCost
	case "delivery":
		frames = int(clampF(lognorm(r, p.DeliveryFrames, p.FramesSigma), float64(p.FramesMin), float64(p.FramesMax)))
		cost *= p.DeliveryCost
		ord = order.Sequential
		expandShare = p.DeliveryExpandShare
	}
	base := lognorm(r, cost, p.JobSigma)
	cycles := 0.5 + r.Float64()
	phase := r.Float64() * 2 * math.Pi
	js := &jobSpec{
		project: show, kind: kind, gpu: gpu, submit: at, deadline: due, ord: ord,
		nPos: frames, failProb: p.RenderFailProb, heavyLo: -1, heavyHi: -1, heavyMult: 1,
		priority: manualPriority[kind] + bonus(show),
	}
	js.priorKey = kind + "/" + map[bool]string{true: "gpu", false: "cpu"}[gpu]
	if r.Float64() < p.HeavyProb {
		l := int(math.Round(float64(frames) * (p.HeavyLenLo + r.Float64()*(p.HeavyLenHi-p.HeavyLenLo))))
		l = max(1, l)
		js.heavyLo = r.IntN(frames - l + 1)
		js.heavyHi = js.heavyLo + l - 1
		js.heavyMult = p.HeavyMults[r.IntN(len(p.HeavyMults))]
	}
	frameCost := func(pos int) float64 {
		c := base * math.Exp(p.TrendAmp*math.Sin(2*math.Pi*cycles*float64(pos)/float64(frames)+phase))
		if pos >= js.heavyLo && pos <= js.heavyHi {
			c *= js.heavyMult
		}
		return c * math.Exp(p.FrameSigma*r.NormFloat64())
	}
	js.expand = r.Float64() < expandShare
	volProb := p.VolumesProbLo + r.Float64()*(p.VolumesProbHi-p.VolumesProbLo)
	for pos := 0; pos < frames; pos++ {
		c := frameCost(pos)
		if !js.expand {
			js.units = append(js.units, unitSpec{key: "beauty", pos: pos, slots: 1, dur: c, parent: -1})
			continue
		}
		g := len(js.units)
		js.units = append(js.units, unitSpec{key: "usd", pos: pos, slots: 1, genKey: "usd-export",
			dur: lognorm(r, p.UsdExportSec, p.UsdExportSigma), parent: -1})
		sh := len(js.units)
		js.units = append(js.units, unitSpec{key: "env", pos: pos, sub: 0, slots: 1, hidden: true, parent: g,
			dur: c * p.EnvCost * math.Exp(p.PassSigma*r.NormFloat64())})
		deps := []int{sh}
		if r.Float64() < volProb {
			rf := len(js.units)
			js.units = append(js.units, unitSpec{key: "volumes", pos: pos, sub: 1, slots: 1, hidden: true, parent: g,
				dur: c * p.VolumesCost * math.Exp(p.PassSigma*r.NormFloat64())})
			deps = append(deps, rf)
		}
		bt := len(js.units)
		js.units = append(js.units, unitSpec{key: "beauty", pos: pos, sub: 2, slots: 1, hidden: true, parent: g, dur: c, deps: deps})
		for k := sh; k <= bt; k++ {
			js.units[g].children = append(js.units[g].children, k)
		}
	}
	rk, _ := order.Units(ord, frames, nil)
	js.rank = make([]int, frames)
	for i, pos := range rk {
		js.rank[pos] = i
	}
	if ord == order.Binary {
		js.coarse = order.CoarsePrefix(frames, 8)
	}
	return js
}

func bonus(project string) int {
	for _, pd := range farmProjects {
		if pd.name == project {
			return pd.bonus
		}
	}
	return 0
}

func ciJob(r *rand.Rand, p params, targets []ciTarget, at float64) *jobSpec {
	n := p.CIShardsBase + poisson(r, p.CIShardsPoisson)
	js := &jobSpec{
		project: "eng", kind: "ci", submit: at, deadline: at + p.CISLO*60, ord: order.FailRate,
		nPos: n, failProb: p.CIFailProb, heavyLo: -1, heavyHi: -1, priority: manualPriority["ci"],
		priorKey: "ci",
	}
	js.units = append(js.units, unitSpec{key: "build", pos: 0, slots: 1, genKey: "discover",
		dur: lognorm(r, p.CIBuildMin*60, p.CIBuildSigma), parent: -1})
	scores := make([]float64, n)
	picked := r.Perm(len(targets))[:n]
	for i, t := range picked {
		tg := targets[t]
		scores[i] = tg.failRate
		k := len(js.units)
		js.units = append(js.units, unitSpec{key: fmt.Sprintf("t%03d", t), pos: i, slots: 1, hidden: true, parent: 0,
			dur: tg.base * math.Exp(p.CIRunSigma*r.NormFloat64())})
		js.units[0].children = append(js.units[0].children, k)
	}
	rk := order.FailRateOrder(scores)
	js.rank = make([]int, n)
	for i, pos := range rk {
		js.rank[pos] = i
	}
	return js
}

func mlJob(r *rand.Rand, p params, at float64) *jobSpec {
	u := r.Float64()
	g := p.MLGPUs[len(p.MLGPUs)-1]
	for i, w := range p.MLGPUWeights {
		if u < w {
			g = p.MLGPUs[i]
			break
		}
		u -= w
	}
	dur := lognorm(r, p.MLHoursMedian*hour, p.MLHoursSigma)
	ck := (p.MLCkptMinLo + r.Float64()*(p.MLCkptMinHi-p.MLCkptMinLo)) * 60
	js := &jobSpec{
		project: "rnd", kind: "ml", gpu: true, submit: at, deadline: at + p.MLDeadlineHours*hour,
		ord: order.Sequential, nPos: 1, rank: []int{0}, heavyLo: -1, heavyHi: -1,
		priority: manualPriority["ml"], priorKey: "ml",
		ml: &mlSpec{ckpt: ck, restore: p.MLRestoreMin * 60, hazard: p.MLHazardPerHour / hour,
			declared: dur * math.Exp(p.MLDeclaredSigma*r.NormFloat64())},
	}
	js.units = []unitSpec{{key: "train", pos: 0, slots: g, dur: dur, parent: -1}}
	return js
}
