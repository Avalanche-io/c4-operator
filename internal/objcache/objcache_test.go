package objcache

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
)

func put(t *testing.T, c *Cache, data []byte) c4.ID {
	t.Helper()
	id := c4.Identify(bytes.NewReader(data))
	if err := c.Put(id, data); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEvictionSkipsLinkedObjectsAndFollowsLRU(t *testing.T) {
	s, err := OpenSet(t.TempDir(), Options{Mode: 0o666, ExecMode: 0o777, Budget: 3000})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.Partition("p-show")
	var ids []c4.ID
	for i := range 5 {
		ids = append(ids, put(t, c, bytes.Repeat([]byte{byte(i)}, 1000)))
		time.Sleep(2 * time.Millisecond)
	}
	// A layer links the oldest object, so it must survive.
	layer := filepath.Join(c.Dir(), "layers", "L")
	os.MkdirAll(layer, 0o755)
	if err := os.Link(c.Path(ids[0]), filepath.Join(layer, "frame.exr")); err != nil {
		t.Fatal(err)
	}
	c.Touch(ids[4]) // recently used
	st := s.Evict()
	if s.Bytes() > 3000 {
		t.Fatalf("still over budget: %d", s.Bytes())
	}
	if !c.Has(ids[0]) {
		t.Fatal("evicted an object a layer links")
	}
	if !c.Has(ids[4]) {
		t.Fatal("evicted the most recently used object")
	}
	if c.Has(ids[1]) {
		t.Fatal("kept the least recently used unlinked object")
	}
	if st.Objects != 2 || st.Bytes != 2000 {
		t.Fatalf("evicted %+v", st)
	}
}

func TestObjectsAreReadOnlyEpochAndExecVariantIsSeparate(t *testing.T) {
	s, _ := OpenSet(t.TempDir(), Options{Mode: 0o666, ExecMode: 0o777, Budget: 1 << 20})
	c, _ := s.Partition("p-show")
	id := put(t, c, []byte("#!/bin/sh\necho hi\n"))
	fi, err := os.Stat(c.Path(id))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o666 || !fi.ModTime().Equal(time.Unix(0, 0)) {
		t.Fatalf("mode %v mtime %v", fi.Mode().Perm(), fi.ModTime())
	}
	x, err := c.ExecPath(id)
	if err != nil {
		t.Fatal(err)
	}
	xi, _ := os.Stat(x)
	if xi.Mode().Perm() != 0o777 || os.SameFile(fi, xi) {
		t.Fatal("the executable variant must be its own inode with its own mode")
	}
}

func TestPartitionsPersistAcrossOpens(t *testing.T) {
	root := t.TempDir()
	s, _ := OpenSet(root, Options{Budget: 1 << 20})
	a, _ := s.Partition("p-a")
	b, _ := s.Partition("p-b")
	put(t, a, []byte("a"))
	put(t, b, []byte("bb"))
	s2, err := OpenSet(root, Options{Budget: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Bytes(); got != 3 {
		t.Fatalf("reopened set holds %d bytes, want 3", got)
	}
	if len(s2.Partitions()) != 2 {
		t.Fatal("partitions lost")
	}
	for _, bad := range []string{"", "../x", "A", ".hidden", "a/b"} {
		if _, err := s2.Partition(bad); err == nil {
			t.Errorf("partition %q accepted", bad)
		}
	}
	_ = fmt.Sprint()
}

// A snapshot shares nothing with its source: whatever is done to either
// afterwards, the other keeps its bytes, its mode and its date.
func TestSnapshotSharesNothingWithItsSource(t *testing.T) {
	s, _ := OpenSet(filepath.Join(t.TempDir(), "cache"), Options{Mode: 0o666, ExecMode: 0o777, Budget: 1 << 20})
	c, _ := s.Partition("p-show")
	src := filepath.Join(t.TempDir(), "tool.sh")
	body := []byte("#!/bin/sh\necho one\n")
	if err := os.WriteFile(src, body, 0o755); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	os.Chtimes(src, when, when)
	snap, err := c.Snapshot(src)
	if err != nil {
		t.Fatal(err)
	}
	id := c4.Identify(bytes.NewReader(body))
	if err := c.Commit(snap, id); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(src)
	obj, err := os.Stat(c.Path(id))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(fi, obj) {
		t.Fatal("the source and the object are one inode")
	}
	if fi.Mode().Perm() != 0o755 || !fi.ModTime().Equal(when) {
		t.Fatalf("the source is now mode %v, dated %v", fi.Mode().Perm(), fi.ModTime().UTC())
	}
	if obj.Mode().Perm() != 0o666 || !obj.ModTime().Equal(time.Unix(0, 0)) {
		t.Fatalf("the object is mode %v, dated %v", obj.Mode().Perm(), obj.ModTime().UTC())
	}
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho two\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.ReadAll(id); !bytes.Equal(got, body) {
		t.Fatalf("a write to the source changed the object: %q", got)
	}
}

// Adopting an executable from a dead upper leaves it executable, so a
// capture that failed after adopting reads the same entry when it runs
// again. Both variants are in the cache, on their own inodes.
func TestAdoptKeepsAnExecutableExecutable(t *testing.T) {
	s, _ := OpenSet(filepath.Join(t.TempDir(), "cache"), Options{Mode: 0o666, ExecMode: 0o777, Budget: 1 << 20})
	c, _ := s.Partition("p-show")
	dir := t.TempDir()
	body := []byte("#!/bin/sh\necho hi\n")
	id := c4.Identify(bytes.NewReader(body))
	exe, plain := filepath.Join(dir, "tool.sh"), filepath.Join(dir, "notes.txt")
	os.WriteFile(exe, body, 0o755)
	os.WriteFile(plain, []byte("notes\n"), 0o644)

	if err := c.Adopt(exe, id, true); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(exe)
	if fi.Mode()&0o111 == 0 {
		t.Fatalf("the adopted executable is now mode %v", fi.Mode().Perm())
	}
	x, err := os.Stat(c.ExecPathOf(id))
	if err != nil || x.Mode().Perm() != 0o777 || !os.SameFile(fi, x) {
		t.Fatalf("the executable variant: %v, %v", x, err)
	}
	obj, err := os.Stat(c.Path(id))
	if err != nil || obj.Mode().Perm() != 0o666 || os.SameFile(obj, x) {
		t.Fatalf("the plain object: %v, %v", obj, err)
	}
	if got, _ := c.ReadAll(id); !bytes.Equal(got, body) {
		t.Fatalf("the plain object reads %q", got)
	}
	// Adopting it again, as a retry does, changes nothing.
	if err := c.Adopt(exe, id, true); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(exe); again.Mode() != fi.Mode() {
		t.Fatalf("a second adopt changed the mode to %v", again.Mode().Perm())
	}

	pid := c4.Identify(bytes.NewReader([]byte("notes\n")))
	if err := c.Adopt(plain, pid, false); err != nil {
		t.Fatal(err)
	}
	pf, _ := os.Stat(plain)
	po, err := os.Stat(c.Path(pid))
	if err != nil || !os.SameFile(pf, po) || po.Mode().Perm() != 0o666 {
		t.Fatalf("a plain file is linked as the object: %v, %v", po, err)
	}
}
