package queue

import (
	"sort"
	"sync"
	"time"
)

// Safe points. A unit announces each moment it could stop and lose nothing
// it wants kept (c4-unit safepoint: the end of an agent turn, a frame or
// chunk boundary, a training checkpoint). The node daemon reports the last
// one (c4_task_safepoint_seq and _time_seconds), the HUD's scrape of the
// node daemons fills a SafepointCache, and the queue reads it: a unit that
// reports safe points can be asked to stop softly (it stops at its next
// one), and what stopping it now would lose is the time since its last.

// safepointForget is how long a unit's safe points are kept after the last
// scrape that saw it.
const safepointForget = 10 * time.Minute

// Safepoint is a unit's last safe point and how often it reaches them.
type Safepoint struct {
	Seq int
	At  time.Time
	// Every is the interval between safe points, learned from the ones
	// seen (0 until a second one is).
	Every time.Duration
}

// Next is when the unit is expected to reach its next safe point (zero
// while its cadence is not known).
func (s Safepoint) Next() time.Time {
	if s.Every <= 0 {
		return time.Time{}
	}
	return s.At.Add(s.Every)
}

// SafepointCache holds every unit pod's last safe point and cadence. It is
// safe for concurrent use; the zero value is ready, and a nil cache has no
// readings.
type SafepointCache struct {
	mu sync.RWMutex
	by map[[2]string]*spEntry // namespace, pod
}

type spEntry struct {
	sp   Safepoint
	seen time.Time // the last scrape that saw the unit
}

// Observe merges one scrape taken at at: each pod's last safe point. A pod
// that announced more since the last scrape updates its cadence: the time
// between the two divided by how many it passed, folded into a moving
// average so a unit whose rhythm changes is followed within a few safe
// points.
func (c *SafepointCache) Observe(at time.Time, readings map[[2]string]Safepoint) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = map[[2]string]*spEntry{}
	}
	for k, r := range readings {
		e := c.by[k]
		if e == nil {
			c.by[k] = &spEntry{sp: Safepoint{Seq: r.Seq, At: r.At}, seen: at}
			continue
		}
		e.seen = at
		if r.Seq <= e.sp.Seq || !r.At.After(e.sp.At) {
			if r.Seq < e.sp.Seq {
				e.sp = Safepoint{Seq: r.Seq, At: r.At} // a new attempt of the unit
			}
			continue
		}
		iv := r.At.Sub(e.sp.At) / time.Duration(r.Seq-e.sp.Seq)
		if e.sp.Every > 0 {
			iv = (7*e.sp.Every + 3*iv) / 10
		}
		e.sp = Safepoint{Seq: r.Seq, At: r.At, Every: iv}
	}
	for k, e := range c.by {
		if at.Sub(e.seen) > safepointForget {
			delete(c.by, k)
		}
	}
}

// Get returns pod's last safe point, if it announced one.
func (c *SafepointCache) Get(namespace, pod string) (Safepoint, bool) {
	if c == nil {
		return Safepoint{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e := c.by[[2]string{namespace, pod}]
	if e == nil {
		return Safepoint{}, false
	}
	return e.sp, true
}

// Cadence is the median interval between safe points of the pods given,
// for a unit whose own is not known yet (0 when none is).
func (c *SafepointCache) Cadence(namespace string, pods []string) time.Duration {
	var ivs []time.Duration
	for _, p := range pods {
		if sp, ok := c.Get(namespace, p); ok && sp.Every > 0 {
			ivs = append(ivs, sp.Every)
		}
	}
	if len(ivs) == 0 {
		return 0
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i] < ivs[j] })
	return ivs[len(ivs)/2]
}

// yieldGrace is how long a unit asked to stop softly has to reach a safe
// point before it is stopped (AnnotationYieldBy).
const yieldGrace = 10 * time.Minute

// safepointOf is the last safe point a running unit of v announced.
func (q *Queue) safepointOf(v *view, idx int) (Safepoint, bool) {
	pod := v.pods[idx]
	if pod == nil || q.Safepoints == nil {
		return Safepoint{}, false
	}
	return q.Safepoints.Get(pod.Namespace, pod.Name)
}

// noteSafepoints records each running unit's last safe point on its view,
// for the policy's snapshot. A safe point the node daemon set on the pod
// (asking for a decision) is newer than the last scrape's.
func (q *Queue) noteSafepoints(views []*view, now time.Time) {
	for _, v := range views {
		v.safe = nil
		for idx := range v.running {
			sp, ok := q.safepointOf(v, idx)
			if n, at := podSafepoint(v.pods[idx]); at && (!ok || n.Seq > sp.Seq) {
				pod := v.pods[idx]
				q.Safepoints.Observe(now, map[[2]string]Safepoint{{pod.Namespace, pod.Name}: {Seq: n.Seq, At: n.At}})
				if sp, ok = q.safepointOf(v, idx); !ok {
					sp, ok = Safepoint{Seq: n.Seq, At: n.At}, true
				}
			}
			if ok {
				if v.safe == nil {
					v.safe = map[int]Safepoint{}
				}
				v.safe[idx] = sp
			}
		}
	}
}
