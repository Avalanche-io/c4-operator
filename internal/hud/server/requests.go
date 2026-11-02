package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A client that isn't a browser may sign each request it sends (request/v1,
// X-C4-Request) with its own key, in place of a bearer token (PEOPLE-KEYS
// 2.4). The HUD verifies it, holds it to the request it came with (method,
// target and body, byte for byte), and takes it once. Until K8 a request
// with no signature is read as before, and only a host runner's signed
// request is taken: its runner/<node> certificate, which the workload
// issuer gave it on its own account's token, makes it that account,
// c4-hosts/<node>, as its bearer token does. A person's, an agent's or a
// unit's signed request comes with K8.

// maxSignedBody bounds the body the HUD reads to check a signed request.
const maxSignedBody = 32 << 20

// requestViewer is the viewer of a request that carries its signature h.
func (s *Server) requestViewer(r *http.Request, h string) (*api.Viewer, error) {
	data, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("the signed request (%s) is not base64", schema.RequestHeader)
	}
	body, err := exactBody(r)
	if err != nil {
		return nil, err
	}
	v, req, err := provenance.VerifyRequest(data, s.trust(r.Context()).roots, time.Now(), s.actsSeen())
	if err != nil {
		return nil, fmt.Errorf("the signed request does not verify: %v", err)
	}
	if err := req.Matches(r.Method, r.RequestURI, body); err != nil {
		return nil, fmt.Errorf("the signed request is not this request: %v", err)
	}
	node, ok := strings.CutPrefix(v.Identity, provenance.IdentityBase+"runner/")
	if !ok || node == "" || strings.Contains(node, "/") {
		return nil, fmt.Errorf("a signed request by %s is not taken here yet: only a host runner's is, until every client signs", v.Identity)
	}
	// its machine's own account, as a reviewed token of that account
	// would be: the same user and groups, so access() reads them alike
	return s.access(r.Context(), HostAccountPrefix+node,
		[]string{"system:serviceaccounts", "system:serviceaccounts:" + hostsNamespace, "system:authenticated"})
}

// exactBody is the request's body as sent, byte for byte (empty for
// none), put back for the handler to read.
func exactBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxSignedBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSignedBody {
		return nil, errors.New("a signed request's body is over 32 MiB")
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

// signedKey holds a signed request's caller in its context.
type signedKey struct{}

type signedCaller struct {
	v   *api.Viewer
	err error
}

// signedOnce checks a signed request's signature once, before any handler
// asks who calls: a request is taken once, so a handler that asks twice
// would otherwise refuse its own request the second time.
func (s *Server) signedOnce(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sig := r.Header.Get(schema.RequestHeader); sig != "" {
			v, err := s.requestViewer(r, sig)
			r = r.WithContext(context.WithValue(r.Context(), signedKey{}, &signedCaller{v: v, err: err}))
		}
		h.ServeHTTP(w, r)
	})
}

// Each certificate a host runner asks the HUD's issuer for costs the farm
// a TokenReview and a signature, and a runner needs one about every half
// hour. So the route takes a few at once from one address, then one every
// few seconds, and at most a few a second from everyone together; past
// that it answers 429 with when to come back.
const (
	issuerPerClient = 6 * time.Second
	issuerBurst     = 10
	issuerAllPerSec = 5
	issuerAllBurst  = 20
	// issuerClients bounds the addresses remembered; past it, those quiet
	// for issuerQuiet are forgotten.
	issuerClients = 4096
	issuerQuiet   = 10 * time.Minute
)

// issuerLimits are the HUD's issuer route's token buckets: one per client
// address, and one for all.
type issuerLimits struct {
	mu      sync.Mutex
	all     *rate.Limiter
	clients map[string]*clientLimit
}

type clientLimit struct {
	l    *rate.Limiter
	seen time.Time
}

// allow reports whether addr may ask now, else how long until it may.
func (q *issuerLimits) allow(addr string, now time.Time) (bool, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.all == nil {
		q.all = rate.NewLimiter(issuerAllPerSec, issuerAllBurst)
		q.clients = map[string]*clientLimit{}
	}
	c := q.clients[addr]
	if c == nil {
		if len(q.clients) >= issuerClients {
			for a, o := range q.clients {
				if now.Sub(o.seen) > issuerQuiet {
					delete(q.clients, a)
				}
			}
		}
		c = &clientLimit{l: rate.NewLimiter(rate.Every(issuerPerClient), issuerBurst)}
		q.clients[addr] = c
	}
	c.seen = now
	if r := c.l.ReserveN(now, 1); !r.OK() || r.DelayFrom(now) > 0 {
		d := r.DelayFrom(now)
		r.CancelAt(now)
		return false, d
	} else if a := q.all.ReserveN(now, 1); !a.OK() || a.DelayFrom(now) > 0 {
		d := a.DelayFrom(now)
		a.CancelAt(now)
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// limitIssuer serves h, the issuer, within the route's limits.
func (s *Server) limitIssuer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := s.issuerLimits.allow(clientAddr(r), time.Now()); !ok {
			secs := int(wait/time.Second) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			fail(w, http.StatusTooManyRequests, "too many certificate requests: ask again in %d s", secs)
			return
		}
		h.ServeHTTP(w, r)
	})
}
