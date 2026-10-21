package huggingface_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/source"
	"github.com/Avalanche-io/c4-operator/internal/source/huggingface"
	"github.com/Avalanche-io/c4-operator/internal/source/huggingface/hftest"
)

// store is a C4 storage writer in memory.
type store struct {
	mu   sync.Mutex
	objs map[c4.ID][]byte
}

func (s *store) put(_ context.Context, id c4.ID, size int64, r io.Reader) (bool, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return false, err
	}
	if int64(len(b)) != size || c4.Identify(bytes.NewReader(b)) != id {
		return false, errors.New("the store was given an ID its bytes do not have")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objs == nil {
		s.objs = map[c4.ID][]byte{}
	}
	_, had := s.objs[id]
	s.objs[id] = b
	return !had, nil
}

func hub(t *testing.T) *hftest.Server {
	s := hftest.New()
	t.Cleanup(s.Close)
	s.Tokens["hf_lease"] = "noor"
	return s
}

func client(s *hftest.Server, token string) *huggingface.Client {
	return &huggingface.Client{Endpoint: s.Endpoint, HTTP: s.Client(), Hosts: []string{"*." + hftest.Hub},
		Token: func(context.Context) (string, error) { return token, nil }}
}

var files = map[string][]byte{
	"config.json":       []byte(`{"model_type": "depth_anything"}`),
	"README.md":         []byte("# a depth model\n"),
	"model.safetensors": bytes.Repeat([]byte("weights!"), 40),
	"onnx/model.onnx":   bytes.Repeat([]byte("onnx"), 40),
	"sub dir/a b.txt":   []byte("a path that needs escaping"),
}

// A revision is resolved once to its commit; the files at that commit are
// listed with the hub's hashes, a filter applied, pages followed; each is
// fetched, checked and stored under its C4 ID, a large one from the CDN,
// which never sees the token.
func TestResolveListFetch(t *testing.T) {
	s := hub(t)
	rp, commit := s.Commit("model", "depth-anything/small", "main", files)
	rp.License = "apache-2.0"
	c := client(s, "hf_lease")
	snap, err := c.Resolve(context.Background(), huggingface.Model, "depth-anything/small", "main")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Commit != commit || snap.Ref != "main" || snap.Gated != "" || snap.License != "apache-2.0" || snap.Source != "huggingface" {
		t.Fatalf("snapshot %+v", snap)
	}
	if err := c.List(context.Background(), &snap, source.Filter{Exclude: []string{"onnx/*"}}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range snap.Files {
		paths = append(paths, f.Path)
		if f.GitBlob == "" || (f.Size >= hftest.LFSThreshold) != (f.SHA256 != "") {
			t.Errorf("%s: hashes %+v", f.Path, f)
		}
	}
	if strings.Join(paths, ",") != "README.md,config.json,model.safetensors,sub dir/a b.txt" {
		t.Fatalf("files %q", paths)
	}
	if err := c.Access(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	st := &store{}
	for _, f := range snap.Files {
		got, err := c.Fetch(context.Background(), snap, f, t.TempDir(), st.put)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != c4.Identify(bytes.NewReader(files[f.Path])) || !got.Stored {
			t.Fatalf("%s: %+v", f.Path, got)
		}
	}
	if len(st.objs) != 4 || s.FollowedWithToken() != 0 {
		t.Fatalf("stored %d, the CDN saw the token %d times", len(st.objs), s.FollowedWithToken())
	}
	if who, err := c.Whoami(context.Background()); err != nil || who != "noor" {
		t.Fatalf("whoami %q %v", who, err)
	}
	if who, err := client(s, "").Whoami(context.Background()); err != nil || who != "" {
		t.Fatalf("anonymous whoami %q %v", who, err)
	}
}

// A gated repository says so when it is resolved, and the access check
// refuses, in the hub's words, before anything is fetched; with an account
// the hub let in, it passes.
func TestGatedBeforeFetch(t *testing.T) {
	s := hub(t)
	rp, _ := s.Commit("model", "meta/llama", "main", files)
	rp.Gated = "manual"
	snap, err := client(s, "").Resolve(context.Background(), "", "meta/llama", "")
	if err != nil || snap.Gated != "manual" {
		t.Fatalf("resolve %+v %v", snap, err)
	}
	if err := client(s, "").List(context.Background(), &snap, source.Filter{}); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"", "hf_lease"} {
		err := client(s, tok).Access(context.Background(), snap)
		if !errors.Is(err, source.ErrGated) || !strings.Contains(err.Error(), "is restricted") {
			t.Fatalf("token %q: %v", tok, err)
		}
	}
	rp.Access["noor"] = true
	if err := client(s, "hf_lease").Access(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Calls() {
		if strings.HasPrefix(c, "GET ") && strings.Contains(c, "/resolve/") {
			t.Fatalf("a file was fetched before access was known: %s", c)
		}
	}
}

// Bytes that are not what the hub's hash says never reach the store, for
// a large file (SHA-256) or a small one (git blob SHA-1).
func TestCorruptNeverStored(t *testing.T) {
	s := hub(t)
	s.Commit("model", "o/m", "main", files)
	c := client(s, "")
	snap, _ := c.Resolve(context.Background(), "", "o/m", "main")
	if err := c.List(context.Background(), &snap, source.Filter{Include: []string{"model.safetensors", "config.json"}}); err != nil {
		t.Fatal(err)
	}
	s.Corrupt("model.safetensors")
	s.Corrupt("config.json")
	st := &store{}
	for _, f := range snap.Files {
		if _, err := c.Fetch(context.Background(), snap, f, t.TempDir(), st.put); !errors.Is(err, source.ErrMismatch) {
			t.Fatalf("%s: %v", f.Path, err)
		}
	}
	if len(st.objs) != 0 {
		t.Fatalf("corrupt bytes were stored: %d", len(st.objs))
	}
}

// A snapshot stays the commit it was pinned to when the branch moves on.
func TestPinnedWhenTheBranchMoves(t *testing.T) {
	s := hub(t)
	s.Commit("dataset", "o/d", "main", map[string][]byte{"a.txt": []byte("first")})
	c := client(s, "")
	snap, err := c.Resolve(context.Background(), huggingface.Dataset, "o/d", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.List(context.Background(), &snap, source.Filter{}); err != nil {
		t.Fatal(err)
	}
	_, second := s.Commit("dataset", "o/d", "main", map[string][]byte{"a.txt": []byte("second")})
	if second == snap.Commit {
		t.Fatal("the branch did not move")
	}
	st := &store{}
	got, err := c.Fetch(context.Background(), snap, snap.Files[0], t.TempDir(), st.put)
	if err != nil || got.ID != c4.Identify(strings.NewReader("first")) {
		t.Fatalf("fetched %+v %v", got, err)
	}
}

// The client reads the RateLimit headers and waits for the window before
// it runs out, so the hub never has to refuse it.
func TestPacesToTheWindow(t *testing.T) {
	s := hub(t)
	s.Quota = 3
	many := map[string][]byte{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		many[n+".txt"] = []byte(n)
	}
	s.Commit("model", "o/many", "main", many)
	var waits []time.Duration
	c := client(s, "")
	c.Sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		s.ResetWindow()
		return nil
	}
	snap, err := c.Resolve(context.Background(), "", "o/many", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.List(context.Background(), &snap, source.Filter{}); err != nil {
		t.Fatal(err)
	}
	st := &store{}
	for _, f := range snap.Files {
		if _, err := c.Fetch(context.Background(), snap, f, t.TempDir(), st.put); err != nil {
			t.Fatal(err)
		}
	}
	if s.Throttled() != 0 || len(waits) == 0 || len(st.objs) != 7 {
		t.Fatalf("throttled %d, waited %v, stored %d", s.Throttled(), waits, len(st.objs))
	}
	for _, w := range waits {
		if w <= 0 || w > 120*time.Second {
			t.Fatalf("waited %v; the window said 120 s", w)
		}
	}
}

// A 429 waits out its window and tries again.
func TestRetriesAfterA429(t *testing.T) {
	s := hub(t)
	s.Quota = 1
	s.Commit("model", "o/m", "main", files)
	waited := 0
	c := client(s, "")
	c.Share = 1
	c.Sleep = func(context.Context, time.Duration) error { waited++; s.ResetWindow(); return nil }
	// Spend the window from another client at the same address.
	if _, err := client(s, "").Resolve(context.Background(), "", "o/m", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resolve(context.Background(), "", "o/m", "main"); err != nil || waited == 0 || s.Throttled() != 1 {
		t.Fatalf("err %v waited %d throttled %d", err, waited, s.Throttled())
	}
}

func TestNotFound(t *testing.T) {
	s := hub(t)
	s.Commit("model", "o/m", "main", files)
	c := client(s, "")
	for _, x := range [][2]string{{"o/none", "main"}, {"o/m", "nope"}} {
		if _, err := c.Resolve(context.Background(), "", x[0], x[1]); !errors.Is(err, source.ErrNotFound) {
			t.Errorf("%s@%s: %v", x[0], x[1], err)
		}
	}
	for _, bad := range []string{"no-slash", "o/../x", "/abs", "o/m/extra"} {
		if _, err := c.Resolve(context.Background(), "", bad, ""); err == nil || errors.Is(err, source.ErrNotFound) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	snap, _ := c.Resolve(context.Background(), "", "o/m", "main")
	if _, err := c.Fetch(context.Background(), snap, source.File{Path: "gone.bin", Size: 1, GitBlob: "x"}, t.TempDir(), (&store{}).put); !errors.Is(err, source.ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
}

// One live read of a small public test repository, anonymous, when asked
// for (C4_HF_LIVE=1): a small file served by the Hub and a 450 KB LFS one
// redirected to its CDN, both checked as the fake's are.
func TestLiveSmallRepository(t *testing.T) {
	if os.Getenv("C4_HF_LIVE") == "" {
		t.Skip("set C4_HF_LIVE=1 to read the Hub")
	}
	c := &huggingface.Client{}
	snap, err := c.Resolve(context.Background(), "", "hf-internal-testing/tiny-random-bert", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.List(context.Background(), &snap, source.Filter{Include: []string{"config.json", "onnx/model.onnx"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Access(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	st := &store{}
	for _, f := range snap.Files {
		got, err := c.Fetch(context.Background(), snap, f, t.TempDir(), st.put)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s@%s %s %d bytes %s", snap.Repo, snap.Commit[:8], f.Path, f.Size, got.ID.String()[:16])
	}
	if len(st.objs) != 2 {
		t.Fatalf("stored %d", len(st.objs))
	}
}

func fetchAll(t *testing.T, c *huggingface.Client, snap source.Snapshot) (*store, error) {
	t.Helper()
	st := &store{}
	for _, f := range snap.Files {
		if _, err := c.Fetch(context.Background(), snap, f, t.TempDir(), st.put); err != nil {
			return st, err
		}
	}
	return st, nil
}

func snapshot(t *testing.T, s *hftest.Server, c *huggingface.Client, include ...string) source.Snapshot {
	t.Helper()
	snap, err := c.Resolve(context.Background(), "", "o/m", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.List(context.Background(), &snap, source.Filter{Include: include}); err != nil {
		t.Fatal(err)
	}
	return snap
}

// A download goes only where the source allows, one hop at a time and a
// few at most; nothing it was refused reaches the store.
func TestRedirectsOnlyWhereAllowed(t *testing.T) {
	for name, set := range map[string]func(*hftest.Server, *huggingface.Client){
		"another host":          func(s *hftest.Server, _ *huggingface.Client) { s.CDNHost = "evil.example" },
		"a look-alike host":     func(s *hftest.Server, _ *huggingface.Client) { s.CDNHost = "hub.example.evil.example" },
		"the Hub's hosts alone": func(_ *hftest.Server, c *huggingface.Client) { c.Hosts = nil },
		"too many hops":         func(s *hftest.Server, _ *huggingface.Client) { s.MultiHop = huggingface.MaxHops + 1 },
	} {
		s := hub(t)
		s.Commit("model", "o/m", "main", files)
		c := client(s, "hf_lease")
		set(s, c)
		st, err := fetchAll(t, c, snapshot(t, s, c, "model.safetensors"))
		if err == nil || len(st.objs) != 0 {
			t.Errorf("%s: %v, stored %d", name, err, len(st.objs))
		}
	}
	s := hub(t)
	s.Commit("model", "o/m", "main", files)
	s.MultiHop = 2
	c := client(s, "hf_lease")
	if st, err := fetchAll(t, c, snapshot(t, s, c)); err != nil || len(st.objs) != 5 {
		t.Fatalf("two hops within the limit: %v", err)
	}
}

// The token goes to the Hub, a hop back to the Hub's own host included,
// and never off it: not to a CDN subdomain of the Hub.
func TestTokenOnlyToTheHub(t *testing.T) {
	for _, sameHost := range []bool{false, true} {
		s := hub(t)
		s.Commit("model", "o/m", "main", files)
		s.SameHostHop = sameHost
		c := client(s, "hf_lease")
		if _, err := fetchAll(t, c, snapshot(t, s, c)); err != nil {
			t.Fatal(err)
		}
		if n := s.FollowedWithToken(); n != 0 {
			t.Fatalf("same-host hop %v: %d requests off the Hub carried the token", sameHost, n)
		}
		if sameHost && s.HubHopsWithToken() == 0 {
			t.Fatal("a hop back to the Hub went without the token")
		}
	}
}

// A CDN's signed URL never appears in an error.
func TestSignedURLsStayOutOfErrors(t *testing.T) {
	for _, host := range []string{"evil.example", "down.hub.example"} {
		s := hub(t)
		s.Commit("model", "o/m", "main", files)
		s.CDNHost = host
		c := client(s, "")
		_, err := fetchAll(t, c, snapshot(t, s, c, "model.safetensors"))
		if err == nil || strings.Contains(err.Error(), "sig-secret") || strings.Contains(err.Error(), "Signature") {
			t.Fatalf("%s: %v", host, err)
		}
	}
}

// The Hub must say which commit it served, and say the pinned one.
func TestCommitRequired(t *testing.T) {
	for name, set := range map[string]func(*hftest.Server){
		"none":    func(s *hftest.Server) { s.NoCommit = true },
		"another": func(s *hftest.Server) { s.LieCommit = strings.Repeat("e", 40) },
	} {
		s := hub(t)
		s.Commit("model", "o/m", "main", files)
		c := client(s, "")
		snap := snapshot(t, s, c)
		set(s)
		if _, err := fetchAll(t, c, snap); !errors.Is(err, source.ErrMoved) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A listing whose paths would leave the tree, or that names a path twice,
// is refused whole.
func TestListingPathsChecked(t *testing.T) {
	for _, p := range []string{"", "/etc/passwd", "../up", "a/../../b", "a//b", "./a", "a\\b", "a\x00b", "config.json"} {
		s := hub(t)
		s.Commit("model", "o/m", "main", files)
		s.ExtraEntries = []map[string]any{{"type": "file", "path": p, "size": 1, "oid": "aa"}}
		c := client(s, "")
		snap, err := c.Resolve(context.Background(), "", "o/m", "main")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.List(context.Background(), &snap, source.Filter{}); err == nil {
			t.Errorf("%q was listed", p)
		}
	}
}

// A body longer than its file stops one byte past it, even one that never
// ends.
func TestOversizedBody(t *testing.T) {
	s := hub(t)
	s.Commit("model", "o/m", "main", files)
	c := client(s, "")
	snap := snapshot(t, s, c)
	s.Endless = true
	if st, err := fetchAll(t, c, snap); !errors.Is(err, source.ErrMismatch) || len(st.objs) != 0 {
		t.Fatalf("%v, stored %d", err, len(st.objs))
	}
}

// With no RateLimit-Policy the client still paces, keeping a request in
// hand; callers at once are counted against the window together; a 429's
// Retry-After is waited out.
func TestPacingEdges(t *testing.T) {
	s := hub(t)
	s.Quota, s.NoPolicy = 3, true
	many := map[string][]byte{}
	for i := 0; i < 12; i++ {
		many[strconv.Itoa(i)+".txt"] = []byte{byte(i)}
	}
	s.Commit("model", "o/m", "main", many)
	var mu sync.Mutex
	c := client(s, "")
	c.Sleep = func(context.Context, time.Duration) error { mu.Lock(); defer mu.Unlock(); s.ResetWindow(); return nil }
	snap := snapshot(t, s, c)
	if _, err := c.Fetch(context.Background(), snap, snap.Files[0], t.TempDir(), (&store{}).put); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(snap.Files))
	for _, f := range snap.Files[1:] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Fetch(context.Background(), snap, f, t.TempDir(), (&store{}).put)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if s.Throttled() != 0 {
		t.Fatalf("throttled %d times", s.Throttled())
	}

	s2 := hub(t)
	s2.Commit("model", "o/m", "main", files)
	s2.Quota, s2.RetryAfter = 1, 300
	var waited time.Duration
	c2 := client(s2, "")
	c2.Sleep = func(_ context.Context, d time.Duration) error { waited = max(waited, d); s2.ResetWindow(); return nil }
	if _, err := client(s2, "").Resolve(context.Background(), "", "o/m", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Resolve(context.Background(), "", "o/m", "main"); err != nil {
		t.Fatal(err)
	}
	if waited != 300*time.Second {
		t.Fatalf("waited at most %v; Retry-After said 300 s, longer than the window's 120", waited)
	}
}

// A listing whose next page is itself is refused, not followed forever.
func TestListingThatGoesRound(t *testing.T) {
	s := hub(t)
	s.Commit("model", "o/m", "main", files)
	s.SelfNext = true
	c := client(s, "")
	snap, _ := c.Resolve(context.Background(), "", "o/m", "main")
	if err := c.List(context.Background(), &snap, source.Filter{}); err == nil || !strings.Contains(err.Error(), "round") {
		t.Fatalf("%v", err)
	}
}

// A signed-in account the Hub has not let in is refused as gated too.
func TestGatedSignedIn(t *testing.T) {
	s := hub(t)
	rp, _ := s.Commit("model", "o/m", "main", files)
	rp.Gated = "auto"
	c := client(s, "hf_lease")
	if err := c.Access(context.Background(), snapshot(t, s, c)); !errors.Is(err, source.ErrGated) || !strings.Contains(err.Error(), "authorized list") {
		t.Fatalf("%v", err)
	}
}

// A probe that fails, by a 502 with no RateLimit or a dropped connection,
// does not stop the bucket: the next caller probes again and goes through.
func TestFailedProbeDoesNotStall(t *testing.T) {
	for name, set := range map[string]func(*hftest.Server){
		"502":     func(s *hftest.Server) { s.FailNext = 1 },
		"dropped": func(s *hftest.Server) { s.DropNext = 1 },
	} {
		s := hub(t)
		s.Commit("model", "o/m", "main", files)
		set(s)
		slept := 0
		c := client(s, "")
		c.Sleep = func(context.Context, time.Duration) error {
			if slept++; slept > 50 {
				return errors.New("still waiting after 50 sleeps")
			}
			return nil
		}
		if _, err := c.Resolve(context.Background(), "", "o/m", "main"); err == nil {
			t.Fatalf("%s: the failed probe succeeded", name)
		}
		if _, err := c.Resolve(context.Background(), "", "o/m", "main"); err != nil || slept > 0 {
			t.Fatalf("%s: after a failed probe: %v, slept %d", name, err, slept)
		}
	}
}

// A request that fails before it is sent still leaves flight: three token
// failures do not shrink the window for the client's life.
func TestFailedRequestsLeaveFlight(t *testing.T) {
	s := hub(t)
	s.Commit("model", "o/m", "main", files)
	s.Quota = 3
	fails := 3
	slept := 0
	c := &huggingface.Client{Endpoint: s.Endpoint, HTTP: s.Client(), Hosts: []string{"*." + hftest.Hub},
		Token: func(context.Context) (string, error) {
			if fails > 0 {
				fails--
				return "", errors.New("the lease is renewing")
			}
			return "", nil
		},
		Sleep: func(context.Context, time.Duration) error { slept++; s.ResetWindow(); return nil }}
	for i := 0; i < 3; i++ {
		if _, err := c.Resolve(context.Background(), "", "o/m", "main"); err == nil {
			t.Fatal("a token failure went through")
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := c.Resolve(context.Background(), "", "o/m", "main"); err != nil {
			t.Fatal(err)
		}
	}
	if slept != 0 {
		t.Fatalf("two requests in a window of three waited %d times: the failures stayed in flight", slept)
	}
}

// Callers at once before anything is known wait for one probe's answer,
// then share the window; none is refused.
func TestFirstCallersWaitForTheProbe(t *testing.T) {
	s := hub(t)
	s.Commit("model", "o/m", "main", files)
	s.Quota = 3
	c := client(s, "")
	c.Sleep = func(_ context.Context, d time.Duration) error {
		if d <= 100*time.Millisecond {
			time.Sleep(d)
		} else {
			s.ResetWindow()
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Resolve(context.Background(), "", "o/m", "main")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := s.Throttled(); n != 0 {
		t.Fatalf("throttled %d", n)
	}
}

// A renamed repository redirects on the Hub; the client follows it with
// the token, so a private one still resolves and serves its files.
func TestRenamedRepository(t *testing.T) {
	s := hub(t)
	rp, commit := s.Commit("model", "o/new", "main", files)
	rp.Private, rp.Access["noor"] = true, true
	s.Renames = map[string]string{"o/old": "o/new"}
	c := client(s, "hf_lease")
	snap, err := c.Resolve(context.Background(), "", "o/old", "main")
	if err != nil || snap.Commit != commit {
		t.Fatalf("%+v %v", snap, err)
	}
	if err := c.List(context.Background(), &snap, source.Filter{Include: []string{"config.json", "model.safetensors"}}); err != nil {
		t.Fatal(err)
	}
	if st, err := fetchAll(t, c, snap); err != nil || len(st.objs) != 2 || s.FollowedWithToken() != 0 {
		t.Fatalf("%v, followed with the token %d", err, s.FollowedWithToken())
	}
}

// An error page of HTML is no part of an error.
func TestNoHTMLInErrors(t *testing.T) {
	s := hub(t)
	s.Commit("model", "o/m", "main", files)
	s.HTMLError = true
	_, err := client(s, "").Resolve(context.Background(), "", "o/m", "main")
	if err == nil || strings.Contains(err.Error(), "<") || strings.Contains(err.Error(), "hidden-detail") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("%v", err)
	}
	s.HTMLError, s.FailNext = false, 1
	if _, err := client(s, "").Resolve(context.Background(), "", "o/m", "main"); err == nil || strings.Contains(err.Error(), "<") {
		t.Fatalf("a proxy's page: %v", err)
	}
}

// A CDN that sends the download back to the Hub: the Hub's answer must
// name the commit; an answer off the Hub that names another is refused.
func TestHopsNameTheCommit(t *testing.T) {
	for name, c := range map[string]struct {
		set func(*hftest.Server)
		ok  bool
	}{
		"back to the Hub":                 {func(s *hftest.Server) { s.HopBack = true }, true},
		"back to the Hub, naming none":    {func(s *hftest.Server) { s.HopBack, s.HopBackCommit = true, "-" }, false},
		"back to the Hub, naming another": {func(s *hftest.Server) { s.HopBack, s.HopBackCommit = true, strings.Repeat("e", 40) }, false},
		"the CDN naming another":          {func(s *hftest.Server) { s.CDNCommit = strings.Repeat("e", 40) }, false},
	} {
		s := hub(t)
		s.Commit("model", "o/m", "main", files)
		cl := client(s, "hf_lease")
		snap := snapshot(t, s, cl, "model.safetensors")
		c.set(s)
		st, err := fetchAll(t, cl, snap)
		if c.ok != (err == nil) || (!c.ok && !errors.Is(err, source.ErrMoved)) || (!c.ok && len(st.objs) != 0) {
			t.Errorf("%s: %v", name, err)
		}
		if c.ok && s.HubHopsWithToken() == 0 {
			t.Errorf("%s: the hop back to the Hub went without the token", name)
		}
	}
}

// A listing that pages forever stops at MaxPages.
func TestPagesAreBounded(t *testing.T) {
	s := hub(t)
	s.Commit("model", "o/m", "main", files)
	s.EndlessPages = true
	c := client(s, "")
	snap, _ := c.Resolve(context.Background(), "", "o/m", "main")
	if err := c.List(context.Background(), &snap, source.Filter{}); err == nil || !strings.Contains(err.Error(), "pages") {
		t.Fatalf("%v", err)
	}
}

// Paths that would collide in a tree on some file system are refused: by
// case, or a file where another path needs a folder.
func TestPathsThatCollide(t *testing.T) {
	for _, p := range []string{"Config.json", "README.md/inside", "sub dir", "SUB DIR/a b.txt"} {
		s := hub(t)
		s.Commit("model", "o/m", "main", files)
		s.ExtraEntries = []map[string]any{{"type": "file", "path": p, "size": 1, "oid": "aa"}}
		c := client(s, "")
		snap, _ := c.Resolve(context.Background(), "", "o/m", "main")
		if err := c.List(context.Background(), &snap, source.Filter{}); err == nil {
			t.Errorf("%q was listed beside the others", p)
		}
	}
}
