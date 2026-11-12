// Package objcache is the on-disk object cache of a node daemon or a
// regional replica: one directory of objects per partition (a project in
// Strict isolation, a sharing group in Shared), under one byte budget.
//
// Objects are named by C4 ID and are never rehashed here: whoever fills
// the cache checks the bytes against the ID before committing them. An
// object is written to a temporary file in the partition, synced, and only
// then given its name, so a reader sees a whole object or none, and after
// a crash a name is never left over bytes that did not reach the disk.
// That holds for a crash of the process or of the operating system on
// every platform, and for a power cut on Linux where the filesystem sends
// the device a flush and the device honours it (not with barriers off, or
// on a virtual disk with unsafe caching). On macOS the sync is fsync(2), not a full flush: after a power
// cut on a drive that reorders its writes, the cache can hold a wrong
// object under a C4 ID, and nothing here finds it. A lost name only loses
// a cache entry, so directories are not synced. On a node, layers hardlink objects;
// an object's link count above one means a layer uses it, and such an
// object is never evicted until the layer goes.
package objcache

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// tmp must be a regular file: anything else is refused, since a link there
// would have its target given the cache's mode and date. Whatever happens,
// tmp is gone when Commit returns.
func (c *Cache) Commit(tmp string, id c4.ID) (err error) {
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	st, err := os.Lstat(tmp)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("commit %s: %s is not a regular file", id, filepath.Base(tmp))
	}
	final := c.Path(id)
	if _, err := os.Stat(final); err == nil {
		os.Remove(tmp)
		c.touch(id)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(tmp, c.set.opts.Mode); err != nil {
		return err
	}
	if err := os.Chtimes(tmp, time.Now(), time.Unix(0, 0)); err != nil {
		return err
	}
	if err := publish(tmp, final, false); err != nil {
		return err
	}
	c.set.add(st.Size())
	c.touch(id)
	return nil
}

// syncFile makes a file's bytes durable. A variable so a test can see
// that it ran, on what, and before which name existed.
var syncFile = syncData

// linkFile is os.Link. A variable so a test can refuse a link the way a
// second filesystem does, and so reach the copy that stands in for it.
var linkFile = os.Link

// publish gives the file at tmp the name final, and is the only way a file
// gets a name under objects or xobjects: the bytes first, then the name.
// It syncs the file, then renames it, or with byLink links it (tmp stays,
// and a name already there is fs.ErrExist). A sync that fails names
// nothing.
//
// The file is opened by name to be synced, since callers hand over a path
// they have closed. On Linux a write-back error raised after the writer
// closed, on an inode since dropped from memory, is not reported to this
// descriptor; syncing the writer's own descriptor would close that, and
// would mean every caller passing an open file.
func publish(tmp, final string, byLink bool) error {
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	err = syncFile(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if byLink {
		return os.Link(tmp, final)
	}
	return os.Rename(tmp, final)
}

// Adopt makes a file of a dead upper directory the cache's object for
// id, without copying. The caller has hashed it. src and the object are
// then one inode, which takes the cache's mode and date, so src must
// never be used again: a write to it would change the object under its
// ID. For a file that stays in use, take a Snapshot and Commit that.
//
// exec says src is an executable. The link then goes to the executable
// variant, so src keeps an executable bit and a capture that is retried
// reads the same entry; the plain object is a copy, so each captured
// executable is stored twice. A variant already cached is left as it is,
// and so is a plain object: src is linked, and changed, only for the one
// that is missing.
func (c *Cache) Adopt(src string, id c4.ID, exec bool) error {
	if !exec {
		if c.Has(id) {
			c.touch(id)
			return nil
		}
		return c.linkInto(src, c.Path(id), c.set.opts.Mode, id)
	}
	if c.set.opts.ExecMode&0o111 != 0 {
		if _, err := os.Stat(c.ExecPathOf(id)); err != nil {
			if err := c.linkInto(src, c.ExecPathOf(id), c.set.opts.ExecMode, id); err != nil {
				return err
			}
		}
	}
	if c.Has(id) {
		c.touch(id)
		return nil
	}
	// The plain object is its own inode: a link would give src the plain
	// mode, and the retry would read the file as not executable.
	tmp, err := c.TempFile()
	if err != nil {
		return err
	}
	tmp.Close()
	if err := copyFile(src, tmp.Name()); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return c.Commit(tmp.Name(), id)
}

// linkInto hardlinks src to final (copying across filesystems) with the
// given mode and the epoch date. The name it uses in tmp is gone when it
// returns, whatever happens.
func (c *Cache) linkInto(src, final string, mode os.FileMode, id c4.ID) error {
	tmp, err := c.TempFile()
	if err != nil {
		return err
	}
	name := tmp.Name()
	tmp.Close()
	os.Remove(name)
	if err := linkFile(src, name); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		if err := copyFile(src, name); err != nil {
			os.Remove(name)
			return err
		}
	}
	if final == c.Path(id) {
		return c.Commit(name, id)
	}
	defer os.Remove(name)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	if err := os.Chtimes(name, time.Now(), time.Unix(0, 0)); err != nil {
		return err
	}
	st, err := os.Stat(name)
	if err != nil {
		return err
	}
	// Published with a link, which fails if another adopter got there
	// first: then the variant is there and was counted once.
	if err := publish(name, final, true); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	c.set.add(st.Size())
	return nil
}

// Snapshot copies src into the partition's tmp directory and returns the
// copy's path: a clone where the filesystem has one (APFS), so no bytes
// move, and a plain copy elsewhere. The copy shares no inode with src, so
// nothing done to either reaches the other, and it keeps nothing of src
// but its bytes: no flags, no extended attributes. Hash the copy and
// Commit it under that ID, or remove it. This is how a file that stays
// in use (a live workspace's) enters the cache.
//
// src must be a regular file, and its last component is never followed:
// the walk saw a file, but in a live workspace the name may be a symlink,
// a FIFO or a folder by now, and following it would copy something else.
// A caller that must not leave a directory tree opens the file beneath it
// (os.Root) and calls SnapshotFile.
func (c *Cache) Snapshot(src string) (string, error) {
	f, err := os.OpenFile(src, SnapshotOpenFlags, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return c.SnapshotFile(f)
}

// SnapshotOpenFlags opens a file for SnapshotFile: read-only, never
// following a symlink at the name, never blocking on a FIFO.
const SnapshotOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// SnapshotFile is Snapshot of a file already open.
func (c *Cache) SnapshotFile(f *os.File) (string, error) {
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is no longer a regular file", f.Name())
	}
	name, err := c.tempName()
	if err != nil {
		return "", err
	}
	if err := cloneOpenFile(f, name); err == nil {
		return name, nil
	}
	os.Remove(name)
	// A fresh name: a clone that could not be cleaned may still hold the first.
	if name, err = c.tempName(); err != nil {
		return "", err
	}
	out, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		os.Remove(name)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// tempName returns a name in the partition's tmp directory that no file has.
func (c *Cache) tempName() (string, error) {
	tmp, err := c.TempFile()
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	tmp.Close()
	return name, os.Remove(name)
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
	// Gone once it has its name; removed here on every other way out.
	defer os.Remove(tmp.Name())
	if err := copyFile(c.Path(id), tmp.Name()); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), c.set.opts.ExecMode); err != nil {
		return "", err
	}
	os.Chtimes(tmp.Name(), time.Now(), time.Unix(0, 0))
	st, _ := os.Stat(tmp.Name())
	if err := publish(tmp.Name(), p, false); err != nil {
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

// Refused takes out the copy of id that a store refused as bytes that are
// not id's. Committing an ID already cached keeps what is there, so
// without this every later send of id would send those bytes again. The
// plain object goes only if it is still seen, the file that was sent: a
// copy committed since is another file and stays. The executable variant
// is copied from the plain object without being hashed, so it is hashed
// here and goes only if it is not id's either. Refused reports whether it
// took the plain object out.
func (c *Cache) Refused(id c4.ID, seen os.FileInfo) (bool, error) {
	removed := false
	fi, err := os.Lstat(c.Path(id))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return false, err
	case seen == nil || os.SameFile(fi, seen):
		if err := c.Remove(id); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		removed = true
	}
	xp := c.ExecPathOf(id)
	xfi, err := os.Lstat(xp)
	if err != nil {
		return removed, nil
	}
	f, err := os.Open(xp)
	if err != nil {
		return removed, nil
	}
	got := c4.Identify(f)
	f.Close()
	if got == id {
		return removed, nil
	}
	if now, err := os.Lstat(xp); err == nil && os.SameFile(now, xfi) && os.Remove(xp) == nil {
		c.set.add(-xfi.Size())
	}
	return removed, nil
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
