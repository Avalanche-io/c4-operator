// Package manifest turns a c4m file (from a ConfigMap or from a store by
// ID) into a View: the flat list of directories and files a node must
// materialize, after the sparse include filter.
//
// Resolution follows the c4 conventions exactly:
//   - a first-line base reference is folded in with c4m.ResolveBase;
//   - a directory described only by its ID (a one-level record, as
//     `c4 id -s` stores them, or a folded subtree) is expanded by fetching
//     that record from the store, and only when the filter can reach it;
//   - every object read from a store is rehashed before it is believed;
//   - sequence entries expand through their ID list.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"
)

// Source is the read side of a store.
type Source interface {
	Open(id c4.ID) (io.ReadCloser, error)
}

// emptyID is the ID of the empty listing: an empty directory.
var emptyID = c4.Identify(strings.NewReader(""))

// ErrIncomplete reports a description that cannot be materialized
// because an entry has no content ID.
var ErrIncomplete = errors.New("description is incomplete")

// Resolved is a resolved Materialization source.
type Resolved struct {
	// RootID is the tree's canonical ID (Manifest.ComputeC4ID): the
	// snapshot ID `c4 id` reports. It is the same whether the tree came
	// as c4m text, as a stored c4m file, or as stored directory records.
	RootID c4.ID
	View   *View
}

// FromBytes resolves a c4m file's bytes, such as a ConfigMap value.
// RootID is the c4m's own canonical ID (Manifest.ComputeC4ID), which is
// the snapshot ID `c4 id` prints for the same tree.
func FromBytes(data []byte, src Source, f Filter) (*Resolved, error) {
	m, err := decode(data, src)
	if err != nil {
		return nil, err
	}
	v, err := build(m, src, f)
	if err != nil {
		return nil, err
	}
	return &Resolved{RootID: m.ComputeC4ID(), View: v}, nil
}

// FromID resolves a description stored under id: a c4m file (as the
// c4 1.0 CLI stores) or a root directory record (as later CLIs store).
func FromID(id c4.ID, src Source, f Filter) (*Resolved, error) {
	if src == nil {
		return nil, fmt.Errorf("resolving %s needs a store", id)
	}
	data, err := ReadVerified(src, id)
	if err != nil {
		return nil, err
	}
	m, err := decode(data, src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", id, err)
	}
	v, err := build(m, src, f)
	if err != nil {
		return nil, err
	}
	return &Resolved{RootID: m.ComputeC4ID(), View: v}, nil
}

// ReadVerified reads an object and rehashes it. The bytes are returned
// only when they hash to id: absent and damaged are one answer.
func ReadVerified(src Source, id c4.ID) ([]byte, error) {
	rc, err := src.Open(id)
	if err != nil {
		return nil, fmt.Errorf("%s not in store: %w", id, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", id, err)
	}
	if got := c4.Identify(bytes.NewReader(data)); got != id {
		return nil, fmt.Errorf("store object %s failed verification (bytes hash to %s)", id, got)
	}
	return data, nil
}

func decode(data []byte, src Source) (*c4m.Manifest, error) {
	if c4m.IsJournal(data) {
		return nil, fmt.Errorf("this is a c4 store journal, not a c4m file")
	}
	m, err := c4m.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("parse c4m: %w", err)
	}
	if m.Base.IsNil() {
		return m, nil
	}
	var s store.Source
	if src != nil {
		s = src
	}
	return c4m.ResolveBase(m, s)
}

// item is one flattened entry.
type item struct {
	path   string // relative; directories end in "/"
	entry  *c4m.Entry
	isDir  bool
	isLink bool
}

type resolver struct {
	src   Source
	f     Filter
	items []item
	seen  map[c4.ID]bool // records on the current expansion path
}

func build(m *c4m.Manifest, src Source, f Filter) (*View, error) {
	r := &resolver{src: src, f: f, seen: map[c4.ID]bool{}}
	if err := r.walk(m, ""); err != nil {
		return nil, err
	}
	return newView(r.items, f)
}

// walk flattens m (entries in filesystem-walk order) under prefix,
// expanding ID-only directories the filter can reach.
func (r *resolver) walk(m *c4m.Manifest, prefix string) error {
	entries := m.Entries
	var stack []string
	for i, e := range entries {
		if e.Depth > len(stack) {
			return fmt.Errorf("malformed c4m: %q at depth %d has no parent directory", e.Name, e.Depth)
		}
		stack = stack[:e.Depth]
		parent := prefix + strings.Join(stack, "")
		name := e.Name
		if err := checkName(name); err != nil {
			return fmt.Errorf("%s%s: %w", parent, name, err)
		}

		if e.IsSequence {
			if err := r.expandSequence(m, e, parent); err != nil {
				return err
			}
			continue
		}

		if !e.IsDir() {
			p := parent + name
			isLink := e.Target != "" || e.IsSymlink()
			if !isLink && e.C4ID.IsNil() && r.f.Includes(p) {
				return fmt.Errorf("%w: %s has no content ID", ErrIncomplete, p)
			}
			r.items = append(r.items, item{path: p, entry: e, isLink: isLink})
			continue
		}

		dirName := strings.TrimSuffix(name, "/") + "/"
		p := parent + dirName
		r.items = append(r.items, item{path: p, entry: e, isDir: true})
		stack = append(stack, dirName)

		hasChildren := i+1 < len(entries) && entries[i+1].Depth > e.Depth
		if hasChildren || e.C4ID.IsNil() || e.C4ID == emptyID || !r.f.MayContain(p) {
			continue
		}
		if err := r.expandRecord(e.C4ID, p); err != nil {
			return err
		}
	}
	return nil
}

// expandRecord fetches the listing for a directory known only by ID and
// flattens it under dir.
func (r *resolver) expandRecord(id c4.ID, dir string) error {
	if r.src == nil {
		return fmt.Errorf("%w: directory %s is described only by ID %s and no store is configured", ErrIncomplete, dir, id)
	}
	if r.seen[id] {
		return fmt.Errorf("directory record cycle at %s (%s)", dir, id)
	}
	data, err := ReadVerified(r.src, id)
	if err != nil {
		return fmt.Errorf("%w: directory %s: %v", ErrIncomplete, dir, err)
	}
	sub, err := decode(data, r.src)
	if err != nil {
		return fmt.Errorf("directory %s (%s): %w", dir, id, err)
	}
	r.seen[id] = true
	defer delete(r.seen, id)
	return r.walk(sub, dir)
}

// expandSequence turns frame.[0001-0100].exr into its member files using
// the ID list carried inline (range data) or stored under the entry's ID.
func (r *resolver) expandSequence(m *c4m.Manifest, e *c4m.Entry, parent string) error {
	seq, err := c4m.ParseSequence(e.Name)
	if err != nil {
		return fmt.Errorf("%s%s: %w", parent, e.Name, err)
	}
	names := seq.Expand()
	var list string
	if data, ok := m.RangeData[e.C4ID]; ok {
		list = data
	} else if r.src != nil && !e.C4ID.IsNil() {
		b, err := ReadVerified(r.src, e.C4ID)
		if err != nil {
			return fmt.Errorf("%w: sequence %s%s: %v", ErrIncomplete, parent, e.Name, err)
		}
		list = string(b)
	} else {
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
		fe := &c4m.Entry{Name: n, C4ID: ids[i], Size: -1, Timestamp: c4m.NullTimestamp()}
		r.items = append(r.items, item{path: parent + n, entry: fe})
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

// checkName refuses anything that could escape the view root. The c4m
// decoder already enforces this; a materializer writing to a host path
// checks again rather than trusting it.
func checkName(name string) error {
	base := strings.TrimSuffix(name, "/")
	if base == "" || base == "." || base == ".." || strings.ContainsAny(base, "/\\\x00") {
		return fmt.Errorf("unsafe entry name %q", name)
	}
	return nil
}
