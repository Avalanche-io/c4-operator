package node

import (
	"context"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/metrics"
)

// RunGC keeps the cache within budget until ctx ends: layers no mounted
// volume uses and past their retention go first, least recently used
// first, then objects no layer links.
func (d *Daemon) RunGC(ctx context.Context) {
	interval := d.Cfg.GCInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		d.gcOnce()
	}
}

func (d *Daemon) gcOnce() {
	inUse := map[string]bool{}
	for _, v := range d.Volumes() {
		for _, k := range v.Layers {
			inUse[k] = true
		}
	}
	over := func() bool { return d.caches.Bytes() > d.caches.Budget() }
	d.mu.Lock()
	stores := make([]interface {
		GC(map[string]bool, time.Duration, func() bool) int
	}, 0, len(d.layers))
	for _, ls := range d.layers {
		stores = append(stores, ls)
	}
	d.mu.Unlock()
	for _, ls := range stores {
		if !over() {
			break
		}
		// Removing a layer frees nothing by itself; its objects become
		// evictable. Evict after each store so we stop as soon as we can.
		if n := ls.GC(inUse, d.Cfg.Retain, over); n > 0 {
			metrics.NodeEvictions.WithLabelValues("layer").Add(float64(n))
			st := d.caches.Evict()
			metrics.NodeEvictions.WithLabelValues("object").Add(float64(st.Objects))
		}
	}
	if over() {
		st := d.caches.Evict()
		metrics.NodeEvictions.WithLabelValues("object").Add(float64(st.Objects))
	}
	metrics.NodeCacheBytes.WithLabelValues().Set(float64(d.caches.Bytes()))
}
