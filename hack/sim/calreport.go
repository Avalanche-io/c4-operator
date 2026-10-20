package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// W90 calibration: coverage of the true remaining work by W90, as built
// (W + Z*Sigma) and calibrated (estimate.Calibration, built from history
// shots of the same arm), broken down by the true heavy multiple, by the
// band of the share of frames finished, and by what the estimator sees.

var bandNames = []string{"under 1/16", "1/16 to 1/4", "1/4 to 1/2", "1/2 to 3/4", "3/4 on"}

// The pass band for a calibrated 90th percentile.
const covLo, covHi = 0.87, 0.93

type tally struct {
	n, asBuilt, cal int
	trials          map[int]*[2]int // per trial: moments, calibrated covered
}

func (t *tally) add(p orderPoint, trial int) {
	if t.trials == nil {
		t.trials = map[int]*[2]int{}
	}
	c := t.trials[trial]
	if c == nil {
		c = &[2]int{}
		t.trials[trial] = c
	}
	c[0]++
	if p.Covered {
		c[1]++
	}
	t.n++
	if p.AsBuilt {
		t.asBuilt++
	}
	if p.Covered {
		t.cal++
	}
}

func (t tally) share(cal bool) float64 {
	if t.n == 0 {
		return 0
	}
	if cal {
		return float64(t.cal) / float64(t.n)
	}
	return float64(t.asBuilt) / float64(t.n)
}

func (t tally) pass() bool { s := t.share(true); return t.n > 0 && s >= covLo && s <= covHi }

// se is the standard error of the calibrated coverage with moments
// clustered by trial (moments of one render are strongly correlated).
func (t tally) se() float64 {
	p := t.share(true)
	var ss float64
	for _, c := range t.trials {
		d := float64(c[1]) - p*float64(c[0])
		ss += d * d
	}
	return math.Sqrt(ss) / float64(max(1, t.n))
}

// consistent reports whether the calibrated coverage is in the pass band
// or within two standard errors of 90%.
func (t tally) consistent() bool { return t.pass() || math.Abs(t.share(true)-0.9) <= 2*t.se() }

func pct(x float64) string { return fmt.Sprintf("%.0f%%", 100*x) }

// coverage tallies the arm's points by (multiplier, band); band -1 is the
// whole render. keep filters points.
func coverage(o *orderResults, arm string, keep func(orderPoint) bool) map[[2]float64]*tally {
	out := map[[2]float64]*tally{}
	for i, r := range o.Rows {
		if r.Arm != arm {
			continue
		}
		for _, p := range r.Pts {
			if !keep(p) {
				continue
			}
			for _, b := range []float64{-1, float64(p.Stratum.Sampled)} {
				k := [2]float64{r.Mult, b}
				if out[k] == nil {
					out[k] = &tally{}
				}
				out[k].add(p, i)
			}
		}
	}
	return out
}

func all(orderPoint) bool { return true }

func stratumName(s estimate.Stratum) string {
	m := [...]string{"none", "mild", "3-3.75x", "3.75-4.7x", "4.7-5.9x", "5.9-7.3x", "7.3-9.2x", "9.2x+"}[s.Measured]
	op := [...]string{"none", "1.5-3x", "3-6x", "6x+"}[s.Open]
	return fmt.Sprintf("%s / measured %s / running past it %s", bandNames[s.Sampled], m, op)
}

// acceptanceMults are the strata of the acceptance test: plain and the
// heavy multiples of 3x and more.
var acceptanceMults = []float64{1, 3, 5, 8}

func writeCalibrationSection(b *strings.Builder, o *orderResults) {
	p := o.P
	fmt.Fprintf(b, "### Calibration of W90\n\nShare of moments at which the true remaining work was at or below W90. A calibrated 90th percentile covers about 90%%; the pass band is %.0f to %.0f%%. As built, W90 is W + %.2f Sigma. Calibrated, it is W plus the conformal 90%% quantile of scores from finished shots in the same stratum (`estimate.Calibration`): %d history shots per frame count and multiplier, drawn like the experiment's from a separate seed, with the same mix of multipliers. Strata are what the estimator sees: the share of frames finished, the heavy multiple measured on finished frames, and a running frame that has outlasted it.\n\n",
		100*covLo, 100*covHi, estimate.DefaultConfig().Z, p.CalTrials)

	rows := [][]string{}
	tot := map[string]map[[2]float64]*tally{}
	for _, arm := range []string{"binary", "sequential"} {
		tot[arm] = coverage(o, arm, all)
	}
	for _, m := range p.Mults {
		k := [2]float64{m, -1}
		rows = append(rows, []string{fmt.Sprintf("%.0fx", m),
			pct(tot["binary"][k].share(false)), pct(tot["binary"][k].share(true)),
			pct(tot["sequential"][k].share(false)), pct(tot["sequential"][k].share(true))})
	}
	table(b, []string{"multiplier", "binary, as built", "binary, calibrated", "sequential, as built", "sequential, calibrated"}, rows)

	obs := coverage(o, "binary", func(p orderPoint) bool { return p.Observable })
	amb := coverage(o, "binary", func(p orderPoint) bool { return !p.Observable })
	bandTable := func(c map[[2]float64]*tally, mults []float64) [][]string {
		var rows [][]string
		for _, m := range mults {
			r := []string{fmt.Sprintf("%.0fx", m)}
			for bi := -1; bi < len(bandNames); bi++ {
				t := c[[2]float64{m, float64(bi)}]
				if t == nil || t.n == 0 {
					r = append(r, "")
					continue
				}
				r = append(r, fmt.Sprintf("%s (%s) n=%d", pct(t.share(true)), pct(t.share(false)), t.n))
			}
			rows = append(rows, r)
		}
		return rows
	}
	head := append([]string{"multiplier", "whole render"}, bandNames...)
	b.WriteString("Binary order, by the share of frames finished: calibrated (as built), and the number of moments.\n\n")
	table(b, head, bandTable(tot["binary"], p.Mults))
	b.WriteString("Binary order, moments at which the multiple is observable: every moment of a plain shot, and the moments of a heavy shot after a frame of its heavy region has finished.\n\n")
	table(b, head, bandTable(obs, p.Mults))
	b.WriteString("Binary order, the other moments: a heavy shot whose heavy frames are all still running or queued. Until one finishes, a 3x, a 5x and an 8x region produce the same observations (the same plain frames finish at the same times, and the heavy frames have run equally long), while the 8x shot has roughly twice the remaining work of the 3x one. No W90 computed from those observations can cover both at 87 to 93%; calibration covers 90% of the mix.\n\n")
	table(b, head, bandTable(amb, p.Mults))

	// What the estimator sees: one row per stratum with enough moments.
	byS := map[estimate.Stratum]*tally{}
	var order []estimate.Stratum
	for i, r := range o.Rows {
		if r.Arm != "binary" {
			continue
		}
		for _, pt := range r.Pts {
			if byS[pt.Stratum] == nil {
				byS[pt.Stratum] = &tally{}
				order = append(order, pt.Stratum)
			}
			byS[pt.Stratum].add(pt, i)
		}
	}
	sortStrata(order)
	rows = nil
	small := 0
	for _, s := range order {
		t := byS[s]
		if t.n < minStratumMoments {
			small += t.n
			continue
		}
		rows = append(rows, []string{stratumName(s), strconv.Itoa(t.n), pct(t.share(false)), pct(t.share(true))})
	}
	fmt.Fprintf(b, "Binary order, by stratum (what the estimator sees), strata with at least %d moments; %d moments fall in smaller strata.\n\n", minStratumMoments, small)
	table(b, []string{"share finished / heavy multiple measured / running frame past it", "moments", "as built", "calibrated"}, rows)

	// The acceptance test for calibration (DESIGN.md 10.8, Calibration):
	// W90 covers 87 to 93% in each stratum, as first written and as
	// revised for what is observable.
	cell := func(name string, t *tally) string {
		return fmt.Sprintf("%s %.1f%% (se %.1f)", name, 100*t.share(true), 100*t.se())
	}
	var failWritten, failObs16, failObs4, failStrata []string
	for _, m := range acceptanceMults {
		for bi := range bandNames {
			k := [2]float64{m, float64(bi)}
			name := fmt.Sprintf("%.0fx %s", m, bandNames[bi])
			if t := tot["binary"][k]; t != nil && !t.pass() {
				failWritten = append(failWritten, cell(name, t))
			}
			t := obs[k]
			if bi == 0 || t == nil || t.n < minStratumMoments || t.pass() {
				continue
			}
			failObs16 = append(failObs16, cell(name, t))
			if bi >= 2 {
				failObs4 = append(failObs4, cell(name, t))
			}
		}
	}
	judged, consistent := 0, 0
	for _, s := range order {
		t := byS[s]
		if t.n < minStratumMoments {
			continue
		}
		judged++
		if t.consistent() {
			consistent++
			continue
		}
		failStrata = append(failStrata, cell(stratumName(s), t))
	}
	verdict := func(f []string) string {
		if len(f) == 0 {
			return "pass"
		}
		return "fail: " + strings.Join(f, "; ")
	}
	b.WriteString("Acceptance (binary, calibrated). Standard errors cluster the moments by trial.\n\n")
	fmt.Fprintf(b, "- Strata the estimator can see, with at least %d moments, covered at 87 to 93%% or within two standard errors of 90%%: %d of %d (two standard errors leave about 1 in 20 out by chance). %s.\n", minStratumMoments, consistent, judged, verdict(failStrata))
	fmt.Fprintf(b, "- Plain, 3x, 5x and 8x at 87 to 93%%, observable moments, every band from 1/4 finished on: %s.\n", verdict(failObs4))
	fmt.Fprintf(b, "- The same from 1/16 finished on: %s.\n", verdict(failObs16))
	fmt.Fprintf(b, "- As first written, every band and every moment, including those at which the multiple cannot be observed: %s.\n\n", verdict(failWritten))
}

// minStratumMoments is the fewest moments a cell needs to be judged: with
// 40 trials per cell and strongly correlated moments within a trial,
// smaller cells swing by more than the pass band on sampling alone.
const minStratumMoments = 300

func sortStrata(s []estimate.Stratum) {
	less := func(a, b estimate.Stratum) bool {
		if a.Sampled != b.Sampled {
			return a.Sampled < b.Sampled
		}
		if a.Measured != b.Measured {
			return a.Measured < b.Measured
		}
		return a.Open < b.Open
	}
	for i := 1; i < len(s); i++ {
		for k := i; k > 0 && less(s[k], s[k-1]); k-- {
			s[k], s[k-1] = s[k-1], s[k]
		}
	}
}

// coverageRows is order_coverage.csv: per arm, multiplier, band and
// observability, the moments and how many each W90 covered.
func coverageRows(o *orderResults) [][]string {
	type key struct {
		arm  string
		mult float64
		band int8
		obs  bool
	}
	m := map[key]*tally{}
	var keys []key
	for i, r := range o.Rows {
		for _, p := range r.Pts {
			k := key{r.Arm, r.Mult, p.Stratum.Sampled, p.Observable}
			if m[k] == nil {
				m[k] = &tally{}
				keys = append(keys, k)
			}
			m[k].add(p, i)
		}
	}
	var rows [][]string
	for _, k := range keys {
		t := m[k]
		rows = append(rows, []string{k.arm, strconv.FormatFloat(k.mult, 'f', -1, 64), strconv.Itoa(int(k.band)), strconv.FormatBool(k.obs), strconv.Itoa(t.n), strconv.Itoa(t.asBuilt), strconv.Itoa(t.cal)})
	}
	return rows
}
