// Package objcache is the on-disk object cache of a node daemon or a
// regional replica: one directory of objects per partition (a project in
// Strict isolation, a sharing group in Shared), under one byte budget.
//
// Objects are named by C4 ID and are never rehashed: bytes are verified
// once, where they enter the system. An object is written to a temporary
// file in the partition and renamed into place when complete, so a
// reader sees a whole object or none. On a node, layers hardlink objects;
// an object's link count above one means a layer uses it, and such an
// object is never evicted until the layer goes.
package objcache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Avalanche-io/c4"
)

// Options configure a cache set.
type Options struct {
	// Mode of a cached object. A node uses 0o666, so the merged view of a
	// writable volume behaves like an emptyDir; a regional replica 0o444.
	Mode os.FileMode
	// ExecMode of an object's executable variant (node only).
	ExecMode os.FileMode
	// Budget in bytes for all partitions together.
	Budget int64
}

// Set is the cache of every partition on one host.
type Set struct {
	root string
	opts Options

	mu     sync.Mutex
	initMu sync.Mutex
	parts  map[string]*Cache
	bytes  int64
}

// OpenSet opens or creates the cache under root (root/p/<partition>/...).
func OpenSet(root string, opts Options) (*Set, error) {
	if opts.Mode == 0 {
		opts.Mode = 0o444
	}
	if opts.ExecMode == 0 {
		opts.ExecMode = 0o555
	}
	s := &Set{root: root, opts: opts, parts: map[string]*Cache{}}
	if err := os.MkdirAll(filepath.Join(root, "p"), 0o700); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(root, "p"))
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := s.Partition(e.Name()); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Root returns the set's root directory.
func (s *Set) Root() string { return s.root }

// Budget returns the byte budget.
func (s *Set) Budget() int64 { return s.opts.Budget }

// Bytes returns the bytes held in all partitions.
func (s *Set) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

func (s *Set) add(n int64) {
	s.mu.Lock()
	s.bytes += n
	s.mu.Unlock()
}

// ValidPartition reports whether name can name a partition directory.
func ValidPartition(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return !strings.HasPrefix(name, ".")
}

// Partition returns (creating if needed) the cache of one partition.
func (s *Set) Partition(name string) (*Cache, error) {
	if !ValidPartition(name) {
		return nil, fmt.Errorf("invalid partition name %q", name)
	}
	s.mu.Lock()
	c, ok := s.parts[name]
	s.mu.Unlock()
	if ok {
		return c, nil
	}
	// One partition is created once: creation clears its tmp directory,
	// which must never race with a fill already writing there.
	s.initMu.Lock()
	defer s.initMu.Unlock()
	s.mu.Lock()
	c, ok = s.parts[name]
	s.mu.Unlock()
	if ok {
		return c, nil
	}
	dir := filepath.Join(s.root, "p", name)
	for _, d := range []string{"objects", "xobjects", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	c = &Cache{set: s, name: name, dir: dir, used: map[c4.ID]time.Time{}}
	os.RemoveAll(filepath.Join(dir, "tmp"))
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o700); err != nil {
		return nil, err
	}
	n, err := c.scan()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.parts[name] = c
	s.bytes += n
	s.mu.Unlock()
	return c, nil
}

// Partitions lists the partitions.
func (s *Set) Partitions() []*Cache {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Cache, 0, len(s.parts))
	for _, c := range s.parts {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// Cache is one partition's objects.
type Cache struct {
	set  *Set
	name string
	dir  string

	mu   sync.Mutex
	used map[c4.ID]time.Time // last use, for LRU
}

// Name returns the partition name.
func (c *Cache) Name() string { return c.name }

// Dir returns the partition directory (objects, layers and volumes of a
// node live under it, on one filesystem, so layers can hardlink).
func (c *Cache) Dir() string { return c.dir }

func shard(id c4.ID) string { return id.String()[3:5] }

// Path is where id lives when cached.
func (c *Cache) Path(id c4.ID) string {
	return filepath.Join(c.dir, "objects", shard(id), id.String())
}

// ExecPathOf is where id's executable variant lives when made.
func (c *Cache) ExecPathOf(id c4.ID) string {
	return filepath.Join(c.dir, "xobjects", shard(id), id.String())
}

// Stat returns the cached object's size.
func (c *Cache) Stat(id c4.ID) (int64, bool) {
	st, err := os.Stat(c.Path(id))
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}

// Has reports whether id is cached.
func (c *Cache) Has(id c4.ID) bool {
	_, ok := c.Stat(id)
	return ok
}

// Open opens a cached object and records the use.
func (c *Cache) Open(id c4.ID) (*os.File, error) {
	f, err := os.Open(c.Path(id))
	if err != nil {
		return nil, err
	}
	c.touch(id)
	return f, nil
}

func (c *Cache) touch(id c4.ID) {
	c.mu.Lock()
	c.used[id] = time.Now()
	c.mu.Unlock()
}

// Touch records a use without opening.
func (c *Cache) Touch(id c4.ID) { c.touch(id) }

// TempFile creates a file for a fill in the partition's tmp directory, on
// the same filesystem as the objects.
func (c *Cache) TempFile() (*os.File, error) {
	return os.CreateTemp(filepath.Join(c.dir, "tmp"), "fill-*")
}

// Commit moves a completed temporary file into place as id. It is safe to
// commit an ID that is already cached; the temporary file is then removed.
func (c *Cache) Commit(tmp string, id c4.ID) error {
	final := c.Path(id)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(tmp, c.set.opts.Mode); err != nil {
		return err
	}
	epoch := time.Unix(0, 0)
	if err := os.Chtimes(tmp, time.Now(), epoch); err != nil {
		return err
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	if _, err := os.Stat(final); err == nil {
		os.Remove(tmp)
		c.touch(id)
		return nil
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	c.set.add(st.Size())
	c.touch(id)
	return nil
}

// Adopt moves an existing file on the same filesystem (a file captured
// from an upper directory) into the cache as id, without copying. The
// caller has hashed it.
func (c *Cache) Adopt(src string, id c4.ID) error {
	if c.Has(id) {
		c.touch(id)
		return nil
	}
	tmp, err := c.TempFile()
	if err != nil {
		return err
	}
	tmp.Close()
	os.Remove(tmp.Name())
	if err := os.Link(src, tmp.Name()); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		if err := copyFile(src, tmp.Name()); err != nil {
			return err
		}
	}
	return c.Commit(tmp.Name(), id)
}

// Put stores small content (records, changesets) under id.
func (c *Cache) Put(id c4.ID, data []byte) error {
	if c.Has(id) {
		return nil
	}
	tmp, err := c.TempFile()
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return c.Commit(tmp.Name(), id)
}

// ReadAll reads a small cached object.
func (c *Cache) ReadAll(id c4.ID) ([]byte, error) {
	f, err := c.Open(id)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// ExecPath returns the path of id's executable variant, making it by copy
// on first use. Hardlinks share an inode and so a mode; an object used
// both as an executable and as a plain file needs two inodes.
func (c *Cache) ExecPath(id c4.ID) (string, error) {
	p := c.ExecPathOf(id)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	tmp, err := c.TempFile()
	if err != nil {
		return "", err
	}
	tmp.Close()
	if err := copyFile(c.Path(id), tmp.Name()); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Chmod(tmp.Name(), c.set.opts.ExecMode); err != nil {
		return "", err
	}
	os.Chtimes(tmp.Name(), time.Now(), time.Unix(0, 0))
	st, _ := os.Stat(tmp.Name())
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", err
	}
	if st != nil {
		c.set.add(st.Size())
	}
	return p, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// scan counts the bytes already in the partition and seeds the LRU.
func (c *Cache) scan() (int64, error) {
	var total int64
	for _, sub := range []string{"objects", "xobjects"} {
		err := filepath.WalkDir(filepath.Join(c.dir, sub), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			total += info.Size()
			if id, err := c4.Parse(d.Name()); err == nil && sub == "objects" {
				c.used[id] = accessTime(info)
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

// EvictStats reports one eviction pass.
type EvictStats struct {
	Objects int
	Bytes   int64
}

// Evict removes objects that no layer links (link count 1), least
// recently used first, until the set is within budget, and returns what
// it removed. Executable variants with no other link go first.
func (s *Set) Evict() EvictStats {
	var st EvictStats
	if s.Bytes() <= s.opts.Budget {
		return st
	}
	type cand struct {
		c    *Cache
		id   c4.ID
		path string
		used time.Time
	}
	var cands []cand
	for _, c := range s.Partitions() {
		c.mu.Lock()
		for id, used := range c.used {
			cands = append(cands, cand{c, id, c.ExecPathOf(id), used.Add(-time.Nanosecond)})
			cands = append(cands, cand{c, id, c.Path(id), used})
		}
		c.mu.Unlock()
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].used.Before(cands[j].used) })
	for _, cd := range cands {
		if s.Bytes() <= s.opts.Budget {
			break
		}
		fi, err := os.Lstat(cd.path)
		if err != nil {
			continue
		}
		if linkCount(fi) > 1 {
			continue
		}
		if err := os.Remove(cd.path); err != nil {
			continue
		}
		s.add(-fi.Size())
		st.Objects++
		st.Bytes += fi.Size()
		if cd.path == cd.c.Path(cd.id) {
			cd.c.mu.Lock()
			delete(cd.c.used, cd.id)
			cd.c.mu.Unlock()
		}
	}
	return st
}

// Remove deletes one object (tests, scrub).
func (c *Cache) Remove(id c4.ID) error {
	fi, err := os.Lstat(c.Path(id))
	if err != nil {
		return err
	}
	if err := os.Remove(c.Path(id)); err != nil {
		return err
	}
	c.set.add(-fi.Size())
	c.mu.Lock()
	delete(c.used, id)
	c.mu.Unlock()
	return nil
}

func linkCount(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
