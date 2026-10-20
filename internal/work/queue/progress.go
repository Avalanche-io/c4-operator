package queue

import (
	"sync"
	"time"
)

// Progress a running unit reports (.c4/progress, read by the node daemon
// and served to the manager alone on GET /units, never as a metric)
// reaches the queue through the HUD's scrape of the node daemons, never
// through the API server (DESIGN.md 11.3). The HUD collector fills a ProgressCache after each scrape; the
// queue reads it when it estimates a running unit.

// progressStale is how long a reading stays usable without a newer one.
const progressStale = 2 * time.Minute

// ProgressCache holds the latest reported progress per unit pod. It is
// safe for concurrent use; the zero value is ready, and a nil cache has
// no readings.
type ProgressCache struct {
	mu sync.RWMutex
	at time.Time
	by map[[2]string]float64 // namespace, pod
}

// Set replaces every reading with those of one scrape taken at at.
func (c *ProgressCache) Set(at time.Time, readings map[[2]string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at, c.by = at, readings
}

// Get returns the progress pod reported, if a reading of it is fresh at
// now.
func (c *ProgressCache) Get(namespace, pod string, now time.Time) (float64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.by == nil || now.Sub(c.at) > progressStale {
		return 0, false
	}
	f, ok := c.by[[2]string{namespace, pod}]
	return f, ok
}
