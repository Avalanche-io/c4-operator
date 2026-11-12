package storeurl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

	// got holds other wrong bytes: these are got's only copy, and stay
	s, _ = treeWith(t, asked, wrong)
	ts, _ := store.NewTreeStore(strings.TrimPrefix(s.For(got).Name(), "file://"))
	w, _ := ts.Create(got)
	io.WriteString(w, "neither")
	w.Close()
	if err := s.Refile(ctx, asked, got, wrong); err == nil {
		t.Error("with other bytes under got: re-filed")
	}
	if b, _ := read(t, s, asked); !bytes.Equal(b, wrong) {
		t.Errorf("with other bytes under got: asked holds %q, want the only copy kept", b)
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

	objects[key], etags[key] = wrong, `"e1"`
	if err := s3.removeWrong(ctx, asked); err != nil {
		t.Fatal(err)
	}
	if _, ok := objects[key]; ok || len(deletes) != 1 || deletes[0] != `"e1"` {
		t.Fatalf("a wrong object: deletes %q, still there %v", deletes, objects[key] != nil)
	}

	deletes = nil
	objects[key], etags[key] = wrong, `"e1"`
	changeBeforeDelete = true
	if err := s3.removeWrong(ctx, asked); err != nil {
		t.Fatal(err)
	}
	changeBeforeDelete = false
	if string(objects[key]) != "the record" {
		t.Fatalf("a correct object put between the read and the delete was deleted: %q", objects[key])
	}

	deletes = nil
	objects[key], etags[key] = []byte("the record"), `"e3"`
	if err := s3.removeWrong(ctx, asked); err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 0 || string(objects[key]) != "the record" {
		t.Fatalf("an object that is its own: deletes %q, holds %q", deletes, objects[key])
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
