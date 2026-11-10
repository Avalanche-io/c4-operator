package objcache

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
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
	id            c4.ID       // what the synced file's bytes hash to
	file          os.FileInfo // the file that was synced
	plain, execed bool        // which of that ID's names existed at that moment
}

// watchSyncs replaces the cache's sync with one that records, for each
// file synced, the ID of the bytes it held, which file it was, and which
// names that ID already had.
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
		fi, err := f.Stat()
		if err != nil {
			t.Errorf("sync: %v", err)
		}
		_, perr := os.Lstat(c.Path(id))
		_, xerr := os.Lstat(c.ExecPathOf(id))
		seen = append(seen, syncSeen{id: id, file: fi, plain: perr == nil, execed: xerr == nil})
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

// wantSynced checks sync number n: it was of id's bytes, all of them; it
// was made while the name about to be given (the plain one, or the
// executable variant) did not exist; and the file that now has that name
// is the file that was synced, not a copy made after.
func wantSynced(t *testing.T, c *Cache, what string, seen *[]syncSeen, n int, id c4.ID, exec bool) {
	t.Helper()
	if len(*seen) < n {
		t.Fatalf("%s: %d syncs so far, want %d", what, len(*seen), n)
	}
	s := (*seen)[n-1]
	if s.id != id {
		t.Fatalf("%s: the file synced held other bytes, or not all of them (they hash to %s)", what, s.id)
	}
	if (exec && s.execed) || (!exec && s.plain) {
		t.Fatalf("%s: the name existed before the bytes were synced", what)
	}
	name := c.Path(id)
	if exec {
		name = c.ExecPathOf(id)
	}
	named, err := os.Stat(name)
	if err != nil {
		t.Fatalf("%s: the object has no name: %v", what, err)
	}
	if !os.SameFile(s.file, named) {
		t.Fatalf("%s: the file that got the name is not the file that was synced", what)
	}
}

func idOf(b []byte) c4.ID { return c4.Identify(bytes.NewReader(b)) }

func tmpWith(t *testing.T, c *Cache, b []byte) string {
	t.Helper()
	f, err := c.TempFile()
	if err != nil {
		t.Fatal(err)
	}
	f.Write(b)
	f.Close()
	return f.Name()
}

func TestEveryRouteSyncsBeforeItNames(t *testing.T) {
	c := syncCache(t)
	seen := watchSyncs(t, c)
	body := func(s string) ([]byte, c4.ID) {
		b := bytes.Repeat([]byte(s), 3000)
		return b, idOf(b)
	}
	n := 0
	next := func(what string, id c4.ID, exec bool) {
		t.Helper()
		n++
		wantSynced(t, c, what, seen, n, id, exec)
		if len(*seen) != n {
			t.Fatalf("%s: %d syncs so far, want %d", what, len(*seen), n)
		}
	}

	// A fill: a temporary file committed under its ID. Whatever its size:
	// nothing, a few bytes, and more than a megabyte.
	for _, b := range [][]byte{nil, []byte("small"), bytes.Repeat([]byte("large "), 400_000)} {
		id := idOf(b)
		if err := c.Commit(tmpWith(t, c, b), id); err != nil {
			t.Fatal(err)
		}
		next("Commit", id, false)
		if got, _ := c.ReadAll(id); !bytes.Equal(got, b) {
			t.Fatal("Commit: the object is not the bytes committed")
		}
	}
	// Committing what is already there names nothing, so syncs nothing.
	b, id := body("a fill ")
	if err := c.Commit(tmpWith(t, c, b), id); err != nil {
		t.Fatal(err)
	}
	next("Commit", id, false)
	if err := c.Commit(tmpWith(t, c, b), id); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != n {
		t.Fatalf("a commit of an object already cached synced again (%d syncs)", len(*seen))
	}

	// Small content put directly.
	b, id = body("a record ")
	if err := c.Put(id, b); err != nil {
		t.Fatal(err)
	}
	next("Put", id, false)

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
	next("Snapshot and Commit", id, false)

	// A dead upper's plain file, adopted by link.
	b, id = body("a consumed file ")
	src = filepath.Join(filepath.Dir(c.Dir()), "upper-plain")
	if err := os.WriteFile(src, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(src, id, false); err != nil {
		t.Fatal(err)
	}
	next("Adopt", id, false)

	// A dead upper's executable: the executable variant by link, then the
	// plain object by copy. Each is synced before its own name.
	adoptExec := func(what, content string) {
		t.Helper()
		b, id := body(content)
		src := filepath.Join(filepath.Dir(c.Dir()), "upper-exec")
		os.Remove(src)
		if err := os.WriteFile(src, b, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := c.Adopt(src, id, true); err != nil {
			t.Fatal(err)
		}
		wantSynced(t, c, what+": the executable variant", seen, n+1, id, true)
		n++
		next(what+": the plain object", id, false)
	}
	adoptExec("Adopt of an executable", "#!/bin/sh\n")

	// The same where the upper is on another filesystem and cannot be
	// linked: the copy that stands in for the link is synced like the rest.
	func() {
		defer func(old func(string, string) error) { linkFile = old }(linkFile)
		linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EXDEV} }
		adoptExec("Adopt of an executable across filesystems", "#!/bin/sh\n# elsewhere\n")
		b, id := body("a consumed file elsewhere ")
		src := filepath.Join(filepath.Dir(c.Dir()), "upper-plain-2")
		if err := os.WriteFile(src, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := c.Adopt(src, id, false); err != nil {
			t.Fatal(err)
		}
		next("Adopt across filesystems", id, false)
	}()

	// An executable variant made on first use from a plain object.
	b, id = body("a tool ")
	if err := c.Put(id, b); err != nil {
		t.Fatal(err)
	}
	next("Put", id, false)
	if _, err := c.ExecPath(id); err != nil {
		t.Fatal(err)
	}
	next("ExecPath", id, true)
}

// A sync that fails names nothing, is reported, and leaves nothing in
// tmp: on every route.
func TestAFailedSyncNamesNothing(t *testing.T) {
	c := syncCache(t)
	// What the routes below start from, made while syncs still work.
	tool := []byte("a tool already cached")
	if err := c.Put(idOf(tool), tool); err != nil {
		t.Fatal(err)
	}
	old := syncFile
	defer func() { syncFile = old }()
	syncFile = func(*os.File) error { return os.ErrInvalid }

	upper := func(name string, b []byte, mode os.FileMode) string {
		src := filepath.Join(filepath.Dir(c.Dir()), name)
		if err := os.WriteFile(src, b, mode); err != nil {
			t.Fatal(err)
		}
		return src
	}
	put, fill, plain, exec := []byte("never durable"), []byte("a fill never durable"), []byte("an adopted file"), []byte("#!/bin/sh\n# adopted\n")
	for _, r := range []struct {
		what string
		id   c4.ID
		do   func() error
	}{
		{"Put", idOf(put), func() error { return c.Put(idOf(put), put) }},
		{"Commit", idOf(fill), func() error { return c.Commit(tmpWith(t, c, fill), idOf(fill)) }},
		{"Adopt", idOf(plain), func() error { return c.Adopt(upper("failed-plain", plain, 0o644), idOf(plain), false) }},
		{"Adopt of an executable", idOf(exec), func() error { return c.Adopt(upper("failed-exec", exec, 0o755), idOf(exec), true) }},
	} {
		if err := r.do(); err == nil {
			t.Errorf("%s: succeeded though its sync failed", r.what)
		}
		if _, err := os.Lstat(c.Path(r.id)); err == nil {
			t.Errorf("%s: an object whose sync failed has its name", r.what)
		}
		if _, err := os.Lstat(c.ExecPathOf(r.id)); err == nil {
			t.Errorf("%s: an executable variant whose sync failed has its name", r.what)
		}
		if left, _ := os.ReadDir(filepath.Join(c.Dir(), "tmp")); len(left) != 0 {
			t.Errorf("%s: left in tmp: %d", r.what, len(left))
		}
	}
	if p, err := c.ExecPath(idOf(tool)); err == nil {
		t.Errorf("ExecPath: returned %s though its sync failed", p)
	}
	if _, err := os.Lstat(c.ExecPathOf(idOf(tool))); err == nil {
		t.Error("ExecPath: a variant whose sync failed has its name")
	}
	if left, _ := os.ReadDir(filepath.Join(c.Dir(), "tmp")); len(left) != 0 {
		t.Errorf("ExecPath: left in tmp: %d", len(left))
	}
}

// The sync behind the seam is a real one: on a file that has been closed
// it fails, where a function that only returned nil would not.
func TestTheSyncIsASync(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "synced-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("bytes")); err != nil {
		t.Fatal(err)
	}
	if err := syncData(f); err != nil {
		t.Fatalf("syncing an open file: %v", err)
	}
	f.Close()
	if err := syncData(f); err == nil {
		t.Fatal("syncing a closed file reported success: nothing was asked of the system")
	}
}
