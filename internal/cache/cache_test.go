package cache_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/go-logr/logr"

	"github.com/Avalanche-io/c4-operator/internal/cache"
	"github.com/Avalanche-io/c4-operator/internal/manifest"
	"github.com/Avalanche-io/c4-operator/internal/tree"
)

// memStore is a store that counts fetches and can lie.
type memStore struct {
	mu    sync.Mutex
	data  map[c4.ID][]byte
	opens atomic.Int64
	lie   map[c4.ID][]byte // bytes returned instead of the real ones
}

func newMemStore() *memStore {
	return &memStore{data: map[c4.ID][]byte{}, lie: map[c4.ID][]byte{}}
}

func (s *memStore) put(b []byte) c4.ID {
	id := c4.Identify(bytes.NewReader(b))
	s.mu.Lock()
	s.data[id] = b
	s.mu.Unlock()
	return id
}

func (s *memStore) Open(id c4.ID) (io.ReadCloser, error) {
	s.opens.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.lie[id]; ok {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	b, ok := s.data[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *memStore) Has(id c4.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[id]
	return ok
}

func open(t *testing.T, budget int64) *cache.Cache {
	t.Helper()
	c, err := cache.Open(t.TempDir(), budget, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnsureFetchesOnce(t *testing.T) {
	s := newMemStore()
	id := s.put([]byte("frame bytes"))
	c := open(t, 1<<30)

	var wg sync.WaitGroup
	var fetched atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.Ensure(id, 11, s, false)
			if err != nil {
				t.Error(err)
			}
			fetched.Add(r.Fetched)
		}()
	}
	wg.Wait()
	if s.opens.Load() != 1 || fetched.Load() != 11 {
		t.Fatalf("opens=%d fetched=%d, want one fetch of 11 bytes", s.opens.Load(), fetched.Load())
	}
	if c.Bytes() != 11 {
		t.Fatalf("cache bytes %d", c.Bytes())
	}
	p, _ := c.Path(id)
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o444 || !fi.ModTime().Equal(cache.Sentinel) {
		t.Fatalf("object mode %v mtime %v", fi.Mode(), fi.ModTime())
	}
}

func TestWrongBytesNeverCachedUnderRequestedID(t *testing.T) {
	s := newMemStore()
	id := s.put([]byte("real"))
	s.lie[id] = []byte("fake")
	c := open(t, 1<<30)
	if _, err := c.Ensure(id, 4, s, false); err == nil {
		t.Fatal("accepted bytes that do not hash to the ID")
	}
	if c.Present(id, 4) {
		t.Fatal("requested ID present after a lying fetch")
	}
}

func TestMissingContent(t *testing.T) {
	s := newMemStore()
	c := open(t, 1<<30)
	_, err := c.Ensure(c4.Identify(strings.NewReader("absent")), -1, s, false)
	if err == nil || !strings.Contains(err.Error(), "not in store") {
		t.Fatalf("err = %v", err)
	}
}

// fixture builds a view over content held in s.
func fixture(t *testing.T, s *memStore, files map[string]string) *manifest.View {
	t.Helper()
	m := c4m.NewManifest()
	dirs := map[string]bool{}
	for p, content := range files {
		id := s.put([]byte(content))
		parts := strings.Split(p, "/")
		for i := range parts[:len(parts)-1] {
			d := strings.Join(parts[:i+1], "/") + "/"
			if !dirs[d] {
				dirs[d] = true
				m.AddEntry(&c4m.Entry{Name: parts[i] + "/", Depth: i, Size: -1, Mode: os.ModeDir | 0o755, Timestamp: c4m.NullTimestamp()})
			}
		}
		m.AddEntry(&c4m.Entry{Name: parts[len(parts)-1], Depth: len(parts) - 1, Size: int64(len(content)), C4ID: id, Mode: 0o644, Timestamp: c4m.NullTimestamp()})
	}
	// Render through text so depths and order are canonical.
	text, err := c4m.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	r, err := manifest.FromBytes(text, nil, manifest.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return r.View
}

func materialize(t *testing.T, c *cache.Cache, s *memStore, v *manifest.View) {
	t.Helper()
	for _, o := range v.Objects() {
		if _, err := c.Ensure(o.ID, o.Size, s, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tree.Build(c.ViewDir(v.ID), v, c.Path, cache.Sentinel); err != nil {
		t.Fatal(err)
	}
}

func TestBuildLinksAndMatchesContentScan(t *testing.T) {
	s := newMemStore()
	v := fixture(t, s, map[string]string{
		"plates/frame.0001.exr": "one",
		"plates/frame.0002.exr": "two",
		"plates/dup.exr":        "one",
		"weights/model.bin":     "weights",
	})
	c := open(t, 1<<30)
	materialize(t, c, s, v)
	dir := c.ViewDir(v.ID)

	got, err := manifest.ScanContentID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != v.ID {
		t.Fatalf("scan of built tree %s != view ID %s", got, v.ID)
	}
	a, _ := os.Stat(filepath.Join(dir, "plates/frame.0001.exr"))
	b, _ := os.Stat(filepath.Join(dir, "plates/dup.exr"))
	if !os.SameFile(a, b) {
		t.Fatal("duplicate content is not one inode")
	}
	if r := tree.Check(dir, v, c.Path); r != "" {
		t.Fatalf("fresh tree fails check: %s", r)
	}
	if s.opens.Load() != 3 {
		t.Fatalf("store opens %d, want 3 unique objects", s.opens.Load())
	}
}

func TestCorruptionDetectedAndRefetched(t *testing.T) {
	s := newMemStore()
	v := fixture(t, s, map[string]string{"a/frame.exr": "pristine plate", "a/other.exr": "other"})
	c := open(t, 1<<30)
	materialize(t, c, s, v)
	dir := c.ViewDir(v.ID)

	// Corrupt through the view: the inode is shared with the cache.
	p := filepath.Join(dir, "a/frame.exr")
	os.Chmod(p, 0o644)
	if err := os.WriteFile(p, []byte("CORRUPT plate!"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := v.Files[0].ID
	if v.Files[0].Path != "a/frame.exr" {
		id = v.Files[1].ID
	}
	r, err := c.Ensure(id, 14, s, false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Refetched || r.Fetched != 14 {
		t.Fatalf("result %+v, want refetch", r)
	}
	if reason := tree.Check(dir, v, c.Path); reason == "" {
		t.Fatal("tree check missed a view still holding the corrupt inode")
	}
	if _, err := tree.Build(dir, v, c.Path, cache.Sentinel); err != nil {
		t.Fatal(err)
	}
	if got, _ := manifest.ScanContentID(dir); got != v.ID {
		t.Fatal("rebuilt tree does not match")
	}
}

func TestFullVerifyCatchesSilentCorruption(t *testing.T) {
	s := newMemStore()
	id := s.put([]byte("0123456789"))
	c := open(t, 1<<30)
	if _, err := c.Ensure(id, 10, s, false); err != nil {
		t.Fatal(err)
	}
	p, _ := c.Path(id)
	os.Chmod(p, 0o644)
	os.WriteFile(p, []byte("01234X6789"), 0o644)
	os.Chmod(p, 0o444)
	os.Chtimes(p, time.Now(), cache.Sentinel) // hide the write from the stat check

	if r, _ := c.Ensure(id, 10, s, false); !r.Hit {
		t.Fatal("expected the stat check to be fooled (that is what Full is for)")
	}
	r, err := c.Ensure(id, 10, s, true)
	if err != nil || !r.Refetched {
		t.Fatalf("full verify: %+v %v", r, err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "0123456789" {
		t.Fatal("not repaired")
	}
}

func TestEvictRespectsViewsInUseAndRetention(t *testing.T) {
	s := newMemStore()
	big := strings.Repeat("x", 1000)
	vA := fixture(t, s, map[string]string{"a.bin": big + "A"})
	vB := fixture(t, s, map[string]string{"b.bin": big + "B"})
	vC := fixture(t, s, map[string]string{"c.bin": big + "C"})
	loose := s.put([]byte(big + "loose"))

	c := open(t, 2500)
	now := time.Now()
	for i, v := range []*manifest.View{vA, vB, vC} {
		materialize(t, c, s, v)
		meta := cache.ViewMeta{LastUsed: now.Add(time.Duration(i-10) * time.Minute)}
		if v == vB {
			meta.Retain = time.Hour // pinned
		}
		c.TouchView(v.ID, meta)
	}
	if _, err := c.Ensure(loose, -1, s, false); err != nil {
		t.Fatal(err)
	}
	if c.Bytes() != 4008 {
		t.Fatalf("bytes %d", c.Bytes())
	}

	st, err := c.Evict(map[c4.ID]bool{vC.ID: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	// loose goes first (unlinked), then view A (unused, unpinned),
	// leaving B (retained) and C (in use).
	if st.Objects != 2 || st.Views != 1 || c.Bytes() != 2002 {
		t.Fatalf("stats %+v bytes %d", st, c.Bytes())
	}
	for _, v := range []*manifest.View{vB, vC} {
		if _, err := os.Stat(c.ViewDir(v.ID)); err != nil {
			t.Fatalf("view %s evicted: %v", v.ID, err)
		}
	}
	if _, err := os.Stat(c.ViewDir(vA.ID)); !os.IsNotExist(err) {
		t.Fatal("view A survived")
	}
}
