package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/work/sched"
)

// A variant is a metamorphic transformation of a point's workload: the
// same jobs on a bigger farm, with looser deadlines, or with a bigger
// burst budget. None should make the policy's on-time count worse beyond
// noise.
type variant string

const (
	vBase     variant = ""
	vCapacity variant = "capacity"
	vDeadline variant = "deadline"
	vBudget   variant = "budget"
	vRepeat   variant = "repeat" // the base again, for determinism
)

var metaInv = map[variant]string{
	vCapacity: "metamorphic-capacity",
	vDeadline: "metamorphic-deadline",
	vBudget:   "metamorphic-budget",
}

// transform returns the workload of a variant. Jobs are shared with the
// base workload unless the variant changes them.
func transform(w *workload, v variant) *workload {
	out := *w
	switch v {
	case vCapacity:
		out.pools = nil
		for _, p := range w.pools {
			if p.burst == nil {
				p.nodes = max(p.nodes+1, int(math.Ceil(float64(p.nodes)*1.5)))
			}
			out.pools = append(out.pools, p)
		}
	case vDeadline:
		copies := map[*jobSpec]*jobSpec{}
		out.jobs = make([]*jobSpec, len(w.jobs))
		for i, js := range w.jobs {
			c := *js
			if c.hasDue && c.due > c.submit {
				c.due = c.submit + 1.5*(c.due-c.submit)
			}
			out.jobs[i] = &c
			copies[js] = &c
		}
		for _, c := range out.jobs {
			ups := make([]*jobSpec, len(c.upstream))
			for k, u := range c.upstream {
				ups[k] = copies[u]
			}
			c.upstream = ups
		}
	case vBudget:
		out.projects = nil
		for _, p := range w.projects {
			if p.budget == 0 {
				p.budget = 1
			}
			p.budget *= 4
			out.projects = append(out.projects, p)
		}
	}
	return &out
}

// applies says whether a variant says anything at a point.
func applies(pt Point, v variant) bool {
	switch v {
	case vDeadline:
		return pt.PastDue == 0
	case vBudget:
		return pt.BurstFrac > 0
	case vCapacity:
		return pt.Nodes > 0
	}
	return true
}

type task struct {
	pt   Point
	seed uint64
	pol  policy
	v    variant
}

// pointResult gathers every run of one (point, seed).
type pointResult struct {
	Pt     Point
	Seed   uint64
	Runs   map[string]*runResult  // by policy, base workload
	Meta   map[variant]*runResult // the checked policy on each variant
	Viol   []found
	Counts map[string]int
	Took   time.Duration
}

// found is a violation with its repro command.
type found struct {
	violation
	Point  string
	Policy string
	Repro  string
	Known  bool
	N      int // times in the run
}

type runOpts struct {
	Workers   int
	Seeds     []uint64
	Baselines bool
	Meta      bool
	Repeat    bool
	Progress  bool
}

// mainPolicy is the policy the metamorphic and determinism checks use.
const mainPolicy = "objective+burst"

func runGrid(pts []Point, o runOpts) []*pointResult {
	var tasks []task
	for _, pt := range pts {
		for _, seed := range o.Seeds {
			for _, pol := range policies {
				if !pol.checked && !o.Baselines {
					continue
				}
				tasks = append(tasks, task{pt: pt, seed: seed, pol: pol})
			}
			main, _ := policyByName(mainPolicy)
			if o.Meta {
				for _, v := range []variant{vCapacity, vDeadline, vBudget} {
					if applies(pt, v) {
						tasks = append(tasks, task{pt: pt, seed: seed, pol: main, v: v})
					}
				}
			}
			if o.Repeat && pt.Nodes <= 300 {
				tasks = append(tasks, task{pt: pt, seed: seed, pol: main, v: vRepeat})
			}
		}
	}
	// Big farms first, so the slowest runs do not start last.
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].pt.Nodes*tasks[i].pt.Days > tasks[j].pt.Nodes*tasks[j].pt.Days })
	type key struct {
		name string
		seed uint64
	}
	loads := map[key]*workload{}
	var lmu sync.Mutex
	load := func(pt Point, seed uint64) *workload {
		lmu.Lock()
		defer lmu.Unlock()
		k := key{pt.Name, seed}
		if w := loads[k]; w != nil {
			return w
		}
		w := build(pt, seed)
		loads[k] = w
		return w
	}
	res := map[key]*pointResult{}
	for _, pt := range pts {
		for _, seed := range o.Seeds {
			res[key{pt.Name, seed}] = &pointResult{Pt: pt, Seed: seed, Runs: map[string]*runResult{}, Meta: map[variant]*runResult{}, Counts: map[string]int{}}
		}
	}
	var mu sync.Mutex
	ch := make(chan task)
	var wg sync.WaitGroup
	done := 0
	for range max(1, o.Workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				t0 := time.Now()
				w := transform(load(t.pt, t.seed), t.v)
				r := newEngine(w, t.pol, t.pol.checked && t.v == vBase).run()
				took := time.Since(t0)
				mu.Lock()
				pr := res[key{t.pt.Name, t.seed}]
				switch t.v {
				case vBase:
					pr.Runs[t.pol.name] = r
				default:
					pr.Meta[t.v] = r
				}
				pr.Took += took
				done++
				if o.Progress {
					fmt.Fprintf(os.Stderr, "[%d/%d] %s (%d nodes) seed %d %s %s: %s, %d decisions, %s on time\n", done, len(tasks), t.pt.Name, int(t.pt.Nodes), t.seed, t.pol.name, t.v,
						took.Round(time.Millisecond), r.Decides, pct(r.onTimePct()))
				}
				mu.Unlock()
			}
		}()
	}
	for _, t := range tasks {
		ch <- t
	}
	close(ch)
	wg.Wait()
	var out []*pointResult
	for _, pt := range pts {
		for _, seed := range o.Seeds {
			pr := res[key{pt.Name, seed}]
			pr.collect()
			out = append(out, pr)
		}
	}
	return out
}

func pct(x float64) string {
	if math.IsNaN(x) {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", x)
}

// repro is the one-line command that reruns a point.
func repro(pt Point, seed uint64) string {
	return fmt.Sprintf("go run ./hack/stress -point '%s' -seed %d", pt.encode(), seed)
}

// collect turns run results into violations: the checked policies'
// invariant failures, determinism, metamorphic comparisons and latency.
func (pr *pointResult) collect() {
	rp := repro(pr.Pt, pr.Seed)
	addF := func(pol string, v violation, n int) {
		pr.Counts[v.Inv] += n
		_, known := knownBugs[v.Inv]
		pr.Viol = append(pr.Viol, found{violation: v, Point: pr.Pt.Name, Policy: pol, Repro: rp, Known: known, N: n})
	}
	for _, name := range sortedRuns(pr.Runs) {
		r := pr.Runs[name]
		for _, inv := range sortedKeys(r.Counts) {
			n := r.Counts[inv]
			var ex violation
			for _, x := range r.Examples {
				if x.Inv == inv {
					ex = x
					break
				}
			}
			ex.Detail = fmt.Sprintf("%s (%d times in the run)", ex.Detail, n)
			addF(name, ex, n)
		}
		if r.DecideMean > time.Second {
			addF(name, violation{Inv: "decide-latency", Detail: fmt.Sprintf("mean decision %s", r.DecideMean.Round(time.Millisecond))}, 1)
		}
	}
	base := pr.Runs[mainPolicy]
	if base == nil {
		return
	}
	if rep := pr.Meta[vRepeat]; rep != nil && (rep.Hash != base.Hash || rep.OnTime != base.OnTime) {
		addF(mainPolicy, violation{Inv: "determinism", Detail: fmt.Sprintf("decision hashes %x and %x, on time %d and %d", base.Hash, rep.Hash, base.OnTime, rep.OnTime)}, 1)
	}
	for _, v := range []variant{vCapacity, vDeadline, vBudget} {
		r := pr.Meta[v]
		if r == nil || base.DueJobs == 0 {
			continue
		}
		// Late jobs (finished late, or unfinished past their deadline)
		// are compared rather than on-time ones: a looser deadline can
		// leave a job unfinished and not yet due at the end of the run,
		// which counts as neither.
		tol := max(2, int(math.Ceil(0.05*float64(base.DueJobs))))
		if r.Late <= base.Late+tol {
			continue
		}
		detail := fmt.Sprintf("%d late jobs with the %s variant, %d in the base (tolerance %d); repro with -variant %s",
			r.Late, v, base.Late, tol, v)
		// A variant that asked for approval of more bursts priced at $0
		// lost its bursts to lateness-unpriced, not to its own change.
		zero := func(x *runResult) int { return x.Why["costs more than the lateness it prevents ($0)"] }
		if zero(r) > zero(base) {
			detail += fmt.Sprintf("; %d of the variant's approval requests priced lateness at $0 (base %d): caused by lateness-unpriced", zero(r), zero(base))
			pr.Counts[metaInv[v]]++
			pr.Viol = append(pr.Viol, found{violation: violation{Inv: metaInv[v], Detail: detail}, Point: pr.Pt.Name, Policy: mainPolicy, Repro: rp, Known: true, N: 1})
			continue
		}
		addF(mainPolicy, violation{Inv: metaInv[v], Detail: detail}, 1)
	}
}

func sortedRuns(m map[string]*runResult) []string {
	var out []string
	for _, p := range policies {
		if m[p.name] != nil {
			out = append(out, p.name)
		}
	}
	return out
}

// perfRow is Decide's cost at one farm size.
type perfRow struct {
	Nodes, Jobs, Running, Runnable int
	Mean                           time.Duration
	Bytes                          float64
	Allocs                         float64
}

// perfSweep measures Decide on the busiest snapshot of a short run at
// each farm size.
func perfSweep(sizes []int, workers int) []perfRow {
	rows := make([]perfRow, len(sizes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, workers))
	snaps := make([]sched.Snapshot, len(sizes))
	for i, n := range sizes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pt := base(fmt.Sprintf("perf-%d", n))
			pt.Nodes, pt.Days, pt.Load, pt.Projects, pt.Arrivals = float64(n), 0.25, 1.3, 8, 0
			snaps[i] = busiest(pt)
		}()
	}
	wg.Wait()
	// Timing runs one size at a time, so sizes do not slow each other.
	for i, n := range sizes {
		s := snaps[i]
		row := perfRow{Nodes: n, Jobs: len(s.Jobs)}
		for _, j := range s.Jobs {
			row.Running += len(j.RunningUnits)
			row.Runnable += j.RunnableTotal
		}
		cfg := sched.DefaultConfig()
		var ms0, ms1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&ms0)
		t := time.Now()
		k := 0
		for k < 3 || (time.Since(t) < 300*time.Millisecond && k < 200) {
			sched.Decide(s, &sched.State{}, cfg)
			k++
		}
		row.Mean = time.Since(t) / time.Duration(k)
		runtime.ReadMemStats(&ms1)
		row.Bytes = float64(ms1.TotalAlloc-ms0.TotalAlloc) / float64(k)
		row.Allocs = float64(ms1.Mallocs-ms0.Mallocs) / float64(k)
		rows[i] = row
	}
	return rows
}

// busiest runs a point's first hours and keeps the snapshot with the most
// jobs and running units.
func busiest(pt Point) sched.Snapshot {
	w := build(pt, 1)
	var best sched.Snapshot
	score := -1
	pol, _ := policyByName(mainPolicy)
	pol.fn = func(s sched.Snapshot, st *sched.State, cfg sched.Config) sched.Decisions {
		n := len(s.Jobs)
		for _, j := range s.Jobs {
			n += len(j.RunningUnits)
		}
		if n > score {
			score, best = n, s
		}
		return sched.Decide(s, st, cfg)
	}
	e := newEngine(w, pol, false)
	e.w = &workload{pt: w.pt, seed: w.seed, pools: w.pools, projects: w.projects, jobs: w.jobs, horizon: pt.Days * day * 0.8}
	e.run()
	return best
}
