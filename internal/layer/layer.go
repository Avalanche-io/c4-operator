// Package layer builds the immutable directories that overlay stacks as
// lowers, in one cache partition on a node.
//
// A full layer is a whole view: directories, and files hardlinked from the
// partition's object cache. A delta layer holds what changes from one view
// to another: added and modified files (hardlinked), removed paths as
// overlay whiteouts, and replaced directories marked opaque. A stack is
// the list of layers that, mounted together, show one view: a full layer
// at the bottom and deltas above it.
//
// Layers are built in a temporary directory and renamed into place, and
// never change after that. A layer costs inodes for its directories and
// directory entries for its files, not the files' bytes.
package layer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// MaxDepth is the most layers a stack may have. Overlay's mount options
// must fit in one page, and every lookup walks every layer.
const MaxDepth = 16

// DirMode is the mode of every directory in a layer: like an emptyDir, a
// writable view lets any container user create files.
const DirMode = 0o777

// Kind of layer.
type Kind string

const (
	Full  Kind = "full"
	Delta Kind = "delta"
)

// Meta describes a layer.
type Meta struct {
	Key      string    `json:"key"`
	Kind     Kind      `json:"kind"`
	From     string    `json:"from,omitempty"` // delta: the view it applies to
	To       string    `json:"to"`             // the view it produces
	Entries  int       `json:"entries"`        // directory entries placed
	Files    int       `json:"files"`          // files of the produced view (full layers)
	Built    time.Time `json:"built"`
	LastUsed time.Time `json:"lastUsed"`
}

// Stack is the layers that show one view, top first.
type Stack struct {
	View   string    `json:"view"`
	Layers []string  `json:"layers"` // keys, top first
	Base   string    `json:"base"`   // the view of the bottom full layer
	Files  int       `json:"files"`  // files in the view
	Built  time.Time `json:"built"`
}

// Store holds the layers and stacks of one partition.
type Store struct {
	cache  *objcache.Cache
	layers string
	stacks string
	mu     sync.Mutex
}

// Open returns the layer store of a cache partition.
func Open(cache *objcache.Cache) (*Store, error) {
	s := &Store{
		cache:  cache,
		layers: filepath.Join(cache.Dir(), "layers"),
		stacks: filepath.Join(cache.Dir(), "stacks"),
	}
	for _, d := range []string{s.layers, s.stacks} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	// Builds interrupted by a restart leave temporary directories.
	matches, _ := filepath.Glob(filepath.Join(s.layers, ".build-*"))
	for _, m := range matches {
		os.RemoveAll(m)
	}
	return s, nil
}

// FullKey names the full layer of a view.
func FullKey(v c4.ID) string { return "f" + v.String()[2:42] }

// DeltaKey names the delta layer from one view to another.
func DeltaKey(from, to c4.ID) string {
	id := c4.Identify(strings.NewReader(from.String() + "\n" + to.String()))
	return "d" + id.String()[2:42]
}

// Dir is a layer's directory.
func (s *Store) Dir(key string) string { return filepath.Join(s.layers, key) }

// Has reports whether a layer is built.
func (s *Store) Has(key string) bool {
	_, err := os.Stat(filepath.Join(s.layers, key+".json"))
	return err == nil
}

// Stack returns the stack of a view, if every layer of it is present.
func (s *Store) Stack(v c4.ID) (*Stack, bool) {
	data, err := os.ReadFile(filepath.Join(s.stacks, v.String()+".json"))
	if err != nil {
		return nil, false
	}
	var st Stack
	if json.Unmarshal(data, &st) != nil || st.View != v.String() {
		return nil, false
	}
	for _, k := range st.Layers {
		if !s.Has(k) {
			return nil, false
		}
	}
	return &st, true
}

// Dirs returns a stack's layer directories, top first.
func (s *Store) Dirs(st *Stack) []string {
	out := make([]string, len(st.Layers))
	for i, k := range st.Layers {
		out[i] = s.Dir(k)
	}
	return out
}

// SaveStack records a stack.
func (s *Store) SaveStack(st *Stack) error {
	if len(st.Layers) == 0 || len(st.Layers) > MaxDepth {
		return fmt.Errorf("stack of %d layers", len(st.Layers))
	}
	return writeJSON(filepath.Join(s.stacks, st.View+".json"), st)
}

// Touch records that a stack's layers were used now.
func (s *Store) Touch(st *Stack) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, k := range st.Layers {
		m, err := s.meta(k)
		if err != nil {
			continue
		}
		m.LastUsed = now
		writeJSON(filepath.Join(s.layers, k+".json"), m)
	}
}

func (s *Store) meta(key string) (*Meta, error) {
	data, err := os.ReadFile(filepath.Join(s.layers, key+".json"))
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Layers lists every built layer, least recently used first.
func (s *Store) Layers() ([]Meta, error) {
	matches, err := filepath.Glob(filepath.Join(s.layers, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, m := range matches {
		key := strings.TrimSuffix(filepath.Base(m), ".json")
		meta, err := s.meta(key)
		if err != nil {
			continue
		}
		out = append(out, *meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.Before(out[j].LastUsed) })
	return out, nil
}

// BuildFull builds the full layer of v. Every object of v must be cached.
func (s *Store) BuildFull(v *view.View) (*Meta, error) {
	key := FullKey(v.ID)
	if m, err := s.meta(key); err == nil {
		return m, nil
	}
	tmp, err := s.tempDir(key)
	if err != nil {
		return nil, err
	}
	n, err := s.fill(tmp, v)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	m := &Meta{Key: key, Kind: Full, To: v.ID.String(), Entries: n, Files: len(v.Files), Built: time.Now(), LastUsed: time.Now()}
	return m, s.publish(key, tmp, m)
}

// fill places v's tree under dir.
func (s *Store) fill(dir string, v *view.View) (int, error) {
	n := 0
	for _, d := range v.Dirs {
		if err := mkdir(filepath.Join(dir, filepath.FromSlash(d))); err != nil {
			return n, err
		}
		n++
	}
	for _, f := range v.Files {
		if err := s.link(f.ID, f.Exec, filepath.Join(dir, filepath.FromSlash(f.Path))); err != nil {
			return n, fmt.Errorf("%s: %w", f.Path, err)
		}
		n++
	}
	for _, l := range v.Symlinks {
		if err := os.Symlink(l.Target, filepath.Join(dir, filepath.FromSlash(l.Path))); err != nil {
			return n, fmt.Errorf("%s: %w", l.Path, err)
		}
		n++
	}
	return n, nil
}

// link hardlinks a cached object (or its executable variant) to dst.
func (s *Store) link(id c4.ID, exec bool, dst string) error {
	src := s.cache.Path(id)
	if exec {
		p, err := s.cache.ExecPath(id)
		if err != nil {
			return err
		}
		src = p
	}
	if err := os.MkdirAll(filepath.Dir(dst), DirMode); err != nil {
		return err
	}
	return os.Link(src, dst)
}

// BuildDelta builds the layer that turns view from into view to, from
// the changes view.Diff computed. The objects of every added file must be
// cached.
func (s *Store) BuildDelta(from, to c4.ID, changes []view.Change) (*Meta, error) {
	key := DeltaKey(from, to)
	if m, err := s.meta(key); err == nil {
		return m, nil
	}
	tmp, err := s.tempDir(key)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, c := range changes {
		p := filepath.Join(tmp, filepath.FromSlash(strings.TrimSuffix(c.Path, "/")))
		if err := ensureParents(tmp, p); err != nil {
			os.RemoveAll(tmp)
			return nil, err
		}
		if err := s.place(p, c); err != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("%s: %w", c.Path, err)
		}
		n++
	}
	m := &Meta{Key: key, Kind: Delta, From: from.String(), To: to.String(), Entries: n, Built: time.Now(), LastUsed: time.Now()}
	return m, s.publish(key, tmp, m)
}

func (s *Store) place(p string, c view.Change) error {
	switch c.Op {
	case view.OpRemove:
		os.RemoveAll(p)
		return whiteout(p)
	case view.OpOpaque:
		os.RemoveAll(p)
		if err := mkdir(p); err != nil {
			return err
		}
		return setOpaque(p)
	}
	e := c.Entry
	switch {
	case e.IsDir():
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
			return nil
		}
		os.RemoveAll(p)
		return mkdir(p)
	case view.IsLink(e):
		os.RemoveAll(p)
		return os.Symlink(e.Target, p)
	}
	os.RemoveAll(p)
	return s.link(e.C4ID, view.IsExec(e), p)
}

// ensureParents creates the directories between root and p's parent as
// plain directories, which overlay merges with the same directories in
// lower layers.
func ensureParents(root, p string) error {
	parent := filepath.Dir(p)
	if parent == root {
		return nil
	}
	rel, err := filepath.Rel(root, parent)
	if err != nil {
		return err
	}
	cur := root
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		if err == nil && fi.IsDir() {
			continue
		}
		if err == nil {
			// A whiteout or file where a directory must merge: a type
			// change; the directory wins.
			os.Remove(cur)
		}
		if err := mkdir(cur); err != nil {
			return err
		}
	}
	return nil
}

func mkdir(p string) error {
	if err := os.MkdirAll(p, DirMode); err != nil {
		return err
	}
	return os.Chmod(p, DirMode)
}

func (s *Store) tempDir(key string) (string, error) {
	var b [6]byte
	rand.Read(b[:])
	tmp := filepath.Join(s.layers, ".build-"+key+"-"+hex.EncodeToString(b[:]))
	if err := os.Mkdir(tmp, DirMode); err != nil {
		return "", err
	}
	return tmp, os.Chmod(tmp, DirMode)
}

// publish renames a finished layer into place, then writes its meta: a
// layer exists once its meta exists.
func (s *Store) publish(key, tmp string, m *Meta) error {
	dst := s.Dir(key)
	if err := os.Rename(tmp, dst); err != nil {
		os.RemoveAll(tmp)
		if _, statErr := os.Stat(dst); statErr == nil {
			// Built concurrently by another publish; theirs is the same.
			return nil
		}
		return err
	}
	return writeJSON(filepath.Join(s.layers, key+".json"), m)
}

// Remove deletes a layer and every stack that uses it.
func (s *Store) Remove(key string) error {
	if err := os.Remove(filepath.Join(s.layers, key+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	old := filepath.Join(s.layers, ".build-gone-"+key)
	if err := os.Rename(s.Dir(key), old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	os.RemoveAll(old)
	matches, _ := filepath.Glob(filepath.Join(s.stacks, "*.json"))
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var st Stack
		if json.Unmarshal(data, &st) != nil {
			continue
		}
		for _, k := range st.Layers {
			if k == key {
				os.Remove(m)
				break
			}
		}
	}
	return nil
}

// GC removes layers no mounted volume uses and that were last used more
// than retain ago, least recently used first, until keep reports false
// (for example once the cache is back under budget). It returns how many
// layers it removed.
func (s *Store) GC(inUse map[string]bool, retain time.Duration, keepGoing func() bool) int {
	layers, err := s.Layers()
	if err != nil {
		return 0
	}
	removed := 0
	cutoff := time.Now().Add(-retain)
	for _, m := range layers {
		if !keepGoing() {
			break
		}
		if inUse[m.Key] || m.LastUsed.After(cutoff) {
			continue
		}
		if s.Remove(m.Key) == nil {
			removed++
		}
	}
	return removed
}

func writeJSON(p string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
