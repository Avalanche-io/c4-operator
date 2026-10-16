// Command sim replays a synthetic week of studio work (render, CI and ML)
// under several scheduling policies, calling the production policy code in
// internal/work/sched, and runs a second experiment on remaining-time
// estimates under sequential and binary frame order. It writes
// hack/sim/out/results.md and CSVs next to it.
//
//	go run ./hack/sim
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

func main() {
	seeds := flag.Int("seeds", 5, "number of seeds (1..n)")
	out := flag.String("out", "hack/sim/out", "output directory")
	only := flag.String("policy", "", "run only these policies, comma-separated (debugging)")
	skipOrder := flag.Bool("skip-order", false, "skip the binary-order experiment")
	trials := flag.Int("order-trials", 40, "binary-order trials per (frames, multiplier)")
	flag.Parse()

	started := time.Now()
	p := defaultParams()
	var seedList []uint64
	for i := 1; i <= *seeds; i++ {
		seedList = append(seedList, uint64(i))
	}
	pols := policies
	if *only != "" {
		pols = nil
		for _, ps := range policies {
			for _, n := range strings.Split(*only, ",") {
				if ps.name == n {
					pols = append(pols, ps)
				}
			}
		}
	}
	results := runAll(p, seedList, pols, mainFarm)
	var tight []policySpec
	for _, ps := range pols {
		if !ps.ablation {
			tight = append(tight, ps)
		}
	}
	tightRes := runAll(p, seedList, tight, gpuTightFarm())
	var ord *orderResults
	if !*skipOrder {
		ord = runOrderExperiment(*trials)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := writeReport(*out, p, seedList, pols, results, tight, tightRes, ord, time.Since(started)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s/results.md in %s\n", *out, time.Since(started).Round(time.Second))
}

// runAll runs every (policy, seed) pair in parallel. Each run owns its
// engine and policy State; the workload and history are read-only.
func runAll(p params, seeds []uint64, pols []policySpec, f farm) map[string][]*runResult {
	type task struct {
		pol  policySpec
		seed uint64
		w    *workload
	}
	hist := map[uint64]*history{}
	loads := map[uint64]*workload{}
	for _, s := range seeds {
		loads[s] = generate(p, s)
		hist[s] = buildHistory(p, loads[s].targets)
	}
	tasks := make(chan task)
	var mu sync.Mutex
	res := map[string][]*runResult{}
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range tasks {
				t0 := time.Now()
				r := newEngine(t.w, hist[t.seed], t.seed, t.pol, f).run()
				fmt.Fprintf(os.Stderr, "%s %-34s seed %d: %s, %d decisions\n", f.name, t.pol.name, t.seed, time.Since(t0).Round(time.Millisecond), r.decides)
				mu.Lock()
				res[t.pol.name] = append(res[t.pol.name], r)
				mu.Unlock()
			}
		}()
	}
	for _, pol := range pols {
		for _, s := range seeds {
			tasks <- task{pol, s, loads[s]}
		}
	}
	close(tasks)
	wg.Wait()
	for _, rs := range res {
		sortBySeed(rs)
	}
	return res
}

func sortBySeed(rs []*runResult) {
	for i := 1; i < len(rs); i++ {
		for k := i; k > 0 && rs[k].Seed < rs[k-1].Seed; k-- {
			rs[k], rs[k-1] = rs[k-1], rs[k]
		}
	}
}
