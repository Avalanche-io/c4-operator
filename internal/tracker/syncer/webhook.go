package syncer

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// deliveries remembers recent delivery ids, so a redelivered event is a
// no-op (ADAPTER.md 6.3). Bounded: the oldest are forgotten first.
type deliveries struct {
	mu    sync.Mutex
	ids   map[string]bool
	order []string
	max   int
}

func (d *deliveries) seen(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ids == nil {
		d.ids, d.max = map[string]bool{}, 10000
	}
	if d.ids[id] {
		return true
	}
	d.ids[id] = true
	d.order = append(d.order, id)
	if len(d.order) > d.max {
		delete(d.ids, d.order[0])
		d.order = d.order[1:]
	}
	return false
}

// Webhooks receives tracker deliveries at /webhooks/{namespace}/{tracker}.
// It verifies and parses each one, remembers what it marks, and answers
// at once (GitHub allows ten seconds); the work happens in the next pass,
// which Kick starts early. Deliveries are hints: polling and resync under
// them make a lost one harmless.
func (s *Syncer) Webhooks() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/{ns}/{name}", func(w http.ResponseWriter, r *http.Request) {
		var t c4v1.Tracker
		if err := s.Client.Get(r.Context(), client.ObjectKey{Namespace: r.PathValue("ns"), Name: r.PathValue("name")}, &t); err != nil {
			http.Error(w, "no such tracker", http.StatusNotFound)
			return
		}
		c, err := s.conn(&t)
		if err != nil {
			http.Error(w, "tracker not ready", http.StatusServiceUnavailable)
			return
		}
		d, err := c.adapter.Receive(r)
		switch {
		case errors.Is(err, tracker.ErrBadSignature):
			http.Error(w, "signature does not verify", http.StatusUnauthorized)
			return
		case errors.Is(err, tracker.ErrUnsupported):
			http.Error(w, "this tracker does not push", http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if s.seenDelivery(d.ID) || d.Self {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.mu.Lock()
		for _, ref := range d.Refs {
			c.dirty[ref.String()] = true
		}
		c.pending = append(c.pending, d.Decisions...)
		s.mu.Unlock()
		s.Kick()
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}

func (s *Syncer) seenDelivery(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	if s.seen == nil {
		s.seen = &deliveries{}
	}
	d := s.seen
	s.mu.Unlock()
	return d.seen(id)
}

// Kick asks Run for a pass now.
func (s *Syncer) Kick() {
	s.mu.Lock()
	ch := s.kick
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Run passes over every Tracker every interval, and at once when a
// delivery kicks it, until ctx ends.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) error {
	s.mu.Lock()
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
	ch := s.kick
	s.mu.Unlock()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := s.SyncAll(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error(err, "pass")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case <-ch:
		}
	}
}
