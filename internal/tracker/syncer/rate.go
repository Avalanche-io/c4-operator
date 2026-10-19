package syncer

import (
	"net/http"
	"strconv"

	"golang.org/x/time/rate"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// limiter is one Tracker's token bucket: c4-sync's own budget, set well
// under the tracker's published limit (ADAPTER.md 6.4).
type limiter struct {
	l *rate.Limiter
}

func newLimiter(r c4v1.TrackerRate) *limiter {
	per := 1.0
	if r.PerSecond != "" {
		if v, err := strconv.ParseFloat(r.PerSecond, 64); err == nil && v > 0 {
			per = v
		}
	}
	burst := int(r.Burst)
	if burst <= 0 {
		burst = 10
	}
	return &limiter{l: rate.NewLimiter(rate.Limit(per), burst)}
}

// rateTransport waits for the bucket before every request.
type rateTransport struct {
	next http.RoundTripper
	lim  *limiter
}

func (t *rateTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.lim.l.Wait(r.Context()); err != nil {
		return nil, err
	}
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}
