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
