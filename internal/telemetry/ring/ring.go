// Package ring keeps what a diagnostic bundle is made of (DESIGN.md
// section 6.9). At tier b (diagnostic bundles), the farm keeps the last
// two hours of the policy's decision-changing ticks, redacted as they are
// recorded and bounded at 64 MiB, so a bundle covers the moment a problem
// happened. At tier a (off) nothing is recorded in advance and a bundle
// holds only the present moment. Either way the ring holds the latest
// decision's input in memory, unredacted, until a bundle redacts it; that
// copy is never written anywhere.
package ring

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// The ring's defaults: two hours, 64 MiB.
const (
	DefaultWindow   = 2 * time.Hour
	DefaultMaxBytes = 64 << 20
)

// Tier is how much telemetry the farm keeps.
type Tier int

// The tiers a farm can run at today: off and diagnostic bundles.
const (
	// TierOff records nothing; a bundle holds the present moment only.
	TierOff Tier = iota
	// TierBundles keeps the ring for diagnostic bundles, locally.
	TierBundles
)

// TierFrom is the tier a farm asks for, held at off when the kill switch
// C4_TELEMETRY=off is set in the manager's environment.
func TierFrom(ring bool) Tier {
	if os.Getenv("C4_TELEMETRY") == "off" || !ring {
		return TierOff
	}
	return TierBundles
}

// Ring records the policy's decisions. Its zero value is not usable; use
// New.
type Ring struct {
	mu       sync.Mutex
	tier     Tier
	policy   telemetry.PolicyKind
	key      func(context.Context) (telemetry.Key, error)
	window   time.Duration
	maxBytes int

	recs    []entry
	bytes   int
	dropped bool
	last    [32]byte // the kept snapshot's decision hash
	noted   bool
	change  [32]byte // the last tick's change key: a redaction only when it changes

	present *input
	red     *telemetry.Redactor // made with the key on first use: one money factor per ring
}

type entry struct {
	at   time.Time // the farm's time, for the window; never written
	size int
	rec  telemetry.Snapshot
}

type input struct {
	s      sched.Snapshot
	before sched.StateDoc
	cfg    sched.Config
	d      sched.Decisions
}

// New is a ring at tier for a farm deciding with policy, naming
// pseudonyms with the key key returns.
func New(tier Tier, policy telemetry.PolicyKind, key func(context.Context) (telemetry.Key, error)) *Ring {
	return &Ring{tier: tier, policy: policy, key: key, window: DefaultWindow, maxBytes: DefaultMaxBytes}
}

// SetBounds changes the window and the byte bound (the farm's settings).
func (r *Ring) SetBounds(window time.Duration, maxBytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if window > 0 {
		r.window = window
	}
	if maxBytes > 0 {
		r.maxBytes = maxBytes
	}
}

// Tier is the ring's tier.
func (r *Ring) Tier() Tier {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tier
}

// Note records one decision: the snapshot, the policy's state before it,
// the configuration and what it decided. The queue calls it every tick. At
// tier b a tick that changed a decision is redacted and kept.
func (r *Ring) Note(s sched.Snapshot, before sched.StateDoc, cfg sched.Config, d sched.Decisions) {
	r.mu.Lock()
	r.present = &input{s: s, before: before, cfg: cfg, d: d}
	tier := r.tier
	r.mu.Unlock()
	if tier != TierBundles {
		return
	}
	change := telemetry.ChangeKey(d)
	r.mu.Lock()
	same := r.noted && change == r.change
	r.noted, r.change = true, change
	r.mu.Unlock()
	if same {
		return
	}
	red, err := r.redactor(context.Background())
	if err != nil {
		return
	}
	rec, _ := red.Redact(s, sched.StateFrom(before), cfg, d)
	out, err := telemetry.Encode(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.DecisionHash == r.last && len(r.recs) > 0 {
		return
	}
	r.last = rec.DecisionHash
	r.recs = append(r.recs, entry{at: s.Now, size: len(out), rec: rec})
	r.bytes += len(out)
	for len(r.recs) > 0 && (r.bytes > r.maxBytes || s.Now.Sub(r.recs[0].at) > r.window) {
		r.bytes -= r.recs[0].size
		r.recs = r.recs[1:]
		r.dropped = true
	}
}

// Snapshots is what a bundle holds: the ring's snapshots within window of
// the latest (all of them for 0), oldest first, then the present moment
// redacted now. Dropped says older ones were let go to stay in bounds.
func (r *Ring) Snapshots(ctx context.Context, window time.Duration) (snaps []telemetry.Snapshot, dropped bool, err error) {
	red, err := r.redactor(ctx)
	if err != nil {
		return nil, false, err
	}
	r.mu.Lock()
	recs := append([]entry(nil), r.recs...)
	present := r.present
	dropped = r.dropped
	last := r.last
	r.mu.Unlock()
	var latest time.Time
	if present != nil {
		latest = present.s.Now
	}
	for _, e := range recs {
		if window > 0 && !latest.IsZero() && latest.Sub(e.at) > window {
			continue
		}
		snaps = append(snaps, e.rec)
	}
	if present != nil {
		rec, _ := red.Redact(present.s, sched.StateFrom(present.before), present.cfg, present.d)
		if len(snaps) == 0 || rec.DecisionHash != last {
			snaps = append(snaps, rec)
		}
	}
	return snaps, dropped, nil
}

// redactor is the ring's redactor, made on first use with the farm key, so
// every snapshot of a bundle shares one secret money factor and the key is
// read once.
func (r *Ring) redactor(ctx context.Context) (*telemetry.Redactor, error) {
	r.mu.Lock()
	red := r.red
	r.mu.Unlock()
	if red != nil {
		return red, nil
	}
	key, err := r.key(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.red == nil {
		r.red = telemetry.NewRedactor(key)
		r.red.Policy = r.policy
	}
	return r.red, nil
}
