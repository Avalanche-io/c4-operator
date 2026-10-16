package view

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// Store is the read side of a content store.
type Store interface {
	Open(id c4.ID) (io.ReadCloser, error)
}

// ErrIncomplete reports a description that cannot be materialized.
var ErrIncomplete = errors.New("description is incomplete")

// MaxChain bounds how deep external base references may nest (a changeset
// whose parent is a changeset whose parent is...). Chains themselves are
// not bounded; long ones are re-based.
const MaxChain = 4096

// KeepLevels is how many of a chain's most recent views a resolution keeps
// as stacking candidates.
const KeepLevels = 16

// Resolved is a resolved source.
type Resolved struct {
	// SourceID is the ID of the description itself: the c4m text, the
	// stored record, or the changeset.
	SourceID c4.ID
	// RootID is the ID of the top description's root listing as recorded
	// (the snapshot ID `c4 id` reports for a full description).
	RootID c4.ID
	// Chain holds the most recent views of the changeset chain (at most
	// KeepLevels), base first. The last one is what pods see. A plain
	// description has one.
	Chain []*View
	// Levels counts every view of the chain, including those not kept.
	Levels int
}

// View returns the view pods see.
func (r *Resolved) View() *View { return r.Chain[len(r.Chain)-1] }

// FromBytes resolves description bytes, such as a ConfigMap value.
func FromBytes(data []byte, st Store, f Filter) (*Resolved, error) {
	r := newResolver(st, f)
	levels, err := r.resolveData(data, 0)
	if err != nil {
		return nil, err
	}
	res, err := r.finish(levels)
	if err != nil {
		return nil, err
	}
	res.SourceID = c4.Identify(bytes.NewReader(data))
	return res, nil
}

// FromID resolves a description stored under id: a c4m file, a record
// (a view ID or a `c4 id -s` snapshot ID), or a changeset.
func FromID(id c4.ID, st Store, f Filter) (*Resolved, error) {
	if st == nil {
		return nil, fmt.Errorf("resolving %s needs a store", id)
	}
	r := newResolver(st, f)
	levels, err := r.resolveID(id, 0)
	if err != nil {
		return nil, err
	}
	res, err := r.finish(levels)
	if err != nil {
		return nil, err
	}
	res.SourceID = id
	return res, nil
}

// ReadObject reads a whole (small) object. It does not rehash: bytes are
// verified once, where they enter the system.
func ReadObject(st Store, id c4.ID) ([]byte, error) {
	rc, err := st.Open(id)
	if err != nil {
		return nil, fmt.Errorf("%s not in store: %w", id, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", id, err)
	}
	return data, nil
}

// item is one flattened entry of a description, as recorded.
type item struct {
	path        string // relative; directories end in "/"
	entry       *c4m.Entry
	isDir       bool
	hasChildren bool // a directory whose children follow in the same block
}

type resolver struct {
	st   Store
	f    Filter
	seen map[c4.ID]bool // descriptions on the current resolution path
}

func newResolver(st Store, f Filter) *resolver {
	return &resolver{st: st, f: f, seen: map[c4.ID]bool{}}
}

func (r *resolver) finish(levels [][]item) (*Resolved, error) {
	res := &Resolved{Levels: len(levels)}
	kept := levels
	if len(kept) > KeepLevels {
		kept = kept[len(kept)-KeepLevels:]
	}
	for _, lv := range kept {
		root, err := buildNode(lv, r.f)
		if err != nil {
			return nil, err
		}
		res.Chain = append(res.Chain, FromNode(root))
	}
	res.RootID = rootID(levels[len(levels)-1])
	return res, nil
}

// buildNode applies the filter and builds the projected tree.
func buildNode(items []item, f Filter) (*Node, error) {
	root := NewNode("")
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if seen[it.path] {
			return nil, fmt.Errorf("path %s appears twice in the description", it.path)
		}
		seen[it.path] = true
		if !f.Includes(it.path) {
			continue
		}
		if it.isDir {
			root.Dir(it.path)
			continue
		}
		dir, name := SplitPath(it.path)
		e := Project(it.entry)
		e.Name = name
		e.Depth = 0
		if !IsLink(e) && e.C4ID.IsNil() {
			return nil, fmt.Errorf("%w: %s has no content ID", ErrIncomplete, it.path)
		}
		root.Dir(dir).Files[name] = e
	}
	return root, nil
}

// rootID is the ID of the root listing: the top-level entries as recorded.
func rootID(items []item) c4.ID {
	m := c4m.NewManifest()
	for _, it := range items {
		if strings.Count(strings.TrimSuffix(it.path, "/"), "/") != 0 {
			continue
		}
		cp := *it.entry
		cp.Depth = 0
		m.AddEntry(&cp)
	}
	return m.ComputeC4ID()
}

func (r *resolver) resolveID(id c4.ID, depth int) ([][]item, error) {
	if r.seen[id] {
		return nil, fmt.Errorf("description cycle at %s", id)
	}
	if id == emptyID {
		return [][]item{nil}, nil
	}
	data, err := ReadObject(r.st, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIncomplete, err)
	}
	r.seen[id] = true
	defer delete(r.seen, id)
	return r.resolveData(data, depth)
}

// resolveData returns the chain of item sets a description resolves to,
// base first.
func (r *resolver) resolveData(data []byte, depth int) ([][]item, error) {
	if c4m.IsJournal(data) {
		return nil, fmt.Errorf("this is a c4 store journal, not a description")
	}
	// A chain of several sealed sections from an external base resolves to
	// one view per checkpoint.
	if sections, err := c4m.DecodePatchChain(bytes.NewReader(data)); err == nil && len(sections) > 1 && !sections[0].BaseID.IsNil() {
		return r.resolveSections(sections, depth)
	}
	m, err := c4m.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("parse c4m: %w", err)
	}
	if m.Base.IsNil() {
		var items []item
		if err := r.walk(m, "", true, &items); err != nil {
			return nil, err
		}
		return [][]item{items}, nil
	}
	if depth >= MaxChain {
		return nil, fmt.Errorf("changeset chain deeper than %d", MaxChain)
	}
	if r.st == nil {
		return nil, fmt.Errorf("%w: changeset over %s and no store is configured", ErrIncomplete, m.Base)
	}
	chain, err := r.resolveID(m.Base, depth+1)
	if err != nil {
		return nil, fmt.Errorf("parent %s: %w", m.Base, err)
	}
	top, err := r.applyPatch(chain[len(chain)-1], m)
	if err != nil {
		return nil, err
	}
	return append(chain, top), nil
}

// resolveSections resolves a chain section by section from its external
// base.
func (r *resolver) resolveSections(sections []*c4m.PatchSection, depth int) ([][]item, error) {
	if depth >= MaxChain {
		return nil, fmt.Errorf("base references nest deeper than %d", MaxChain)
	}
	if r.st == nil {
		return nil, fmt.Errorf("%w: chain over %s and no store is configured", ErrIncomplete, sections[0].BaseID)
	}
	levels, err := r.resolveID(sections[0].BaseID, depth+1)
	if err != nil {
		return nil, fmt.Errorf("chain base %s: %w", sections[0].BaseID, err)
	}
	for _, sec := range sections {
		top, err := r.applyPatch(levels[len(levels)-1], &c4m.Manifest{Version: "1.0", Entries: sec.Entries})
		if err != nil {
			return nil, err
		}
		levels = append(levels, top)
	}
	return levels, nil
}

// walk flattens m (entries in filesystem-walk order) under prefix. With
// expand, directories known only by ID are fetched as records when the
// filter can reach them.
func (r *resolver) walk(m *c4m.Manifest, prefix string, expand bool, out *[]item) error {
	entries := m.Entries
	var stack []string
	for i, e := range entries {
		if e.Depth > len(stack) {
			return fmt.Errorf("malformed c4m: %q at depth %d has no parent directory", e.Name, e.Depth)
		}
		stack = stack[:e.Depth]
		parent := prefix + strings.Join(stack, "")
		if err := CheckName(e.Name); err != nil {
			return fmt.Errorf("%s%s: %w", parent, e.Name, err)
		}

		if e.IsSequence {
			if err := r.expandSequence(m, e, parent, out); err != nil {
				return err
			}
			continue
		}

		if !e.IsDir() {
			*out = append(*out, item{path: parent + e.Name, entry: e})
			continue
		}

		dirName := strings.TrimSuffix(e.Name, "/") + "/"
		p := parent + dirName
		hasChildren := i+1 < len(entries) && entries[i+1].Depth > e.Depth
		*out = append(*out, item{path: p, entry: e, isDir: true, hasChildren: hasChildren})
		stack = append(stack, dirName)

		if !expand || hasChildren || e.C4ID.IsNil() || e.C4ID == emptyID || !r.f.MayContain(p) {
			continue
		}
		if err := r.expandRecord(e.C4ID, p, out); err != nil {
			return err
		}
	}
	return nil
}

// expandRecord fetches the listing of a directory known only by ID.
func (r *resolver) expandRecord(id c4.ID, dir string, out *[]item) error {
	if r.st == nil {
		return fmt.Errorf("%w: directory %s is described only by ID %s and no store is configured", ErrIncomplete, dir, id)
	}
	if r.seen[id] {
		return fmt.Errorf("directory record cycle at %s (%s)", dir, id)
	}
	data, err := ReadObject(r.st, id)
	if err != nil {
		return fmt.Errorf("%w: directory %s: %v", ErrIncomplete, dir, err)
	}
	sub, err := c4m.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return fmt.Errorf("directory %s (%s): %w", dir, id, err)
	}
	r.seen[id] = true
	defer delete(r.seen, id)
	return r.walk(sub, dir, true, out)
}

// applyPatch folds a changeset block over its parent's items with c4m
// patch semantics, matched by path (as c4m.ApplyPatch): an entry
// identical to the parent's, with no children under it, removes that
// path and everything below it; a differing entry replaces the parent's;
// a new path is added. It works on sparse parents: only paths the filter
// reached are present, and patch entries elsewhere are kept as given.
func (r *resolver) applyPatch(parent []item, block *c4m.Manifest) ([]item, error) {
	var patch []item
	if err := r.walk(block, "", false, &patch); err != nil {
		return nil, err
	}
	byPath := make(map[string]item, len(parent)+len(patch))
	for _, it := range parent {
		byPath[it.path] = it
	}
	removeUnder := func(dir string) {
		for p := range byPath {
			if p != dir && strings.HasPrefix(p, dir) {
				delete(byPath, p)
			}
		}
	}

	// Removals first: an entry identical to the parent's at the same path,
	// with nothing under it. Doing them first keeps a type change (file
	// "a" replacing directory "a/") independent of entry order.
	rest := patch[:0:0]
	for _, p := range patch {
		old, ok := byPath[p.path]
		if !ok || p.hasChildren || !entriesIdentical(old.entry, p.entry) {
			rest = append(rest, p)
			continue
		}
		delete(byPath, old.path)
		if old.isDir {
			removeUnder(old.path)
		}
	}

	for _, p := range rest {
		old, exists := lookup(byPath, p.path)
		switch {
		case !exists:
			byPath[p.path] = p
			if !p.isDir || p.hasChildren || p.entry.C4ID.IsNil() || p.entry.C4ID == emptyID || !r.f.MayContain(p.path) {
				continue
			}
			var sub []item
			if err := r.expandRecord(p.entry.C4ID, p.path, &sub); err != nil {
				return nil, err
			}
			for _, s := range sub {
				byPath[s.path] = s
			}
		case !p.hasChildren && entriesIdentical(old.entry, p.entry):
			delete(byPath, old.path)
			if old.isDir {
				removeUnder(old.path)
			}
		default:
			if old.path != p.path {
				// A file replaced a directory or the reverse.
				delete(byPath, old.path)
				if old.isDir {
					removeUnder(old.path)
				}
			}
			byPath[p.path] = p
		}
	}

	out := make([]item, 0, len(byPath))
	for _, it := range byPath {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// lookup finds the parent's item at a path as a file or a directory.
func lookup(m map[string]item, p string) (item, bool) {
	if it, ok := m[p]; ok {
		return it, true
	}
	alt := strings.TrimSuffix(p, "/")
	if alt == p {
		alt = p + "/"
	}
	it, ok := m[alt]
	return it, ok
}

func entriesIdentical(a, b *c4m.Entry) bool {
	return strings.TrimSuffix(a.Name, "/") == strings.TrimSuffix(b.Name, "/") &&
		a.Mode == b.Mode &&
		a.Timestamp.Equal(b.Timestamp) &&
		a.Size == b.Size &&
		a.C4ID == b.C4ID &&
		a.Target == b.Target &&
		a.HardLink == b.HardLink &&
		a.FlowDirection == b.FlowDirection &&
		a.FlowTarget == b.FlowTarget
}

// expandSequence turns frame.[0001-0100].exr into its member files using
// the ID list carried inline (range data) or stored under the entry's ID.
func (r *resolver) expandSequence(m *c4m.Manifest, e *c4m.Entry, parent string, out *[]item) error {
	seq, err := c4m.ParseSequence(e.Name)
	if err != nil {
		return fmt.Errorf("%s%s: %w", parent, e.Name, err)
	}
	names := seq.Expand()
	var list string
	switch data, ok := m.RangeData[e.C4ID]; {
	case ok:
		list = data
	case r.st != nil && !e.C4ID.IsNil():
		b, err := ReadObject(r.st, e.C4ID)
		if err != nil {
			return fmt.Errorf("%w: sequence %s%s: %v", ErrIncomplete, parent, e.Name, err)
		}
		list = string(b)
	default:
		return fmt.Errorf("%w: sequence %s%s has no ID list", ErrIncomplete, parent, e.Name)
	}
	ids, err := parseIDList(list)
	if err != nil {
		return fmt.Errorf("sequence %s%s: %w", parent, e.Name, err)
	}
	if len(ids) != len(names) {
		return fmt.Errorf("sequence %s%s: %d IDs for %d frames", parent, e.Name, len(ids), len(names))
	}
	for i, n := range names {
		if err := CheckName(n); err != nil {
			return err
		}
		fe := &c4m.Entry{Name: n, C4ID: ids[i], Size: -1, Timestamp: c4m.NullTimestamp()}
		*out = append(*out, item{path: parent + n, entry: fe})
	}
	return nil
}

func parseIDList(s string) ([]c4.ID, error) {
	compact := strings.Join(strings.Fields(s), "")
	if len(compact) == 0 || len(compact)%90 != 0 {
		return nil, fmt.Errorf("ID list is not a sequence of 90-character C4 IDs")
	}
	ids := make([]c4.ID, 0, len(compact)/90)
	for i := 0; i < len(compact); i += 90 {
		id, err := c4.Parse(compact[i : i+90])
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
