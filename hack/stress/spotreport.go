package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// spotResult is every spot policy's run at one spot point.
type spotResult struct {
	Pt   Point
	Seed uint64
	Runs map[string]*runResult
	Viol []found
}

// runSpotGrid runs each spot point under every spot policy.
func runSpotGrid(pts []Point, seeds []uint64, workers int, progress bool) []*spotResult {
	type task struct {
		pr  *spotResult
		pol policy
	}
	var out []*spotResult
	var tasks []task
	for _, pt := range pts {
		for _, s := range seeds {
			pr := &spotResult{Pt: pt, Seed: s, Runs: map[string]*runResult{}}
			out = append(out, pr)
			for _, pol := range spotPolicies {
				tasks = append(tasks, task{pr, pol})
			}
		}
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].pr.Pt.Nodes > tasks[j].pr.Pt.Nodes })
	var mu sync.Mutex
	var wg sync.WaitGroup
	ch := make(chan task)
	done := 0
	for range max(1, workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				t0 := time.Now()
				w := spotWorkload(build(t.pr.Pt, t.pr.Seed), t.pol.name)
				r := newEngine(w, t.pol, t.pol.checked).run()
				mu.Lock()
				t.pr.Runs[t.pol.name] = r
				done++
				if progress {
					fmt.Fprintf(os.Stderr, "[spot %d/%d] %s %s %s: %s, %s on time, $%.0f\n", done, len(tasks), t.pr.Pt.Name,
						providerNames[int(t.pr.Pt.Provider)%4], t.pol.name, time.Since(t0).Round(time.Millisecond), pct(r.onTimePct()), r.BurstCost)
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
	for _, pr := range out {
		rp := repro(pr.Pt, pr.Seed)
		for _, name := range []string{"cheapest-first"} {
			r := pr.Runs[name]
			if r == nil {
				continue
			}
			for _, inv := range sortedKeys(r.Counts) {
				var ex violation
				for _, x := range r.Examples {
					if x.Inv == inv {
						ex = x
						break
					}
				}
				_, known := knownBugs[inv]
				pr.Viol = append(pr.Viol, found{violation: ex, Point: pr.Pt.Name, Policy: name, Repro: rp + " -spot", Known: known, N: r.Counts[inv]})
			}
		}
	}
	return out
}

// spotSummary is a policy's totals over a set of spot points.
type spotSummary struct {
	onTime                                 []float64
	cost, lost, late                       float64
	interrupts, kills, saves, moved, risky int
	byLen                                  [4]float64
}

func summarize(rs []*spotResult, pol string, keep func(*spotResult) bool) spotSummary {
	var s spotSummary
	for _, pr := range rs {
		if !keep(pr) {
			continue
		}
		r := pr.Runs[pol]
		if r == nil {
			continue
		}
		if x := r.onTimePct(); !math.IsNaN(x) {
			s.onTime = append(s.onTime, x)
		}
		s.cost += r.BurstCost
		s.lost += r.SpotLostSlotHours
		s.late += r.LateDollars
		s.interrupts += r.SpotInterruptions
		s.kills += r.SpotKills
		s.saves += r.SpotSaves
		s.moved += r.SpotMoved
		s.risky += r.SpotRiskyUnits
		for i := range s.byLen {
			s.byLen[i] += r.SpotLostByLen[i]
		}
	}
	return s
}

// writeSpot writes spot.md and its heatmaps.
func writeSpot(dir string, rs []*spotResult, quick bool, took time.Duration) error {
	if len(rs) == 0 {
		return nil
	}
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	cmd := "go run ./hack/stress"
	if quick {
		cmd += " -quick"
	}
	p("# Spot capacity by provider\n\n")
	p("`%s` ran %d spot points (a Latin hypercube over provider, interruption band and diversification, capacity-crunch scale, discount, maximum price, Azure eviction, AWS rebalance recommendations, checkpoint write time, farm size, load, deadlines and unit length) under four policies, in %s. ",
		cmd, len(rs), took.Round(time.Second))
	p("Each point's burst capacity is an on-demand pool and a spot pool per pool class; the provider models and every assumed number are in `hack/stress/spot.go`.\n\n")
	p("- `all-spot`: `sched.Decide` with only the spot pools.\n- `all-on-demand`: only the on-demand pools.\n- `cheapest-first`: both; `sched.Decide` picks the cheaper pool (spot) first.\n")
	p("Every policy has the same burst ceiling: a policy with one pool gets both pools' limits.\n")
	p("- `completion-aware`: both, with the wrapper in `spot.go` moving a unit to on-demand when its expected interruption loss (hazard x remaining time x (work since its last checkpoint + lateness cost of the delay)) exceeds the on-demand premium, and checkpointing or moving units on a rebalance recommendation or a notice.\n\n")
	p("## By provider\n\nMeans per point; cost is burst spend (spot and on-demand), lost is spot work thrown away by interruptions.\n\n")
	p("| Provider | Policy | On time | Burst $ per point | Lateness $ per point | Lost slot-hours per point | Interruptions | Units killed | Saves on a warning | Units moved on demand |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, prov := range []string{"aws", "gcp", "azure"} {
		keep := func(pr *spotResult) bool { return providerNames[int(pr.Pt.Provider)%4] == prov }
		n := 0
		for _, pr := range rs {
			if keep(pr) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		for _, pol := range spotPolicies {
			s := summarize(rs, pol.name, keep)
			m, _ := meanSD(s.onTime)
			fn := float64(n)
			p("| %s (%d) | `%s` | %s | $%.0f | $%.0f | %.1f | %d | %d | %d | %d |\n", prov, n, pol.name, pct(m), s.cost/fn, s.late/fn, s.lost/fn, s.interrupts, s.kills, s.saves, s.moved)
		}
	}
	p("\n## Exposure by unit length\n\nSpot work lost to interruptions, in slot-hours over all points, by the length of the unit that was running. Short frames lose little when interrupted: the frame granularity is what bounds a render's exposure, and a long unit without checkpoints is what the completion-aware policy keeps off spot.\n\n")
	p("| Policy |")
	for _, l := range lenBins {
		p(" %s |", l)
	}
	p("\n|---|")
	for range lenBins {
		p("---|")
	}
	p("\n")
	for _, pol := range spotPolicies {
		s := summarize(rs, pol.name, func(*spotResult) bool { return true })
		p("| `%s` |", pol.name)
		for _, x := range s.byLen {
			p(" %.1f |", x)
		}
		p("\n")
	}
	p("\n")
	// Heatmaps: completion-aware against cheapest-first.
	hz := []float64{1, 3, 10, 31}
	ul := []float64{2, 10, 30, 121}
	type hmap struct {
		file, title string
		val         func(*spotResult) (float64, bool)
		kind        rampKind
		format      string
	}
	maps := []hmap{
		{"spot-ontime-delta.svg", "On time, completion-aware minus cheapest-first (points)", func(pr *spotResult) (float64, bool) {
			a, c := pr.Runs["completion-aware"], pr.Runs["cheapest-first"]
			if a == nil || c == nil || a.DueJobs == 0 {
				return 0, false
			}
			return a.onTimePct() - c.onTimePct(), true
		}, rampDiverging, "%+.0f"},
		{"spot-cost-delta.svg", "Burst $ plus lateness $, completion-aware minus cheapest-first", func(pr *spotResult) (float64, bool) {
			a, c := pr.Runs["completion-aware"], pr.Runs["cheapest-first"]
			if a == nil || c == nil {
				return 0, false
			}
			return -((a.BurstCost + a.LateDollars) - (c.BurstCost + c.LateDollars)), true
		}, rampDiverging, "%+.0f"},
		{"spot-lost.svg", "Spot work lost per point under all-spot (slot-hours)", func(pr *spotResult) (float64, bool) {
			r := pr.Runs["all-spot"]
			if r == nil {
				return 0, false
			}
			return r.SpotLostSlotHours, true
		}, rampOrange, "%.1f"},
	}
	p("## Heatmaps\n\nOver capacity-crunch scale (hazard multiplier) and median unit length in minutes. In the cost map, blue is money saved by the completion-aware policy (burst spend plus lateness cost), red money lost.\n\n")
	for _, m := range maps {
		g := newGrid(len(ul)-1, len(hz)-1)
		for _, pr := range rs {
			v, ok := m.val(pr)
			if !ok || math.IsNaN(v) {
				continue
			}
			g.add(bin(ul, pr.Pt.UnitMin), bin(hz, pr.Pt.HazardScale), v)
		}
		svg := heatmapSVG(m.title, "median unit length (min)", "hazard scale", ul, hz, g, m.kind, m.format)
		if err := os.WriteFile(filepath.Join(dir, m.file), []byte(svg), 0o644); err != nil {
			return err
		}
		p("![%s](%s)\n\n", m.title, m.file)
	}
	var viol []found
	for _, pr := range rs {
		viol = append(viol, pr.Viol...)
	}
	p("## Invariants on spot points\n\n")
	if len(viol) == 0 {
		p("The invariants held on every decision of `cheapest-first` at every spot point.\n\n")
	} else {
		for _, f := range viol {
			p("- %s `%s`: %s %s\n  `%s`\n", f.Point, f.Inv, f.Job, mdEscape(f.Detail), f.Repro)
		}
		p("\n")
	}
	p("## Points\n\n| Point | Provider | Nodes | Load | Unit min | Hazard x | Band | Diversity | Max price |")
	for _, pol := range spotPolicies {
		p(" %s on time, $ |", pol.name)
	}
	p("\n|---|---|---|---|---|---|---|---|---|")
	for range spotPolicies {
		p("---|")
	}
	p("\n")
	for _, pr := range rs {
		pt := pr.Pt
		p("| %s | %s | %d | %.2g | %.3g | %.3g | %d | %d | %.2g |", pt.Name, providerNames[int(pt.Provider)%4], int(pt.Nodes), pt.Load, pt.UnitMin, pt.HazardScale, int(pt.SpotBand), int(pt.Diversity), pt.MaxPrice)
		for _, pol := range spotPolicies {
			if r := pr.Runs[pol.name]; r != nil {
				p(" %d/%d, $%.0f |", r.OnTime, r.DueJobs, r.BurstCost)
			} else {
				p(" |")
			}
		}
		p("\n")
	}
	p("\nRerun a point with `go run ./hack/stress -point '<encoded point>' -spot`; the encoded points are in `spot.csv`.\n")
	var c strings.Builder
	c.WriteString("point,provider,policy,on_time,due,burst_usd,late_usd,lost_slot_hours,interruptions,kills,saves,moved,point_encoded\n")
	for _, pr := range rs {
		for _, pol := range spotPolicies {
			if r := pr.Runs[pol.name]; r != nil {
				fmt.Fprintf(&c, "%s,%s,%s,%d,%d,%.2f,%.2f,%.2f,%d,%d,%d,%d,%q\n", pr.Pt.Name, providerNames[int(pr.Pt.Provider)%4], pol.name,
					r.OnTime, r.DueJobs, r.BurstCost, r.LateDollars, r.SpotLostSlotHours, r.SpotInterruptions, r.SpotKills, r.SpotSaves, r.SpotMoved, pr.Pt.encode())
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "spot.csv"), []byte(c.String()), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "spot.md"), []byte(b.String()), 0o644)
}
