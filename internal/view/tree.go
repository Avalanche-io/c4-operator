// Package view defines views, the exact trees containers see, and the
// content-addressed records they are stored as.
//
// A view's identity is the C4 ID of its root record under one projection:
// names, sizes, content IDs and symlink targets are real; timestamps are
// null everywhere; mode is null everywhere except on executable regular
// files, which record -rwxr-xr-x. A tree without executable files or
// symlinks therefore has the ID `c4 id` reports in content mode.
//
// A record is the one-level canonical c4m listing of one directory's
// children under that projection. Its bytes hash to the directory's ID,
// so records are ordinary immutable objects: any cache tier can hold them,
// and a view can be fetched top-down from its ID alone (root record, then
// subdirectory records, then file objects).
package view

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// ExecMode is the mode an executable regular file records.
const ExecMode = 0o755

// emptyID is the ID of the empty listing: an empty directory's ID.
var emptyID = c4.Identify(strings.NewReader(""))

// EmptyID returns the ID of an empty directory.
func EmptyID() c4.ID { return emptyID }

// FileEntry returns the projected entry of a regular file.
func FileEntry(name string, size int64, id c4.ID, exec bool) *c4m.Entry {
	e := &c4m.Entry{Name: name, Size: size, C4ID: id, Timestamp: c4m.NullTimestamp()}
	if exec {
		e.Mode = ExecMode
	}
	return e
}

// SymlinkEntry returns the projected entry of a symlink: its target is
// real, its size and ID are null.
func SymlinkEntry(name, target string) *c4m.Entry {
	return &c4m.Entry{Name: name, Size: -1, Target: target, Timestamp: c4m.NullTimestamp()}
}

// Project returns the projected copy of an entry read from any
// description (a full c4m, a content-mode c4m, a git tree).
func Project(e *c4m.Entry) *c4m.Entry {
	cp := &c4m.Entry{Name: e.Name, Size: e.Size, C4ID: e.C4ID, Timestamp: c4m.NullTimestamp(), Depth: e.Depth}
	switch {
	case e.IsDir():
		cp.Name = strings.TrimSuffix(e.Name, "/") + "/"
		cp.Target = ""
	case e.Target != "" || e.IsSymlink():
		cp.Target = e.Target
		cp.Size = -1
		cp.C4ID = c4.ID{}
	case e.Mode&0o111 != 0:
		cp.Mode = ExecMode
	}
	return cp
}

// IsExec reports whether a projected entry is an executable file.
func IsExec(e *c4m.Entry) bool { return !e.IsDir() && e.Target == "" && e.Mode&0o111 != 0 }

// IsLink reports whether an entry is a symlink.
func IsLink(e *c4m.Entry) bool { return e.Target != "" || e.IsSymlink() }

// Node is a directory of a view in memory. Files holds regular files and
// symlinks; Dirs holds subdirectories by name ("x/").
type Node struct {
	Name  string // "x/"; empty for the root
	Files map[string]*c4m.Entry
	Dirs  map[string]*Node
	// Entry is this directory's projected entry as its parent lists it,
	// filled by Identify.
	Entry *c4m.Entry
}

// NewNode returns an empty directory.
func NewNode(name string) *Node {
	return &Node{Name: name, Files: map[string]*c4m.Entry{}, Dirs: map[string]*Node{}}
}

// Dir returns the directory at path p ("a/b/"), creating it and its parents.
func (n *Node) Dir(p string) *Node {
	cur := n
	for _, c := range splitDir(p) {
		next, ok := cur.Dirs[c]
		if !ok {
			next = NewNode(c)
			cur.Dirs[c] = next
		}
		cur = next
	}
	return cur
}

// Lookup returns the directory at p, or nil.
func (n *Node) Lookup(p string) *Node {
	cur := n
	for _, c := range splitDir(p) {
		next, ok := cur.Dirs[c]
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// splitDir splits "a/b/" into ["a/", "b/"].
func splitDir(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] += "/"
	}
	return parts
}

// SplitPath splits a slash path into its parent directory ("a/b/", or ""
// at the top) and its base name. Directories keep their trailing slash.
func SplitPath(p string) (dir, name string) {
	trimmed := strings.TrimSuffix(p, "/")
	i := strings.LastIndex(trimmed, "/")
	if i < 0 {
		return "", p
	}
	return p[:i+1], p[i+1:]
}

// listing returns a directory's children as a one-level manifest, in
// canonical order. Identify must have run on the subdirectories.
func (n *Node) listing() *c4m.Manifest {
	children := make([]*c4m.Entry, 0, len(n.Files)+len(n.Dirs))
	for _, e := range n.Files {
		children = append(children, e)
	}
	for _, d := range n.Dirs {
		children = append(children, d.Entry)
	}
	return listingOf(children)
}

// Record returns the directory's record: the canonical text whose C4 ID
// is the directory's ID. Identify must have run.
func (n *Node) Record() []byte {
	return []byte(n.listing().Canonical())
}

// Identify computes IDs and sizes bottom-up, the way the c4 scanner does
// in content mode: a directory's ID is the ID of its one-level canonical
// listing, and its size comes from c4m.PropagateMetadata over itself and
// its direct children. It returns the ID of n.
func (n *Node) Identify() c4.ID {
	for _, d := range n.Dirs {
		d.Identify()
	}
	n.Entry = dirEntry(n.Name, n.listing())
	return n.Entry.C4ID
}

// dirEntry is the projected entry of a directory whose one-level listing
// (canonicalized) is m.
func dirEntry(name string, m *c4m.Manifest) *c4m.Entry {
	// The ID is the hash of the listing exactly as recorded. ComputeC4ID
	// would first re-run PropagateMetadata over the one-level listing, which
	// reads a child directory with a null size (one holding a symlink) as
	// empty and records 0, so the record would disagree with the child's
	// own entry and with c4m's ID of the same tree as one nested manifest.
	id := c4.Identify(strings.NewReader(m.Canonical()))
	self := &c4m.Entry{Name: name, Size: -1, Timestamp: c4m.NullTimestamp()}
	scope := []*c4m.Entry{self}
	for _, c := range m.Entries {
		cp := *c
		cp.Depth = 1
		scope = append(scope, &cp)
	}
	if len(m.Entries) == 0 {
		self.Size = 0
	}
	c4m.PropagateMetadata(scope)
	self.C4ID = id
	self.Timestamp = c4m.NullTimestamp()
	return self
}

// listingOf canonicalizes a set of child entries into a one-level listing.
func listingOf(children []*c4m.Entry) *c4m.Manifest {
	m := c4m.NewManifest()
	for _, e := range children {
		cp := *e
		cp.Depth = 0
		m.AddEntry(&cp)
	}
	// Not Canonicalize: the children are already projected, and every child
	// directory's size was propagated by dirEntry from its own children.
	return m
}

// Walk visits every directory, depth first, parents before children.
func (n *Node) Walk(prefix string, fn func(dir string, n *Node) error) error {
	if err := fn(prefix, n); err != nil {
		return err
	}
	names := make([]string, 0, len(n.Dirs))
	for k := range n.Dirs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if err := n.Dirs[k].Walk(prefix+k, fn); err != nil {
			return err
		}
	}
	return nil
}

// Records returns every directory record of the tree rooted at n,
// deepest first, so that writing them in order leaves the root record
// last: when the root record exists, the whole tree does. Identify must
// have run.
func (n *Node) Records() []Record {
	var out []Record
	var visit func(d *Node, depth int)
	visit = func(d *Node, depth int) {
		for _, c := range d.Dirs {
			visit(c, depth+1)
		}
		out = append(out, Record{ID: d.Entry.C4ID, Data: d.Record(), depth: depth})
	}
	visit(n, 0)
	sort.SliceStable(out, func(i, j int) bool { return out[i].depth > out[j].depth })
	// Identical subtrees appear once.
	seen := map[c4.ID]bool{}
	dedup := out[:0]
	for _, r := range out {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		dedup = append(dedup, r)
	}
	return dedup
}

// Record is one directory record.
type Record struct {
	ID    c4.ID
	Data  []byte
	depth int
}

// ParseRecord decodes a record into its entries, projected, and checks
// that every name is safe to create on disk.
func ParseRecord(data []byte) ([]*c4m.Entry, error) {
	m, err := c4m.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("parse record: %w", err)
	}
	if !m.Base.IsNil() {
		return nil, fmt.Errorf("a record has no base; this is a changeset")
	}
	out := make([]*c4m.Entry, 0, len(m.Entries))
	for _, e := range m.Entries {
		if e.Depth != 0 {
			return nil, fmt.Errorf("not a one-level record: %q at depth %d", e.Name, e.Depth)
		}
		if err := CheckName(e.Name); err != nil {
			return nil, err
		}
		if e.IsSequence {
			return nil, fmt.Errorf("record entry %q is a sequence", e.Name)
		}
		out = append(out, Project(e))
	}
	return out, nil
}

// CheckName refuses anything that could escape a directory.
func CheckName(name string) error {
	base := strings.TrimSuffix(name, "/")
	if base == "" || base == "." || base == ".." || strings.ContainsAny(base, "/\\\x00") {
		return fmt.Errorf("unsafe entry name %q", name)
	}
	return nil
}
