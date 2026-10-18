package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type report struct {
	Grid    gridOpts
	Quick   bool
	Points  []Point
	Results []*pointResult
	Perf    []perfRow
	Took    time.Duration
	Workers int
	Spot    []*spotResult
}

func (r *report) runs() int {
	n := 0
	for _, pr := range r.Results {
		n += len(pr.Runs) + len(pr.Meta)
	}
	return n
}

// total counts violations of known bugs (known) or of everything else.
func (r *report) total(known bool) int {
	n := 0
	for _, pr := range r.Results {
		for _, f := range pr.Viol {
			if f.Known == known {
				n += f.N
			}
		}
	}
	for _, pr := range r.Spot {
		for _, f := range pr.Viol {
			if f.Known == known {
				n += f.N
			}
		}
	}
	return n
}

// failing lists a point's failing invariants, each marked known if every
// finding of it at the point has a known cause.
func (pr *pointResult) failing() map[string]bool {
	out := map[string]bool{}
	for _, f := range pr.Viol {
		k, seen := out[f.Inv]
		out[f.Inv] = f.Known && (k || !seen)
	}
	return out
}

func (r *report) command() string {
	c := "go run ./hack/stress"
	if r.Quick {
		c += " -quick"
	}
	if r.Grid.Seed != 1 {
		c += fmt.Sprintf(" -grid-seed %d", r.Grid.Seed)
	}
	return c
}

func (r *report) write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	lhs := 0
	for _, pt := range r.Points {
		if strings.HasPrefix(pt.Name, "g") {
			lhs++
		}
	}
	p("# Scheduler stress grid\n\n")
	p("`%s` ran %d grid points (%d Latin hypercube samples over %d dimensions, plus %d corners), %d runs in all, in %s with %d workers. ",
		r.command(), len(r.Results), lhs, sampledDims(), len(r.Points)-lhs, r.runs(), r.Took.Round(time.Second), r.Workers)
	p("Every run calls the production policy (`sched.Decide`) or a baseline through `sched.Policy` on a simulated farm; the invariants below are checked on every decision of `objective` and `objective+burst`.\n\n")
	p("%d violations of invariants without a known cause, %d of known bugs.\n\n", r.total(false), r.total(true))

	if len(r.Spot) > 0 {
		p("Spot capacity by provider (AWS, GCP, Azure), with and without the completion-aware spot policy, is in [spot.md](spot.md).\n\n")
	}
	r.writeInvariants(p)
	r.writeFindings(p)
	r.writeComparison(p)
	if err := r.writeHeatmaps(dir, p); err != nil {
		return err
	}
	r.writePrediction(p)
	r.writePerf(p)
	r.writePoints(p)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	return r.writeCSV(filepath.Join(dir, "violations.csv"))
}

type pf func(string, ...any)

func (r *report) byInv() map[string][]found {
	out := map[string][]found{}
	for _, pr := range r.Results {
		for _, f := range pr.Viol {
			out[f.Inv] = append(out[f.Inv], f)
		}
	}
	return out
}

func (r *report) writeInvariants(p pf) {
	by := r.byInv()
	p("## Invariants\n\n| Invariant | Holds when | Points violating | Status |\n|---|---|---|---|\n")
	for _, inv := range invariants {
		fs := by[inv.name]
		pts := map[string]bool{}
		for _, f := range fs {
			pts[f.Point] = true
		}
		allKnown := true
		for _, f := range fs {
			allKnown = allKnown && f.Known
		}
		status := "holds"
		_, known := knownBugs[inv.name]
		switch {
		case len(fs) > 0 && allKnown:
			status = "**fails: known cause**"
		case len(fs) > 0:
			status = "**fails**"
		case known:
			status = "holds (known bug not triggered)"
		}
		p("| `%s` | %s | %d of %d | %s |\n", inv.name, inv.what, len(pts), len(r.Results), status)
	}
	p("\nThe prediction scores (AtRisk against actual lateness) and Decide's cost by farm size are measured, not asserted; see below.\n\n")
}

func (r *report) writeFindings(p pf) {
	by := r.byInv()
	if len(by) == 0 {
		p("## Findings\n\nNo invariant failed.\n\n")
		return
	}
	p("## Findings\n\nEach failing invariant with up to five points, one example each, and the command that reruns the point (it prints every violation of the run). Add `-variant <name>` for metamorphic findings.\n\n")
	for _, inv := range invariants {
		fs := by[inv.name]
		if len(fs) == 0 {
			continue
		}
		p("### `%s`\n\n", inv.name)
		if why, ok := knownBugs[inv.name]; ok {
			p("Known bug: %s.\n\n", why)
		}
		if why, ok := causes[inv.name]; ok {
			p("%s\n\n", why)
		}
		seen := map[string]bool{}
		k := 0
		for _, f := range fs {
			if seen[f.Point] || k >= 5 {
				continue
			}
			seen[f.Point] = true
			k++
			p("- %s, %s, `%s`: %s %s\n  `%s`\n", f.Point, clock(f.At), f.Policy, f.Job, mdEscape(f.Detail), f.Repro)
		}
		if n := len(fs); n > k {
			p("- and %d more in `violations.csv`\n", n-k)
		}
		p("\n")
	}
}

// causes explains invariants that failed for a reason found in the code.
// Filled in from the investigation of each finding.
var causes = map[string]string{
	"losses": "The large losses come from `starvation-over-deadline` (see Findings): on small farms the starvation guard gives slots to jobs with a day or more of slack ahead of jobs due within hours, and a guard unit that has run past MaxLostWork cannot be preempted. The rest are quota points: the baselines ignore quotas, and where nominal quotas and borrowing limits are small the policy may not use capacity they use; at the extreme (a small farm whose nominal quotas and borrowing limits all round to 0) nothing may run at all, as Kueue's rules require.",
}

func mdEscape(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

type polStat struct {
	name                               string
	onTime                             []float64
	late, lateUSD, burst, util, preemp float64
	wins, losses, ties                 int
}

func (r *report) writeComparison(p pf) {
	ref := mainPolicy
	stats := map[string]*polStat{}
	for _, pol := range policies {
		stats[pol.name] = &polStat{name: pol.name}
	}
	for _, pr := range r.Results {
		base := pr.Runs[ref]
		for name, run := range pr.Runs {
			s := stats[name]
			if x := run.onTimePct(); !math.IsNaN(x) {
				s.onTime = append(s.onTime, x)
			}
			s.late += run.LateHours
			s.lateUSD += run.LateDollars
			s.burst += run.BurstCost
			s.util += run.Util
			s.preemp += float64(run.Preemptions)
			if base == nil || name == ref || base.DueJobs == 0 {
				continue
			}
			tol := max(1, int(math.Ceil(0.02*float64(base.DueJobs))))
			switch {
			case run.OnTime > base.OnTime+tol:
				s.losses++ // the baseline beat the policy
			case run.OnTime < base.OnTime-tol:
				s.wins++
			default:
				s.ties++
			}
		}
	}
	n := float64(len(r.Results))
	p("## Policy against baselines\n\n")
	p("The same workloads under each policy. `objective` is `sched.Decide` without burst requests, `objective+burst` with them; the baselines never burst. ")
	p("Wins and losses count points where `objective+burst` finished more jobs on time than the baseline, or fewer, by more than 2%% of the point's deadline jobs. ")
	p("Baselines ignore quotas, so on points with tight borrowing limits they use capacity the policy is not allowed to lend.\n\n")
	p("| Policy | Mean on time | Median on time | Late hours | Lateness cost | Burst spend | Owned use | Preemptions | objective+burst wins / ties / losses |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, pol := range policies {
		s := stats[pol.name]
		if len(s.onTime) == 0 && s.util == 0 {
			continue
		}
		mean, _ := meanSD(s.onTime)
		wl := "-"
		if pol.name != ref {
			wl = fmt.Sprintf("%d / %d / %d", s.wins, s.ties, s.losses)
		}
		p("| `%s` | %s | %s | %.0f | $%.0f | $%.0f | %.0f%% | %.0f | %s |\n", pol.name, pct(mean), pct(quantile(s.onTime, 0.5)),
			s.late, s.lateUSD, s.burst, 100*s.util/n, s.preemp, wl)
	}
	p("\n")
	r.writeRegions(p)
	r.writeLosses(p)
}

// writeLosses lists the points where objective+burst trails the best
// baseline the most, with the commands that rerun them.
func (r *report) writeLosses(p pf) {
	type loss struct {
		pr   *pointResult
		d    float64
		best string
	}
	var ls []loss
	for _, pr := range r.Results {
		d, ok := pr.deltaBest()
		if !ok || d > -10 {
			continue
		}
		best, bv := "", math.Inf(-1)
		for _, pol := range policies {
			if run := pr.Runs[pol.name]; run != nil && !pol.checked && run.onTimePct() > bv {
				best, bv = pol.name, run.onTimePct()
			}
		}
		ls = append(ls, loss{pr, d, best})
	}
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].d < ls[j].d })
	if len(ls) == 0 {
		p("`objective+burst` is within 10 points of the best baseline at every point.\n\n")
		return
	}
	p("Points where `objective+burst` trails the best baseline by more than 10 points:\n\n")
	for i, l := range ls {
		if i == 8 {
			p("- and %d more\n", len(ls)-i)
			break
		}
		run := l.pr.Runs[mainPolicy]
		p("- %s (%s): %s on time against %s for `%s`. `%s`\n", l.pr.Pt.Name, l.pr.Pt.label(), pct(run.onTimePct()), pct(run.onTimePct()-l.d), l.best, repro(l.pr.Pt, l.pr.Seed))
	}
	p("\n")
	if why, ok := causes["losses"]; ok {
		p("%s\n\n", why)
	}
}

// writeRegions says where the policy wins and loses against the best
// baseline, by load and deadline tightness.
func (r *report) writeRegions(p pf) {
	type acc struct{ d []float64 }
	cells := map[[2]int]*acc{}
	for _, pr := range r.Results {
		if !strings.HasPrefix(pr.Pt.Name, "g") {
			continue
		}
		d, ok := pr.deltaBest()
		if !ok {
			continue
		}
		k := [2]int{bin(loadEdges, pr.Pt.Load), bin(tightEdges, pr.Pt.Tightness)}
		if cells[k] == nil {
			cells[k] = &acc{}
		}
		cells[k].d = append(cells[k].d, d)
	}
	p("On-time points of `objective+burst` minus the best baseline at each point, mean by region (positive: the policy is ahead):\n\n")
	p("| Load \\ tightness |")
	for i := 0; i+1 < len(tightEdges); i++ {
		p(" %.2g-%.2g |", tightEdges[i], tightEdges[i+1])
	}
	p("\n|---|")
	for i := 0; i+1 < len(tightEdges); i++ {
		p("---|")
	}
	p("\n")
	for li := 0; li+1 < len(loadEdges); li++ {
		p("| %.2g-%.2g |", loadEdges[li], loadEdges[li+1])
		for ti := 0; ti+1 < len(tightEdges); ti++ {
			c := cells[[2]int{li, ti}]
			if c == nil {
				p(" |")
				continue
			}
			m, _ := meanSD(c.d)
			p(" %+.1f (%d) |", m, len(c.d))
		}
		p("\n")
	}
	p("\n")
}

// deltaBest is objective+burst's on-time percentage minus the best
// baseline's at the point.
func (pr *pointResult) deltaBest() (float64, bool) {
	base := pr.Runs[mainPolicy]
	if base == nil || base.DueJobs == 0 {
		return 0, false
	}
	best := math.Inf(-1)
	for _, pol := range policies {
		if pol.checked {
			continue
		}
		if run := pr.Runs[pol.name]; run != nil {
			best = math.Max(best, run.onTimePct())
		}
	}
	if math.IsInf(best, -1) {
		return 0, false
	}
	return base.onTimePct() - best, true
}

var (
	loadEdges  = []float64{0.3, 0.65, 1.0, 1.35, 1.7, 2.0}
	tightEdges = []float64{0.5, 1, 2, 4, 8, 12}
	nodeEdges  = []float64{1, 4, 16, 64, 256, 1024, 3001}
	burstEdges = []float64{0, 0.15, 0.4, 0.7, 1.01}
	budgEdges  = []float64{0, 0.08, 0.35, 0.65, 1.01}
)

func bin(edges []float64, x float64) int {
	for i := 0; i+1 < len(edges); i++ {
		if x < edges[i+1] {
			return i
		}
	}
	return len(edges) - 2
}

func (r *report) writeHeatmaps(dir string, p pf) error {
	nodes := nodeEdges
	if r.Quick {
		nodes = []float64{1, 4, 12, 24, 49}
	}
	type hm struct {
		file, title, xn, yn string
		xe, ye              []float64
		x, y                func(Point) float64
		val                 func(*pointResult) (float64, bool)
		kind                rampKind
		format              string
	}
	onTime := func(pr *pointResult) (float64, bool) {
		run := pr.Runs[mainPolicy]
		if run == nil || run.DueJobs == 0 {
			return 0, false
		}
		return run.onTimePct(), true
	}
	viol := func(pr *pointResult) (float64, bool) {
		n := 0
		for _, known := range pr.failing() {
			if !known {
				n++
			}
		}
		return float64(n), true
	}
	burst := func(pr *pointResult) (float64, bool) {
		run := pr.Runs[mainPolicy]
		if run == nil || pr.Pt.BurstFrac == 0 {
			return 0, false
		}
		return run.BurstCost / (math.Max(pr.Pt.Nodes, 16) * pr.Pt.Days), true
	}
	maps := []hm{
		{"ontime-load-tightness.svg", "On time, objective+burst (%)", "deadline tightness", "load", tightEdges, loadEdges,
			func(p Point) float64 { return p.Tightness }, func(p Point) float64 { return p.Load }, onTime, rampBlue, "%.0f"},
		{"ontime-nodes-load.svg", "On time, objective+burst (%)", "owned nodes", "load", nodes, loadEdges,
			func(p Point) float64 { return p.Nodes }, func(p Point) float64 { return p.Load }, onTime, rampBlue, "%.0f"},
		{"delta-load-tightness.svg", "On time, objective+burst minus best baseline (points)", "deadline tightness", "load", tightEdges, loadEdges,
			func(p Point) float64 { return p.Tightness }, func(p Point) float64 { return p.Load }, (*pointResult).deltaBest, rampDiverging, "%+.0f"},
		{"burst-budget.svg", "Burst spend per owned node-day ($)", "burst budget (share of a full day)", "burst pool size (share of owned)", budgEdges, burstEdges,
			func(p Point) float64 { return p.BurstBudget }, func(p Point) float64 { return p.BurstFrac }, burst, rampBlue, "%.2f"},
		{"violations-nodes-load.svg", "Invariants failing without a known cause (per point)", "owned nodes", "load", nodes, loadEdges,
			func(p Point) float64 { return p.Nodes }, func(p Point) float64 { return p.Load }, viol, rampOrange, "%.1f"},
		{"violations-load-tightness.svg", "Invariants failing without a known cause (per point)", "deadline tightness", "load", tightEdges, loadEdges,
			func(p Point) float64 { return p.Tightness }, func(p Point) float64 { return p.Load }, viol, rampOrange, "%.1f"},
	}
	p("## Heatmaps\n\nMeans over the Latin hypercube points in each cell (corners excluded); the number of points is under each value. Hover a cell for its range.\n\n")
	for _, m := range maps {
		g := newGrid(len(m.xe)-1, len(m.ye)-1)
		for _, pr := range r.Results {
			if !strings.HasPrefix(pr.Pt.Name, "g") {
				continue
			}
			v, ok := m.val(pr)
			if !ok || math.IsNaN(v) {
				continue
			}
			g.add(bin(m.xe, m.x(pr.Pt)), bin(m.ye, m.y(pr.Pt)), v)
		}
		svg := heatmapSVG(m.title, m.xn, m.yn, m.xe, m.ye, g, m.kind, m.format)
		if err := os.WriteFile(filepath.Join(dir, m.file), []byte(svg), 0o644); err != nil {
			return err
		}
		p("![%s](%s)\n\n", m.title, m.file)
	}
	return nil
}

func (r *report) writePrediction(p pf) {
	var all predStats
	for _, pr := range r.Results {
		if run := pr.Runs[mainPolicy]; run != nil {
			all.merge(run.Pred)
		}
	}
	p("## Feasibility predictions\n\n")
	p("Each deadline job's `AtRisk` under `objective+burst`, at its first decision with work ready and again halfway to its deadline, against whether it finished late.\n\n")
	p("| When | At risk and late | At risk, on time | On track, late | On track, on time | Precision | Recall |\n|---|---|---|---|---|---|---|\n")
	for k, name := range []string{"first decision", "halfway"} {
		p("| %s | %d | %d | %d | %d | %s | %s |\n", name, all.TP[k], all.FP[k], all.FN[k], all.TN[k], pct(100*all.precision(k)), pct(100*all.recall(k)))
	}
	p("\n")
}

func (r *report) writePerf(p pf) {
	if len(r.Perf) == 0 {
		return
	}
	p("## Decide's cost by farm size\n\n")
	p("One `sched.Decide` call on the busiest snapshot of a short overloaded run (load 1.3, eight projects) at each size, averaged over repeated calls.\n\n")
	p("| Owned nodes | Jobs | Running units | Runnable units | Time per call | Allocated per call | Allocations per call |\n|---|---|---|---|---|---|---|\n")
	for _, row := range r.Perf {
		p("| %d | %d | %d | %d | %s | %s | %.0f |\n", row.Nodes, row.Jobs, row.Running, row.Runnable, row.Mean.Round(time.Microsecond), bytesH(row.Bytes), row.Allocs)
	}
	p("\n")
	worst := time.Duration(0)
	for _, pr := range r.Results {
		for _, run := range pr.Runs {
			worst = max(worst, run.DecideMax)
		}
	}
	p("The slowest single decision in the grid's runs took %s.\n\n", worst.Round(time.Millisecond))
}

func bytesH(b float64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KiB", b/(1<<10))
	}
	return fmt.Sprintf("%.0f B", b)
}

func (r *report) writePoints(p pf) {
	p("## Points\n\nOn time per policy (jobs on time of jobs with a deadline), and failing invariants.\n\n| Point | Farm and load |")
	for _, pol := range policies {
		p(" %s |", pol.name)
	}
	p(" Failing |\n|---|---|")
	for range policies {
		p("---|")
	}
	p("---|\n")
	res := append([]*pointResult(nil), r.Results...)
	sort.SliceStable(res, func(i, j int) bool { return res[i].Pt.Name < res[j].Pt.Name })
	for _, pr := range res {
		p("| %s | %s |", pr.Pt.Name, pr.Pt.label())
		for _, pol := range policies {
			run := pr.Runs[pol.name]
			if run == nil {
				p(" |")
				continue
			}
			p(" %d/%d |", run.OnTime, run.DueJobs)
		}
		var inv []string
		fail := pr.failing()
		for _, k := range sortedBool(fail) {
			if fail[k] {
				k += "*"
			}
			inv = append(inv, k)
		}
		p(" %s |\n", strings.Join(inv, ", "))
	}
	p("\n`*` marks an invariant whose findings at the point all have a known cause. Every point's full definition is in `violations.csv` (for points with findings) or printed by `-point <name>` for corners.\n")
}

func (r *report) writeCSV(path string) error {
	var b strings.Builder
	b.WriteString("point,seed,policy,invariant,known,at_seconds,job,detail,repro\n")
	for _, pr := range r.Results {
		for _, f := range pr.Viol {
			fmt.Fprintf(&b, "%s,%d,%s,%s,%v,%.0f,%s,%q,%q\n", f.Point, pr.Seed, f.Policy, f.Inv, f.Known, f.At, f.Job, f.Detail, f.Repro)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func meanSD(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return math.NaN(), math.NaN()
	}
	m := 0.0
	for _, x := range xs {
		m += x
	}
	m /= float64(len(xs))
	v := 0.0
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	if len(xs) < 2 {
		return m, 0
	}
	return m, math.Sqrt(v / float64(len(xs)-1))
}

func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func sampledDims() int {
	n := 0
	for _, d := range dims {
		if d.lo != d.hi {
			n++
		}
	}
	return n
}

func sortedBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
