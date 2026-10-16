package view

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// Edit is one change to a view, as capture or a git import finds it.
type Edit struct {
	// Path is relative and slash-separated. A directory may end in "/".
	Path string
	// Entry is the projected file, symlink or directory entry to put at
	// Path, creating parent directories as needed. Nil deletes Path and
	// everything under it.
	Entry *c4m.Entry
	// Opaque, with a directory Entry, drops everything the parent view had
	// under the directory before later edits fill it.
	Opaque bool
}

// Applied is the result of Apply.
type Applied struct {
	Parent c4.ID
	ID     c4.ID // the new view; equal to Parent when nothing changed
	// Records new to this view (directories along changed paths), deepest
	// first. Every other record is the parent's.
	Records []Record
	// Changeset is the c4m patch: Parent on the first line, then the
	// changed entries. Empty when nothing changed.
	Changeset   []byte
	ChangesetID c4.ID

	Added, Modified, Deleted int
	Bytes                    int64 // size of added and modified files
	// Files lists the added and modified regular files, the objects a
	// writer must make sure the store holds.
	Files []ChangedFile
}

// ChangedFile is an added or modified regular file of an Applied view.
type ChangedFile struct {
	Path string
	ID   c4.ID
	Size int64
	Exec bool
}

// Apply applies edits to the view parent and returns the new view's ID,
// the records it adds, and the changeset that describes it. It reads only
// the parent's records along edited paths, so its cost is proportional
// to the change, not to the size of the view.
//
// Edits apply in order. Callers that mix kinds should pass opaque
// directories first, then deletions, then puts with parents before
// children; SortEdits does that.
func Apply(ctx context.Context, parent c4.ID, edits []Edit, src RecordSource) (*Applied, error) {
	t := &atree{ctx: ctx, src: src, root: &anode{base: parent}}
	for _, e := range edits {
		if err := t.apply(e); err != nil {
			return nil, err
		}
	}
	res := &Applied{Parent: parent}
	rootEntry := t.identify(t.root, &res.Records)
	res.ID = rootEntry.C4ID
	if res.ID == parent {
		res.Records = nil
		return res, nil
	}
	var patch []*c4m.Entry
	if err := t.emit(t.root, "", 0, &patch, res); err != nil {
		return nil, err
	}
	m := c4m.NewManifest()
	m.Entries = patch
	var buf bytes.Buffer
	if err := c4m.NewEncoder(&buf).EncodePatch(&c4m.PatchResult{Patch: m, OldID: parent, NewID: res.ID}); err != nil {
		return nil, err
	}
	// The closing checkpoint seals the patch: the changeset is a one-section
	// c4m chain, verifiable and appendable to a stream.
	buf.WriteString(res.ID.String())
	buf.WriteByte('\n')
	res.Changeset = buf.Bytes()
	res.ChangesetID = c4.Identify(bytes.NewReader(res.Changeset))
	return res, nil
}

// SortEdits orders edits for Apply: opaque directories, then deletions,
// then puts, each by path so parents come before children.
func SortEdits(edits []Edit) {
	rank := func(e Edit) int {
		switch {
		case e.Opaque:
			return 0
		case e.Entry == nil:
			return 1
		}
		return 2
	}
	sort.SliceStable(edits, func(i, j int) bool {
		ri, rj := rank(edits[i]), rank(edits[j])
		if ri != rj {
			return ri < rj
		}
		return edits[i].Path < edits[j].Path
	})
}

// anode is a directory of the view being edited. Its children are read
// from its base record only when an edit reaches it.
type anode struct {
	name   string
	base   c4.ID                 // ID in the parent view; nil for a new directory
	orig   map[string]*c4m.Entry // base children by name, once loaded
	files  map[string]*c4m.Entry
	dirs   map[string]*anode
	loaded bool
	dirty  bool
	entry  *c4m.Entry // as the parent lists it: the base entry, or recomputed
}

type atree struct {
	ctx  context.Context
	src  RecordSource
	root *anode
}

func (t *atree) load(n *anode) error {
	if n.loaded {
		return nil
	}
	n.loaded = true
	n.files = map[string]*c4m.Entry{}
	n.dirs = map[string]*anode{}
	n.orig = map[string]*c4m.Entry{}
	if n.base.IsNil() || n.base == emptyID {
		return nil
	}
	entries, err := fetchRecord(t.ctx, t.src, n.base)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n.orig[e.Name] = e
		if !e.IsDir() {
			n.files[e.Name] = e
			continue
		}
		n.dirs[e.Name] = &anode{name: e.Name, base: e.C4ID, entry: e}
	}
	return nil
}

// walkTo returns the directory at the components dirs, loading and marking
// every directory on the way dirty. With create, missing directories are
// made (replacing a file of the same name); without it, a missing one
// returns nil.
func (t *atree) walkTo(dirs []string, create bool) (*anode, error) {
	cur := t.root
	path := []*anode{cur}
	for _, c := range dirs {
		if err := t.load(cur); err != nil {
			return nil, err
		}
		next, ok := cur.dirs[c]
		if !ok {
			if !create {
				return nil, nil
			}
			delete(cur.files, strings.TrimSuffix(c, "/"))
			next = &anode{name: c, loaded: true, files: map[string]*c4m.Entry{}, dirs: map[string]*anode{}}
			cur.dirs[c] = next
		}
		cur = next
		path = append(path, cur)
	}
	if err := t.load(cur); err != nil {
		return nil, err
	}
	for _, n := range path {
		n.dirty = true
	}
	return cur, nil
}

func (t *atree) apply(e Edit) error {
	p := strings.TrimPrefix(e.Path, "/")
	if p == "" {
		return fmt.Errorf("edit with an empty path")
	}
	isDir := strings.HasSuffix(p, "/") || (e.Entry != nil && e.Entry.IsDir())
	dir, name := SplitPath(strings.TrimSuffix(p, "/"))
	for _, c := range splitDir(dir) {
		if err := CheckName(c); err != nil {
			return err
		}
	}
	if err := CheckName(name); err != nil {
		return err
	}

	switch {
	case e.Entry == nil:
		parent, err := t.walkTo(splitDir(dir), false)
		if err != nil || parent == nil {
			return err
		}
		delete(parent.files, name)
		delete(parent.dirs, name+"/")
		return nil

	case isDir:
		n, err := t.walkTo(splitDir(dir+name+"/"), true)
		if err != nil {
			return err
		}
		if e.Opaque {
			n.files = map[string]*c4m.Entry{}
			n.dirs = map[string]*anode{}
		}
		return nil

	default:
		parent, err := t.walkTo(splitDir(dir), true)
		if err != nil {
			return err
		}
		delete(parent.dirs, name+"/")
		cp := Project(e.Entry)
		cp.Name = name
		cp.Depth = 0
		parent.files[name] = cp
		return nil
	}
}

// identify recomputes the entries of dirty directories bottom-up and
// collects their records, deepest first.
func (t *atree) identify(n *anode, records *[]Record) *c4m.Entry {
	if !n.dirty {
		if n.entry == nil {
			// The untouched root.
			return &c4m.Entry{C4ID: n.base}
		}
		return n.entry
	}
	children := make([]*c4m.Entry, 0, len(n.files)+len(n.dirs))
	for _, e := range n.files {
		children = append(children, e)
	}
	names := make([]string, 0, len(n.dirs))
	for k := range n.dirs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		children = append(children, t.identify(n.dirs[k], records))
	}
	m := listingOf(children)
	n.entry = dirEntry(n.name, m)
	if n.entry.C4ID != n.base {
		*records = append(*records, Record{ID: n.entry.C4ID, Data: []byte(m.Canonical())})
	}
	return n.entry
}

// emit writes the patch entries for directory n at depth: removals as
// exact copies of the parent's entries, additions and modifications as
// new entries, and changed directories as their new entry followed by
// their own changes.
func (t *atree) emit(n *anode, prefix string, depth int, out *[]*c4m.Entry, res *Applied) error {
	at := func(e *c4m.Entry) *c4m.Entry {
		cp := *e
		cp.Depth = depth
		return &cp
	}
	for _, name := range sortedNames(n.orig) {
		if _, ok := n.files[name]; ok {
			continue
		}
		if _, ok := n.dirs[name]; ok {
			continue
		}
		*out = append(*out, at(n.orig[name]))
		res.Deleted++
	}
	for _, name := range sortedNames(n.files) {
		e := n.files[name]
		o, ok := n.orig[name]
		if ok && sameEntry(o, e) {
			continue
		}
		*out = append(*out, at(e))
		if ok {
			res.Modified++
		} else {
			res.Added++
		}
		if !IsLink(e) {
			res.Files = append(res.Files, ChangedFile{Path: prefix + name, ID: e.C4ID, Size: e.Size, Exec: IsExec(e)})
			if e.Size > 0 {
				res.Bytes += e.Size
			}
		}
	}
	names := make([]string, 0, len(n.dirs))
	for k := range n.dirs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		child := n.dirs[name]
		o, existed := n.orig[name]
		if existed && o.C4ID == child.entry.C4ID {
			continue
		}
		*out = append(*out, at(child.entry))
		if !existed {
			res.Added++
			if err := t.emitAll(child, prefix+name, depth+1, out, res); err != nil {
				return err
			}
			continue
		}
		if err := t.emit(child, prefix+name, depth+1, out, res); err != nil {
			return err
		}
	}
	return nil
}

// emitAll writes the whole contents of a new directory. Subdirectories it
// holds by ID only are written as ID-only entries, which readers expand
// from their records.
func (t *atree) emitAll(n *anode, prefix string, depth int, out *[]*c4m.Entry, res *Applied) error {
	if !n.loaded {
		return nil
	}
	for _, name := range sortedNames(n.files) {
		cp := *n.files[name]
		cp.Depth = depth
		*out = append(*out, &cp)
		res.Added++
		if !IsLink(&cp) {
			res.Files = append(res.Files, ChangedFile{Path: prefix + name, ID: cp.C4ID, Size: cp.Size, Exec: IsExec(&cp)})
			if cp.Size > 0 {
				res.Bytes += cp.Size
			}
		}
	}
	names := make([]string, 0, len(n.dirs))
	for k := range n.dirs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		child := n.dirs[name]
		cp := *child.entry
		cp.Depth = depth
		*out = append(*out, &cp)
		res.Added++
		if err := t.emitAll(child, prefix+name, depth+1, out, res); err != nil {
			return err
		}
	}
	return nil
}
