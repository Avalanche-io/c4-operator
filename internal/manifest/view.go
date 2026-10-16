package manifest

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// File is one regular file of a view.
type File struct {
	Path string // relative, slash-separated
	ID   c4.ID
	Size int64 // -1 when the description does not record it
}

// Symlink is one symbolic link of a view, created as recorded and never
// followed by the materializer.
type Symlink struct {
	Path   string
	Target string
}

// View is exactly what lands on a node: directories, files, and links,
// after the include filter.
//
// ID is the content-level C4 ID of the tree (c4's content projection:
// names, sizes, and IDs real; mode and timestamp null at every level).
// For a tree without symlinks it equals what scanning the materialized
// directory in content mode reports, so a pod can check the tree it was
// given against the Materialization's status.viewID.
type View struct {
	ID       c4.ID
	Dirs     []string // relative, each ending in "/", parents before children
	Files    []File
	Symlinks []Symlink
	manifest *c4m.Manifest
}

// Object is one unique piece of content a view needs.
type Object struct {
	ID   c4.ID
	Size int64
}

// Totals summarizes a view.
type Totals struct {
	Files         int64
	Bytes         int64 // sum over files; duplicates count each time
	UniqueObjects int64
	UniqueBytes   int64 // what a cold node fetches
	SizesKnown    bool  // false when some file sizes were not recorded
}

// Objects returns the unique content IDs of the view, sorted by ID.
func (v *View) Objects() []Object {
	seen := make(map[c4.ID]int64, len(v.Files))
	for _, f := range v.Files {
		if old, ok := seen[f.ID]; !ok || old < f.Size {
			seen[f.ID] = f.Size
		}
	}
	out := make([]Object, 0, len(seen))
	for id, size := range seen {
		out = append(out, Object{ID: id, Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Cmp(out[j].ID) < 0 })
	return out
}

// Totals computes file and byte counts.
func (v *View) Totals() Totals {
	t := Totals{Files: int64(len(v.Files)), SizesKnown: true}
	for _, f := range v.Files {
		if f.Size < 0 {
			t.SizesKnown = false
			continue
		}
		t.Bytes += f.Size
	}
	for _, o := range v.Objects() {
		t.UniqueObjects++
		if o.Size > 0 {
			t.UniqueBytes += o.Size
		}
	}
	return t
}

// Marshal renders the view's content-projected c4m file. ParseView
// reverses it.
func (v *View) Marshal() ([]byte, error) {
	return c4m.Marshal(v.manifest)
}

// ParseView decodes a view c4m file and checks that it describes the
// tree want. Views cached on a node are rechecked this way on load: the
// file name is a claim, the recomputed ID is the fact.
func ParseView(data []byte, want c4.ID) (*View, error) {
	m, err := c4m.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, err
	}
	r := &resolver{seen: map[c4.ID]bool{}}
	if err := r.walk(m, ""); err != nil {
		return nil, err
	}
	v, err := newView(r.items, Filter{})
	if err != nil {
		return nil, err
	}
	if v.ID != want {
		return nil, fmt.Errorf("view description recomputes to %s, not %s", v.ID, want)
	}
	return v, nil
}

// dirNode is a directory of the view under construction.
type dirNode struct {
	name     string // "x/" (empty for the root)
	files    []*c4m.Entry
	children map[string]*dirNode
	entry    *c4m.Entry // this directory's own entry, resolved bottom-up
}

func newDirNode(name string) *dirNode {
	return &dirNode{name: name, children: map[string]*dirNode{}}
}

// newView applies the filter and computes content-level identity.
func newView(items []item, f Filter) (*View, error) {
	root := newDirNode("")
	v := &View{}
	seenPath := map[string]bool{}

	ensureDir := func(p string) *dirNode {
		n := root
		if p == "" {
			return n
		}
		acc := ""
		for _, c := range strings.SplitAfter(p, "/") {
			if c == "" {
				continue
			}
			acc += c
			child, ok := n.children[c]
			if !ok {
				child = newDirNode(c)
				n.children[c] = child
				v.Dirs = append(v.Dirs, acc)
			}
			n = child
		}
		return n
	}
	parentOf := func(p string) string {
		i := strings.LastIndex(strings.TrimSuffix(p, "/"), "/")
		if i < 0 {
			return ""
		}
		return p[:i+1]
	}

	for _, it := range items {
		if seenPath[it.path] {
			return nil, fmt.Errorf("path %s appears twice in the description", it.path)
		}
		seenPath[it.path] = true
		if !f.Includes(it.path) {
			continue
		}
		if it.isDir {
			ensureDir(it.path)
			continue
		}
		dir := ensureDir(parentOf(it.path))
		name := it.path[len(parentOf(it.path)):]
		ce := &c4m.Entry{
			Name:      name,
			Size:      it.entry.Size,
			C4ID:      it.entry.C4ID,
			Timestamp: c4m.NullTimestamp(),
		}
		if it.isLink {
			ce.Target = it.entry.Target
			v.Symlinks = append(v.Symlinks, Symlink{Path: it.path, Target: it.entry.Target})
		} else {
			v.Files = append(v.Files, File{Path: it.path, ID: it.entry.C4ID, Size: it.entry.Size})
		}
		dir.files = append(dir.files, ce)
	}

	sort.Strings(v.Dirs) // "a/" < "a/b/": parents first
	sort.Slice(v.Files, func(i, j int) bool { return v.Files[i].Path < v.Files[j].Path })
	sort.Slice(v.Symlinks, func(i, j int) bool { return v.Symlinks[i].Path < v.Symlinks[j].Path })

	v.ID = identify(root)
	m := c4m.NewManifest()
	emit(m, root, 0)
	v.manifest = m
	return v, nil
}

// identify resolves each directory bottom-up the way the c4 scanner does
// in content mode: size and timestamp from c4m.PropagateMetadata over
// [self, direct children], ID from the one-level canonical listing of
// the direct children (Manifest.ComputeC4ID over just those).
func identify(n *dirNode) c4.ID {
	children := make([]*c4m.Entry, 0, len(n.files)+len(n.children))
	children = append(children, n.files...)
	for _, c := range n.children {
		identify(c)
		children = append(children, c.entry)
	}

	listing := c4m.NewManifest()
	for _, c := range children {
		cp := *c
		cp.Depth = 0
		listing.AddEntry(&cp)
	}
	id := listing.ComputeC4ID()

	self := &c4m.Entry{Name: n.name, Size: -1, Timestamp: c4m.NullTimestamp()}
	scope := []*c4m.Entry{self}
	for _, c := range children {
		cp := *c
		cp.Depth = 1
		scope = append(scope, &cp)
	}
	c4m.PropagateMetadata(scope)
	self.C4ID = id
	n.entry = self
	return id
}

// emit writes the tree into m in walk order: files first, then
// directories, each in natural order.
func emit(m *c4m.Manifest, n *dirNode, depth int) {
	files := append([]*c4m.Entry(nil), n.files...)
	sort.Slice(files, func(i, j int) bool { return c4m.NaturalLess(files[i].Name, files[j].Name) })
	for _, f := range files {
		cp := *f
		cp.Depth = depth
		m.AddEntry(&cp)
	}
	names := make([]string, 0, len(n.children))
	for k := range n.children {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return c4m.NaturalLess(names[i], names[j]) })
	for _, k := range names {
		c := n.children[k]
		cp := *c.entry
		cp.Depth = depth
		m.AddEntry(&cp)
		emit(m, c, depth+1)
	}
}
