package capture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/overlay"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// flakySink refuses every file while down is set, as a store that is
// unreachable does.
type flakySink struct {
	*storeSink
	down atomic.Bool
}

func (s *flakySink) PutFile(ctx context.Context, p string, id c4.ID, path string) error {
	if s.down.Load() {
		return errors.New("store unavailable")
	}
	return s.storeSink.PutFile(ctx, p, id, path)
}

// plainWalk reports a directory of new files as entries, reading the exec
// bit from each file's mode as overlay.Walk does. No whiteouts: these
// tests only add.
func plainWalk(upper string) ([]overlay.Entry, error) {
	var out []overlay.Entry
	err := filepath.WalkDir(upper, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == upper {
			return err
		}
		rel, _ := filepath.Rel(upper, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out = append(out, overlay.Entry{Path: rel + "/", Full: p, Kind: overlay.Dir})
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, overlay.Entry{Path: rel, Full: p, Kind: overlay.File, Size: fi.Size(), Exec: fi.Mode()&0o111 != 0})
		return nil
	})
	return out, err
}

type liveFixture struct {
	root  string
	cache *objcache.Cache
	sink  *flakySink
	base  *view.View
	src   string
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	root := t.TempDir()
	set, err := objcache.OpenSet(filepath.Join(root, "cache"), objcache.Options{Mode: 0o666, ExecMode: 0o777, Budget: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := set.Partition("p-show")
	sink := &flakySink{storeSink: &storeSink{objs: map[c4.ID][]byte{}}}
	src := filepath.Join(root, "src")
	writeTree(t, src, map[string]string{"README.md": "readme"})
	v, err := view.ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Files {
		data, _ := os.ReadFile(filepath.Join(src, f.Path))
		cache.Put(f.ID, data)
	}
	for _, r := range v.Records() {
		cache.Put(r.ID, r.Data)
		sink.PutBytes(context.Background(), "", r.ID, r.Data)
	}
	return &liveFixture{root: root, cache: cache, sink: sink, base: v, src: src}
}

func (f *liveFixture) input(upper string, consume bool) Input {
	return Input{
		Project: "show", Parent: f.base.ID, Upper: upper, Cache: f.cache, Sink: f.sink, Workers: 2,
		Records: view.RecordFunc(func(_ context.Context, id c4.ID) ([]byte, error) { return f.cache.ReadAll(id) }),
		Walk:    plainWalk, Consume: consume,
	}
}

// writeUpper makes the files an agent wrote: a script it can run, and
// a private note.
func writeUpper(t *testing.T, dir, script string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "tool.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, p := range []string{"bin/tool.sh", "notes.txt"} {
		if err := os.Chtimes(filepath.Join(dir, p), when, when); err != nil {
			t.Fatal(err)
		}
	}
}

// wantID is the view of the base with the agent's files added, as a scan
// of that tree gives it.
func (f *liveFixture) wantID(t *testing.T, name, script string) c4.ID {
	t.Helper()
	dir := filepath.Join(f.root, name)
	writeTree(t, dir, map[string]string{"README.md": "readme"})
	writeUpper(t, dir, script)
	v, err := view.ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return v.ID
}

type attrs struct {
	mode fs.FileMode
	time time.Time
	size int64
}

func (a attrs) String() string {
	return fmt.Sprintf("mode %v, dated %s, %d bytes", a.mode.Perm(), a.time.UTC().Format(time.RFC3339), a.size)
}

func statAll(t *testing.T, dir string, paths ...string) map[string]attrs {
	t.Helper()
	out := map[string]attrs{}
	for _, p := range paths {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		out[p] = attrs{fi.Mode(), fi.ModTime(), fi.Size()}
	}
	return out
}

// A workspace that stays in use after a capture finds every file as it
// left it, and nothing it writes afterwards reaches what was captured.
func TestCaptureLeavesALiveUpperAlone(t *testing.T) {
	f := newLiveFixture(t)
	upper := filepath.Join(f.root, "upper")
	const one, two = "#!/bin/sh\necho one\n", "#!/bin/sh\necho two\n"
	writeUpper(t, upper, one)
	before := statAll(t, upper, "bin/tool.sh", "notes.txt")

	res, err := Run(context.Background(), f.input(upper, false))
	if err != nil {
		t.Fatal(err)
	}
	if want := f.wantID(t, "want1", one); res.Output != want {
		t.Fatalf("captured %s, the agent's tree is %s", res.Output, want)
	}
	for p, was := range before {
		now := statAll(t, upper, p)[p]
		if now != was {
			t.Errorf("%s changed under the capture: %s, was %s", p, now, was)
		}
	}
	id1 := c4.Identify(bytes.NewReader([]byte(one)))
	obj, err := os.Stat(f.cache.Path(id1))
	if err != nil {
		t.Fatalf("the script isn't in the cache: %v", err)
	}
	fi, _ := os.Stat(filepath.Join(upper, "bin", "tool.sh"))
	if os.SameFile(fi, obj) {
		t.Fatal("the workspace's script and the cache's object are one inode")
	}
	if left, _ := filepath.Glob(filepath.Join(f.cache.Dir(), "tmp", "*")); len(left) != 0 {
		t.Errorf("the capture left %d temporary files in the cache", len(left))
	}

	// The agent edits its script in place and captures again.
	w, err := os.OpenFile(filepath.Join(upper, "bin", "tool.sh"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(two)
	w.Close()
	data, err := f.cache.ReadAll(id1)
	if err != nil {
		t.Fatal(err)
	}
	if got := c4.Identify(bytes.NewReader(data)); got != id1 {
		t.Fatalf("an edit in the workspace changed the object the first capture stored: %s now reads %q", id1, data)
	}
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	os.Chtimes(filepath.Join(upper, "bin", "tool.sh"), when, when)
	res2, err := Run(context.Background(), f.input(upper, false))
	if err != nil {
		t.Fatal(err)
	}
	if want := f.wantID(t, "want2", two); res2.Output != want {
		t.Fatalf("the second capture gave %s, the agent's tree is %s", res2.Output, want)
	}
}

// A capture that fails part way and is run again gives the result a
// capture that never failed gives: the same files, each still executable
// or not as the agent left it. Both for a live upper and for a dead one
// whose files are linked into the cache.
func TestCaptureRetriedAfterAFailureIsTheSame(t *testing.T) {
	for _, consume := range []bool{false, true} {
		name := "live"
		if consume {
			name = "consumed"
		}
		t.Run(name, func(t *testing.T) {
			f := newLiveFixture(t)
			upper := filepath.Join(f.root, "upper")
			const script = "#!/bin/sh\necho one\n"
			writeUpper(t, upper, script)
			want := f.wantID(t, "want", script)

			f.sink.down.Store(true)
			if _, err := Run(context.Background(), f.input(upper, consume)); err == nil {
				t.Fatal("a capture with the store down succeeded")
			}
			f.sink.down.Store(false)
			res, err := Run(context.Background(), f.input(upper, consume))
			if err != nil {
				t.Fatal(err)
			}
			if res.Output != want {
				t.Fatalf("the retried capture gave %s, the agent's tree is %s", res.Output, want)
			}
			// And a third run changes nothing.
			again, err := Run(context.Background(), f.input(upper, consume))
			if err != nil || again.Output != want {
				t.Fatalf("a further run gave %s (%v), not %s", again.Output, err, want)
			}
			id := c4.Identify(bytes.NewReader([]byte(script)))
			// The capture itself made the variant; ExecPath would make one.
			if fi, err := os.Stat(f.cache.ExecPathOf(id)); consume && (err != nil || fi.Mode()&0o111 == 0) {
				t.Fatalf("the script's executable variant: %v %v", fi, err)
			}
			if fi, err := os.Stat(f.cache.Path(id)); err != nil || fi.Mode().Perm() != 0o666 {
				t.Fatalf("the script's plain object: %v %v", fi, err)
			}
		})
	}
}

func tmpLeft(f *liveFixture) []string {
	left, _ := filepath.Glob(filepath.Join(f.cache.Dir(), "tmp", "*"))
	return left
}

// In a live workspace a name the walk saw as a file can be something else
// a moment later. A capture never follows it: a symlink swapped in, to a
// file outside the workspace or inside it, is not read, not stored, and
// not given the cache's mode and date.
func TestCaptureNeverFollowsAFileThatBecameALink(t *testing.T) {
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, where := range []string{"outside", "inside"} {
		t.Run(where, func(t *testing.T) {
			f := newLiveFixture(t)
			upper := filepath.Join(f.root, "upper")
			writeUpper(t, upper, "#!/bin/sh\necho one\n")
			body := []byte("the secret\n")
			target := filepath.Join(f.root, "outside", "secret.txt")
			if where == "inside" {
				target = filepath.Join(upper, "aaa-private.txt")
			}
			os.MkdirAll(filepath.Dir(target), 0o700)
			if err := os.WriteFile(target, body, 0o600); err != nil {
				t.Fatal(err)
			}
			os.Chtimes(target, when, when)

			in := f.input(upper, false)
			in.Walk = func(u string) ([]overlay.Entry, error) {
				out, err := plainWalk(u)
				var kept []overlay.Entry
				for _, e := range out {
					if e.Path != "aaa-private.txt" { // the walk did not report the target itself
						kept = append(kept, e)
					}
				}
				// After the walk, before the copy: the swap.
				p := filepath.Join(u, "bin", "tool.sh")
				os.Remove(p)
				if err := os.Symlink(target, p); err != nil {
					t.Fatal(err)
				}
				return kept, err
			}
			if _, err := Run(context.Background(), in); err == nil {
				t.Error("a capture went through a file that had become a symlink")
			}
			fi, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 || !fi.ModTime().Equal(when) {
				t.Errorf("the capture changed the link's target: mode %v, dated %s", fi.Mode().Perm(), fi.ModTime().UTC().Format(time.RFC3339))
			}
			id := c4.Identify(bytes.NewReader(body))
			if _, err := os.Lstat(f.cache.Path(id)); err == nil {
				t.Error("the target's bytes are in the cache")
			}
			f.sink.mu.Lock()
			_, stored := f.sink.objs[id]
			f.sink.mu.Unlock()
			if stored {
				t.Error("the target's bytes are in the store")
			}
			if left := tmpLeft(f); len(left) != 0 {
				t.Errorf("left in the cache's tmp: %v", left)
			}
		})
	}
}

// A name that is a folder by the time it is copied, or a file that can't
// be read, fails the capture. Nothing is stored under the nil C4 ID, and a
// store that trusts the node's IDs is never handed one.
func TestCaptureRefusesWhatIsNoLongerAFile(t *testing.T) {
	f := newLiveFixture(t)
	upper := filepath.Join(f.root, "upper")
	writeUpper(t, upper, "#!/bin/sh\necho one\n")
	in := f.input(upper, false)
	in.Walk = func(u string) ([]overlay.Entry, error) {
		out, err := plainWalk(u)
		p := filepath.Join(u, "notes.txt")
		os.Remove(p)
		os.MkdirAll(filepath.Join(p, "inner"), 0o755)
		os.WriteFile(filepath.Join(p, "inner", "x"), []byte("x"), 0o644)
		return out, err
	}
	res, err := Run(context.Background(), in)
	if err == nil {
		t.Fatalf("a capture went through a file that had become a folder: %+v", res)
	}
	var none c4.ID
	if _, err := os.Lstat(f.cache.Path(none)); err == nil {
		t.Error("the cache holds an object under the nil C4 ID")
	}
	f.sink.mu.Lock()
	_, stored := f.sink.objs[none]
	f.sink.mu.Unlock()
	if stored {
		t.Error("the store holds an object under the nil C4 ID")
	}
	if left := tmpLeft(f); len(left) != 0 {
		t.Errorf("left in the cache's tmp: %v", left)
	}
}

// The copies a capture does not keep are removed: all but one of several
// files with one content, and a file rewritten with the view's own bytes.
func TestCaptureRemovesTheCopiesItDidNotKeep(t *testing.T) {
	f := newLiveFixture(t)
	upper := filepath.Join(f.root, "upper")
	writeUpper(t, upper, "#!/bin/sh\necho one\n")
	os.WriteFile(filepath.Join(upper, "copy-of-notes.txt"), []byte("notes\n"), 0o644)
	os.WriteFile(filepath.Join(upper, "README.md"), []byte("readme"), 0o644) // the view's own bytes
	res, err := Run(context.Background(), f.input(upper, false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Reused != 1 {
		t.Errorf("files rewritten without changing: %d, want 1", res.Reused)
	}
	if left := tmpLeft(f); len(left) != 0 {
		t.Errorf("left in the cache's tmp: %v", left)
	}
}

// A folder on the way to a file, swapped after the walk for a symlink to a
// folder outside the upper, is not followed: the file is opened beneath
// the upper, so nothing outside it is read, cached or stored.
func TestCaptureNeverLeavesTheUpperThroughAFolder(t *testing.T) {
	f := newLiveFixture(t)
	upper := filepath.Join(f.root, "upper")
	writeUpper(t, upper, "#!/bin/sh\necho one\n")
	outside := filepath.Join(f.root, "outside")
	body := []byte("#!/bin/sh\necho the host's own\n")
	os.MkdirAll(outside, 0o755)
	if err := os.WriteFile(filepath.Join(outside, "tool.sh"), body, 0o755); err != nil {
		t.Fatal(err)
	}
	in := f.input(upper, false)
	in.Walk = func(u string) ([]overlay.Entry, error) {
		out, err := plainWalk(u)
		// After the walk, before the copy: bin becomes a way out.
		if err := os.Rename(filepath.Join(u, "bin"), filepath.Join(f.root, "bin-was")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(u, "bin")); err != nil {
			t.Fatal(err)
		}
		return out, err
	}
	if _, err := Run(context.Background(), in); err == nil {
		t.Error("a capture went out of the upper through a folder that had become a symlink")
	}
	id := c4.Identify(bytes.NewReader(body))
	if _, err := os.Lstat(f.cache.Path(id)); err == nil {
		t.Error("the outside file's bytes are in the cache")
	}
	f.sink.mu.Lock()
	_, stored := f.sink.objs[id]
	f.sink.mu.Unlock()
	if stored {
		t.Error("the outside file's bytes are in the store")
	}
	if left := tmpLeft(f); len(left) != 0 {
		t.Errorf("left in the cache's tmp: %v", left)
	}
}

// With Consume the files are hashed where they are. A name that can't be
// read by then (here it has become a folder) hashes to no ID at all, and
// that must fail the capture, never reach the cache as an object.
func TestConsumedCaptureRefusesWhatCannotBeRead(t *testing.T) {
	f := newLiveFixture(t)
	upper := filepath.Join(f.root, "upper")
	writeUpper(t, upper, "#!/bin/sh\necho one\n")
	in := f.input(upper, true)
	in.Walk = func(u string) ([]overlay.Entry, error) {
		out, err := plainWalk(u)
		p := filepath.Join(u, "bin", "tool.sh")
		os.Remove(p)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return out, err
	}
	_, err := Run(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("a consumed capture of a name that can't be read: %v", err)
	}
	if _, err := os.Lstat(f.cache.Path(c4.ID{})); err == nil {
		t.Error("an object is stored under the nil C4 ID")
	}
}
