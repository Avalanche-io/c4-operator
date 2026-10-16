// Package tier is the content API every cache tier speaks, and the client
// that walks the tiers.
//
//	GET  /{c4id}   object bytes; Range: bytes=N- resumes (206)
//	HEAD /{c4id}   200 if this tier can serve the object, 404 if not
//	PUT  /         body is content; this tier computes its ID (regional only)
//
// Requests carry X-C4-Project (the cache partition and credential scope),
// Authorization (the caller's projected service-account token), and
// X-C4-Hop: peer on node-to-node requests, which a peer never forwards to
// another peer. A tier at capacity answers 503 with Retry-After and never
// forwards the excess upstream.
//
// The path shape is c4d's content plane, so a c4d server can sit at the
// authoritative end unchanged.
package tier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// Header names.
const (
	HeaderProject = "X-C4-Project"
	HeaderHop     = "X-C4-Hop"
	HeaderID      = "X-C4-ID"
	HeaderServed  = "X-C4-Served"
	HopPeer       = "peer"
)

// Errors a Source returns.
var (
	// ErrNotFound: the source does not have the object. From an
	// authoritative source it means the object does not exist.
	ErrNotFound = errors.New("not found")
	// ErrBusy: the source is shedding load; try another candidate.
	ErrBusy = errors.New("busy")
	// ErrDenied: the source refused the caller.
	ErrDenied = errors.New("denied")
)

// Source is one place an object can be read from: a peer, a regional
// replica, or a store shard.
type Source interface {
	Name() string
	// Kind labels metrics: peer, regional, or store.
	Kind() string
	// Authoritative sources answer NotFound only for objects that do not
	// exist.
	Authoritative() bool
	// Get reads id from offset. total is the object's size (-1 if
	// unknown); start is where the body begins (0 if the source restarted).
	Get(ctx context.Context, project string, id c4.ID, offset int64) (body io.ReadCloser, total, start int64, err error)
}

// StatusError is an HTTP answer a tier did not expect. The peer is up and
// answered, so it does not open a circuit breaker.
type StatusError struct {
	Source string
	Code   int
	Msg    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.Source, e.Code, e.Msg)
}

// Unreachable reports whether err means a source could not be reached or
// dropped the connection, as opposed to answering with an error. Only
// these open circuit breakers.
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrBusy) || errors.Is(err, ErrDenied) {
		return false
	}
	return true
}

// TokenFunc returns the bearer token to present upstream.
type TokenFunc func() string

// HTTPSource is a peer node daemon or a regional replica.
type HTTPSource struct {
	SourceName string
	SourceKind string
	Base       string // http://host:port
	Auth       bool   // authoritative
	Hop        string // HopPeer for node-to-node requests
	Token      TokenFunc
	Client     *http.Client
}

// Name returns the member name.
func (s *HTTPSource) Name() string { return s.SourceName }

// Kind returns peer or regional.
func (s *HTTPSource) Kind() string { return s.SourceKind }

// Authoritative reports whether NotFound is final.
func (s *HTTPSource) Authoritative() bool { return s.Auth }

// Get reads from the tier API.
func (s *HTTPSource) Get(ctx context.Context, project string, id c4.ID, offset int64) (io.ReadCloser, int64, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base+"/"+id.String(), nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set(HeaderProject, project)
	if s.Hop != "" {
		req.Header.Set(HeaderHop, s.Hop)
	}
	if s.Token != nil {
		if t := s.Token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, resp.ContentLength, 0, nil
	case http.StatusPartialContent:
		start, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			resp.Body.Close()
			return nil, 0, 0, err
		}
		return resp.Body, total, start, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, 0, 0, ErrNotFound
	case http.StatusServiceUnavailable, http.StatusTooManyRequests:
		return nil, 0, 0, ErrBusy
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, 0, 0, fmt.Errorf("%w: %s", ErrDenied, string(body))
	}
	return nil, 0, 0, &StatusError{Source: s.SourceName, Code: resp.StatusCode, Msg: string(body)}
}

func parseContentRange(v string) (start, total int64, err error) {
	var end int64
	if _, err := fmt.Sscanf(v, "bytes %d-%d/%d", &start, &end, &total); err != nil {
		return 0, 0, fmt.Errorf("bad Content-Range %q", v)
	}
	return start, total, nil
}

// ShardSource reads a project's store shard. It is authoritative.
type ShardSource struct {
	Shard storeurl.Shard
}

// Name returns the shard name.
func (s ShardSource) Name() string { return s.Shard.Name() }

// Kind returns "store".
func (s ShardSource) Kind() string { return "store" }

// Authoritative is true.
func (s ShardSource) Authoritative() bool { return true }

// Get reads from the shard.
func (s ShardSource) Get(ctx context.Context, _ string, id c4.ID, offset int64) (io.ReadCloser, int64, int64, error) {
	body, total, start, err := s.Shard.Get(ctx, id, offset)
	if errors.Is(err, storeurl.ErrNotFound) {
		return nil, 0, 0, ErrNotFound
	}
	return body, total, start, err
}

// Breakers skip sources that just failed, for a growing interval, without
// changing membership: rendezvous homes stay stable while a peer restarts.
type Breakers struct {
	mu sync.Mutex
	m  map[string]*breaker
}

type breaker struct {
	fails int
	until time.Time
}

// NewBreakers returns an empty set.
func NewBreakers() *Breakers { return &Breakers{m: map[string]*breaker{}} }

// Allow reports whether name may be tried now.
func (b *Breakers) Allow(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	br, ok := b.m[name]
	return !ok || time.Now().After(br.until)
}

// Fail records a failure and opens the breaker for 0.5 s doubling to 30 s.
func (b *Breakers) Fail(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	br, ok := b.m[name]
	if !ok {
		br = &breaker{}
		b.m[name] = br
	}
	br.fails++
	d := 500 * time.Millisecond << min(br.fails-1, 6)
	br.until = time.Now().Add(min(d, 30*time.Second))
}

// Ok closes the breaker.
func (b *Breakers) Ok(name string) {
	b.mu.Lock()
	delete(b.m, name)
	b.mu.Unlock()
}

// Backoff is a jittered exponential backoff.
type Backoff struct {
	Base, Max time.Duration
}

// Delay returns the wait before retry attempt n (0-based), with full
// jitter.
func (b Backoff) Delay(n int) time.Duration {
	base, maxd := b.Base, b.Max
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if maxd <= 0 {
		maxd = 5 * time.Second
	}
	d := base << min(n, 16)
	if d <= 0 || d > maxd {
		d = maxd
	}
	return time.Duration(rand.Int64N(int64(d)) + 1)
}

// RetryAfter is the Retry-After value a shedding tier sends, in seconds,
// with jitter so that refused clients do not return together.
func RetryAfter() string { return strconv.Itoa(1 + rand.IntN(2)) }
