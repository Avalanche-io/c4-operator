package view

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// File is one regular file of a view.
type File struct {
	Path string // relative, slash-separated
	ID   c4.ID
	Size int64 // -1 when the description does not record it
	Exec bool
}

// Symlink is one symbolic link of a view, created as recorded and never
// followed by the materializer.
type Symlink struct {
	Path   string
	Target string
}

// View is a tree as containers see it.
type View struct {
	ID       c4.ID
	Root     *Node
	Dirs     []string // relative, each ending in "/", parents before children
	Files    []File   // sorted by path
	Symlinks []Symlink
}

// Object is one unique piece of content a view needs.
type Object struct {
	ID   c4.ID
	Size int64
	Exec bool // some path uses it as an executable
}

// Totals summarizes a view.
type Totals struct {
	Files         int64
	Bytes         int64 // sum over files; duplicates count each time
	UniqueObjects int64
	UniqueBytes   int64 // what a cold node fetches
	Dirs          int64
}

// FromNode identifies a tree and flattens it into a View.
func FromNode(root *Node) *View {
	v := &View{Root: root}
	v.ID = root.Identify()
	root.Walk("", func(dir string, n *Node) error {
		if dir != "" {
			v.Dirs = append(v.Dirs, dir)
		}
		for name, e := range n.Files {
			p := dir + name
			if IsLink(e) {
				v.Symlinks = append(v.Symlinks, Symlink{Path: p, Target: e.Target})
				continue
			}
			v.Files = append(v.Files, File{Path: p, ID: e.C4ID, Size: e.Size, Exec: IsExec(e)})
		}
		return nil
	})
	sort.Slice(v.Files, func(i, j int) bool { return v.Files[i].Path < v.Files[j].Path })
	sort.Slice(v.Symlinks, func(i, j int) bool { return v.Symlinks[i].Path < v.Symlinks[j].Path })
	return v
}

// Objects returns the unique content IDs of the view, sorted by size,
// largest first, so long transfers start early.
func (v *View) Objects() []Object {
	type acc struct {
		size int64
		exec bool
	}
	seen := make(map[c4.ID]*acc, len(v.Files))
	for _, f := range v.Files {
		a, ok := seen[f.ID]
		if !ok {
			a = &acc{size: f.Size}
			seen[f.ID] = a
		}
		if f.Size > a.size {
			a.size = f.Size
		}
		a.exec = a.exec || f.Exec
	}
	out := make([]Object, 0, len(seen))
	for id, a := range seen {
		out = append(out, Object{ID: id, Size: a.size, Exec: a.exec})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return out[i].ID.Cmp(out[j].ID) < 0
	})
	return out
}

// Totals computes file and byte counts.
func (v *View) Totals() Totals {
	t := Totals{Files: int64(len(v.Files)), Dirs: int64(len(v.Dirs))}
	for _, f := range v.Files {
		if f.Size > 0 {
			t.Bytes += f.Size
		}
	}
	for _, o := range v.Objects() {
		t.UniqueObjects++
		if o.Size > 0 {
			t.UniqueBytes += o.Size
		}
	}
	return t
}

// Records returns the view's directory records, deepest first.
func (v *View) Records() []Record { return v.Root.Records() }

// Manifest renders the whole view as one nested c4m (files first, then
// directories, each in natural order).
func (v *View) Manifest() *c4m.Manifest {
	m := c4m.NewManifest()
	emit(m, v.Root, 0)
	return m
}

// Marshal renders the view as one nested c4m file.
func (v *View) Marshal() ([]byte, error) { return c4m.Marshal(v.Manifest()) }

func emit(m *c4m.Manifest, n *Node, depth int) {
	files := make([]*c4m.Entry, 0, len(n.Files))
	for _, e := range n.Files {
		files = append(files, e)
	}
	sort.Slice(files, func(i, j int) bool { return c4m.NaturalLess(files[i].Name, files[j].Name) })
	for _, f := range files {
		cp := *f
		cp.Depth = depth
		m.AddEntry(&cp)
	}
	names := make([]string, 0, len(n.Dirs))
	for k := range n.Dirs {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return c4m.NaturalLess(names[i], names[j]) })
	for _, k := range names {
		c := n.Dirs[k]
		cp := *c.Entry
		cp.Depth = depth
		m.AddEntry(&cp)
		emit(m, c, depth+1)
	}
}

// Parse decodes a nested c4m that was produced by Marshal (or any c4m
// without bases, sequences or unresolved directories) and checks that it
// describes the view want. Views cached on a node are rechecked this way:
// a file name is a claim, the recomputed ID is the fact.
func Parse(data []byte, want c4.ID) (*View, error) {
	m, err := c4m.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, err
	}
	root, err := nodeFromManifest(m)
	if err != nil {
		return nil, err
	}
	v := FromNode(root)
	if !want.IsNil() && v.ID != want {
		return nil, fmt.Errorf("view description recomputes to %s, not %s", v.ID, want)
	}
	return v, nil
}

// nodeFromManifest builds a tree from a fully nested c4m.
func nodeFromManifest(m *c4m.Manifest) (*Node, error) {
	root := NewNode("")
	var stack []*Node
	for i, e := range m.Entries {
		if e.Depth > len(stack) {
			return nil, fmt.Errorf("malformed c4m: %q at depth %d has no parent", e.Name, e.Depth)
		}
		stack = stack[:e.Depth]
		parent := root
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		if err := CheckName(e.Name); err != nil {
			return nil, err
		}
		if e.IsSequence {
			return nil, fmt.Errorf("%s: sequences must be expanded first", e.Name)
		}
		pe := Project(e)
		if !pe.IsDir() {
			parent.Files[pe.Name] = pe
			continue
		}
		hasChildren := i+1 < len(m.Entries) && m.Entries[i+1].Depth > e.Depth
		if !hasChildren && !pe.C4ID.IsNil() && pe.C4ID != emptyID {
			return nil, fmt.Errorf("%s: directory known only by ID; resolve records first", pe.Name)
		}
		d := NewNode(pe.Name)
		parent.Dirs[pe.Name] = d
		stack = append(stack, d)
	}
	return root, nil
}
