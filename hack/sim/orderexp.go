package main

import (
	"math"
	"math/rand/v2"
	"runtime"
	"sort"
	"sync"

	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/order"
)

// The second experiment isolates the estimator. One shot is rendered on a
// fixed number of slots, in sequential or in binary order, and at 99
// evenly spaced moments of its render the estimate of remaining time is
// compared with the truth.
//
// Arms:
//   - binary: binary order, the estimate package (interpolation with
//     region-correlated errors, censored running units);
//   - sequential: sequential order, the same estimator;
//   - sequential-mean: sequential order, remaining work estimated as the
//     mean of completed frames times the frames left, the way render
//     managers usually compute an ETA.
//
// Estimated remaining time is max(W/P, t_path), the scheduler's own
// formula, with P the slot count. True remaining time is the arm's actual
// finish minus now.

type orderParams struct {
	Frames       []int
	Mults        []float64 // 1 is the control: no heavy region
	Slots        int
	BaseMin      float64 // median minutes per frame
	JobSigma     float64
	FrameSigma   float64
	TrendAmp     float64
	HeavyLenLo   float64
	HeavyLenHi   float64
	Trials       int
	SlackFrac    float64 // deadline = start + (1+SlackFrac) * true duration
	SettleFrac   float64 // settled when slack error <= SettleFrac * true slack from then on
	GridPoints   int
	Seed         uint64
	HeavyFactor  float64
	PriorSamples int
	CalTrials    int // history shots per (frames, multiplier) for the W calibration
}

func defaultOrderParams(trials int) orderParams {
	return orderParams{
		Frames: []int{120, 240, 480}, Mults: []float64{1, 2, 3, 5, 8}, Slots: 16,
		BaseMin: 20, JobSigma: 0.4, FrameSigma: 0.2, TrendAmp: 0.15,
		HeavyLenLo: 0.08, HeavyLenHi: 0.25, Trials: trials,
		SlackFrac: 0.5, SettleFrac: 0.1, GridPoints: 99, Seed: 0xB1A5, HeavyFactor: 3,
		PriorSamples: 20000, CalTrials: 200,
	}
}

var orderArms = []string{"binary", "sequential", "sequential-mean"}

type trialArm struct {
	N         int
	Mult      float64
	Trial     int
	Arm       string
	Makespan  float64   // seconds
	AbsErr    []float64 // per grid point, |est-true|/true remaining time
	SignedErr []float64
	Pts       []orderPoint // per grid point (estimator arms)
	FlagAt    float64      // fraction of makespan when the true heavy region is first flagged; -1 never
	FalseFlag bool         // a region not overlapping the true one was flagged at some grid point
	SettleAt  float64      // fraction of makespan
	InBand    float64      // share of grid moments with slack error within the band
}

// orderPoint is the W90 check at one grid point.
type orderPoint struct {
	Done    float64 // share of frames finished
	Stratum estimate.Stratum
	Score   float64 // calibration score against the true remaining work
	// Observable is false for a heavy shot until a frame of its heavy
	// region has finished: before that, nothing the estimator sees
	// differs between a 3x and an 8x region.
	Observable bool
	AsBuilt    bool // true remaining work <= W + Z*Sigma
	Covered    bool // true remaining work <= the calibrated W90 (as built without a calibration)
}

type orderResults struct {
	P    orderParams
	Rows []trialArm
}

type shot struct {
	dur              []float64
	heavyLo, heavyHi int
}

func makeShot(r *rand.Rand, p orderParams, n int, mult float64) shot {
	base := lognorm(r, p.BaseMin*60, p.JobSigma)
	cycles := 0.5 + r.Float64()
	phase := r.Float64() * 2 * math.Pi
	s := shot{dur: make([]float64, n), heavyLo: -1, heavyHi: -1}
	if mult > 1 {
		l := max(1, int(math.Round(float64(n)*(p.HeavyLenLo+r.Float64()*(p.HeavyLenHi-p.HeavyLenLo)))))
		s.heavyLo = r.IntN(n - l + 1)
		s.heavyHi = s.heavyLo + l - 1
	}
	for i := range s.dur {
		c := base * math.Exp(p.TrendAmp*math.Sin(2*math.Pi*cycles*float64(i)/float64(n)+phase))
		if i >= s.heavyLo && i <= s.heavyHi {
			c *= mult
		}
		s.dur[i] = c * math.Exp(p.FrameSigma*r.NormFloat64())
	}
	return s
}

// orderPrior is the population distribution of frame durations, the
// history an estimator starts from.
func orderPrior(p orderParams) estimate.Prior {
	r := rand.New(rand.NewPCG(historySeed, 2))
	var a acc
	for i := 0; i < p.PriorSamples/100; i++ {
		s := makeShot(r, p, 100, 1)
		for _, d := range s.dur {
			a.add(d)
		}
	}
	return a.prior()
}

type orderTask struct {
	n     int
	mult  float64
	trial int
	s     shot
}

func orderTasks(r *rand.Rand, p orderParams, trials int) []orderTask {
	var tasks []orderTask
	for _, n := range p.Frames {
		for _, m := range p.Mults {
			for t := 0; t < trials; t++ {
				tasks = append(tasks, orderTask{n, m, t, makeShot(r, p, n, m)})
			}
		}
	}
	return tasks
}

func runOrderExperiment(trials int) *orderResults {
	p := defaultOrderParams(trials)
	prior := orderPrior(p)
	// The calibration is built first, from shots drawn like the
	// experiment's but from the history seed: the finished jobs a farm
	// would have. The estimator arms are calibrated separately, as two
	// classes would be.
	hist := orderTasks(rand.New(rand.NewPCG(historySeed, 4)), p, p.CalTrials)
	cals := map[string]*estimate.Calibration{}
	for _, row := range runOrderTasks(p, prior, hist, []string{"binary", "sequential"}, nil) {
		c := cals[row.Arm]
		if c == nil {
			c = estimate.NewCalibration(0)
			cals[row.Arm] = c
		}
		for _, pt := range row.Pts {
			c.Add(pt.Stratum, pt.Score)
		}
	}
	tasks := orderTasks(rand.New(rand.NewPCG(p.Seed, 3)), p, p.Trials)
	return &orderResults{P: p, Rows: runOrderTasks(p, prior, tasks, orderArms, cals)}
}

func runOrderTasks(p orderParams, prior estimate.Prior, tasks []orderTask, arms []string, cals map[string]*estimate.Calibration) []trialArm {
	out := make([][]trialArm, len(tasks))
	var wg sync.WaitGroup
	ch := make(chan int)
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				t := tasks[i]
				for _, arm := range arms {
					ta := runArm(p, prior, t.s, arm, cals[arm])
					ta.N, ta.Mult, ta.Trial = t.n, t.mult, t.trial
					out[i] = append(out[i], ta)
				}
			}
		}()
	}
	for i := range tasks {
		ch <- i
	}
	close(ch)
	wg.Wait()
	var rows []trialArm
	for _, o := range out {
		rows = append(rows, o...)
	}
	return rows
}

// schedule list-schedules the shot on P slots in the given order and
// returns each frame's start and end.
func schedule(dur []float64, ord []int, slots int) (start, end []float64) {
	n := len(dur)
	start = make([]float64, n)
	end = make([]float64, n)
	free := make([]float64, slots) // time each slot frees up
	for _, f := range ord {
		k := 0
		for i := 1; i < slots; i++ {
			if free[i] < free[k] {
				k = i
			}
		}
		start[f] = free[k]
		end[f] = free[k] + dur[f]
		free[k] = end[f]
	}
	return start, end
}

func runArm(p orderParams, prior estimate.Prior, s shot, arm string, cal *estimate.Calibration) trialArm {
	n := len(s.dur)
	kind := order.Sequential
	if arm == "binary" {
		kind = order.Binary
	}
	ord, _ := order.Units(kind, n, nil)
	start, end := schedule(s.dur, ord, p.Slots)
	makespan := 0.0
	for _, e := range end {
		makespan = math.Max(makespan, e)
	}
	ta := trialArm{Arm: arm, Makespan: makespan, FlagAt: -1, SettleAt: -1}
	cfg := estimate.DefaultConfig()
	cfg.HeavyFactor = p.HeavyFactor
	cfg.Default = prior
	cfg.Calibration = cal
	// Frames by completion time, to feed the estimator incrementally.
	byEnd := make([]int, n)
	for i := range byEnd {
		byEnd[i] = i
	}
	sort.Slice(byEnd, func(a, b int) bool { return end[byEnd[a]] < end[byEnd[b]] })
	est := estimate.New(cfg)
	est.SetPrior("f", prior)
	fed := 0
	slack := p.SlackFrac * makespan
	settledFrom := -1
	for g := 1; g <= p.GridPoints; g++ {
		t := makespan * float64(g) / float64(p.GridPoints+1)
		for fed < n && end[byEnd[fed]] <= t {
			f := byEnd[fed]
			est.Observe("f", f, s.dur[f])
			fed++
		}
		var q []estimate.Unit
		var run []estimate.RunningUnit
		trueWork := 0.0
		sumDone, nDone := 0.0, 0
		heavyDone := 0
		for f := 0; f < n; f++ {
			switch {
			case end[f] <= t:
				sumDone += s.dur[f]
				nDone++
				if f >= s.heavyLo && f <= s.heavyHi {
					heavyDone++
				}
			case start[f] <= t:
				run = append(run, estimate.RunningUnit{Unit: estimate.Unit{Key: "f", Pos: f}, Elapsed: t - start[f]})
				trueWork += end[f] - t
			default:
				q = append(q, estimate.Unit{Key: "f", Pos: f})
				trueWork += s.dur[f]
			}
		}
		var remEst float64
		switch arm {
		case "sequential-mean":
			mean := prior.Mean
			if nDone > 0 {
				mean = sumDone / float64(nDone)
			}
			w, path := float64(len(q))*mean, 0.0
			if len(q) > 0 {
				path = mean
			}
			for _, r := range run {
				rem := mean - r.Elapsed
				if rem <= 0 {
					rem = 0.25 * r.Elapsed
				}
				w += rem
				path = math.Max(path, rem)
			}
			remEst = math.Max(w/float64(p.Slots), path)
		default:
			sum := est.Summarize(q, run)
			path := 0.0
			for _, u := range q {
				path = math.Max(path, est.Predict("f", u.Pos).Mean)
			}
			for _, r := range run {
				rem, _ := est.Remainder(est.Predict("f", r.Pos), 0, r.Elapsed)
				path = math.Max(path, rem)
			}
			remEst = math.Max(sum.W/float64(p.Slots), path)
			ta.Pts = append(ta.Pts, orderPoint{
				Done:       float64(nDone) / float64(n),
				Stratum:    sum.Stratum,
				Score:      estimate.Score(sum, trueWork),
				Observable: s.heavyLo < 0 || heavyDone > 0,
				AsBuilt:    trueWork <= sum.W+cfg.Z*sum.Sigma,
				Covered:    trueWork <= sum.W90,
			})
			for _, h := range est.Heavy("f", n) {
				overlaps := s.heavyLo >= 0 && h.Lo <= s.heavyHi && h.Hi >= s.heavyLo
				if overlaps && ta.FlagAt < 0 {
					ta.FlagAt = t / makespan
				}
				if !overlaps {
					ta.FalseFlag = true
				}
			}
		}
		trueRem := makespan - t
		e := (remEst - trueRem) / trueRem
		ta.AbsErr = append(ta.AbsErr, math.Abs(e))
		ta.SignedErr = append(ta.SignedErr, e)
		if math.Abs(remEst-trueRem) <= p.SettleFrac*slack {
			ta.InBand += 1 / float64(p.GridPoints)
			if settledFrom < 0 {
				settledFrom = g
			}
		} else {
			settledFrom = -1
		}
	}
	if settledFrom > 0 {
		ta.SettleAt = float64(settledFrom) / float64(p.GridPoints+1)
	}
	return ta
}
