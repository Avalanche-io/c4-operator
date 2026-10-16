package tier

import (
	"context"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/fill"
	"github.com/Avalanche-io/c4-operator/internal/metrics"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// Authorizer decides whether a request may use the tier. It returns the
// caller's name, or an HTTP status and error.
type Authorizer func(r *http.Request) (caller string, status int, err error)

// Server answers the content API for one process: a node daemon (serving
// peers) or a regional replica.
type Server struct {
	Tier    string // node or regional (metrics)
	Caches  *objcache.Set
	Fills   *fill.Group
	Fetcher *Fetcher
	// Authorize is nil only in tests.
	Authorize Authorizer
	// Partition maps a project to its cache partition, and refuses
	// projects this tier does not serve.
	Partition func(project string) (string, error)
	// Route returns the upstream sources for a miss, or nil when this
	// server must not fetch (a node that is not the object's home, or any
	// peer-hop request that would cascade).
	Route func(r *http.Request, project string, id c4.ID) Route
	// Store returns a project's authoritative store, for HEAD on a miss and
	// for PUT. Nil on node daemons.
	Store func(project string) (*storeurl.Sharded, error)
	// MaxServing caps concurrent object responses (0: unlimited).
	MaxServing int
	// FillTimeout bounds one upstream fill (0: 6 h).
	FillTimeout time.Duration
	// MaxRate limits each response to this many bytes per second (0: no
	// limit). It is a test knob: on a kind cluster every link runs at
	// memory speed, and a slower link makes transfers long enough to kill
	// a replica in the middle of one.
	MaxRate int64
	Log     logr.Logger

	serving atomic.Int64
}

// ServeHTTP routes GET, HEAD and PUT.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Write([]byte("ok"))
		return
	}
	project := r.Header.Get(HeaderProject)
	if s.Authorize != nil {
		if _, status, err := s.Authorize(r); err != nil {
			metrics.TierRequests.WithLabelValues(s.Tier, project, "denied").Inc()
			http.Error(w, err.Error(), status)
			return
		}
	}
	if project == "" {
		http.Error(w, "missing "+HeaderProject, http.StatusBadRequest)
		return
	}
	partition, err := s.Partition(project)
	if err != nil {
		metrics.TierRequests.WithLabelValues(s.Tier, project, "denied").Inc()
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	cache, err := s.Caches.Partition(partition)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.Method == http.MethodPut && r.URL.Path == "/" {
		s.ingest(w, r, project, cache)
		return
	}
	id, err := c4.Parse(strings.TrimPrefix(r.URL.Path, "/"))
	if err != nil {
		http.Error(w, "invalid C4 ID", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.get(w, r, project, partition, cache, id)
	case http.MethodHead:
		s.head(w, r, project, cache, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) admit() bool {
	if s.MaxServing <= 0 {
		s.serving.Add(1)
		return true
	}
	if s.serving.Add(1) > int64(s.MaxServing) {
		s.serving.Add(-1)
		return false
	}
	return true
}

func (s *Server) shed(w http.ResponseWriter, project string) {
	metrics.TierRequests.WithLabelValues(s.Tier, project, "busy").Inc()
	w.Header().Set("Retry-After", RetryAfter())
	http.Error(w, "tier at capacity", http.StatusServiceUnavailable)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, project, partition string, cache *objcache.Cache, id c4.ID) {
	if !s.admit() {
		s.shed(w, project)
		return
	}
	defer s.serving.Add(-1)

	if f, err := cache.Open(id); err == nil {
		defer f.Close()
		metrics.TierRequests.WithLabelValues(s.Tier, project, "hit").Inc()
		w.Header().Set(HeaderServed, "hit")
		w.Header().Set("Content-Type", "application/octet-stream")
		cw := &countingWriter{w: w, rate: s.MaxRate, served: metrics.TierServedBytes.WithLabelValues(s.Tier, project)}
		http.ServeContent(cw, r, "", time.Time{}, f)
		return
	}

	key := partition + "/" + id.String()
	flight, ok, err := s.Fills.Lookup(key)
	if err != nil {
		s.shed(w, project)
		return
	}
	served := "join"
	if !ok {
		route := s.Route(r, project, id)
		if route == nil {
			metrics.TierRequests.WithLabelValues(s.Tier, project, "notfound").Inc()
			http.Error(w, "not here", http.StatusNotFound)
			return
		}
		var started bool
		flight, started, err = s.Fills.Join(key, cache.TempFile, func(f *fill.Flight) { s.run(f, project, id, cache, route) })
		if errors.Is(err, fill.ErrBusy) {
			s.shed(w, project)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if started {
			served = "fill"
		}
	}
	defer flight.Release()
	metrics.TierRequests.WithLabelValues(s.Tier, project, served).Inc()
	s.serveFlight(w, r, project, flight, served)
}

// run is a fill's leader: detached from any request, bounded by
// FillTimeout.
func (s *Server) run(f *fill.Flight, project string, id c4.ID, cache *objcache.Cache, route Route) {
	timeout := s.FillTimeout
	if timeout <= 0 {
		timeout = 6 * time.Hour
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	metrics.TierFills.WithLabelValues(s.Tier).Inc()
	defer metrics.TierFills.WithLabelValues(s.Tier).Dec()
	if err := s.Fetcher.Fill(ctx, f, project, id, route); err != nil {
		s.Log.V(1).Info("fill failed", "id", id.String(), "project", project, "err", err.Error())
		f.Fail(err)
		return
	}
	f.Commit(func(tmp string) (string, error) {
		if err := cache.Commit(tmp, id); err != nil {
			return "", err
		}
		return cache.Path(id), nil
	})
}

func (s *Server) serveFlight(w http.ResponseWriter, r *http.Request, project string, f *fill.Flight, served string) {
	ctx := r.Context()
	size, err := f.Size(ctx)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	start := rangeStart(r.Header.Get("Range"))
	if start > 0 && start >= size {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	body, err := f.Reader(ctx, start)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()
	h := w.Header()
	h.Set(HeaderServed, served)
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(size-start, 10))
	if start > 0 {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, size-1, size))
		w.WriteHeader(http.StatusPartialContent)
	}
	cw := &countingWriter{w: w, rate: s.MaxRate, served: metrics.TierServedBytes.WithLabelValues(s.Tier, project)}
	io.Copy(cw, body)
}

// rangeStart parses "bytes=N-"; anything else serves the whole object.
func rangeStart(v string) int64 {
	rest, ok := strings.CutPrefix(v, "bytes=")
	if !ok || !strings.HasSuffix(rest, "-") || strings.Contains(rest, ",") {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(rest, "-"), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (s *Server) head(w http.ResponseWriter, r *http.Request, project string, cache *objcache.Cache, id c4.ID) {
	if size, ok := cache.Stat(id); ok {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	if s.Store == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	st, err := s.Store(project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	size, err := st.Head(r.Context(), id)
	switch {
	case errors.Is(err, storeurl.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
	}
}

// ingest is PUT /: the tier computes the ID of what it receives, refuses a
// body that does not hash to the ID the client claimed, writes it to the
// store under the computed ID only if the store lacks it, and keeps a copy
// in its cache, since the next stage usually reads what this one wrote.
// The writer never chooses the key.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request, project string, cache *objcache.Cache) {
	if s.Store == nil {
		http.Error(w, "this tier does not accept writes", http.StatusMethodNotAllowed)
		return
	}
	var claimed c4.ID
	if v := r.Header.Get(HeaderID); v != "" {
		id, err := c4.Parse(v)
		if err != nil {
			http.Error(w, "invalid "+HeaderID, http.StatusBadRequest)
			return
		}
		claimed = id
	}
	st, err := s.Store(project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp, err := cache.TempFile()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := sha512.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		metrics.TierIngest.WithLabelValues(s.Tier, project, "error").Inc()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	metrics.TierIngestBytes.WithLabelValues(s.Tier, project).Add(float64(n))
	var id c4.ID
	copy(id[:], h.Sum(nil))
	if !claimed.IsNil() && claimed != id {
		os.Remove(tmp.Name())
		metrics.TierIngest.WithLabelValues(s.Tier, project, "mismatch").Inc()
		http.Error(w, fmt.Sprintf("body hashes to %s, not %s", id, claimed), http.StatusUnprocessableEntity)
		return
	}
	if err := cache.Commit(tmp.Name(), id); err != nil {
		os.Remove(tmp.Name())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result, err := s.writeThrough(r.Context(), st, cache, id)
	if err != nil {
		metrics.TierIngest.WithLabelValues(s.Tier, project, "error").Inc()
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	metrics.TierIngest.WithLabelValues(s.Tier, project, result).Inc()
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(id.String()))
}

func (s *Server) writeThrough(ctx context.Context, st *storeurl.Sharded, cache *objcache.Cache, id c4.ID) (string, error) {
	_, err := st.Head(ctx, id)
	if err == nil {
		return "present", nil
	}
	if !errors.Is(err, storeurl.ErrNotFound) {
		return "", err
	}
	f, err := cache.Open(id)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if err := st.Put(ctx, id, f, info.Size()); err != nil {
		return "", err
	}
	return "stored", nil
}

// countingWriter counts what it writes into the served-bytes series as it
// goes, so a long transfer shows in the metrics while it runs, and, with a
// rate, paces it.
type countingWriter struct {
	w      http.ResponseWriter
	n      int64
	rate   int64
	start  time.Time
	served prometheus.Counter
}

func (c *countingWriter) Header() http.Header  { return c.w.Header() }
func (c *countingWriter) WriteHeader(code int) { c.w.WriteHeader(code) }

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.rate <= 0 {
		n, err := c.w.Write(p)
		c.n += int64(n)
		c.served.Add(float64(n))
		return n, err
	}
	if c.start.IsZero() {
		c.start = time.Now()
	}
	written := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), 64<<10)]
		n, err := c.w.Write(chunk)
		written += n
		c.n += int64(n)
		c.served.Add(float64(n))
		if err != nil {
			return written, err
		}
		p = p[n:]
		ahead := time.Duration(float64(c.n)/float64(c.rate)*float64(time.Second)) - time.Since(c.start)
		if ahead > 0 {
			time.Sleep(ahead)
		}
	}
	return written, nil
}
