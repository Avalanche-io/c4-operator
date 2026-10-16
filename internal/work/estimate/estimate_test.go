package estimate

import (
	"math"
	"math/rand/v2"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestInterpolationExactOnLinear(t *testing.T) {
	e := New(DefaultConfig())
	for _, p := range []int{0, 10, 20, 30, 40} {
		e.Observe("beauty", p, 100+5*float64(p))
	}
	for p := 0; p <= 40; p++ {
		got := e.Predict("beauty", p)
		if !near(got.Mean, 100+5*float64(p), 1e-9) {
			t.Fatalf("pos %d: %v", p, got.Mean)
		}
	}
	// Leave-one-out residuals on linear data are zero, so the relative
	// spread shrinks toward zero from the prior as residuals accumulate.
	rms, n := e.tracks["beauty"].loo()
	if n != 3 || rms != 0 {
		t.Fatalf("loo %v %v", rms, n)
	}
	p := e.Predict("beauty", 15)
	if p.Source != Interpolated || p.Region.Lo != 10 {
		t.Fatalf("source %v region %v", p.Source, p.Region)
	}
	// mean 175 between 150 and 200: sd = hypot(|200-150|/2, rel*175),
	// rel = sqrt((3*0^2 + 3*0.25^2)/(3+3))
	rel := math.Sqrt(3 * 0.0625 / 6)
	if !near(p.SDCorr, math.Hypot(25, rel*175), 1e-9) {
		t.Fatalf("sd %v", p.SDCorr)
	}
}

func TestFallbackChain(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Default = Prior{Mean: 77, SD: 7}
	e := New(cfg)
	if p := e.Predict("beauty", 5); p.Source != ClassDefault || p.Mean != 77 {
		t.Fatalf("default: %+v", p)
	}
	e.SetPrior("beauty", Prior{Mean: 300, SD: 90})
	if p := e.Predict("beauty", 5); p.Source != History || p.Mean != 300 {
		t.Fatalf("history: %+v", p)
	}
	e.Observe("shadow", 1, 40)
	e.Observe("shadow", 2, 60)
	if p := e.Predict("beauty", 5); p.Source != JobMedian || p.Mean != 50 {
		t.Fatalf("job median: %+v", p)
	}
	e.Observe("beauty", 10, 200)
	e.Observe("beauty", 11, 220)
	e.Observe("beauty", 12, 900)
	e.Observe("beauty", 13, 210)
	p := e.Predict("beauty", 5)
	if p.Source != OneSided || p.Mean != 220 || p.Region.Lo != edgeLeft {
		t.Fatalf("one-sided left: %+v", p)
	}
	p = e.Predict("beauty", 50)
	if p.Source != OneSided || p.Mean != 900*0+210 && p.Mean != 220 {
		// nearest three on the right edge are 13, 12, 11: 210, 900, 220
		t.Fatalf("one-sided right: %+v", p)
	}
	if p.Mean != 220 || p.Region.Lo != edgeRight {
		t.Fatalf("one-sided right: %+v", p)
	}
	if p := e.Predict("beauty", 12); p.Source != Measured || p.Mean != 900 {
		t.Fatalf("measured: %+v", p)
	}
}

func TestSummarizeRegionsAndRunning(t *testing.T) {
	cfg := DefaultConfig()
	e := New(cfg)
	e.Observe("k", 0, 100)
	e.Observe("k", 10, 100)
	e.Observe("k", 20, 300)
	// Units 1..9 share region 0 and units 11..19 region 10.
	var q []Unit
	for p := 1; p < 20; p++ {
		if p == 10 {
			continue
		}
		q = append(q, Unit{Key: "k", Pos: p})
	}
	s := e.Summarize(q, nil)
	var w, c0, c10 float64
	for _, u := range q {
		p := e.Predict("k", u.Pos)
		w += p.Mean
		if u.Pos < 10 {
			c0 += p.SDCorr
		} else {
			c10 += p.SDCorr
		}
	}
	if !near(s.W, w, 1e-6) || !near(s.Sigma, math.Hypot(c0, c10), 1e-6) {
		t.Fatalf("summary %+v want W %v sigma %v", s, w, math.Hypot(c0, c10))
	}
	if !near(s.W90, s.W+1.28*s.Sigma, 1e-9) {
		t.Fatal("W90")
	}
	// A running unit before its estimate contributes the rest; past it, a
	// quarter of elapsed again.
	r := e.Summarize(nil, []RunningUnit{{Unit: Unit{Key: "k", Pos: 5, Slots: 2}, Elapsed: 40}})
	if !near(r.W, 2*60, 1e-9) {
		t.Fatalf("running remainder %v", r.W)
	}
	r = e.Summarize(nil, []RunningUnit{{Unit: Unit{Key: "k", Pos: 5}, Elapsed: 400}})
	if !near(r.W, 100, 1e-9) {
		t.Fatalf("overrun remainder %v", r.W)
	}
	// A checkpointed queued unit needs only what is left.
	r = e.Summarize([]Unit{{Key: "k", Pos: 5, Done: 30}}, nil)
	if !near(r.W, 70, 1e-9) {
		t.Fatalf("checkpointed %v", r.W)
	}
}

func TestNoLocality(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NoLocality = true
	e := New(cfg)
	for i, d := range []float64{60, 90, 75, 300, 80} {
		e.Observe("shard", i, d)
	}
	p := e.Predict("shard", 2)
	if p.Mean != 80 || p.SDInd <= 0 || p.Region.Lo != wholeKey {
		t.Fatalf("%+v", p)
	}
	if e.Heavy("shard", 5) != nil {
		t.Fatal("heavy regions on an axis with no locality")
	}
}

func TestGeneratorFanOutNarrows(t *testing.T) {
	cfg := DefaultConfig()
	e := New(cfg)
	e.SetGenPrior("rib", GenPrior{
		Children: Prior{Mean: 2, SD: 1},
		Work:     Prior{Mean: 1000, SD: 600},
		Longest:  Prior{Mean: 700, SD: 300},
	})
	g0 := e.Generator("rib")
	if g0.Mean != 1000 || g0.PerSD != 600 {
		t.Fatalf("prior %+v", g0)
	}
	s0 := e.Summarize([]Unit{{Key: "eval", Pos: 0, Gen: "rib"}, {Key: "eval", Pos: 1, Gen: "rib"}}, nil)
	if s0.Unevaluated != 2 {
		t.Fatal(s0)
	}
	// Generators in this job expand to 3 children and about 1800 s of work.
	e.Expanded("rib", 3)
	g1 := e.Generator("rib")
	if !near(g1.Mean, 1500, 1e-9) {
		t.Fatalf("after one expansion %+v", g1)
	}
	r := rand.New(rand.NewPCG(3, 4))
	prev := g1.MeanSD
	for i := 0; i < 20; i++ {
		e.Expanded("rib", 3)
		e.ExpansionDone("rib", 1800+r.NormFloat64()*50, 900)
		g := e.Generator("rib")
		if g.MeanSD > prev+1e-9 {
			t.Fatalf("band widened at %d: %v > %v", i, g.MeanSD, prev)
		}
		prev = g.MeanSD
	}
	g := e.Generator("rib")
	// The prior (scaled to 3 children: 1500 +- 900) is outweighed 20 to 3
	// by observations near 1800 +- 50.
	if math.Abs(g.Mean-1800) > 60 || g.PerSD > 0.4*900 || g.MeanSD > 0.1*900 {
		t.Fatalf("did not converge: %+v", g)
	}
}

func TestHeavyDetectedOnOneSample(t *testing.T) {
	e := New(DefaultConfig())
	// A 241-unit axis, heavy (5x) on 100..140. Binary order's first five
	// samples are 0, 240, 120, 60, 180: one of them lands in the region.
	cost := func(p int) float64 {
		if p >= 100 && p <= 140 {
			return 500
		}
		return 100
	}
	for _, p := range []int{0, 240, 60} {
		e.Observe("b", p, cost(p))
	}
	if h := e.Heavy("b", 241); len(h) != 0 {
		t.Fatalf("flagged before measuring: %+v", h)
	}
	e.Observe("b", 120, cost(120))
	h := e.Heavy("b", 241)
	if len(h) != 1 || h[0].Lo > 120 || h[0].Hi < 120 || h[0].Measured != 1 || h[0].Factor != 5 {
		t.Fatalf("heavy %+v", h)
	}
	// More samples tighten the flagged region toward the true one.
	for _, p := range []int{90, 100, 110, 130, 140, 150} {
		e.Observe("b", p, cost(p))
	}
	h = e.Heavy("b", 241)
	if len(h) != 1 || h[0].Lo < 91 || h[0].Hi > 149 || h[0].Lo > 100 || h[0].Hi < 140 {
		t.Fatalf("refined %+v", h)
	}
}

func TestCriticalPath(t *testing.T) {
	// Render frame: shadow (2) and reflection (3) before beauty (10);
	// CI: build (5) then shards (1, 4, 2).
	dur := []float64{2, 3, 10, 5, 1, 4, 2}
	deps := [][]int{nil, nil, {0, 1}, nil, {3}, {3}, {3}}
	got, err := CriticalPath(dur, deps)
	if err != nil || got != 13 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := CriticalPath([]float64{1, 1}, [][]int{{1}, {0}}); err == nil {
		t.Fatal("cycle accepted")
	}
	if _, err := CriticalPath([]float64{1}, [][]int{{5}}); err == nil {
		t.Fatal("bad edge accepted")
	}
}

func TestPlus(t *testing.T) {
	a := Summary{W: 10, Sigma: 3, TPath: 5, Queued: 1}
	b := Summary{W: 20, Sigma: 4, TPath: 2, Running: 2}
	c := a.Plus(b)
	if c.W != 30 || c.Sigma != 5 || c.TPath != 5 || c.W90 != 30+1.28*5 || c.Queued != 1 || c.Running != 2 {
		t.Fatalf("%+v", c)
	}
}
