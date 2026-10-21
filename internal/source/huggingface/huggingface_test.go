package huggingface_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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
	return &huggingface.Client{Endpoint: s.URL, Token: func(context.Context) (string, error) { return token, nil }}
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
	if len(st.objs) != 4 || s.CDNAuth() != 0 {
		t.Fatalf("stored %d, the CDN saw the token %d times", len(st.objs), s.CDNAuth())
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

// One live read of a small public repository, anonymous, when asked for
// (C4_HF_LIVE=1): the Hub answers as the fake does.
func TestLiveSmallRepository(t *testing.T) {
	if os.Getenv("C4_HF_LIVE") == "" {
		t.Skip("set C4_HF_LIVE=1 to read the Hub")
	}
	c := &huggingface.Client{}
	snap, err := c.Resolve(context.Background(), "", "depth-anything/Depth-Anything-V2-Small-hf", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.List(context.Background(), &snap, source.Filter{Include: []string{"config.json", "preprocessor_config.json"}}); err != nil {
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
