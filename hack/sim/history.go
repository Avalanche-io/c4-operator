package main

import (
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// history stands in for what a farm learns from earlier weeks: per class
// key and per generator key, the distribution of durations and fan-out
// across many jobs of the same kind. It is built from the workload
// generator with a seed no experiment uses, so it describes the population
// a job comes from, never the job itself.
type history struct {
	unit map[string]estimate.Prior    // priorKey|key
	gen  map[string]estimate.GenPrior // priorKey|genKey
	def  map[string]estimate.Prior    // priorKey: any unit of the class
}

type acc struct{ n, sum, sq float64 }

func (a *acc) add(x float64) { a.n++; a.sum += x; a.sq += x * x }
func (a acc) prior() estimate.Prior {
	if a.n == 0 {
		return estimate.Prior{}
	}
	m := a.sum / a.n
	v := math.Max(0, a.sq/a.n-m*m)
	return estimate.Prior{Mean: m, SD: math.Sqrt(v)}
}

const historySeed = 0x415752 // not used by any experiment

func buildHistory(p params, targets []ciTarget) *history {
	h := &history{unit: map[string]estimate.Prior{}, gen: map[string]estimate.GenPrior{}, def: map[string]estimate.Prior{}}
	r := rand.New(rand.NewPCG(historySeed, 1))
	units := map[string]*acc{}
	defs := map[string]*acc{}
	type genAcc struct{ kids, work, longest acc }
	gens := map[string]*genAcc{}
	get := func(m map[string]*acc, k string) *acc {
		if m[k] == nil {
			m[k] = &acc{}
		}
		return m[k]
	}
	observe := func(js *jobSpec) {
		for _, u := range js.units {
			get(units, js.priorKey+"|"+u.key).add(u.dur)
			if u.genKey == "" {
				get(defs, js.priorKey).add(u.dur)
			}
			if u.genKey == "" {
				continue
			}
			g := gens[js.priorKey+"|"+u.genKey]
			if g == nil {
				g = &genAcc{}
				gens[js.priorKey+"|"+u.genKey] = g
			}
			work, longest := 0.0, 0.0
			for _, c := range u.children {
				work += js.units[c].dur * float64(js.units[c].slots)
				longest = math.Max(longest, js.units[c].dur)
			}
			g.kids.add(float64(len(u.children)))
			g.work.add(work)
			g.longest.add(longest)
		}
	}
	for _, kind := range []string{"dailies", "preview", "delivery"} {
		for _, gpu := range []bool{false, true} {
			for i := 0; i < 400; i++ {
				observe(renderJob(r, p, kind, "showA", gpu, 0, day))
			}
		}
	}
	for i := 0; i < 3000; i++ {
		observe(ciJob(r, p, targets, 0))
	}
	for k, a := range units {
		h.unit[k] = a.prior()
	}
	for k, a := range defs {
		h.def[k] = a.prior()
	}
	for k, g := range gens {
		h.gen[k] = estimate.GenPrior{Children: g.kids.prior(), Work: g.work.prior(), Longest: g.longest.prior()}
	}
	// A CI system knows each test target's history.
	s := p.CIRunSigma
	for i, t := range targets {
		m := t.base * math.Exp(s*s/2)
		h.unit[fmt.Sprintf("ci|t%03d", i)] = estimate.Prior{Mean: m, SD: m * math.Sqrt(math.Exp(s*s)-1)}
	}
	return h
}

// estimatorFor returns a fresh estimator for a job, seeded with history.
func (h *history) estimatorFor(js *jobSpec) *estimate.Estimator {
	cfg := estimate.DefaultConfig()
	if d, ok := h.def[js.priorKey]; ok && d.Mean > 0 {
		cfg.Default = d
	}
	switch js.kind {
	case "ci":
		cfg.NoLocality = true
	case "ml":
		cfg.Default = estimate.Prior{Mean: js.ml.declared, SD: 0.15 * js.ml.declared}
	}
	e := estimate.New(cfg)
	seen := map[string]bool{}
	for _, u := range js.units {
		if seen[u.key] {
			continue
		}
		seen[u.key] = true
		if js.kind == "ml" {
			e.SetPrior(u.key, cfg.Default)
			continue
		}
		if pr, ok := h.unit[js.priorKey+"|"+u.key]; ok {
			e.SetPrior(u.key, pr)
		}
		if u.genKey != "" {
			if gp, ok := h.gen[js.priorKey+"|"+u.genKey]; ok {
				e.SetGenPrior(u.genKey, gp)
			}
		}
	}
	return e
}
