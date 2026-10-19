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

// RunProgress publishes the progress and the last safe point every running
// unit reports, as the c4_task_progress and c4_task_safepoint_* gauges by
// namespace and pod, until ctx ends. The HUD reads them from the node's
// metrics and passes them to the queue.
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
		k := [2]string{v.Namespace, v.Pod}
		if f, ok := ReadProgress(upper); ok {
			metrics.NodeTaskProgress.WithLabelValues(k[0], k[1]).Set(f)
			now[k] = true
		}
		if sp, ok := ReadSafepoint(upper); ok {
			metrics.NodeTaskSafepoint.WithLabelValues(k[0], k[1]).Set(float64(sp.Seq))
			metrics.NodeTaskSafepointTime.WithLabelValues(k[0], k[1]).Set(float64(sp.At.UnixMilli()) / 1000)
			now[k] = true
		}
	}
	for k := range shown {
		if !now[k] {
			metrics.NodeTaskProgress.DeleteLabelValues(k[0], k[1])
			metrics.NodeTaskSafepoint.DeleteLabelValues(k[0], k[1])
			metrics.NodeTaskSafepointTime.DeleteLabelValues(k[0], k[1])
		}
	}
	return now
}

// Safepoint is the last safe point a unit announced (c4-unit safepoint):
// its number and when.
type Safepoint struct {
	Seq        int
	At         time.Time
	Checkpoint string
}

// ReadSafepoint reads a running unit's .c4/safepoint ("seq N", "at TIME"
// lines) at the root of its writable volume's upper directory. ok is false
// when the unit announced none.
func ReadSafepoint(upper string) (Safepoint, bool) {
	f, err := os.Open(filepath.Join(upper, ".c4", "safepoint"))
	if err != nil {
		return Safepoint{}, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return Safepoint{}, false
	}
	var sp Safepoint
	for _, l := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		switch k {
		case "seq":
			sp.Seq, _ = strconv.Atoi(v)
		case "at":
			sp.At, _ = time.Parse(time.RFC3339Nano, v)
		case "checkpoint":
			sp.Checkpoint = v
		}
	}
	if sp.Seq <= 0 || sp.At.IsZero() {
		return Safepoint{}, false
	}
	return sp, true
}
