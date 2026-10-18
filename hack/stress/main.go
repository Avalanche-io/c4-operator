// Command stress runs the scheduling policy in work/sched over a
// seeded sample of farms and workloads (a Latin hypercube over farm
// scale, load, deadlines, durations, DAG shape, quotas, arrivals, burst
// economics, faults, nudges and training gangs, plus hand-picked
// corners), checks invariants on every decision, runs metamorphic
// variants and baselines on the same work, and writes a report with
// heatmaps.
//
//	go run ./hack/stress -quick          # the CI grid
//	go run ./hack/stress                 # the full grid
//	go run ./hack/stress -point '...' -seed 3   # rerun one point
//
// Every violation in the report carries the command that reproduces it.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

func main() {
	quick := flag.Bool("quick", false, "the quick grid (CI): small farms, short windows")
	n := flag.Int("n", 0, "Latin hypercube points (default 100, quick 24)")
	gridSeed := flag.Uint64("grid-seed", 1, "seed of the grid sample")
	seeds := flag.Int("seeds", 1, "workload seeds per point")
	workers := flag.Int("workers", 4, "parallel runs")
	out := flag.String("out", "hack/stress/out", "output directory")
	point := flag.String("point", "", "run one point (encoded name=value list or a corner's name) and print its violations")
	seed := flag.Uint64("seed", 1, "workload seed for -point")
	variantF := flag.String("variant", "", "with -point: also run this metamorphic variant (capacity, deadline, budget)")
	specOut := flag.String("spec", "", "with -point: write the point as a c4-hud's cmd/hudmock scenario spec to this file")
	noBase := flag.Bool("no-baselines", false, "skip the baseline policies")
	noPerf := flag.Bool("no-perf", false, "skip the Decide latency sweep")
	spotN := flag.Int("spot-n", 0, "spot grid points (default 48, quick 12; see spot.go)")
	noSpot := flag.Bool("no-spot", false, "skip the spot grid")
	spotPoint := flag.Bool("spot", false, "with -point: run the spot policies on it")
	perfOnly := flag.String("perf", "", "only measure Decide at these farm sizes (comma-separated node counts)")
	flag.Parse()
	// The runs allocate many short-lived snapshots; a larger heap target
	// spends less of the grid's time collecting them.
	debug.SetGCPercent(400)

	if *perfOnly != "" {
		var sizes []int
		for _, f := range strings.Split(*perfOnly, ",") {
			var n int
			fmt.Sscan(f, &n)
			sizes = append(sizes, n)
		}
		for _, row := range perfSweep(sizes, *workers) {
			fmt.Printf("%6d nodes: %4d jobs, %5d running, %6d runnable: %s per call, %s, %.0f allocs\n",
				row.Nodes, row.Jobs, row.Running, row.Runnable, row.Mean.Round(time.Microsecond), bytesH(row.Bytes), row.Allocs)
		}
		return
	}
	if *point != "" {
		if *spotPoint {
			os.Exit(runSpotPoint(*point, *seed, *workers))
		}
		os.Exit(runPoint(*point, *seed, *variantF, *specOut, *workers))
	}
	started := time.Now()
	g := gridOpts{N: 100, Seed: *gridSeed}
	sizes := []int{3, 30, 300, 1000, 3000, 10000}
	if *quick {
		g = gridOpts{N: 24, Seed: *gridSeed, MaxNodes: 24, MaxDays: 0.5, Drain: 0.5}
		sizes = []int{3, 30, 300}
	}
	if *n > 0 {
		g.N = *n
	}
	pts := gridPoints(g, *quick)
	var ss []uint64
	for i := 1; i <= *seeds; i++ {
		ss = append(ss, uint64(i))
	}
	res := runGrid(pts, runOpts{Workers: *workers, Seeds: ss, Baselines: !*noBase, Meta: true, Repeat: true, Progress: true})
	var perf []perfRow
	if !*noPerf {
		perf = perfSweep(sizes, *workers)
	}
	var spot []*spotResult
	var spotTook time.Duration
	if !*noSpot {
		so := gridOpts{N: 48, Seed: *gridSeed, Drain: 1}
		if *quick {
			so = gridOpts{N: 12, Seed: *gridSeed, MaxNodes: 24, MaxDays: 0.5, Drain: 0.5}
		}
		if *spotN > 0 {
			so.N = *spotN
		}
		t := time.Now()
		spot = runSpotGrid(sampleSpot(so), ss, *workers, true)
		spotTook = time.Since(t)
	}
	rep := &report{Grid: g, Quick: *quick, Points: pts, Results: res, Perf: perf, Took: time.Since(started), Workers: *workers, Spot: spot}
	if err := rep.write(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := writeSpot(*out, spot, *quick, spotTook); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s/README.md: %d points, %d runs, %d violations (%d with a known cause) in %s\n",
		*out, len(res), rep.runs(), rep.total(false)+rep.total(true), rep.total(true), time.Since(started).Round(time.Second))
	if rep.total(false) > 0 {
		os.Exit(1)
	}
}

// gridPoints is the Latin hypercube sample plus the corners. The quick
// grid shrinks the corners to its farm sizes and windows.
func gridPoints(g gridOpts, quick bool) []Point {
	pts := sample(g)
	for _, c := range corners() {
		if quick {
			c.Nodes = min(c.Nodes, 8)
			c.Days = min(c.Days, 0.3)
			c.Drain = g.Drain
		}
		pts = append(pts, c)
	}
	return pts
}

func runPoint(enc string, seed uint64, v, specOut string, workers int) int {
	pt, err := decodePoint(enc)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if specOut != "" {
		if err := writeSpec(pt, seed, specOut); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("wrote", specOut)
	}
	o := runOpts{Workers: workers, Seeds: []uint64{seed}, Baselines: true, Meta: v != "", Repeat: true}
	res := runGrid([]Point{pt}, o)[0]
	fmt.Printf("%s seed %d: %s\n", pt.Name, seed, pt.label())
	for _, name := range sortedRuns(res.Runs) {
		r := res.Runs[name]
		fmt.Printf("  %-16s on time %4d/%-4d (%s)  late %6.1fh  burst $%-8.0f util %4.0f%%  preempt %4d  decisions %5d (mean %s)\n",
			name, r.OnTime, r.DueJobs, pct(r.onTimePct()), r.LateHours, r.BurstCost, 100*r.Util, r.Preemptions, r.Decides, r.DecideMean.Round(time.Microsecond))
	}
	for vv, r := range res.Meta {
		if vv == vRepeat {
			continue
		}
		fmt.Printf("  variant %-8s on time %4d/%-4d (%s)\n", vv, r.OnTime, r.DueJobs, pct(r.onTimePct()))
	}
	if len(res.Viol) == 0 {
		fmt.Println("no violations")
		return 0
	}
	for _, f := range res.Viol {
		k := ""
		if f.Known {
			k = " [known]"
		}
		fmt.Printf("  %s%s (%s)\n", strings.TrimSpace(f.String()), k, f.Policy)
	}
	return 1
}

func runSpotPoint(enc string, seed uint64, workers int) int {
	pt, err := decodePoint(enc)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if pt.Provider == 0 {
		fmt.Fprintln(os.Stderr, "not a spot point: set provider=1 (aws), 2 (gcp) or 3 (azure)")
		return 2
	}
	pr := runSpotGrid([]Point{pt}, []uint64{seed}, workers, false)[0]
	fmt.Printf("%s seed %d: %s, %s\n", pt.Name, seed, providerNames[int(pt.Provider)%4], pt.label())
	for _, pol := range spotPolicies {
		r := pr.Runs[pol.name]
		fmt.Printf("  %-17s on time %4d/%-4d (%s)  burst $%-8.0f late $%-8.0f lost %6.1f slot-h  interruptions %4d  kills %4d  saves %4d  moved %4d\n",
			pol.name, r.OnTime, r.DueJobs, pct(r.onTimePct()), r.BurstCost, r.LateDollars, r.SpotLostSlotHours, r.SpotInterruptions, r.SpotKills, r.SpotSaves, r.SpotMoved)
	}
	for _, f := range pr.Viol {
		fmt.Printf("  %s (%s)\n", f.String(), f.Policy)
	}
	if len(pr.Viol) > 0 {
		return 1
	}
	return 0
}
