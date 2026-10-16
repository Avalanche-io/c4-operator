// Package cache is the per-node content-addressed cache.
//
// Layout under the root (one filesystem, so views can hardlink objects):
//
//	objects/          a c4 TreeStore keyed by C4 ID
//	views/<viewID>/   materialized trees, files hardlinked from objects/
//	meta/<viewID>.c4m the view's description (rechecked on load)
//	meta/<viewID>.json retention and last use
//
// The filesystem is the index. An object's link count is its reference
// count (1 means no view uses it), its access time is its LRU position,
// and its modification time is a fixed sentinel: any write to an object,
// including one made through a view, moves the mtime and fails the cheap
// check, so the object is rehashed and refetched before it is served
// again. Full verification rehashes regardless.
package cache

import (
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"
	"github.com/go-logr/logr"
)

// Sentinel is the modification time every cached object carries. It is
// c4m's null timestamp, so materialized files read as "time unknown".
var Sentinel = c4m.NullTimestamp()

// objectPerm is the mode of every cached object: read-only to everyone.
const objectPerm = 0o444

// ErrMissing reports content absent from the store.
var ErrMissing = errors.New("content not in store")

// Source is the read side of a store.
type Source interface {
	Open(id c4.ID) (io.ReadCloser, error)
	Has(id c4.ID) bool
}

// Cache is a node's content-addressed cache.
type Cache struct {
	root    string
	objects *store.TreeStore
	budget  int64
	log     logr.Logger

	locks     keyedMutex // per object
	viewLocks keyedMutex // per view
	// gc excludes eviction from in-flight builds: builds take the read
	// side, eviction the write side.
	gc sync.RWMutex

	mu    sync.Mutex
	bytes int64
}

// Open opens or creates a cache at root with a byte budget for objects.
func Open(root string, budget int64, log logr.Logger) (*Cache, error) {
	for _, d := range []string{"objects", "views", "meta"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	ts, err := store.NewTreeStore(filepath.Join(root, "objects"))
	if err != nil {
		return nil, err
	}
	c := &Cache{root: root, objects: ts, budget: budget, log: log}
	c.cleanTemps()
	err = ts.Walk(func(_ c4.ID, size int64) error {
		c.bytes += size
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// cleanTemps removes leftovers of interrupted fetches and builds.
func (c *Cache) cleanTemps() {
	for _, pat := range []string{"objects/.ingest.*", "views/.build-*", "views/.old-*"} {
		matches, _ := filepath.Glob(filepath.Join(c.root, pat))
		for _, m := range matches {
			os.RemoveAll(m)
		}
	}
}

// Root returns the cache root.
func (c *Cache) Root() string { return c.root }

// Bytes returns the bytes held in objects.
func (c *Cache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// Budget returns the object byte budget.
func (c *Cache) Budget() int64 { return c.budget }

// Result describes one Ensure.
type Result struct {
	Hit       bool  // present and valid; nothing fetched
	Refetched bool  // present but failed verification; replaced
	Fetched   int64 // bytes read from the store
	Reason    string
}

// Present reports whether id is cached and passes the cheap check,
// without fetching. It is used to plan a preflight.
func (c *Cache) Present(id c4.ID, size int64) bool {
	p, ok := c.objects.ContentPath(id)
	if !ok {
		return false
	}
	return c.check(p, id, size, false) == ""
}

// Ensure makes id present and valid in the cache, fetching from src
// when absent or when the cached copy fails verification. full rehashes
// a present object instead of trusting the stat check.
func (c *Cache) Ensure(id c4.ID, size int64, src Source, full bool) (Result, error) {
	unlock := c.locks.lock(id)
	defer unlock()

	var res Result
	if p, ok := c.objects.ContentPath(id); ok {
		reason := c.check(p, id, size, full)
		if reason == "" {
			now := time.Now()
			os.Chtimes(p, now, Sentinel) // LRU position; mtime unchanged
			res.Hit = true
			return res, nil
		}
		c.log.Info("cached object failed verification; refetching", "id", id.String(), "reason", reason)
		c.remove(id, p)
		res.Refetched = true
		res.Reason = reason
	}

	n, err := c.fetch(id, src)
	if err != nil {
		return res, err
	}
	if size >= 0 && n != size {
		c.remove(id, "")
		return res, fmt.Errorf("%s: store returned %d bytes, description says %d", id, n, size)
	}
	res.Fetched = n
	return res, nil
}

// fetch streams id from src into the store. TreeStore.Put names the
// object by the hash of the bytes it actually wrote, so a wrong or
// truncated response can never appear under the requested ID.
func (c *Cache) fetch(id c4.ID, src Source) (int64, error) {
	rc, err := src.Open(id)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %v", ErrMissing, id, err)
	}
	defer rc.Close()
	got, err := c.objects.Put(rc)
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", id, err)
	}
	if got != id {
		// got is genuine content under its true name; leave it to the
		// evictor and refuse the request.
		return 0, fmt.Errorf("fetch %s: store returned bytes that hash to %s", id, got)
	}
	p, ok := c.objects.ContentPath(id)
	if !ok {
		return 0, fmt.Errorf("fetch %s: object vanished after write", id)
	}
	if err := os.Chmod(p, objectPerm); err != nil {
		return 0, err
	}
	if err := os.Chtimes(p, time.Now(), Sentinel); err != nil {
		return 0, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.bytes += st.Size()
	c.mu.Unlock()
	return st.Size(), nil
}

// check returns "" when the object at p is valid, or why it is not.
func (c *Cache) check(p string, id c4.ID, size int64, full bool) string {
	st, err := os.Lstat(p)
	if err != nil {
		return err.Error()
	}
	switch {
	case !st.Mode().IsRegular():
		return "not a regular file"
	case size >= 0 && st.Size() != size:
		return fmt.Sprintf("size %d, want %d", st.Size(), size)
	case !st.ModTime().Equal(Sentinel):
		return "modified since it was verified (mtime moved)"
	case st.Mode().Perm() != objectPerm:
		return fmt.Sprintf("mode changed to %v", st.Mode().Perm())
	}
	if !full {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return err.Error()
	}
	var got c4.ID
	copy(got[:], h.Sum(nil))
	if got != id {
		return "content does not hash to its ID"
	}
	return ""
}

func (c *Cache) remove(id c4.ID, p string) {
	if p == "" {
		p, _ = c.objects.ContentPath(id)
	}
	st, err := os.Lstat(p)
	if err != nil {
		return
	}
	if err := c.objects.Remove(id); err != nil {
		return
	}
	c.mu.Lock()
	c.bytes -= st.Size()
	c.mu.Unlock()
}

// Path returns the cached object's path for linking.
func (c *Cache) Path(id c4.ID) (string, error) {
	p, ok := c.objects.ContentPath(id)
	if !ok {
		return "", fmt.Errorf("%s not in cache", id)
	}
	return p, nil
}

// BuildLock holds off eviction while a view is assembled; call the
// returned func when done.
func (c *Cache) BuildLock() func() {
	c.gc.RLock()
	return c.gc.RUnlock
}

// ViewLock serializes work on one view.
func (c *Cache) ViewLock(id c4.ID) func() {
	return c.viewLocks.lock(id)
}

// ViewDir is where view id is materialized.
func (c *Cache) ViewDir(id c4.ID) string {
	return filepath.Join(c.root, "views", id.String())
}

// keyedMutex is a lock per ID.
type keyedMutex struct {
	mu sync.Mutex
	m  map[c4.ID]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(id c4.ID) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[c4.ID]*keyedEntry{}
	}
	e, ok := k.m[id]
	if !ok {
		e = &keyedEntry{}
		k.m[id] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, id)
		}
		k.mu.Unlock()
	}
}

func isViewName(name string) bool {
	return len(name) == 90 && strings.HasPrefix(name, "c4")
}
