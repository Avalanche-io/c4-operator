// Command measure runs the git importer over a repository into a sink
// that stores nothing, to separate the importer's own cost (git, hashing,
// records) from the store's write latency. `c4-operator import-git`
// measures the same import against real stores.
//
//	go run ./hack/e2e/monorepo/measure -repo /tmp/mono/repo
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/gitimport"
)

// nullSink accepts every object without storing it.
type nullSink struct{}

func (nullSink) Put(context.Context, c4.ID, int64, io.Reader) (bool, error) { return true, nil }

func (nullSink) Copy(context.Context, c4.ID, int64, gitimport.Source) (bool, error) {
	return true, nil
}

func main() {
	repo := flag.String("repo", "", "repository")
	runs := flag.Int("runs", 3, "repetitions")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile here")
	flag.Parse()
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	src, err := gitimport.OpenTreeSource(filepath.Join(*repo, ".c4", "store"))
	if err != nil {
		log.Fatal(err)
	}
	for run := range *runs {
		sum, err := gitimport.Run(context.Background(), gitimport.Options{Repo: *repo, Sink: nullSink{}, C4Git: src})
		if err != nil {
			log.Panic(err)
		}
		var ms, rb []float64
		var base float64
		for _, c := range sum.Commits {
			if c.Base {
				base = c.Millis
				continue
			}
			ms = append(ms, c.Millis)
			rb = append(rb, float64(c.RecordBytes))
		}
		sort.Float64s(rb)
		sort.Float64s(ms)
		var total float64
		for _, m := range ms {
			total += m
		}
		p := func(q float64) float64 { return ms[int(float64(len(ms)-1)*q/100)] }
		fmt.Printf("run %d: %d commits in %s; base %.1fms; per commit p50 %.2fms p90 %.2fms p99 %.2fms max %.2fms mean %.2fms; new record bytes per commit p50 %.0f p90 %.0f\n",
			run+1, len(sum.Commits), sum.Elapsed.Round(1e6), base, p(50), p(90), p(99), ms[len(ms)-1], total/float64(len(ms)),
			rb[len(rb)/2], rb[int(float64(len(rb)-1)*0.9)])
	}
}
