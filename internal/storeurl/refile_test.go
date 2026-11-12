package storeurl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"
)

// treeWith is a one-shard tree store holding data under id, whatever data
// is, and the path of its file.
func treeWith(t *testing.T, id c4.ID, data []byte) (*Sharded, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenSharded([]Spec{{Name: "s0", URL: dir}}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := store.NewTreeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ts.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	w.Close()
	p, ok := ts.ContentPath(id)
	if !ok {
		t.Fatal("not stored")
	}
	return s, p
}

func read(t *testing.T, s *Sharded, id c4.ID) ([]byte, bool) {
	t.Helper()
	rc, err := s.Open(id)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b, true
}

// A wrong object is re-filed (Joshua, 2026-11-11): its bytes are under
// their own ID, and gone from under the one they were stored under, which
// is then free for a correct Put. Done again, it does nothing more.
func TestAWrongObjectIsRefiledUnderItsOwnID(t *testing.T) {
	ctx := context.Background()
	asked := c4.Identify(strings.NewReader("the record"))
	wrong := []byte("not the record")
	got := c4.Identify(bytes.NewReader(wrong))
	s, _ := treeWith(t, asked, wrong)
	if err := s.Refile(ctx, asked, got, wrong); err != nil {
		t.Fatal(err)
	}
	if b, ok := read(t, s, got); !ok || !bytes.Equal(b, wrong) {
		t.Fatalf("under its own ID: %q, %v", b, ok)
	}
	if _, ok := read(t, s, asked); ok {
		t.Fatal("still under the ID it was stored under")
	}
	if err := s.Refile(ctx, asked, got, wrong); err != nil {
		t.Fatalf("again: %v", err)
	}
	if err := s.Put(ctx, asked, strings.NewReader("the record"), 10); err != nil {
		t.Fatal(err)
	}
	if b, _ := read(t, s, asked); string(b) != "the record" {
		t.Fatalf("a correct Put after the re-file stored %q", b)
	}
}

// What the re-file must never do: write over an object that is its own,
// lose the only copy of an object's bytes, remove a correct object put
// since the read, or take an object a c4d keeps.
func TestTheRefileKeepsWhatIsRight(t *testing.T) {
	ctx := context.Background()
	asked := c4.Identify(strings.NewReader("the record"))
	wrong := []byte("not the record")
	got := c4.Identify(bytes.NewReader(wrong))

	// got holds its own bytes already: nothing is written under it
	s, _ := treeWith(t, asked, wrong)
	if err := s.Put(ctx, got, bytes.NewReader(wrong), int64(len(wrong))); err != nil {
		t.Fatal(err)
	}
	if err := s.Refile(ctx, asked, got, wrong); err != nil {
		t.Fatal(err)
	}
	if _, ok := read(t, s, asked); ok {
		t.Error("with got stored already: still under asked")
	}

	// got holds other wrong bytes: those are not got's, so nothing of got's
	// is there to keep; they are set aside and placed under their own ID,
	// and got takes its own (2026-11-11)
	s, _ = treeWith(t, asked, wrong)
	ts, _ := store.NewTreeStore(strings.TrimPrefix(s.For(got).Name(), "file://"))
	w, _ := ts.Create(got)
	io.WriteString(w, "neither")
	w.Close()
	if err := s.Refile(ctx, asked, got, wrong); err != nil {
		t.Errorf("with other bytes under got: %v", err)
	}
	if b, _ := read(t, s, got); !bytes.Equal(b, wrong) {
		t.Errorf("with other bytes under got: got holds %q", b)
	}
	if b, _ := read(t, s, c4.Identify(strings.NewReader("neither"))); string(b) != "neither" {
		t.Errorf("the other bytes are not under their own ID: %q", b)
	}
	if _, ok := read(t, s, asked); ok {
		t.Error("with other bytes under got: still under asked")
	}

	// asked holds its own bytes by the time the re-file looks: kept
	s, p := treeWith(t, asked, wrong)
	os.Chmod(p, 0o644)
	os.WriteFile(p, []byte("the record"), 0o644)
	if err := s.Refile(ctx, asked, got, wrong); err != nil {
		t.Fatal(err)
	}
	if b, _ := read(t, s, asked); string(b) != "the record" {
		t.Errorf("a repaired object: asked holds %q", b)
	}

	// a correct object put at asked's path after the re-file took the
	// wrong one's file aside: it stays
	s, p = treeWith(t, asked, wrong)
	refileTook = func(path string) {
		if path == p {
			os.WriteFile(p, []byte("the record"), 0o444)
		}
	}
	t.Cleanup(func() { refileTook = nil })
	if err := s.Refile(ctx, asked, got, wrong); err != nil {
		t.Fatal(err)
	}
	refileTook = nil
	if b, ok := read(t, s, asked); !ok || string(b) != "the record" {
		t.Errorf("a correct object put during the re-file: asked holds %q, %v", b, ok)
	}

	// a c4d store repairs its own
	c4d, err := OpenSharded([]Spec{{Name: "c", URL: "c4d://localhost:1"}}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c4d.Refile(ctx, asked, got, wrong); !errors.Is(err, ErrNoRefile) {
		t.Errorf("a c4d store: %v", err)
	}
	if err := s.Refile(ctx, asked, got, []byte("other")); err == nil {
		t.Error("bytes that are not got's were re-filed as got's")
	}
}

// On S3 the re-file deletes the wrong object only on the ETag it read it
// at (If-Match), so an object put under the ID since is never deleted; an
// object that is its own is never deleted at all.
func TestAnS3RefileDeletesOnlyWhatItRead(t *testing.T) {
	ctx := context.Background()
	asked := c4.Identify(strings.NewReader("the record"))
	wrong := []byte("not the record")
	var mu sync.Mutex
	objects := map[string][]byte{}
	etags := map[string]string{}
	var deletes []string
	changeBeforeDelete := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := r.URL.Path
		switch r.Method {
		case http.MethodGet:
			b, ok := objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", etags[key])
			w.Write(b)
		case http.MethodDelete:
			deletes = append(deletes, r.Header.Get("If-Match"))
			if changeBeforeDelete {
				objects[key], etags[key] = []byte("the record"), `"e2"`
			}
			if m := r.Header.Get("If-Match"); m != "" && m != etags[key] {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer srv.Close()
	sh, err := Open("s3://bucket/p/?endpoint="+url.QueryEscape(srv.URL), Credentials{AccessKeyID: "K", SecretAccessKey: "S"})
	if err != nil {
		t.Fatal(err)
	}
	s3 := sh.(*s3Shard)
	key := "/bucket/p/" + asked.String()
	gotWrong := c4.Identify(bytes.NewReader(wrong))

	objects[key], etags[key] = wrong, `"e1"`
	if err := s3.removeWrong(ctx, asked, gotWrong); err != nil {
		t.Fatal(err)
	}
	if _, ok := objects[key]; ok || len(deletes) != 1 || deletes[0] != `"e1"` {
		t.Fatalf("a wrong object: deletes %q, still there %v", deletes, objects[key] != nil)
	}

	deletes = nil
	objects[key], etags[key] = wrong, `"e1"`
	changeBeforeDelete = true
	if err := s3.removeWrong(ctx, asked, gotWrong); err != nil {
		t.Fatal(err)
	}
	changeBeforeDelete = false
	if string(objects[key]) != "the record" {
		t.Fatalf("a correct object put between the read and the delete was deleted: %q", objects[key])
	}

	deletes = nil
	objects[key], etags[key] = []byte("the record"), `"e3"`
	if err := s3.removeWrong(ctx, asked, gotWrong); err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 0 || string(objects[key]) != "the record" {
		t.Fatalf("an object that is its own: deletes %q, holds %q", deletes, objects[key])
	}

	// a third object's bytes under the key by then: not the ones re-filed,
	// perhaps their only copy, so not deleted
	deletes = nil
	objects[key], etags[key] = []byte("a third object"), `"e4"`
	if err := s3.removeWrong(ctx, asked, gotWrong); err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 0 || string(objects[key]) != "a third object" {
		t.Fatalf("a third object's bytes: deletes %q, holds %q", deletes, objects[key])
	}
}

// A copy from another store is hashed as it streams: the right bytes are
// stored, and bytes that are not the ID's are refused at their end, with
// nothing under the ID in either a tree store or S3. Once, a copy put what
// it read.
func TestACopyStoresOnlyTheIDsOwnBytes(t *testing.T) {
	ctx := context.Background()
	right := bytes.Repeat([]byte("frame "), 200_000) // 1.2 MB, more than one read
	id := c4.Identify(bytes.NewReader(right))
	from, _ := treeWith(t, id, right)
	to, _ := treeWith(t, c4.Identify(strings.NewReader("unrelated")), []byte("unrelated"))
	if err := to.Copy(ctx, from, id, int64(len(right))); err != nil {
		t.Fatal(err)
	}
	if b, _ := read(t, to, id); !bytes.Equal(b, right) {
		t.Fatalf("the right bytes copied: %d bytes", len(b))
	}

	wrong := append(append([]byte(nil), right[:len(right)-1]...), '!')
	from, _ = treeWith(t, id, wrong)
	to, _ = treeWith(t, c4.Identify(strings.NewReader("unrelated")), []byte("unrelated"))
	if err := to.Copy(ctx, from, id, int64(len(wrong))); !errors.Is(err, ErrNotItsID) {
		t.Fatalf("wrong bytes copied: %v", err)
	}
	if _, err := to.Head(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a tree store keeps wrong bytes under the ID: %v", err)
	}

	var puts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			puts++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s3, err := OpenSharded([]Spec{{Name: "s3", URL: "s3://bucket/p/?endpoint=" + url.QueryEscape(srv.URL)}}, Credentials{AccessKeyID: "K", SecretAccessKey: "S"})
	if err != nil {
		t.Fatal(err)
	}
	small := []byte("not the record")
	asked := c4.Identify(strings.NewReader("the record"))
	from, _ = treeWith(t, asked, small)
	if err := s3.Copy(ctx, from, asked, int64(len(small))); !errors.Is(err, ErrNotItsID) || puts != 0 {
		t.Fatalf("wrong bytes into S3: %v, %d PUTs sent", err, puts)
	}
}

// A Put whose reader fails stores nothing under the ID on a tree store.
// The store's writer publishes on Close, and once a failed copy left what
// it had under the ID.
func TestAFailedPutLeavesNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := treeWith(t, c4.Identify(strings.NewReader("unrelated")), []byte("unrelated"))
	id := c4.Identify(strings.NewReader("the whole object"))
	r := io.MultiReader(strings.NewReader("the whole"), iotestErr{})
	if err := s.Put(ctx, id, r, 16); err == nil {
		t.Fatal("a Put whose reader failed succeeded")
	}
	if _, err := s.Head(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a failed Put left something under the ID: %v", err)
	}
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errors.New("the source failed") }

// A Put that fails takes nothing from a concurrent one that did not (the
// reader of this branch): X's reader stalls mid-copy, Y puts the right
// bytes, X's reader fails. Y's object is there, right. Once, X's failure
// removed it, and before that X's Close renamed its part over it.
func TestAFailedPutTakesNothingFromAConcurrentOne(t *testing.T) {
	ctx := context.Background()
	s, _ := treeWith(t, c4.Identify(strings.NewReader("unrelated")), []byte("unrelated"))
	right := []byte("the whole object")
	id := c4.Identify(bytes.NewReader(right))

	release := make(chan struct{})
	started := make(chan struct{})
	x := &gatedFail{prefix: []byte("the whole"), started: started, release: release}
	var xerr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); xerr = s.Put(ctx, id, x, int64(len(right))) }()
	<-started // X has passed Has and Create, and is mid-copy
	if err := s.Put(ctx, id, bytes.NewReader(right), int64(len(right))); err != nil {
		t.Fatal(err)
	}
	if b, ok := read(t, s, id); !ok || !bytes.Equal(b, right) {
		t.Fatalf("Y alone: %q %v", b, ok)
	}
	close(release)
	wg.Wait()
	if b, ok := read(t, s, id); xerr == nil || !ok || !bytes.Equal(b, right) {
		t.Errorf("after a failing Put (%v) beside a successful one: present %v, %q", xerr, ok, b)
	}
}

type gatedFail struct {
	prefix  []byte
	sent    bool
	started chan struct{}
	release chan struct{}
}

func (g *gatedFail) Read(p []byte) (int, error) {
	if !g.sent {
		g.sent = true
		close(g.started)
		return copy(p, g.prefix), nil
	}
	<-g.release
	return 0, errors.New("source failed")
}

// The re-file removes from under the ID only the bytes it re-filed: other
// bytes there by then (another object's, perhaps its only copy) stay.
// Once, it removed whatever was not the ID's own.
func TestTheRefileRemovesOnlyWhatItRefiled(t *testing.T) {
	ctx := context.Background()
	asked := c4.Identify(strings.NewReader("the record"))
	w1 := []byte("wrong one")
	w2 := []byte("wrong two: some other object's only copy")
	g1 := c4.Identify(bytes.NewReader(w1))
	g2 := c4.Identify(bytes.NewReader(w2))
	s, _ := treeWith(t, asked, w2) // asked holds w2 by the time removeWrong looks
	if err := s.Refile(ctx, asked, g1, w1); err != nil {
		t.Fatal(err)
	}
	if b, ok := read(t, s, asked); !ok || !bytes.Equal(b, w2) {
		t.Errorf("other bytes under asked: %q, %v, want them kept", b, ok)
	}
	if _, ok := read(t, s, g1); !ok {
		t.Error("the re-filed bytes are not under their own ID")
	}
	_ = g2
}

// A leaf split while the re-file holds a file aside (the window covers
// hashing it) does not lose it: the file aside is in the store's root
// under a dot name, which a split never moves, and it goes back to where
// the ID is now. Here the object is its own, a correct one put since the
// read that found a wrong one. Once, the split moved it under a name
// nothing reads, and the ID was lost.
func TestASplitDuringARefileLosesNothing(t *testing.T) {
	ctx := context.Background()
	right := []byte("the record")
	asked := c4.Identify(bytes.NewReader(right))
	wrong := []byte("not the record")
	got := c4.Identify(bytes.NewReader(wrong))
	s, p := treeWith(t, asked, right)
	ts := s.For(asked).(*treeShard).ts
	ts.SetSplitThreshold(4)
	refileTook = func(path string) {
		if path != p {
			return
		}
		for i := 0; i < 8; i++ { // other writers fill the leaf past the threshold
			ts.Put(strings.NewReader(fmt.Sprintf("filler %d", i)))
		}
	}
	t.Cleanup(func() { refileTook = nil })
	err := s.Refile(ctx, asked, got, wrong)
	refileTook = nil
	if b, ok := read(t, s, asked); err != nil || !ok || !bytes.Equal(b, right) {
		t.Errorf("after a split during the re-file (%v): asked %q, %v", err, b, ok)
	}
	strays := 0
	filepath.WalkDir(ts.Root(), func(path string, d os.DirEntry, err error) error {
		if strings.Contains(path, refileAside) {
			strays++
		}
		return nil
	})
	if strays != 0 {
		t.Errorf("%d files left aside", strays)
	}
}

// What a crash leaves in the middle of a re-file, a file taken aside, is
// mended when the store is opened: one that is the ID it was taken from
// goes back under it; any other (the bytes re-filed, under their own ID
// already) goes.
func TestARefileCutShortIsMendedOnOpen(t *testing.T) {
	right := []byte("the record")
	asked := c4.Identify(bytes.NewReader(right))
	wrong := []byte("not the record")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, refileAside+asked.String()+".0123456789abcdef"), right, 0o444)
	other := c4.Identify(strings.NewReader("another"))
	os.WriteFile(filepath.Join(dir, refileAside+other.String()+".fedcba9876543210"), wrong, 0o444)
	s, err := OpenSharded([]Spec{{Name: "s0", URL: dir}}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := read(t, s, asked); !ok || !bytes.Equal(b, right) {
		t.Errorf("the object taken aside, its ID's own: %q, %v", b, ok)
	}
	if _, ok := read(t, s, other); ok {
		t.Error("bytes that are not their ID's were put back under it")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, refileAside+"*")); len(left) != 0 {
		t.Errorf("left aside: %v", left)
	}
}

// An object still pending publication (a tree store in batch mode) is
// being written, and the re-file leaves it alone.
func TestTheRefileLeavesAnObjectBeingWritten(t *testing.T) {
	ts, err := store.NewTreeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts.SetSyncMode(store.SyncBatch)
	sh := &treeShard{root: ts.Root(), ts: ts}
	asked := c4.Identify(strings.NewReader("the record"))
	w, err := ts.Create(asked)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("not yet"))
	w.Close()
	got := c4.Identify(strings.NewReader("not yet"))
	if err := sh.removeWrong(context.Background(), asked, got); err == nil {
		t.Error("an object pending publication was taken")
	}
	if p, ok := ts.ContentPath(asked); !ok {
		t.Error("the pending object is gone")
	} else if b, _ := os.ReadFile(p); string(b) != "not yet" {
		t.Errorf("the pending object holds %q", b)
	}
}

// swapped puts each of objs' bytes under the next one's ID, a cycle (two
// is a swap), and answers the store and the IDs.
func swapped(t *testing.T, objs ...string) (*Sharded, string, []c4.ID) {
	t.Helper()
	dir := t.TempDir()
	ts, err := store.NewTreeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]c4.ID, len(objs))
	for i, o := range objs {
		ids[i] = c4.Identify(strings.NewReader(o))
	}
	for i, o := range objs {
		w, err := ts.Create(ids[(i+1)%len(objs)])
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, o)
		w.Close()
	}
	s, err := OpenSharded([]Spec{{Name: "s0", URL: dir}}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	return s, dir, ids
}

// healed checks every object of a cycle is under its own ID, and nothing
// is left aside.
func healed(t *testing.T, s *Sharded, dir string, objs []string, ids []c4.ID) {
	t.Helper()
	for i, o := range objs {
		if b, ok := read(t, s, ids[i]); !ok || string(b) != o {
			t.Errorf("%q under its own ID: %q, %v", o, b, ok)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, refileAside+"*")); len(left) != 0 {
		t.Errorf("left aside: %v", left)
	}
}

// Wrong objects under each other's IDs come apart (2026-11-11,
// within Joshua's ruling: the bytes in A's slot are not A, so nothing of
// A's is there to keep): re-filing A's bytes, found under B, sets aside
// what A's slot holds, writes A, and re-files what it set aside by the
// same rule. A swap and a 3-cycle each heal from one read.
func TestASwapAndACycleHeal(t *testing.T) {
	ctx := context.Background()
	for _, objs := range [][]string{{"alpha", "beta"}, {"alpha", "beta", "gamma"}} {
		s, dir, ids := swapped(t, objs...)
		// a read of ids[1] finds objs[0]'s bytes
		if err := s.Refile(ctx, ids[1], ids[0], []byte(objs[0])); err != nil {
			t.Fatalf("%d-cycle: %v", len(objs), err)
		}
		healed(t, s, dir, objs, ids)
	}
}

// A crash between any two steps of a chained re-file loses nothing: every
// byte string is under its own ID or set aside, and the store, opened
// again, places what was aside. Here the re-file stops at each step in
// turn, of a swap and of a 3-cycle.
func TestACrashedRefileHealsOnOpen(t *testing.T) {
	ctx := context.Background()
	crash := errors.New("crash")
	for _, objs := range [][]string{{"alpha", "beta"}, {"alpha", "beta", "gamma"}} {
		for _, at := range []string{"aside", "written", "chained", "placed"} {
			for nth := 1; nth <= len(objs); nth++ {
				s, dir, ids := swapped(t, objs...)
				n := 0
				step = func(name string) error {
					if name == at {
						n++
						if n == nth {
							return crash
						}
					}
					return nil
				}
				err := s.Refile(ctx, ids[1], ids[0], []byte(objs[0]))
				step = func(string) error { return nil }
				if n < nth {
					continue // the chain is shorter than that: no crash there
				}
				if !errors.Is(err, crash) {
					t.Fatalf("%d-cycle, crash at %s #%d: %v", len(objs), at, nth, err)
				}
				again, err := OpenSharded([]Spec{{Name: "s0", URL: dir}}, Credentials{})
				if err != nil {
					t.Fatal(err)
				}
				// what the crash left under a wrong ID is found by a read,
				// and re-filed, as any wrong object is
				for i := range objs {
					if b, ok := read(t, again, ids[i]); ok && c4.Identify(bytes.NewReader(b)) != ids[i] {
						again.Refile(ctx, ids[i], c4.Identify(bytes.NewReader(b)), b)
					}
				}
				healed(t, again, dir, objs, ids)
			}
		}
	}
	t.Cleanup(func() { step = func(string) error { return nil } })
}

// A chain longer than a re-file follows stops, and deletes nothing: what
// it could not place stays aside, its ID not present.
func TestAChainTooLongStopsAndKeepsEverything(t *testing.T) {
	ctx := context.Background()
	objs := make([]string, maxRefileHops+3)
	for i := range objs {
		objs[i] = fmt.Sprintf("object %d", i)
	}
	s, dir, ids := swapped(t, objs...)
	if err := s.Refile(ctx, ids[1], ids[0], []byte(objs[0])); !errors.Is(err, ErrRefileTooFar) {
		t.Fatalf("a %d-cycle: %v", len(objs), err)
	}
	// every object's bytes are somewhere: under their own ID, under
	// another's, or aside
	have := map[string]bool{}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			have[string(b)] = true
		}
		return nil
	})
	for _, o := range objs {
		if !have[o] {
			t.Errorf("%q is gone", o)
		}
	}
}
