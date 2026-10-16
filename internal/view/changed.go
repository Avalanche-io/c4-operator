package view

import (
	"context"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// FileChange is one file that differs between two views: what a CI system
// asks to decide which targets a change affects (DESIGN.md section 8.2).
type FileChange struct {
	Path string `json:"path"`
	// Op is added, modified or removed.
	Op string `json:"op"`
	// ID and Size are the file after the change; OldID and OldSize before
	// it. A symlink has its target in Target and no ID.
	ID      string `json:"id,omitempty"`
	Size    int64  `json:"size"`
	OldID   string `json:"oldId,omitempty"`
	OldSize int64  `json:"oldSize,omitempty"`
	Target  string `json:"target,omitempty"`
}

// ChangedFiles lists the files that differ from view from to view to,
// reading only the records of directories whose IDs differ, so its cost is
// proportional to the change, not to the views. limit bounds the result
// (0: no bound); truncated reports whether it was reached.
func ChangedFiles(ctx context.Context, from, to c4.ID, src RecordSource, limit int) (out []FileChange, truncated bool, err error) {
	w := &changeWalk{ctx: ctx, src: src, limit: limit}
	if from != to {
		err = w.dir("", from, to)
	}
	return w.out, w.truncated, err
}

type changeWalk struct {
	ctx       context.Context
	src       RecordSource
	limit     int
	out       []FileChange
	truncated bool
}

func (w *changeWalk) full() bool {
	if w.limit > 0 && len(w.out) >= w.limit {
		w.truncated = true
		return true
	}
	return false
}

func (w *changeWalk) entries(id c4.ID) (map[string]*c4m.Entry, error) {
	out := map[string]*c4m.Entry{}
	if id.IsNil() || id == emptyID {
		return out, nil
	}
	es, err := fetchRecord(w.ctx, w.src, id)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		out[e.Name] = e
	}
	return out, nil
}

func (w *changeWalk) dir(path string, from, to c4.ID) error {
	a, err := w.entries(from)
	if err != nil {
		return err
	}
	b, err := w.entries(to)
	if err != nil {
		return err
	}
	for _, name := range sortedNames(unionNames(a, b)) {
		if w.full() {
			return nil
		}
		oe, had := a[name]
		ne, has := b[name]
		p := path + name
		switch {
		case had && has && sameEntry(oe, ne):
		case had && has && oe.IsDir() && ne.IsDir():
			if err := w.dir(p, oe.C4ID, ne.C4ID); err != nil {
				return err
			}
		default:
			if had {
				if err := w.side(p, oe, "removed"); err != nil {
					return err
				}
			}
			if has {
				if err := w.side(p, ne, "added"); err != nil {
					return err
				}
			}
		}
	}
	w.merge()
	return nil
}

// side lists every file under an entry that exists on one side only.
func (w *changeWalk) side(p string, e *c4m.Entry, op string) error {
	if !e.IsDir() {
		fc := FileChange{Path: p, Op: op}
		id := ""
		if !e.C4ID.IsNil() {
			id = e.C4ID.String()
		}
		if op == "removed" {
			fc.OldID, fc.OldSize = id, e.Size
		} else {
			fc.ID, fc.Size, fc.Target = id, e.Size, e.Target
		}
		w.out = append(w.out, fc)
		return nil
	}
	children, err := w.entries(e.C4ID)
	if err != nil {
		return err
	}
	for _, name := range sortedNames(children) {
		if w.full() {
			return nil
		}
		if err := w.side(strings.TrimSuffix(p, "/")+"/"+name, children[name], op); err != nil {
			return err
		}
	}
	return nil
}

// merge turns a removed and an added change of the same file into one
// modification.
func (w *changeWalk) merge() {
	byPath := map[string]int{}
	var keep []FileChange
	for _, c := range w.out {
		i, ok := byPath[c.Path]
		if !ok {
			byPath[c.Path] = len(keep)
			keep = append(keep, c)
			continue
		}
		prev := &keep[i]
		if prev.Op == "removed" && c.Op == "added" {
			prev.Op, prev.ID, prev.Size, prev.Target = "modified", c.ID, c.Size, c.Target
		}
	}
	w.out = keep
}

func unionNames(a, b map[string]*c4m.Entry) map[string]*c4m.Entry {
	out := make(map[string]*c4m.Entry, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
