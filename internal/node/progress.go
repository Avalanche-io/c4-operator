package node

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/auth"
	"github.com/Avalanche-io/c4-operator/internal/metrics"
	"github.com/Avalanche-io/c4-operator/internal/tier"
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

// UnitReport is what one running unit last reported: its progress line
// and its last safe point. Pod names carry job names, so reports never go
// into metrics; the HUD reads them from GET /units with its tier token.
type UnitReport struct {
	Namespace string          `json:"namespace"`
	Pod       string          `json:"pod"`
	Progress  *float64        `json:"progress,omitempty"`
	Safepoint *SafepointShown `json:"safepoint,omitempty"`
}

// SafepointShown is a unit's last safe point as GET /units shows it.
type SafepointShown struct {
	Seq int       `json:"seq"`
	At  time.Time `json:"at"`
}

// RunProgress reads the progress and the last safe point every running
// unit reports, for GET /units, until ctx ends. The HUD reads them there
// and passes them to the queue.
func (d *Daemon) RunProgress(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 2 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		d.progressOnce()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (d *Daemon) progressOnce() {
	var out []UnitReport
	for _, v := range d.Volumes() {
		if !v.Capture || v.ReadOnly || v.Pod == "" {
			continue
		}
		upper, _ := d.volumeDirs(v.Partition, v.ID)
		r := UnitReport{Namespace: v.Namespace, Pod: v.Pod}
		if f, ok := ReadProgress(upper); ok {
			r.Progress = &f
		}
		if sp, ok := ReadSafepoint(upper); ok {
			r.Safepoint = &SafepointShown{Seq: sp.Seq, At: sp.At.UTC()}
		}
		if r.Progress != nil || r.Safepoint != nil {
			out = append(out, r)
		}
	}
	d.unitsMu.Lock()
	d.units = out
	d.unitsMu.Unlock()
}

// unitsHandler serves GET /units: the reports, to the manager only
// (authorize; nil admits anyone, in tests).
func (d *Daemon) unitsHandler(authorize tier.Authorizer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authorize != nil {
			if _, status, err := authorize(r); err != nil {
				http.Error(w, err.Error(), status)
				return
			}
		}
		d.unitsMu.Lock()
		out := d.units
		d.unitsMu.Unlock()
		if out == nil {
			out = []UnitReport{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
}

// peerHandler is the peer port's handler: the tier API (srv), /metrics
// for a scraper the API server lets get it, and /units for the manager.
func (d *Daemon) peerHandler(srv http.Handler) http.Handler {
	mux := http.NewServeMux()
	if d.Reviewer != nil {
		mux.Handle("/metrics", (&auth.MetricsGuard{Reviewer: d.Reviewer}).Wrap(metrics.Handler()))
		mux.Handle("GET /units", d.unitsHandler(d.Reviewer.TierAuthorizer(d.Cfg.Namespace+"/c4-manager")))
	} else {
		mux.Handle("/metrics", metrics.Handler())
		mux.Handle("GET /units", d.unitsHandler(nil))
	}
	mux.Handle("/", srv)
	return mux
}

// Safepoint is the last safe point a unit announced (c4-unit safepoint):
// its number and when.
type Safepoint struct {
	Seq        int
	At         time.Time
	Checkpoint string
	// Wait is the wait the unit asks for there (c4-unit wait), as JSON:
	// it stops and resumes when the wait is decided.
	Wait json.RawMessage
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
		case "wait":
			if json.Valid([]byte(v)) {
				sp.Wait = json.RawMessage(v)
			}
		}
	}
	if sp.Seq <= 0 || sp.At.IsZero() {
		return Safepoint{}, false
	}
	return sp, true
}
