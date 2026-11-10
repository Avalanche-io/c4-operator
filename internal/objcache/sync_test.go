package objcache

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Avalanche-io/c4"
)

// A file gets its name in the cache only after its bytes were synced: the
// order that keeps a crash from leaving a name over bytes that never
// reached the disk. These tests watch the sync call itself. They show the
// order, on every route into the cache; they cannot show what a device
// kept.

// syncSeen is one sync as the cache made it.
type syncSeen struct {
	id            c4.ID // what the synced file's bytes hash to
	plain, execed bool  // which of that ID's names existed at that moment
}

// watchSyncs replaces the cache's sync with one that records, for each
// file synced, the ID of the bytes it held and which names that ID already
// had.
func watchSyncs(t *testing.T, c *Cache) *[]syncSeen {
	t.Helper()
	var seen []syncSeen
	old := syncFile
	t.Cleanup(func() { syncFile = old })
	syncFile = func(f *os.File) error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Errorf("sync: %v", err)
		}
		id := c4.Identify(f)
		_, perr := os.Lstat(c.Path(id))
		_, xerr := os.Lstat(c.ExecPathOf(id))
		seen = append(seen, syncSeen{id: id, plain: perr == nil, execed: xerr == nil})
		return old(f)
	}
	return &seen
}

func syncCache(t *testing.T) *Cache {
	t.Helper()
	set, err := OpenSet(t.TempDir(), Options{Mode: 0o644, ExecMode: 0o755, Budget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	c, err := set.Partition("p-sync")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// wantSynced checks the last sync was of id's bytes, made while the name
// about to be given (the plain one, or the executable variant) did not
// exist yet.
func wantSynced(t *testing.T, what string, seen *[]syncSeen, n int, id c4.ID, exec bool) {
	t.Helper()
	if len(*seen) != n {
		t.Fatalf("%s: %d syncs so far, want %d", what, len(*seen), n)
	}
	s := (*seen)[n-1]
	if s.id != id {
		t.Fatalf("%s: the file synced held other bytes, or not all of them (they hash to %s)", what, s.id)
	}
	if (exec && s.execed) || (!exec && s.plain) {
		t.Fatalf("%s: the name existed before the bytes were synced", what)
	}
}

func TestEveryRouteSyncsBeforeItNames(t *testing.T) {
	c := syncCache(t)
	seen := watchSyncs(t, c)
	body := func(s string) ([]byte, c4.ID) {
		b := bytes.Repeat([]byte(s), 3000)
		return b, c4.Identify(bytes.NewReader(b))
	}
	tmpWith := func(b []byte) string {
		f, err := c.TempFile()
		if err != nil {
			t.Fatal(err)
		}
		f.Write(b)
		f.Close()
		return f.Name()
	}

	// A fill: a temporary file committed under its ID.
	b, id := body("a fill ")
	if err := c.Commit(tmpWith(b), id); err != nil {
		t.Fatal(err)
	}
	wantSynced(t, "Commit", seen, 1, id, false)
	if got, _ := c.ReadAll(id); !bytes.Equal(got, b) {
		t.Fatal("Commit: the object is not the bytes committed")
	}
	// Committing what is already there names nothing, so syncs nothing.
	if err := c.Commit(tmpWith(b), id); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 {
		t.Fatalf("a commit of an object already cached synced again (%d syncs)", len(*seen))
	}

	// Small content put directly.
	b, id = body("a record ")
	if err := c.Put(id, b); err != nil {
		t.Fatal(err)
	}
	wantSynced(t, "Put", seen, 2, id, false)

	// A snapshot of a live file, then its commit.
	src := filepath.Join(t.TempDir(), "live.txt")
	b, id = body("a live file ")
	if err := os.WriteFile(src, b, 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(snap, id); err != nil {
		t.Fatal(err)
	}
	wantSynced(t, "Snapshot and Commit", seen, 3, id, false)

	// A dead upper's plain file, adopted by link.
	b, id = body("a consumed file ")
	src = filepath.Join(filepath.Dir(c.Dir()), "upper-plain")
	if err := os.WriteFile(src, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(src, id, false); err != nil {
		t.Fatal(err)
	}
	wantSynced(t, "Adopt", seen, 4, id, false)

	// A dead upper's executable: the executable variant by link, then the
	// plain object by copy. Each is synced before its own name.
	b, id = body("#!/bin/sh\n")
	src = filepath.Join(filepath.Dir(c.Dir()), "upper-exec")
	if err := os.WriteFile(src, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(src, id, true); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 6 {
		t.Fatalf("an adopted executable made %d syncs, want two more", len(*seen)-4)
	}
	if s := (*seen)[4]; s.id != id || s.execed {
		t.Fatalf("an adopted executable's variant: synced %s, variant already named %v", s.id, s.execed)
	}
	if s := (*seen)[5]; s.id != id || s.plain {
		t.Fatalf("an adopted executable's plain object: synced %s, already named %v", s.id, s.plain)
	}

	// An executable variant made on first use from a plain object.
	b, id = body("a tool ")
	if err := c.Put(id, b); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecPath(id); err != nil {
		t.Fatal(err)
	}
	wantSynced(t, "ExecPath", seen, 8, id, true)
}

// A sync that fails names nothing, and leaves nothing in tmp.
func TestAFailedSyncNamesNothing(t *testing.T) {
	c := syncCache(t)
	old := syncFile
	defer func() { syncFile = old }()
	syncFile = func(*os.File) error { return os.ErrInvalid }
	b := []byte("never durable")
	id := c4.Identify(bytes.NewReader(b))
	if err := c.Put(id, b); err == nil {
		t.Fatal("a put whose sync failed succeeded")
	}
	if c.Has(id) {
		t.Fatal("an object whose sync failed has its name")
	}
	if left, _ := os.ReadDir(filepath.Join(c.Dir(), "tmp")); len(left) != 0 {
		t.Fatalf("left in tmp: %d", len(left))
	}
}
