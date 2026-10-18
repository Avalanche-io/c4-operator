package node

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/metrics"
)

// ReadProgress reads a running unit's progress line, .c4/progress at the
// root of its writable volume (here, the volume's upper directory): a
// number from 0 to 1. ok is false when the unit reports none.
func ReadProgress(upper string) (float64, bool) {
	f, err := os.Open(filepath.Join(upper, ".c4", "progress"))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil {
		return 0, false
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	v, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
	if err != nil || v != v {
		return 0, false
	}
	return min(1, max(0, v)), true
}

// RunProgress publishes the progress every running unit reports, as the
// c4_task_progress gauge by namespace and pod, until ctx ends. The HUD
// reads it from the node's metrics.
func (d *Daemon) RunProgress(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 2 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	shown := map[[2]string]bool{}
	for {
		shown = d.progressOnce(shown)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (d *Daemon) progressOnce(shown map[[2]string]bool) map[[2]string]bool {
	now := map[[2]string]bool{}
	for _, v := range d.Volumes() {
		if !v.Capture || v.ReadOnly || v.Pod == "" {
			continue
		}
		upper, _ := d.volumeDirs(v.Partition, v.ID)
		f, ok := ReadProgress(upper)
		if !ok {
			continue
		}
		k := [2]string{v.Namespace, v.Pod}
		metrics.NodeTaskProgress.WithLabelValues(k[0], k[1]).Set(f)
		now[k] = true
	}
	for k := range shown {
		if !now[k] {
			metrics.NodeTaskProgress.DeleteLabelValues(k[0], k[1])
		}
	}
	return now
}
