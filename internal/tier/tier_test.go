package tier

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/fill"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// fakeSource serves one object with programmable failures.
type fakeSource struct {
	name  string
	kind  string
	auth  bool
	data  []byte
	calls atomic.Int64
	// cutAfter > 0 sends only that many bytes, then fails the body.
	cutAfter int64
	// ignoreRange restarts from 0 on ranged requests.
	ignoreRange bool
	// gate, when set, is waited on after sending half of the object.
	gate chan struct{}
	err  error
	// offsets records requested offsets.
	mu      sync.Mutex
	offsets []int64
}

func (s *fakeSource) Name() string        { return s.name }
func (s *fakeSource) Kind() string        { return s.kind }
func (s *fakeSource) Authoritative() bool { return s.auth }

func (s *fakeSource) Get(ctx context.Context, _ string, id c4.ID, offset int64) (io.ReadCloser, int64, int64, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.offsets = append(s.offsets, offset)
	failure := s.err
	s.mu.Unlock()
	if failure != nil {
		return nil, 0, 0, failure
	}
	start := offset
	if s.ignoreRange {
		start = 0
	}
	pr, pw := io.Pipe()
	go func() {
		rest := s.data[start:]
		if s.gate != nil {
			half := len(rest) / 2
			pw.Write(rest[:half])
			<-s.gate
			rest = rest[half:]
		}
		if s.cutAfter > 0 && int64(len(rest)) > s.cutAfter {
			pw.Write(rest[:s.cutAfter])
			pw.CloseWithError(errors.New("connection reset"))
			return
		}
		pw.Write(rest)
		pw.Close()
	}()
	return pr, int64(len(s.data)), start, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

type harness struct {
	srv    *Server
	http   *httptest.Server
	caches *objcache.Set
}

func newHarness(t *testing.T, limits fill.Limits, route func(project string, id c4.ID) []Source) *harness {
	t.Helper()
	caches, err := objcache.OpenSet(t.TempDir(), objcache.Options{Budget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Tier:      "test",
		Caches:    caches,
		Fills:     fill.NewGroup(limits),
		Fetcher:   &Fetcher{Tier: "test", Breakers: NewBreakers(), Backoff: Backoff{Base: 5 * time.Millisecond, Max: 20 * time.Millisecond}},
		Partition: func(p string) (string, error) { return "p-" + p, nil },
		Route: func(_ *http.Request, project string, id c4.ID) Route {
			if route == nil {
				return nil
			}
			return func() []Source { return route(project, id) }
		},
		Log: logr.Discard(),
	}
	h := &harness{srv: s, http: httptest.NewServer(s), caches: caches}
	t.Cleanup(func() {
		h.http.Close()
		// Fills commit after their last byte is served; let them finish
		// before the cache directory is removed.
		for range 200 {
			if s.Fills.Stats().Fills == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return h
}

func (h *harness) get(t *testing.T, id c4.ID, rangeStart int64) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.http.URL+"/"+id.String(), nil)
	req.Header.Set(HeaderProject, "show")
	if rangeStart > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", rangeStart))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func TestConcurrentRequestsShareOneUpstreamFetch(t *testing.T) {
	data := randomBytes(3 << 20)
	id := c4.Identify(bytes.NewReader(data))
	gate := make(chan struct{})
	up := &fakeSource{name: "regional-0", kind: "regional", auth: true, data: data, gate: gate}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{up} })

	const n = 64
	var wg sync.WaitGroup
	results := make([][]byte, n)
	served := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, h.http.URL+"/"+id.String(), nil)
			req.Header.Set(HeaderProject, "show")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			results[i], _ = io.ReadAll(resp.Body)
			served[i] = resp.Header.Get(HeaderServed)
		}(i)
	}
	time.Sleep(200 * time.Millisecond)
	close(gate)
	wg.Wait()
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("%d requesters caused %d upstream fetches, want 1", n, got)
	}
	fills := 0
	for i := range n {
		if !bytes.Equal(results[i], data) {
			t.Fatalf("requester %d got %d bytes (%q), want the object", i, len(results[i]), results[i][:min(len(results[i]), 200)])
		}
		if served[i] == "fill" {
			fills++
		}
	}
	if fills != 1 {
		t.Fatalf("%d responses report starting the fill", fills)
	}
	// Later requests are cache hits.
	resp, body := h.get(t, id, 0)
	if resp.Header.Get(HeaderServed) != "hit" || !bytes.Equal(body, data) || up.calls.Load() != 1 {
		t.Fatal("a later request did not hit the cache")
	}
}

func TestWaitersReceiveBytesBeforeTheFillEnds(t *testing.T) {
	data := randomBytes(1 << 20)
	id := c4.Identify(bytes.NewReader(data))
	gate := make(chan struct{})
	up := &fakeSource{name: "r", kind: "regional", auth: true, data: data, gate: gate}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{up} })

	req, _ := http.NewRequest(http.MethodGet, h.http.URL+"/"+id.String(), nil)
	req.Header.Set(HeaderProject, "show")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	half := make([]byte, len(data)/2)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, half)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first half did not arrive while the upstream was still sending")
	}
	if !bytes.Equal(half, data[:len(half)]) {
		t.Fatal("wrong bytes")
	}
	close(gate)
	rest, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(append(half, rest...), data) {
		t.Fatal("object incomplete after the gate opened")
	}
}

func TestPartialTransferResumesAtTheNextSource(t *testing.T) {
	data := randomBytes(2 << 20)
	id := c4.Identify(bytes.NewReader(data))
	a := &fakeSource{name: "regional-0", kind: "regional", auth: true, data: data, cutAfter: 700_000}
	b := &fakeSource{name: "regional-1", kind: "regional", auth: true, data: data}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{a, b} })
	_, body := h.get(t, id, 0)
	if !bytes.Equal(body, data) {
		t.Fatalf("got %d bytes", len(body))
	}
	if b.calls.Load() != 1 || b.offsets[0] != 700_000 {
		t.Fatalf("second source asked at offsets %v, want [700000]", b.offsets)
	}
}

func TestSourceThatIgnoresRangeRestartsTheObject(t *testing.T) {
	data := randomBytes(1 << 20)
	id := c4.Identify(bytes.NewReader(data))
	a := &fakeSource{name: "a", kind: "store", auth: true, data: data, cutAfter: 300_000}
	b := &fakeSource{name: "c4d", kind: "store", auth: true, data: data, ignoreRange: true}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{a, b} })
	_, body := h.get(t, id, 0)
	if !bytes.Equal(body, data) {
		t.Fatal("object wrong after a restart")
	}
}

func TestRangeOnACachedObject(t *testing.T) {
	data := randomBytes(100_000)
	id := c4.Identify(bytes.NewReader(data))
	up := &fakeSource{name: "r", kind: "regional", auth: true, data: data}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{up} })
	h.get(t, id, 0)
	resp, body := h.get(t, id, 60_000)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[60_000:]) {
		t.Fatalf("status %d, %d bytes", resp.StatusCode, len(body))
	}
}

func TestShedsBeyondTheFillLimit(t *testing.T) {
	gate := make(chan struct{})
	var bg sync.WaitGroup
	defer func() {
		close(gate)
		bg.Wait()
	}()
	objects := make([][]byte, 3)
	ids := make([]c4.ID, 3)
	for i := range objects {
		objects[i] = randomBytes(200_000)
		ids[i] = c4.Identify(bytes.NewReader(objects[i]))
	}
	// The first two fills hold their slots at the gate; the third source is
	// ungated, so if it is not shed the test fails instead of hanging.
	srcs := make([]*fakeSource, 3)
	for i := range srcs {
		srcs[i] = &fakeSource{name: "r", kind: "regional", auth: true, data: objects[i]}
		if i < 2 {
			srcs[i].gate = gate
		}
	}
	h := newHarness(t, fill.Limits{Fills: 2}, func(_ string, id c4.ID) []Source {
		for i := range ids {
			if ids[i] == id {
				return []Source{srcs[i]}
			}
		}
		return nil
	})
	for i := range 2 {
		bg.Add(1)
		go func(id c4.ID) {
			defer bg.Done()
			req, _ := http.NewRequest(http.MethodGet, h.http.URL+"/"+id.String(), nil)
			req.Header.Set(HeaderProject, "show")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}(ids[i])
	}
	// Wait until both fills hold their slots (a fixed sleep raced under load).
	deadline := time.Now().Add(10 * time.Second)
	for srcs[0].calls.Load() == 0 || srcs[1].calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first two fills never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	resp, _ := h.get(t, ids[2], 0)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("third fill: status %d, Retry-After %q; want 503 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestNonHomeAnswersNotFoundWithoutFetching(t *testing.T) {
	h := newHarness(t, fill.Limits{}, nil)
	resp, _ := h.get(t, c4.Identify(strings.NewReader("x")), 0)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestAuthoritativeNotFoundEndsTheFill(t *testing.T) {
	peer := &fakeSource{name: "node-b", kind: "peer", err: ErrNotFound}
	regional := &fakeSource{name: "regional-0", kind: "regional", auth: true, err: ErrNotFound}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{peer, regional} })
	start := time.Now()
	resp, _ := h.get(t, c4.Identify(strings.NewReader("never uploaded")), 0)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("a missing object was retried instead of reported")
	}
}

func TestBusyUpstreamIsRetriedNotBypassed(t *testing.T) {
	data := randomBytes(50_000)
	id := c4.Identify(bytes.NewReader(data))
	regional := &fakeSource{name: "regional-0", kind: "regional", auth: true, data: data, err: ErrBusy}
	store := &fakeSource{name: "store", kind: "store", auth: true, data: data}
	f := &Fetcher{Tier: "node", Breakers: NewBreakers(), Backoff: Backoff{Base: 5 * time.Millisecond, Max: 10 * time.Millisecond}}
	caches, _ := objcache.OpenSet(t.TempDir(), objcache.Options{Budget: 1 << 30})
	cache, _ := caches.Partition("p-show")
	g := fill.NewGroup(fill.Limits{})
	done := make(chan error, 1)
	flight, _, err := g.Join("k", cache.TempFile, func(fl *fill.Flight) {
		err := f.Fill(context.Background(), fl, "show", id, func() []Source { return []Source{regional} })
		if err != nil {
			fl.Fail(err)
		} else {
			fl.Commit(func(tmp string) (string, error) { return tmp, nil })
		}
		done <- err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer flight.Release()
	time.Sleep(100 * time.Millisecond)
	if regional.calls.Load() < 3 {
		t.Fatalf("busy regional tried %d times", regional.calls.Load())
	}
	if store.calls.Load() != 0 {
		t.Fatal("the fetcher went around the busy tier")
	}
	regional.mu.Lock()
	regional.err = nil
	regional.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestVerifyFillRefusesWrongBytes(t *testing.T) {
	data := randomBytes(10_000)
	id := c4.Identify(bytes.NewReader(data))
	wrong := &fakeSource{name: "bad", kind: "regional", auth: true, data: randomBytes(10_000)}
	f := &Fetcher{Tier: "node", Breakers: NewBreakers(), Verify: true, MaxRounds: 1}
	caches, _ := objcache.OpenSet(t.TempDir(), objcache.Options{Budget: 1 << 30})
	cache, _ := caches.Partition("p-show")
	g := fill.NewGroup(fill.Limits{})
	errc := make(chan error, 1)
	flight, _, _ := g.Join("k", cache.TempFile, func(fl *fill.Flight) {
		err := f.Fill(context.Background(), fl, "show", id, func() []Source { return []Source{wrong} })
		fl.Fail(errors.New("done"))
		errc <- err
	})
	defer flight.Release()
	if err := <-errc; err == nil || !errors.Is(err, errCorrupt) {
		t.Fatalf("err = %v, want corruption", err)
	}
}

func TestIngestComputesTheKey(t *testing.T) {
	h := newHarness(t, fill.Limits{}, nil)
	shard, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.Store = func(string) (*storeurl.Sharded, error) { return shard, nil }
	data := []byte("frame 1001")
	id := c4.Identify(bytes.NewReader(data))
	src := &HTTPSource{SourceName: "regional-0", SourceKind: "regional", Base: h.http.URL, Client: http.DefaultClient}
	up := &Uploader{Candidates: func(string, c4.ID) []*HTTPSource { return []*HTTPSource{src} }, Breakers: NewBreakers(), Rounds: 1}
	if err := up.PutBytes(context.Background(), "show", id, data); err != nil {
		t.Fatal(err)
	}
	if _, err := shard.Head(context.Background(), id); err != nil {
		t.Fatalf("not stored: %v", err)
	}
	// A body that does not hash to the claimed ID is refused and stored
	// nowhere.
	other := c4.Identify(strings.NewReader("something else"))
	err = up.PutBytes(context.Background(), "show", other, []byte("poisoned bytes"))
	if err == nil || !errors.Is(err, errRejected) {
		t.Fatalf("mismatched upload: %v", err)
	}
	if _, err := shard.Head(context.Background(), other); !errors.Is(err, storeurl.ErrNotFound) {
		t.Fatal("a mismatched upload reached the store")
	}
	// The ingested object is cached at the tier (write-through).
	resp, body := h.get(t, id, 0)
	if resp.Header.Get(HeaderServed) != "hit" || !bytes.Equal(body, data) {
		t.Fatal("ingested object not cached")
	}
}

// misplacedSource answers every read with a 206 that starts somewhere it
// wasn't asked to: a broken or hostile upstream.
type misplacedSource struct{ data []byte }

func (s *misplacedSource) Name() string        { return "misplaced" }
func (s *misplacedSource) Kind() string        { return "peer" }
func (s *misplacedSource) Authoritative() bool { return false }
func (s *misplacedSource) Get(_ context.Context, _ string, _ c4.ID, offset int64) (io.ReadCloser, int64, int64, error) {
	start := offset + 7
	return io.NopCloser(bytes.NewReader(s.data[start:])), int64(len(s.data)), start, nil
}

// A source whose 206 starts neither where it was asked nor at 0 is a
// failure of that source, never a restart: its bytes are not written at
// offset 0, so the object the tier serves and caches is the object asked
// for, even with verification off.
func TestMisplacedRangeIsNotTheObject(t *testing.T) {
	data := randomBytes(4096)
	id := c4.Identify(bytes.NewReader(data))
	honest := &fakeSource{name: "store", kind: "store", auth: true, data: data}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source {
		return []Source{&misplacedSource{data: data}, honest}
	})
	resp, body := h.get(t, id, 0)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("served %d: %d bytes, the object's %v", resp.StatusCode, len(body), bytes.Equal(body, data))
	}
	cache, _ := h.caches.Partition("p-show")
	for i := 0; !cache.Has(id); i++ {
		if i > 200 {
			t.Fatal("never cached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := cache.ReadAll(id)
	if c4.Identify(bytes.NewReader(got)) != id {
		t.Fatal("the cache holds other bytes under the object's ID")
	}
}

// wrongSizeSource answers where it is asked, but with another object, of
// another size.
type wrongSizeSource struct{ data []byte }

func (s *wrongSizeSource) Name() string        { return "wrong-size" }
func (s *wrongSizeSource) Kind() string        { return "peer" }
func (s *wrongSizeSource) Authoritative() bool { return false }
func (s *wrongSizeSource) Get(_ context.Context, _ string, _ c4.ID, offset int64) (io.ReadCloser, int64, int64, error) {
	other := bytes.Repeat([]byte{'x'}, len(s.data)+5)
	return io.NopCloser(bytes.NewReader(other[offset:])), int64(len(other)), offset, nil
}

// Once a fill knows the object's size, a source that gives another size
// for the same ID is not giving this object.
func TestOtherSizeIsNotTheObject(t *testing.T) {
	data := randomBytes(4096)
	id := c4.Identify(bytes.NewReader(data))
	cut := &fakeSource{name: "store", kind: "store", auth: true, data: data, cutAfter: 1000}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source {
		return []Source{cut, &wrongSizeSource{data: data}}
	})
	resp, body := h.get(t, id, 0)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("served %d: %d bytes, the object's %v", resp.StatusCode, len(body), bytes.Equal(body, data))
	}
}

// When the store refuses the project, the fill ends at once instead of
// asking again every round until it times out; a peer's refusal only
// moves on to the next source.
func TestStoreRefusalEndsTheFill(t *testing.T) {
	data := randomBytes(100)
	id := c4.Identify(bytes.NewReader(data))
	store := &fakeSource{name: "store", kind: "store", auth: true, data: data, err: fmt.Errorf("%w: project refused", ErrDenied)}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{store} })
	start := time.Now()
	resp, _ := h.get(t, id, 0)
	if resp.StatusCode == http.StatusOK || time.Since(start) > 2*time.Second || store.calls.Load() != 1 {
		t.Fatalf("refused store: %d after %v, %d asks", resp.StatusCode, time.Since(start), store.calls.Load())
	}
	peer := &fakeSource{name: "peer", kind: "peer", data: data, err: fmt.Errorf("%w: not yours", ErrDenied)}
	ok := &fakeSource{name: "store", kind: "store", auth: true, data: data}
	h2 := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{peer, ok} })
	if resp, body := h2.get(t, id, 0); resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("a peer's refusal ended the fill: %d", resp.StatusCode)
	}
}

// A waiter on a fill that verifies never receives a whole object that
// then fails its hash: its response ends short of its length, which its
// client sees as a failure. An object that verifies arrives whole.
func TestVerifyingFillHoldsTheLastByte(t *testing.T) {
	data := randomBytes(64 << 10)
	id := c4.Identify(bytes.NewReader(data))
	forged := append([]byte(nil), data...)
	forged[len(forged)-1] ^= 0xff
	for name, c := range map[string]struct {
		served []byte
		whole  bool
	}{"forged": {forged, false}, "true": {data, true}} {
		src := &fakeSource{name: "peer", kind: "peer", auth: true, data: c.served}
		h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{src} })
		h.srv.Fetcher.Verify = true
		h.srv.Fetcher.MaxRounds = 1
		req, _ := http.NewRequest(http.MethodGet, h.http.URL+"/"+id.String(), nil)
		req.Header.Set(HeaderProject, "show")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if c.whole {
				t.Fatalf("%s: %v", name, err)
			}
			continue
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		got := rerr == nil && resp.StatusCode == http.StatusOK && bytes.Equal(body, data)
		if got != c.whole {
			t.Fatalf("%s: status %d, %d bytes, read error %v", name, resp.StatusCode, len(body), rerr)
		}
		if !c.whole && rerr == nil && resp.StatusCode == http.StatusOK && len(body) == len(data) {
			t.Fatalf("%s: a whole forged object was served", name)
		}
	}
}

// A fill started for a project that verifies its fills hashes what it
// fetches, whoever asked: a forged object is never served or cached.
func TestFillsVerifyForProjectsThatAsk(t *testing.T) {
	data := randomBytes(4096)
	id := c4.Identify(bytes.NewReader(data))
	forged := append([]byte(nil), data...)
	forged[0] ^= 0xff
	src := &fakeSource{name: "peer", kind: "peer", data: forged}
	h := newHarness(t, fill.Limits{}, func(string, c4.ID) []Source { return []Source{src} })
	h.srv.Fetcher.MaxRounds = 1
	h.srv.VerifyFill = func(project string) bool { return project == "show" }
	resp, body := h.get(t, id, 0)
	if resp.StatusCode == http.StatusOK && bytes.Equal(body, forged) {
		t.Fatal("a forged object was served for a project that verifies")
	}
	time.Sleep(50 * time.Millisecond)
	cache, _ := h.caches.Partition("p-show")
	if cache.Has(id) {
		t.Fatal("a forged object was cached")
	}
}
