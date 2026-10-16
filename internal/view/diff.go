package view

import (
	"context"
	"sort"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// Op is the kind of a Change.
type Op int

const (
	// OpAdd creates or replaces the path with Entry. A directory add is
	// followed by adds for everything under it.
	OpAdd Op = iota
	// OpRemove deletes the path and everything under it (a whiteout).
	OpRemove
	// OpOpaque replaces a directory's whole contents: nothing below it in
	// lower layers shows through, and adds for its full new contents
	// follow.
	OpOpaque
)

// Change is one step from one view to another, as a delta layer needs it.
type Change struct {
	Path  string // relative; directories end in "/"
	Op    Op
	Entry *c4m.Entry // projected; for OpAdd and OpOpaque
}

// DiffStats summarizes a diff.
type DiffStats struct {
	Adds, Removes, Opaques int
	Unchanged              int // entries of compared directories left as they are
}

// Diff computes the changes that turn view from into view to by walking
// both views' records top-down and skipping every subtree whose ID is
// equal, so its cost is proportional to the change. A directory that
// loses more children than it keeps is replaced (OpOpaque) rather than
// patched with whiteouts.
func Diff(ctx context.Context, from, to c4.ID, src RecordSource) ([]Change, DiffStats, error) {
	d := &differ{ctx: ctx, src: src}
	if from == to {
		return nil, d.st, nil
	}
	if err := d.dir("", from, to); err != nil {
		return nil, d.st, err
	}
	return d.out, d.st, nil
}

type differ struct {
	ctx context.Context
	src RecordSource
	out []Change
	st  DiffStats
}

func (d *differ) entries(id c4.ID) (map[string]*c4m.Entry, error) {
	out := map[string]*c4m.Entry{}
	if id == emptyID || id.IsNil() {
		return out, nil
	}
	es, err := fetchRecord(d.ctx, d.src, id)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		out[e.Name] = e
	}
	return out, nil
}

func (d *differ) dir(path string, from, to c4.ID) error {
	a, err := d.entries(from)
	if err != nil {
		return err
	}
	b, err := d.entries(to)
	if err != nil {
		return err
	}

	removed, kept := 0, 0
	for name := range a {
		if _, ok := b[name]; ok {
			kept++
			continue
		}
		removed++
	}
	if path != "" && removed > kept && removed > 1 {
		d.out = append(d.out, Change{Path: path, Op: OpOpaque, Entry: &c4m.Entry{Name: lastName(path), C4ID: to}})
		d.st.Opaques++
		return d.addAll(path, b)
	}

	for _, name := range sortedNames(a) {
		if _, ok := b[name]; ok {
			continue
		}
		d.out = append(d.out, Change{Path: path + name, Op: OpRemove})
		d.st.Removes++
	}
	for _, name := range sortedNames(b) {
		ne := b[name]
		oe, ok := a[name]
		switch {
		case ok && sameEntry(oe, ne):
			d.st.Unchanged++
		case ok && oe.IsDir() && ne.IsDir():
			if err := d.dir(path+name, oe.C4ID, ne.C4ID); err != nil {
				return err
			}
		default:
			if err := d.add(path+name, ne); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *differ) add(p string, e *c4m.Entry) error {
	d.out = append(d.out, Change{Path: p, Op: OpAdd, Entry: e})
	d.st.Adds++
	if !e.IsDir() {
		return nil
	}
	children, err := d.entries(e.C4ID)
	if err != nil {
		return err
	}
	return d.addAll(p, children)
}

func (d *differ) addAll(dir string, children map[string]*c4m.Entry) error {
	for _, name := range sortedNames(children) {
		if err := d.add(dir+name, children[name]); err != nil {
			return err
		}
	}
	return nil
}

// sameEntry compares projected entries: same kind, content, size, exec
// bit and target.
func sameEntry(a, b *c4m.Entry) bool {
	return a.IsDir() == b.IsDir() && a.C4ID == b.C4ID && a.Size == b.Size && a.Mode == b.Mode && a.Target == b.Target
}

func sortedNames(m map[string]*c4m.Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lastName(p string) string {
	_, name := SplitPath(p)
	return name
}
