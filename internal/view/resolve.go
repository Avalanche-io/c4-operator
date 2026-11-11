package view

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	// RootID is the description's C4 ID: the ID of its root listing, each
	// top-level line as stated (for a changeset, the state it resolves to).
	// It is what `c4 id -q` reports except where a top-level directory's
	// line is stale or null and its listed children would derive it; see
	// descriptionID.
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
// checked where they cross into a cache or the store (DESIGN.md section 1).
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
	// seal is the bare ID that ended the description being resolved, when
	// it is a changeset or chain in the old form (splitSeal).
	seal c4.ID
	// desc is the root listing of the level last resolved, as written: the
	// top-level lines of a c4m or record as decoded, or of a changeset
	// applied over its parent's with the library's ApplyPatch. RootID reads
	// nothing below the top level, so nothing below it is kept, and each
	// level costs its own lines, not the chain's.
	desc *c4m.Manifest
	// made holds the records ApplyPatch made for the changesets resolved,
	// read through st (madeStore) as if stored.
	made map[c4.ID][]byte
	// view is the view of the level last resolved, when it is known
	// without resolving it again unfiltered: a stored record's own ID, the
	// view a changeset made, the empty tree. plain is that level's
	// description when it is a plain c4m and view is not known.
	view  c4.ID
	plain []byte
}

func newResolver(st Store, f Filter) *resolver {
	made := map[c4.ID][]byte{}
	return &resolver{st: madeStore{made, st}, f: f, seen: map[c4.ID]bool{}, made: made}
}

// madeStore serves the records a resolution made, then the store.
type madeStore struct {
	made map[c4.ID][]byte
	st   Store
}

func (m madeStore) Open(id c4.ID) (io.ReadCloser, error) {
	if b, ok := m.made[id]; ok {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	if m.st == nil {
		return nil, fmt.Errorf("%s: no store", id)
	}
	return m.st.Open(id)
}

// apply applies a changeset's patch over the view of the level resolved
// before it, with the operator's one patch reader (ApplyPatch), and
// returns the made view's items as the resolver walks any view: from its
// records, under the filter. The made records are kept in r.made.
func (r *resolver) apply(patch *c4m.Manifest) ([]item, error) {
	base, err := r.baseView()
	if err != nil {
		return nil, err
	}
	a, err := ApplyPatch(context.Background(), base, patch, StoreRecords(r.st))
	if err != nil {
		return nil, err
	}
	for _, rec := range a.Records {
		r.made[rec.ID] = rec.Data
	}
	r.view, r.plain = a.ID, nil
	return r.viewItems(a.ID)
}

// baseView is the view of the level last resolved: known, or resolved
// again with no filter when that level was a plain c4m.
func (r *resolver) baseView() (c4.ID, error) {
	if !r.view.IsNil() {
		return r.view, nil
	}
	if r.plain == nil {
		return c4.ID{}, errors.New("a changeset's base has no view")
	}
	all, err := NewFilter(nil)
	if err != nil {
		return c4.ID{}, err
	}
	res, err := FromBytes(r.plain, r.st, all)
	if err != nil {
		return c4.ID{}, err
	}
	v := res.View()
	recs, err := v.Records()
	if err != nil {
		return c4.ID{}, err
	}
	for _, rec := range recs {
		r.made[rec.ID] = rec.Data
	}
	r.view = v.ID
	return v.ID, nil
}

// viewItems walks a view from its root record, under the filter.
func (r *resolver) viewItems(id c4.ID) ([]item, error) {
	if id == emptyID {
		return nil, nil
	}
	data, err := ReadObject(r.st, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIncomplete, err)
	}
	m, err := c4m.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("view %s: %w", id, err)
	}
	var items []item
	if err := r.walk(m, "", true, &items); err != nil {
		return nil, err
	}
	return items, nil
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
	res.RootID = descriptionID(r.desc)
	// A changeset or chain in the old form ends with the ID of what it
	// resolves to, and two writers wrote that line. This system's wrote
	// the view's ID. The c4 CLI's diff (through 1.0.24) wrote the ID of
	// the description as recorded, with its dates and modes, which is
	// RootID. Either is checked: the description's ID always, the view's
	// when the resolution is whole (under a filter the view is partial
	// and its ID is another, so the line is only dropped).
	if !r.seal.IsNil() && r.seal != res.RootID {
		if got := res.View().ID; r.f.All() && got != r.seal {
			return nil, fmt.Errorf("the description ends with %s; it resolves to the view %s and the description %s", r.seal, got, res.RootID)
		}
	}
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

// descriptionID is a description's C4 ID: the C4 ID of the canonical text
// of its root listing, each top-level line as the description states it.
// A top-level directory known only by its ID keeps a null size, except
// that the empty listing's ID says the directory is empty, size 0, as c4
// 50ba056 has it. Not ComputeC4ID: on the pinned library that reads every
// directory known only by its ID as empty and hashes a 0 the description
// does not state.
//
// It equals what `c4 id -q` reports except where a top-level line is
// stale, or null where its listed children would derive it: c4's
// normaliser re-derives every directory whose children are listed, and a
// changeset deepens a directory its base knows only by its ID with a
// partial listing (view.Apply restates a changed directory with its new
// size and ID and lists only the changed child), which the normaliser
// would wrongly take as whole. Until c4 rules that a partial listing keeps
// a stated size and ID and nulls an unstated one, the line is kept as
// recorded, which is right for every changeset this system writes.
// topLevel is a copy of m's top-level entries alone.
func topLevel(m *c4m.Manifest) *c4m.Manifest {
	out := c4m.NewManifest()
	for _, e := range m.Entries {
		if e.Depth == 0 {
			cp := *e
			out.AddEntry(&cp)
		}
	}
	return out
}

func descriptionID(m *c4m.Manifest) c4.ID {
	cp := m.Copy()
	for _, e := range cp.Entries {
		if e.Depth == 0 && e.IsDir() && e.Size < 0 && e.C4ID == emptyID {
			e.Size = 0
		}
	}
	var b bytes.Buffer
	if err := cp.WriteCanonical(&b); err != nil {
		return c4.ID{}
	}
	return c4.Identify(&b)
}

func (r *resolver) resolveID(id c4.ID, depth int) ([][]item, error) {
	if r.seen[id] {
		return nil, fmt.Errorf("description cycle at %s", id)
	}
	if id == emptyID {
		r.desc = c4m.NewManifest()
		r.view, r.plain = emptyID, nil
		return [][]item{nil}, nil
	}
	data, err := ReadObject(r.st, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIncomplete, err)
	}
	r.seen[id] = true
	defer delete(r.seen, id)
	levels, err := r.resolveData(data, depth)
	if err == nil && r.view.IsNil() {
		// A stored one-level record is a view, named by its own ID.
		if _, rerr := ParseRecord(data); rerr == nil {
			r.view, r.plain = id, nil
		}
	}
	return levels, err
}

// resolveData returns the chain of item sets a description resolves to,
// base first.
func (r *resolver) resolveData(data []byte, depth int) ([][]item, error) {
	if c4m.IsJournal(data) {
		return nil, fmt.Errorf("this is a c4 store journal, not a description")
	}
	// A changeset or chain stored before 2026-11-09 ends with the bare ID
	// of the view it resolves to, which the c4m library refuses. It is a
	// stored object named by ID in signed records, so it is still read,
	// without that line.
	data, seal := splitSeal(data)
	if depth == 0 {
		r.seal = seal
	}
	// A chain of several sections from an external base resolves to one
	// view per section.
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
		r.desc = topLevel(m)
		r.view, r.plain = c4.ID{}, data
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
	top, err := r.apply(m)
	if err != nil {
		return nil, err
	}
	r.desc = topLevel(c4m.ApplyPatch(r.desc, m.Copy()))
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
		patch := &c4m.Manifest{Version: "1.0", Entries: sec.Entries}
		top, err := r.apply(patch)
		if err != nil {
			return nil, err
		}
		levels = append(levels, top)
		r.desc = topLevel(c4m.ApplyPatch(r.desc, patch.Copy()))
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
