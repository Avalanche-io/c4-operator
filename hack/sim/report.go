package main

import (
	_ "embed"
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/work/sched"
)

//go:embed discussion.md
var discussion string

type agg struct {
	missed, jobs, late, missPct, p50, p95, over, ucpu, ugpu, uci, useG, lostG, lostC, cost, burstH, pre, mlUse, mlLost, approvals []float64
}

func aggregate(rs []*runResult, p params) agg {
	var a agg
	for _, r := range rs {
		t := r.renderTotals()
		a.missed = append(a.missed, float64(t.Missed))
		a.jobs = append(a.jobs, float64(t.Jobs))
		a.missPct = append(a.missPct, 100*float64(t.Missed)/float64(max(1, t.Jobs)))
		a.late = append(a.late, t.LateHours)
		a.p50 = append(a.p50, quantile(r.CITTG, 0.5))
		a.p95 = append(a.p95, quantile(r.CITTG, 0.95))
		n := 0
		for _, x := range r.CITTG {
			if x > p.CISLO {
				n++
			}
		}
		a.over = append(a.over, 100*float64(n)/float64(max(1, len(r.CITTG))))
		a.ucpu = append(a.ucpu, 100*r.Util["cpu"])
		a.ugpu = append(a.ugpu, 100*r.Util["gpu"])
		a.uci = append(a.uci, 100*r.Util["ci"])
		a.useG = append(a.useG, r.UsefulGPUh)
		a.lostG = append(a.lostG, r.LostGPUh)
		a.lostC = append(a.lostC, r.LostCPUh)
		a.cost = append(a.cost, r.BurstCost)
		a.burstH = append(a.burstH, r.BurstSlotHours)
		a.pre = append(a.pre, float64(r.preemptions))
		a.mlUse = append(a.mlUse, r.MLUsefulh)
		a.mlLost = append(a.mlLost, r.MLLosth)
		a.approvals = append(a.approvals, float64(len(r.approvalJobs)))
	}
	return a
}

// ms formats mean and sample standard deviation across seeds.
func ms(xs []float64, prec int) string {
	m, s := meanSD(xs)
	return fmt.Sprintf("%.*f ± %.*f", prec, m, prec, s)
}

func table(b *strings.Builder, head []string, rows [][]string) {
	w := make([]int, len(head))
	for i, h := range head {
		w[i] = len([]rune(h))
	}
	for _, r := range rows {
		for i, c := range r {
			w[i] = max(w[i], len([]rune(c)))
		}
	}
	line := func(cells []string) {
		b.WriteString("|")
		for i, c := range cells {
			fmt.Fprintf(b, " %s%s |", c, strings.Repeat(" ", w[i]-len([]rune(c))))
		}
		b.WriteString("\n")
	}
	line(head)
	sep := make([]string, len(head))
	for i := range sep {
		sep[i] = strings.Repeat("-", w[i])
	}
	line(sep)
	for _, r := range rows {
		line(r)
	}
	b.WriteString("\n")
}

func writeReport(dir string, p params, seeds []uint64, pols []policySpec, res map[string][]*runResult, tight []policySpec, tightRes map[string][]*runResult, ord *orderResults, took time.Duration) error {
	var b strings.Builder
	seedStr := make([]string, len(seeds))
	for i, s := range seeds {
		seedStr[i] = strconv.FormatUint(s, 10)
	}
	b.WriteString("# Work scheduling simulation\n\n")
	fmt.Fprintf(&b, "Reproduce with `go run ./hack/sim` from the repository root (seeds %s; %d binary-order trials per cell). ", strings.Join(seedStr, ", "), orderTrials(ord))
	fmt.Fprintf(&b, "This run took %s on %d CPUs with %s. Every number below is produced by that command; nothing is edited by hand except the discussion at the end, which is kept in `hack/sim/discussion.md`.\n\n", took.Round(time.Second), runtime.GOMAXPROCS(0), runtime.Version())
	b.WriteString("The simulator calls the production policy code (`internal/work/sched.Decide`) and the estimator (`internal/work/estimate`) on snapshots it builds, as the controller will. Baselines are in the same package with the same signature.\n\n")

	var mainPols, abl []policySpec
	for _, pol := range pols {
		if pol.ablation {
			abl = append(abl, pol)
		} else {
			mainPols = append(mainPols, pol)
		}
	}
	aggs := map[string]agg{}
	for _, pol := range pols {
		aggs[pol.name] = aggregate(res[pol.name], p)
	}

	b.WriteString("## Results\n\nMean ± standard deviation across seeds. A render deadline is missed when the job finishes after it; late hours sum the lateness of missed jobs. CI time to green runs from submission to the last shard passing. Utilization is busy slot time over owned capacity for the week (Monday 00:00 to Monday 00:00), including work later lost. Useful GPU hours are GPU time whose work was kept (finished units, and ML progress saved in a checkpoint); lost GPU hours are GPU time thrown away by failures and preemptions, plus ML restore time.\n\n")
	head := []string{"policy", "render deadlines missed", "missed %", "late hours", "CI p50 min", "CI p95 min", "CI over 15 min %", "util cpu %", "util gpu %", "util ci %", "useful GPU h", "lost GPU h", "burst $"}
	row := func(name string) []string {
		a := aggs[name]
		return []string{name, ms(a.missed, 1), ms(a.missPct, 1), ms(a.late, 1), ms(a.p50, 1), ms(a.p95, 1), ms(a.over, 1),
			ms(a.ucpu, 1), ms(a.ugpu, 1), ms(a.uci, 1), ms(a.useG, 0), ms(a.lostG, 1), ms(a.cost, 0)}
	}
	var rows [][]string
	for _, pol := range mainPols {
		rows = append(rows, row(pol.name))
	}
	table(&b, head, rows)
	if len(res["edf"]) > 0 {
		jobsMean, _ := meanSD(aggs["edf"].jobs)
		fmt.Fprintf(&b, "Render jobs with deadlines per week: %.0f on average. The spread across seeds is large because each seed is a different week (job counts, shot lengths, where heavy shots land); compare policies within a seed, below.\n\n", jobsMean)
	}
	b.WriteString("![late hours by policy](late_hours.svg)\n\n")

	b.WriteString("### Paired against EDF, same seed\n\nDifference from EDF on the same week (policy minus EDF): mean ± standard deviation of the per-seed difference, and on how many seeds the policy had fewer, equal or more late hours.\n\n")
	if base := res["edf"]; len(base) > 0 {
		rows = nil
		for _, pol := range pols {
			if pol.name == "edf" {
				continue
			}
			var dm, dl, dp []float64
			better, same, worse := 0, 0, 0
			for i, r := range res[pol.name] {
				e := base[i]
				tr, te := r.renderTotals(), e.renderTotals()
				dm = append(dm, float64(tr.Missed-te.Missed))
				d := tr.LateHours - te.LateHours
				dl = append(dl, d)
				dp = append(dp, quantile(r.CITTG, 0.95)-quantile(e.CITTG, 0.95))
				switch {
				case math.Abs(d) < 0.05:
					same++
				case d < 0:
					better++
				default:
					worse++
				}
			}
			rows = append(rows, []string{pol.name, ms(dm, 1), ms(dl, 1), ms(dp, 1), fmt.Sprintf("%d / %d / %d", better, same, worse)})
		}
		table(&b, []string{"policy", "Δ missed", "Δ late hours", "Δ CI p95 min", "seeds with fewer / equal / more late hours"}, rows)
	}

	b.WriteString("### By job kind\n\nMissed deadlines and late hours per kind. ML runs carry a deadline of submission plus 36 hours.\n\n")
	rows = nil
	for _, pol := range pols {
		r := []string{pol.name}
		for _, k := range []string{"dailies", "preview", "delivery", "ml"} {
			var m, l, n []float64
			for _, x := range res[pol.name] {
				s := x.stat(k)
				m = append(m, float64(s.Missed))
				l = append(l, s.LateHours)
				n = append(n, float64(s.Jobs))
			}
			nm, _ := meanSD(n)
			r = append(r, fmt.Sprintf("%s of %.0f", ms(m, 1), nm), ms(l, 1))
		}
		rows = append(rows, r)
	}
	table(&b, []string{"policy", "dailies missed", "dailies late h", "preview missed", "preview late h", "delivery missed", "delivery late h", "ML missed", "ML late h"}, rows)

	b.WriteString("### By show\n\nLate hours of each show's render jobs (fairness across projects).\n\n")
	rows = nil
	for _, pol := range pols {
		r := []string{pol.name}
		for _, show := range showNames {
			var l []float64
			for _, x := range res[pol.name] {
				s := 0.0
				for _, j := range x.Jobs {
					if j.Project == show && j.Done > j.Deadline {
						s += j.Done - j.Deadline
					}
				}
				l = append(l, s)
			}
			r = append(r, ms(l, 1))
		}
		rows = append(rows, r)
	}
	table(&b, []string{"policy", "showA late h", "showB late h", "showC late h"}, rows)

	b.WriteString("### Preemption, lost work and burst\n\nPreemptions counts units stopped (yields and kills). Lost CPU hours include task failures, which every policy suffers alike (failures are drawn per unit and attempt from the seed, not from the policy). Bursts needing approval counts distinct jobs for which the policy found a burst that would restore the deadline but was over the project's budget or cost more than the lateness it prevents; the simulation never approves those.\n\n")
	rows = nil
	for _, pol := range pols {
		a := aggs[pol.name]
		reasons := map[string]float64{}
		for _, r := range res[pol.name] {
			for k, v := range r.PreemptBy {
				reasons[k] += float64(v) / float64(len(res[pol.name]))
			}
		}
		var rs []string
		for _, k := range sortedKeys(reasons) {
			rs = append(rs, fmt.Sprintf("%s %.0f", k, reasons[k]))
		}
		if len(rs) == 0 {
			rs = []string{"none"}
		}
		rows = append(rows, []string{pol.name, ms(a.pre, 0), strings.Join(rs, ", "), ms(a.lostC, 0), ms(a.mlUse, 0), ms(a.mlLost, 1), ms(a.burstH, 0), ms(a.cost, 0), ms(a.approvals, 1)})
	}
	table(&b, []string{"policy", "preemptions", "by reason (mean)", "lost CPU h", "ML useful GPU h", "ML lost GPU h", "burst slot-hours", "burst $", "bursts needing approval"}, rows)

	if len(abl) > 0 {
		b.WriteString("### Ablations\n\nThe objective policy with parts of DESIGN.md section 9.9 restored as written (see the discussion for what changed and why). Same columns as the main table.\n\n")
		rows = nil
		for _, pol := range append([]policySpec{policyByName("objective")}, abl...) {
			if res[pol.name] != nil {
				rows = append(rows, row(pol.name))
			}
		}
		table(&b, head, rows)
	}

	if len(tight) > 0 {
		b.WriteString("## Sensitivity: half the GPUs\n\nThe main farm's GPU pool runs at about a third of capacity, so ML runs and GPU renders rarely contend there. This scenario replays the same weeks (same seeds, same jobs) with 16 owned GPUs instead of 32 and every GPU quota halved. Everything else is unchanged.\n\n")
		taggs := map[string]agg{}
		rows = nil
		for _, pol := range tight {
			taggs[pol.name] = aggregate(tightRes[pol.name], p)
			a := taggs[pol.name]
			var gl, gm []float64
			for _, r := range tightRes[pol.name] {
				l, m := 0.0, 0.0
				for _, j := range r.Jobs {
					if j.GPU && j.Kind != "ml" && j.Done > j.Deadline {
						l += j.Done - j.Deadline
						m++
					}
				}
				gl = append(gl, l)
				gm = append(gm, m)
			}
			var mlMiss []float64
			for _, r := range tightRes[pol.name] {
				mlMiss = append(mlMiss, float64(r.stat("ml").Missed))
			}
			rows = append(rows, []string{pol.name, ms(a.missed, 1), ms(a.late, 1), ms(gm, 1), ms(gl, 1), ms(mlMiss, 1), ms(a.ugpu, 1), ms(a.mlUse, 0), ms(a.mlLost, 1), ms(a.lostG, 1), ms(a.pre, 0), ms(a.cost, 0)})
		}
		table(&b, []string{"policy", "render missed", "render late h", "GPU render missed", "GPU render late h", "ML missed", "util gpu %", "ML useful GPU h", "ML lost GPU h", "lost GPU h", "preemptions", "burst $"}, rows)
	}
	if ord != nil {
		writeOrderSection(&b, ord)
	}
	writeParams(&b, p, ord)
	b.WriteString("## Discussion\n\n")
	b.WriteString(strings.TrimSpace(discussion))
	b.WriteString("\n")

	if err := os.WriteFile(filepath.Join(dir, "results.md"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := writeCSVs(dir, p, pols, res, ord); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "late_hours.svg"), []byte(lateHoursSVG(mainPols, aggs)), 0o644); err != nil {
		return err
	}
	if ord != nil {
		if err := os.WriteFile(filepath.Join(dir, "order_error.svg"), []byte(orderErrorSVG(ord)), 0o644); err != nil {
			return err
		}
	}
	if os.Getenv("SIMDEBUG") != "" && res["edf"] != nil && res["objective"] != nil {
		debugJobs(res, "edf", "objective")
	}
	return nil
}

func policyByName(n string) policySpec {
	for _, p := range policies {
		if p.name == n {
			return p
		}
	}
	return policySpec{name: n}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func orderTrials(o *orderResults) int {
	if o == nil {
		return 0
	}
	return o.P.Trials
}

func orderCells(o *orderResults, arm string, multOK func(float64) bool) []trialArm {
	var out []trialArm
	for _, r := range o.Rows {
		if r.Arm == arm && multOK(r.Mult) {
			out = append(out, r)
		}
	}
	return out
}

func writeOrderSection(b *strings.Builder, o *orderResults) {
	p := o.P
	fmt.Fprintf(b, "## Binary against sequential order\n\nOne shot at a time on %d slots, rendered in binary or in sequential order: %d trials for each frame count %v and each heavy-region multiplier %v (1 is the control, with no heavy region). At %d evenly spaced moments of the render the estimated remaining time, max(W/P, t_path), is compared with the truth. The `sequential-mean` arm uses the usual render-manager estimate (mean of completed frames times frames left) instead of the estimate package.\n\n",
		p.Slots, p.Trials, p.Frames, p.Mults, p.GridPoints)
	fracs := []float64{0.1, 0.25, 0.5, 0.75}
	b.WriteString("### Error of the remaining-time estimate\n\nMean of |estimated - true| / true remaining time, over all frame counts, at the given fraction of the render's elapsed time; then the mean over the whole render and the mean signed error (negative: underestimate).\n\n")
	head := []string{"multiplier", "arm"}
	for _, f := range fracs {
		head = append(head, fmt.Sprintf("at %.0f%%", 100*f))
	}
	head = append(head, "whole render", "signed")
	var rows [][]string
	for _, m := range p.Mults {
		for _, arm := range orderArms {
			cells := orderCells(o, arm, func(x float64) bool { return x == m })
			r := []string{fmt.Sprintf("%.0fx", m), arm}
			for _, f := range fracs {
				g := int(math.Round(f*float64(p.GridPoints+1))) - 1
				var xs []float64
				for _, c := range cells {
					xs = append(xs, c.AbsErr[g])
				}
				mm, _ := meanSD(xs)
				r = append(r, fmt.Sprintf("%.3f", mm))
			}
			var all, sgn []float64
			for _, c := range cells {
				all = append(all, c.AbsErr...)
				sgn = append(sgn, c.SignedErr...)
			}
			ma, _ := meanSD(all)
			msg, _ := meanSD(sgn)
			r = append(r, fmt.Sprintf("%.3f", ma), fmt.Sprintf("%+.3f", msg))
			rows = append(rows, r)
		}
	}
	table(b, head, rows)
	b.WriteString("Relative error grows near the end of every render because the true remainder approaches zero; the chart therefore shows the absolute error as a fraction of the render, averaged over trials with a heavy region of 3x or more.\n\n![remaining-time error against elapsed time](order_error.svg)\n\n")

	fmt.Fprintf(b, "### Heavy region flagged, and slack settled\n\nA heavy region is flagged when the estimator reports a run of frames above %.0fx the reference cost that overlaps the true region (a running frame counts once its elapsed time alone is above the threshold). Times are fractions of the render. False flags are trials in which a region not overlapping the true one was flagged at some moment. The deadline is set so the true slack is %.0f%% of the render; the slack estimate is within the band when its error is at most %.0f%% of the true slack, and has settled when it stays in the band from then on (trials that never settle count as 1.00).\n\n",
		p.HeavyFactor, 100*p.SlackFrac, 100*p.SettleFrac)
	rows = nil
	for _, m := range p.Mults {
		for _, arm := range orderArms {
			cells := orderCells(o, arm, func(x float64) bool { return x == m })
			var flag, settle, band []float64
			nfalse, nsettle := 0, 0
			for _, c := range cells {
				if c.FlagAt >= 0 {
					flag = append(flag, c.FlagAt)
				}
				if c.FalseFlag {
					nfalse++
				}
				band = append(band, c.InBand)
				if c.SettleAt >= 0 {
					settle = append(settle, c.SettleAt)
					nsettle++
				} else {
					settle = append(settle, 1)
				}
			}
			flagCell, falseCell := "n/a", "n/a"
			if arm != "sequential-mean" {
				falseCell = fmt.Sprintf("%.0f%%", 100*float64(nfalse)/float64(len(cells)))
				if m > 1 {
					flagCell = fmt.Sprintf("%.0f%%", 100*float64(len(flag))/float64(len(cells)))
					if len(flag) > 0 {
						flagCell += fmt.Sprintf(", median at %.2f", quantile(flag, 0.5))
					}
				}
			}
			bm, _ := meanSD(band)
			rows = append(rows, []string{fmt.Sprintf("%.0fx", m), arm, flagCell, falseCell,
				fmt.Sprintf("%.0f%%", 100*bm),
				fmt.Sprintf("%.2f", quantile(settle, 0.5)), fmt.Sprintf("%.2f", quantile(settle, 0.9)),
				fmt.Sprintf("%.0f%%", 100*float64(nsettle)/float64(len(cells)))})
		}
	}
	table(b, []string{"multiplier", "arm", "heavy region flagged", "false flags", "slack within band, share of render", "slack settled, median", "slack settled, p90", "settled before the end"}, rows)

	b.WriteString("### Makespan\n\nThe order also changes when the shot finishes on a fixed number of slots. Binary order leaves frames from every part of the shot, heavy ones included, to its last levels, so the last wave can hold long frames that start late; sequential order finishes with whatever lies at the end of the shot. Ratio of binary to sequential makespan for the same shot.\n\n")
	rows = nil
	for _, m := range p.Mults {
		seq := map[[2]int]float64{}
		for _, c := range orderCells(o, "sequential", func(x float64) bool { return x == m }) {
			seq[[2]int{c.N, c.Trial}] = c.Makespan
		}
		var ratios []float64
		worse := 0
		for _, c := range orderCells(o, "binary", func(x float64) bool { return x == m }) {
			r := c.Makespan / seq[[2]int{c.N, c.Trial}]
			ratios = append(ratios, r)
			if r > 1.05 {
				worse++
			}
		}
		mr, _ := meanSD(ratios)
		rows = append(rows, []string{fmt.Sprintf("%.0fx", m), fmt.Sprintf("%.3f", mr), fmt.Sprintf("%.3f", quantile(ratios, 0.5)), fmt.Sprintf("%.3f", quantile(ratios, 0.9)), fmt.Sprintf("%.0f%%", 100*float64(worse)/float64(len(ratios)))})
	}
	table(b, []string{"multiplier", "mean ratio", "median", "p90", "binary more than 5% longer"}, rows)

	b.WriteString("### Calibration of W90\n\nShare of moments at which the true remaining work was at or below the estimator's W90. A calibrated 90th percentile would cover about 90%.\n\n")
	rows = nil
	for _, m := range p.Mults {
		r := []string{fmt.Sprintf("%.0fx", m)}
		for _, arm := range []string{"binary", "sequential"} {
			n, k := 0, 0
			for _, c := range orderCells(o, arm, func(x float64) bool { return x == m }) {
				for _, v := range c.Covered {
					n++
					if v {
						k++
					}
				}
			}
			r = append(r, fmt.Sprintf("%.0f%%", 100*float64(k)/float64(max(1, n))))
		}
		rows = append(rows, r)
	}
	table(b, []string{"multiplier", "binary", "sequential"}, rows)
}

func writeParams(b *strings.Builder, p params, o *orderResults) {
	b.WriteString("## Every parameter\n\n### Farm\n\n")
	var rows [][]string
	for _, pd := range mainFarm.pools {
		if pd.burst == nil {
			rows = append(rows, []string{pd.name, strconv.Itoa(pd.slots), "owned", "", ""})
			continue
		}
		rows = append(rows, []string{pd.name, fmt.Sprintf("0 to %d", pd.burst.max), "burst", pd.burst.delay.String(), fmt.Sprintf("$%.2f", pd.burst.price)})
	}
	table(b, []string{"pool", "slots", "kind", "provisioning", "per slot-hour"}, rows)
	b.WriteString("A CPU slot is a 32 vCPU render node; a GPU slot is one 48 GB class GPU; a CI slot is an 8 vCPU runner. Burst prices are approximate public list prices for comparable instances (c7i.8xlarge, one GPU of a g6e.xlarge), spot at about 40 to 45 percent of on-demand, not quoted from a live price list. Provisioning includes boot, image pull and C4 cache pre-warm. Spot interruptions are not modeled. Render jobs may use their pool type's owned and burst pools; CI uses only the CI pool; ML runs use only the owned GPU pool. The baselines see the burst pools but never request them.\n\n")
	rows = nil
	for _, pd := range mainFarm.projects {
		var q []string
		for _, k := range sortedKeys(pd.nominal) {
			q = append(q, fmt.Sprintf("%s %d", k, pd.nominal[k]))
		}
		rows = append(rows, []string{pd.name, strings.Join(q, ", "), fmt.Sprintf("$%.0f", pd.budget), fmt.Sprintf("$%.0f", pd.lateCost), strconv.Itoa(pd.bonus)})
	}
	table(b, []string{"project", "nominal quota (borrowing unlimited)", "burst budget per day", "lateness cost per hour", "manual priority bonus"}, rows)
	var mp []string
	for _, k := range sortedKeys(manualPriority) {
		mp = append(mp, fmt.Sprintf("%s %d", k, manualPriority[k]))
	}
	fmt.Fprintf(b, "Manual baseline priorities by job type: %s, plus the show bonus. Quotas apply only to the objective policy; the baselines have none.\n\n", strings.Join(mp, ", "))

	b.WriteString("### Workload\n\n```\n")
	b.WriteString(fields(p))
	b.WriteString("\n```\n\nRender jobs whose frames are generators evaluate a scene description (45 s median, on the job's pool) and expand into a shadow pass (0.2x the frame's cost), a reflection pass with a per-job probability (0.3x), and a beauty pass that depends on both. CI pipelines are a build that discovers 4 + Poisson(10) shards from a fixed monorepo of 400 test targets, each with its own duration and failure rate; shards run in order of failure rate. Failures happen per attempt (render and CI 2%, the fourth attempt always succeeds; ML runs at 0.5% per hour of progress) and are drawn from the seed and the unit, so every policy sees the same failures. ML runs checkpoint at a per-run interval; a yield waits for the next checkpoint and loses only restore time, a kill loses the work since the last checkpoint.\n\nThe estimator's history priors are built from 2,400 render jobs and 3,000 CI pipelines drawn from the same generator with a seed no experiment uses, so they describe the population a job comes from, not the job. An ML run's prior is its declared length (steps times historical step time), off by a lognormal factor with sigma 0.15.\n\n")

	b.WriteString("### Policy settings\n\n```\n")
	b.WriteString(fields(sched.DefaultConfig()))
	fmt.Fprintf(b, "\n```\n\nSimulation: a decision at most every %.0f s after a change and at least every %.0f s; a job's estimate is refreshed on change or every %.0f s while it runs. The Manual wrangler reviews every 2 hours.\n\n", coalesce, heartbeat, stale)
	if o != nil {
		b.WriteString("### Binary-order experiment\n\n```\n")
		b.WriteString(fields(o.P))
		b.WriteString("\n```\n\n")
	}
}

// fields lists a struct's fields one per line as "Name: value".
func fields(v any) string {
	rv := reflect.ValueOf(v)
	var lines []string
	for i := 0; i < rv.NumField(); i++ {
		lines = append(lines, fmt.Sprintf("%s: %v", rv.Type().Field(i).Name, rv.Field(i).Interface()))
	}
	return strings.Join(lines, "\n")
}

func writeCSVs(dir string, p params, pols []policySpec, res map[string][]*runResult, o *orderResults) error {
	write := func(name string, head []string, rows [][]string) error {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		w := csv.NewWriter(f)
		w.Write(head)
		w.WriteAll(rows)
		if err := w.Error(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	ff := func(x float64) string { return strconv.FormatFloat(x, 'f', 3, 64) }
	var runs, jobs [][]string
	for _, pol := range pols {
		for _, r := range res[pol.name] {
			t := r.renderTotals()
			n := 0
			for _, x := range r.CITTG {
				if x > p.CISLO {
					n++
				}
			}
			row := []string{pol.name, strconv.FormatUint(r.Seed, 10), strconv.Itoa(t.Jobs), strconv.Itoa(t.Missed), ff(t.LateHours)}
			for _, k := range []string{"dailies", "preview", "delivery", "ml"} {
				s := r.stat(k)
				row = append(row, strconv.Itoa(s.Jobs), strconv.Itoa(s.Missed), ff(s.LateHours))
			}
			row = append(row, strconv.Itoa(len(r.CITTG)), ff(quantile(r.CITTG, 0.5)), ff(quantile(r.CITTG, 0.95)), strconv.Itoa(n),
				ff(r.Util["cpu"]), ff(r.Util["gpu"]), ff(r.Util["ci"]), ff(r.UsefulGPUh), ff(r.LostGPUh), ff(r.UsefulCPUh), ff(r.LostCPUh),
				ff(r.MLUsefulh), ff(r.MLLosth), ff(r.BurstCost), ff(r.BurstSlotHours), strconv.Itoa(r.preemptions), strconv.Itoa(len(r.approvalJobs)),
				strconv.Itoa(r.Unfinished), strconv.Itoa(r.decides))
			runs = append(runs, row)
			for _, j := range r.Jobs {
				if j.Kind == "ci" {
					continue
				}
				jobs = append(jobs, []string{pol.name, strconv.FormatUint(r.Seed, 10), j.ID, j.Kind, j.Project, strconv.FormatBool(j.GPU), strconv.FormatBool(j.Expand),
					strconv.Itoa(j.Units), ff(j.WorkSlotHours), ff(j.HeavyMult), ff(j.Submit), ff(j.Deadline), ff(j.Done), ff(math.Max(0, j.Done-j.Deadline))})
			}
		}
	}
	runHead := []string{"policy", "seed", "render_jobs", "render_missed", "render_late_hours"}
	for _, k := range []string{"dailies", "preview", "delivery", "ml"} {
		runHead = append(runHead, k+"_jobs", k+"_missed", k+"_late_hours")
	}
	runHead = append(runHead, "ci_pipelines", "ci_p50_min", "ci_p95_min", "ci_over_slo", "util_cpu", "util_gpu", "util_ci",
		"useful_gpu_h", "lost_gpu_h", "useful_cpu_h", "lost_cpu_h", "ml_useful_gpu_h", "ml_lost_gpu_h", "burst_dollars", "burst_slot_hours",
		"preemptions", "bursts_needing_approval", "unfinished_jobs", "decisions")
	if err := write("runs.csv", runHead, runs); err != nil {
		return err
	}
	if err := write("jobs.csv", []string{"policy", "seed", "job", "kind", "project", "gpu", "expand", "units", "work_slot_hours", "heavy_mult", "submit_h", "deadline_h", "done_h", "late_h"}, jobs); err != nil {
		return err
	}
	if o == nil {
		return nil
	}
	var trials, errs [][]string
	type cell struct {
		n    int
		mult float64
		arm  string
	}
	sums := map[cell][][3]float64{} // per grid moment: sum |rel|, sum signed rel, count
	var cells []cell
	for _, r := range o.Rows {
		trials = append(trials, []string{strconv.Itoa(r.N), ff(r.Mult), strconv.Itoa(r.Trial), r.Arm, ff(r.Makespan / hour), ff(r.FlagAt), strconv.FormatBool(r.FalseFlag), ff(r.SettleAt), ff(r.InBand)})
		k := cell{r.N, r.Mult, r.Arm}
		if sums[k] == nil {
			sums[k] = make([][3]float64, len(r.AbsErr))
			cells = append(cells, k)
		}
		for g := range r.AbsErr {
			sums[k][g][0] += r.AbsErr[g]
			sums[k][g][1] += r.SignedErr[g]
			sums[k][g][2]++
		}
	}
	for _, k := range cells {
		for g, v := range sums[k] {
			f := float64(g+1) / float64(o.P.GridPoints+1)
			errs = append(errs, []string{strconv.Itoa(k.n), ff(k.mult), k.arm, ff(f), strconv.FormatFloat(v[0]/v[2], 'f', 4, 64), strconv.FormatFloat(v[1]/v[2], 'f', 4, 64), strconv.Itoa(int(v[2]))})
		}
	}
	if err := write("order_trials.csv", []string{"frames", "mult", "trial", "arm", "makespan_h", "flag_at", "false_flag", "settle_at", "in_band_share"}, trials); err != nil {
		return err
	}
	return write("order_error.csv", []string{"frames", "mult", "arm", "elapsed_frac", "mean_abs_rel_error", "mean_signed_rel_error", "trials"}, errs)
}

// Chart styling: the light surface and the first three categorical slots
// of the reference palette (validated together for all pairs).
const (
	svgSurface = "#fcfcfb"
	svgInk     = "#0b0b0b"
	svgInk2    = "#52514e"
	svgGrid    = "#e4e3df"
)

var svgSeries = []string{"#2a78d6", "#eb6834", "#1baf7a"}

func orderErrorSVG(o *orderResults) string {
	const W, H, L, R, T, B = 760, 400, 64, 150, 48, 52
	pw, ph := float64(W-L-R), float64(H-T-B)
	const ymax = 1.0
	x := func(f float64) float64 { return float64(L) + f*pw }
	y := func(v float64) float64 { return float64(T) + ph - math.Min(v, ymax)/ymax*ph }
	type end struct {
		arm  string
		x, y float64
		c    string
	}
	var ends []end
	var s strings.Builder
	fmt.Fprintf(&s, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="Helvetica, Arial, sans-serif">`+"\n", W, H, W, H)
	fmt.Fprintf(&s, `<rect width="%d" height="%d" fill="%s"/>`+"\n", W, H, svgSurface)
	fmt.Fprintf(&s, `<text x="%d" y="24" font-size="15" fill="%s">Remaining-time estimate error, shots with a heavy region of 3x or more (%d trials per arm)</text>`+"\n", L, svgInk, len(orderCells(o, "binary", func(m float64) bool { return m >= 3 })))
	for _, v := range []float64{0, 0.25, 0.5, 0.75, 1} {
		fmt.Fprintf(&s, `<line x1="%d" x2="%.1f" y1="%.1f" y2="%.1f" stroke="%s"/>`+"\n", L, x(1), y(v), y(v), svgGrid)
		fmt.Fprintf(&s, `<text x="%d" y="%.1f" font-size="11" fill="%s" text-anchor="end">%.2f</text>`+"\n", L-8, y(v)+4, svgInk2, v)
	}
	for _, f := range []float64{0, 0.25, 0.5, 0.75, 1} {
		fmt.Fprintf(&s, `<text x="%.1f" y="%d" font-size="11" fill="%s" text-anchor="middle">%.0f%%</text>`+"\n", x(f), H-B+18, svgInk2, 100*f)
	}
	fmt.Fprintf(&s, `<text x="%.1f" y="%d" font-size="12" fill="%s" text-anchor="middle">elapsed time, fraction of the render</text>`+"\n", x(0.5), H-12, svgInk2)
	fmt.Fprintf(&s, `<text transform="translate(18 %.1f) rotate(-90)" font-size="12" fill="%s" text-anchor="middle">mean |error|, fraction of the render</text>`+"\n", y(0.5), svgInk2)
	for i, arm := range orderArms {
		cells := orderCells(o, arm, func(m float64) bool { return m >= 3 })
		var pts []string
		var last end
		for g := 0; g < o.P.GridPoints; g++ {
			f := float64(g+1) / float64(o.P.GridPoints+1)
			var xs []float64
			for _, c := range cells {
				xs = append(xs, math.Abs(c.SignedErr[g])*(1-f))
			}
			m, _ := meanSD(xs)
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(f), y(m)))
			last = end{arm, x(f), y(m), svgSeries[i]}
		}
		fmt.Fprintf(&s, `<polyline fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round" points="%s"><title>%s</title></polyline>`+"\n", svgSeries[i], strings.Join(pts, " "), arm)
		ends = append(ends, last)
	}
	// Direct labels at the line ends, nudged apart so they never overlap.
	sort.Slice(ends, func(i, j int) bool { return ends[i].y < ends[j].y })
	ly := make([]float64, len(ends))
	prev := -100.0
	for i, e := range ends {
		ly[i] = math.Max(e.y+4, prev+15)
		prev = ly[i]
	}
	if over := ly[len(ly)-1] - (float64(T) + ph); over > 0 {
		for i := range ly {
			ly[i] -= over
		}
	}
	for i, e := range ends {
		ly := ly[i]
		fmt.Fprintf(&s, `<circle cx="%.1f" cy="%.1f" r="4" fill="%s" stroke="%s" stroke-width="2"/>`+"\n", e.x, e.y, e.c, svgSurface)
		fmt.Fprintf(&s, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="%s" stroke-width="2"/>`+"\n", e.x+8, e.x+18, ly-4, ly-4, e.c)
		fmt.Fprintf(&s, `<text x="%.1f" y="%.1f" font-size="12" fill="%s">%s</text>`+"\n", e.x+22, ly, svgInk, e.arm)
	}
	s.WriteString("</svg>\n")
	return s.String()
}

func lateHoursSVG(pols []policySpec, aggs map[string]agg) string {
	const W, L, R, T, rowH = 760, 150, 70, 48, 34
	H := T + rowH*len(pols) + 44
	maxv := 0.0
	for _, p := range pols {
		for _, v := range aggs[p.name].late {
			maxv = math.Max(maxv, v)
		}
	}
	step := niceStep(maxv / 4)
	maxv = math.Ceil(maxv/step) * step
	pw := float64(W - L - R)
	x := func(v float64) float64 { return float64(L) + v/maxv*pw }
	var s strings.Builder
	fmt.Fprintf(&s, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="Helvetica, Arial, sans-serif">`+"\n", W, H, W, H)
	fmt.Fprintf(&s, `<rect width="%d" height="%d" fill="%s"/>`+"\n", W, H, svgSurface)
	fmt.Fprintf(&s, `<text x="%d" y="24" font-size="15" fill="%s">Render late hours per week: mean (bar) and each seed (dots)</text>`+"\n", L, svgInk)
	for v := 0.0; v <= maxv+1e-9; v += step {
		fmt.Fprintf(&s, `<line x1="%.1f" x2="%.1f" y1="%d" y2="%d" stroke="%s"/>`+"\n", x(v), x(v), T, H-40, svgGrid)
		fmt.Fprintf(&s, `<text x="%.1f" y="%d" font-size="11" fill="%s" text-anchor="middle">%.0f</text>`+"\n", x(v), H-24, svgInk2, v)
	}
	fmt.Fprintf(&s, `<text x="%.1f" y="%d" font-size="12" fill="%s" text-anchor="middle">late hours</text>`+"\n", x(maxv/2), H-6, svgInk2)
	for i, p := range pols {
		a := aggs[p.name]
		m, _ := meanSD(a.late)
		y := float64(T + i*rowH)
		fmt.Fprintf(&s, `<text x="%d" y="%.1f" font-size="12" fill="%s" text-anchor="end">%s</text>`+"\n", L-10, y+rowH/2+4, svgInk, p.name)
		fmt.Fprintf(&s, `<rect x="%d" y="%.1f" width="%.1f" height="%d" rx="4" fill="%s"><title>%s: %.1f late hours (mean)</title></rect>`+"\n", L, y+8, math.Max(0, x(m)-float64(L)), rowH-16, svgSeries[0], p.name, m)
		for _, v := range a.late {
			fmt.Fprintf(&s, `<circle cx="%.1f" cy="%.1f" r="4" fill="%s" stroke="%s" stroke-width="2"><title>%.1f</title></circle>`+"\n", x(v), y+rowH/2, svgInk2, svgSurface, v)
		}
		fmt.Fprintf(&s, `<text x="%.1f" y="%.1f" font-size="12" fill="%s">%.0f</text>`+"\n", x(maxv)+8, y+rowH/2+4, svgInk, m)
	}
	s.WriteString("</svg>\n")
	return s.String()
}

// niceStep rounds x up to 1, 2 or 5 times a power of ten.
func niceStep(x float64) float64 {
	if x <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(x)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*p >= x {
			return m * p
		}
	}
	return 10 * p
}

func debugJobs(res map[string][]*runResult, a, b string) {
	ra, rb := res[a][0], res[b][0]
	idx := map[string]jobOutcome{}
	for _, j := range rb.Jobs {
		idx[j.ID] = j
	}
	fmt.Printf("preempt %s: %v\n", b, rb.PreemptBy)
	for _, x := range ra.Jobs {
		y := idx[x.ID]
		la, lb := x.Done-x.Deadline, y.Done-y.Deadline
		if (la > 0) == (lb > 0) {
			continue
		}
		fmt.Printf("%-22s %-8s gpu=%-5v exp=%-5v units=%4d work=%6.1f heavy=%.0f sub=%6.2f dl=%6.2f %s=%6.2f %s=%6.2f\n",
			x.ID, x.Kind, x.GPU, x.Expand, x.Units, x.WorkSlotHours, x.HeavyMult, x.Submit, x.Deadline, a, la, b, lb)
	}
}
