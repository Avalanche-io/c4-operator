package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// The HUD's issuer routes answer anyone who reaches them, and each request
// costs the farm work: a TokenReview and a signature for a host runner's
// certificate, a store read or write for a person's enrolment. So each
// route takes a burst from one client address, then a steady pace, and a
// pace from everyone together; past that it answers 429 with when to come
// back. The address is clientAddr's: X-Forwarded-For only behind the SSO
// proxy, else the connection's.

// budget is one route's limits.
type budget struct {
	// perClient is how often one address may ask, past its burst.
	perClient time.Duration
	burst     int
	// allPerSec and allBurst are everyone's together.
	allPerSec rate.Limit
	allBurst  int
}

var (
	// hostIssuerBudget is /workload/v1/certificate's: a host runner needs
	// one certificate about every half hour, so a few at once and then
	// one every few seconds is plenty.
	hostIssuerBudget = budget{perClient: 6 * time.Second, burst: 10, allPerSec: 5, allBurst: 20}
	// peopleBudget is /issuer/v1/people/'s, looser per address: a studio's
	// people sign in from behind one NAT, together at the start of a day,
	// each browser reading the config, index and devices and enrolling,
	// and each CLI reading its code back every second while it waits. Two
	// hundred people's sign-ins at once (some five calls each) fit the
	// burst, and a steady ten a second carries a floor's ordinary use.
	peopleBudget = budget{perClient: 100 * time.Millisecond, burst: 1000, allPerSec: 50, allBurst: 2000}
)

const (
	// limitClients bounds the addresses a route remembers; past it, those
	// quiet for limitQuiet are forgotten.
	limitClients = 4096
	limitQuiet   = 10 * time.Minute
)

// routeLimits are one route's token buckets: one per client address, and
// one for all, made on first use from b.
type routeLimits struct {
	mu      sync.Mutex
	all     *rate.Limiter
	clients map[string]*clientLimit
	// now is the clock a request is measured by; nil is the wall clock. A
	// test sets it, so what refills between calls is what it says, not
	// what a loaded machine's scheduler leaves.
	now func() time.Time
}

type clientLimit struct {
	l    *rate.Limiter
	seen time.Time
}

// allow reports whether addr may ask now under b, else how long until it
// may.
func (q *routeLimits) allow(b budget, addr string, now time.Time) (bool, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.all == nil {
		q.all = rate.NewLimiter(b.allPerSec, b.allBurst)
		q.clients = map[string]*clientLimit{}
	}
	c := q.clients[addr]
	if c == nil {
		if len(q.clients) >= limitClients {
			for a, o := range q.clients {
				if now.Sub(o.seen) > limitQuiet {
					delete(q.clients, a)
				}
			}
		}
		c = &clientLimit{l: rate.NewLimiter(rate.Every(b.perClient), b.burst)}
		q.clients[addr] = c
	}
	c.seen = now
	r := c.l.ReserveN(now, 1)
	if !r.OK() || r.DelayFrom(now) > 0 {
		d := r.DelayFrom(now)
		r.CancelAt(now)
		return false, d
	}
	if a := q.all.ReserveN(now, 1); !a.OK() || a.DelayFrom(now) > 0 {
		d := a.DelayFrom(now)
		a.CancelAt(now)
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// limited serves h within q's limits under b.
func limited(q *routeLimits, b budget, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now
		if q.now != nil {
			now = q.now
		}
		if ok, wait := q.allow(b, clientAddr(r), now()); !ok {
			secs := int(wait/time.Second) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			fail(w, http.StatusTooManyRequests, "too many requests to the farm's issuer from here: ask again in %d s", secs)
			return
		}
		h.ServeHTTP(w, r)
	})
}
