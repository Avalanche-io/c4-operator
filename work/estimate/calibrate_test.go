package estimate

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestConformalQuantile(t *testing.T) {
	c := NewCalibration(0)
	c.Min = 1
	s := Stratum{Sampled: 2}
	for i := 1; i <= 99; i++ {
		c.Add(s, float64(i))
	}
	// The ceil((n+1)q)-th smallest of 1..99: ceil(100*0.9) = 90.
	if q, ok := c.Quantile(s, 0.9); !ok || q != 90 {
		t.Fatalf("q90 %v %v", q, ok)
	}
	// A rank past n is the largest score.
	if q, _ := c.Quantile(s, 0.999); q != 99 {
		t.Fatalf("q99.9 %v", q)
	}
}

func TestConformalCoverageOnExchangeableScores(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	c := NewCalibration(0)
	s := Stratum{}
	for range 2000 {
		c.Add(s, r.ExpFloat64())
	}
	q, _ := c.Quantile(s, 0.9)
	hit := 0
	for range 20000 {
		if r.ExpFloat64() <= q {
			hit++
		}
	}
	if got := float64(hit) / 20000; got < 0.88 || got > 0.92 {
		t.Fatalf("coverage %.3f", got)
	}
}

func TestCalibrationFallsBackToCoarserStrata(t *testing.T) {
	c := NewCalibration(0)
	c.Min = 3
	fine := Stratum{Sampled: 1, Measured: 2, Open: 1}
	c.Add(fine, 5)
	c.Add(Stratum{Sampled: 1, Measured: 2, Open: 0}, 1)
	c.Add(Stratum{Sampled: 1, Measured: 2, Open: 0}, 1)
	// fine has 1 score; (1, 2, any) has 3: 1, 1, 5.
	if q, ok := c.Quantile(fine, 0.5); !ok || q != 1 {
		t.Fatalf("coarser: %v %v", q, ok)
	}
	if c.Count(fine) != 1 {
		t.Fatalf("count %d", c.Count(fine))
	}
	// Nothing at all for another share-finished band.
	if _, ok := c.Quantile(Stratum{Sampled: 4}, 0.5); ok {
		t.Fatal("expected no quantile")
	}
}

func TestCalibrationRingKeepsTheMostRecent(t *testing.T) {
	c := NewCalibration(3)
	c.Min = 1
	s := Stratum{}
	for _, x := range []float64{100, 100, 100, 1, 2, 3} {
		c.Add(s, x)
	}
	if q, _ := c.Quantile(s, 0.99); q != 3 {
		t.Fatalf("q %v", q)
	}
}

func TestSummarizeUsesCalibration(t *testing.T) {
	cfg := DefaultConfig()
	plain := New(cfg)
	for _, p := range []int{0, 50, 100} {
		plain.Observe("b", p, 100)
	}
	queued := []Unit{{Key: "b", Pos: 25}, {Key: "b", Pos: 75}}
	as := plain.Summarize(queued, nil)
	if as.Calibrated || as.W90 != as.W+cfg.Z*as.Sigma || as.W50 != as.W {
		t.Fatalf("as built %+v", as)
	}
	cal := NewCalibration(0)
	cal.Min = 1
	cal.Add(as.Stratum, 2)
	cfg.Calibration = cal
	e := New(cfg)
	for _, p := range []int{0, 50, 100} {
		e.Observe("b", p, 100)
	}
	s := e.Summarize(queued, nil)
	want := s.W + 2*Scale(s)
	if !s.Calibrated || math.Abs(s.W90-want) > 1e-9 || s.W50 != s.W90 || s.W95 != s.W90 {
		t.Fatalf("calibrated %+v want W90 %v", s, want)
	}
	// Scores are what the job turned out to need, in units of Scale.
	if got := Score(s, want); math.Abs(got-2) > 1e-9 {
		t.Fatalf("score %v", got)
	}
}

func TestStratumSeesHeavyUnitsAndCensoredOnes(t *testing.T) {
	e := New(DefaultConfig())
	queued := func(n int) []Unit {
		var q []Unit
		for i := range n {
			q = append(q, Unit{Key: "b", Pos: 1000 + i})
		}
		return q
	}
	for p := range 10 {
		e.Observe("b", p, 100+float64(p))
	}
	// 10 finished of 40: the 1/4 to 1/2 band; nothing heavy.
	if st := e.stratum(queued(30), nil); st != (Stratum{Sampled: 2}) {
		t.Fatalf("plain %+v", st)
	}
	// A running unit at 4x the base is censored evidence of a heavy unit.
	run := []RunningUnit{{Unit: Unit{Key: "b", Pos: 500}, Elapsed: 420}}
	if st := e.stratum(queued(29), run); st.Measured != 0 || st.Open != 2 {
		t.Fatalf("open %+v", st)
	}
	// Once two heavy units at about 5x finish, the multiple is measured,
	// and a unit running at 4x has not outlasted it.
	e.Observe("b", 20, 500)
	e.Observe("b", 21, 520)
	if st := e.stratum(queued(27), run); st.Measured != 1+bandOf(5.1, measuredEdges) || st.Open != 0 {
		t.Fatalf("measured %+v", st)
	}
	// One slow plain unit is not a mild region; several are.
	m := New(DefaultConfig())
	for p := range 40 {
		m.Observe("b", p, 100)
	}
	m.Observe("b", 40, 210)
	if st := m.stratum(nil, nil); st.Measured != 0 {
		t.Fatalf("one slow unit %+v", st)
	}
	for p := 41; p < 44; p++ {
		m.Observe("b", p, 200)
	}
	if st := m.stratum(nil, nil); st.Measured != 1 {
		t.Fatalf("mild %+v", st)
	}
}

func TestPlusAddsCalibratedPercentiles(t *testing.T) {
	a := Summary{W: 10, Sigma: 3, W50: 10, W80: 12, W90: 14, W95: 16, Calibrated: true}
	b := Summary{W: 20, Sigma: 4, W50: 19, W80: 25, W90: 30, W95: 33, Calibrated: true}
	if c := a.Plus(b); !c.Calibrated || c.W90 != 44 || c.W50 != 29 {
		t.Fatalf("%+v", c)
	}
	b.Calibrated = false
	if c := a.Plus(b); c.Calibrated || c.W90 != 30+1.28*5 {
		t.Fatalf("%+v", c)
	}
}
