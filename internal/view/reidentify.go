package view

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Avalanche-io/c4"
)

// Reidentify reads the records of the view id as they are stored, without
// the check Load makes, and identifies the tree they describe with this
// library. The view it returns has the ID this library gives that tree:
// id itself, unless a library change derives a record differently (c4
// #107 keeps a directory's null size null where the library before it
// wrote 0). Each record is read through src, so its bytes are checked
// against its own ID; what is not checked is that the root recomputes to
// id, since that is the question asked. Records() gives the new records.
//
// Only a view is re-identified: a record any of whose entries is not as a
// view holds it (a description's dates, a changeset's base line) is
// refused (ErrNotAView, or the parse error), so a table never renames a
// description or a changeset.
func Reidentify(ctx context.Context, id c4.ID, src RecordSource, workers int) (*View, error) {
	root := NewNode("")
	if err := loadInto(ctx, root, id, src, workers, fetchViewRecord); err != nil {
		return nil, err
	}
	return FromNode(root), nil
}

// Renames is the table a library change leaves: each view ID an earlier
// library gave, and the ID this one gives the same records. It is an
// index, not an authority: Reidentify of the old ID gives the new one, so
// every line is checked in one call, and a lost table is rebuilt from the
// records. Only views whose IDs changed are in it.
//
// Its text is one line per view, "<old> <new>", sorted by the old ID: a
// plain-text object, stored by content like any other.
type Renames map[c4.ID]c4.ID

// ParseRenames reads a table. A line that is not two C4 IDs, a view named
// twice, or a view renamed to itself is an error naming the line.
func ParseRenames(data []byte) (Renames, error) {
	r := Renames{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			return nil, fmt.Errorf("renames line %d: not two C4 IDs", n)
		}
		old, err := c4.Parse(f[0])
		if err != nil {
			return nil, fmt.Errorf("renames line %d: %w", n, err)
		}
		cur, err := c4.Parse(f[1])
		if err != nil {
			return nil, fmt.Errorf("renames line %d: %w", n, err)
		}
		if old == cur {
			return nil, fmt.Errorf("renames line %d: %s renamed to itself", n, old)
		}
		if _, dup := r[old]; dup {
			return nil, fmt.Errorf("renames line %d: %s is named twice", n, old)
		}
		r[old] = cur
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// Bytes is the table's text: sorted, so the same table has one C4 ID.
func (r Renames) Bytes() []byte {
	olds := make([]c4.ID, 0, len(r))
	for old := range r {
		olds = append(olds, old)
	}
	sort.Slice(olds, func(i, j int) bool { return olds[i].String() < olds[j].String() })
	var b bytes.Buffer
	for _, old := range olds {
		fmt.Fprintf(&b, "%s %s\n", old, r[old])
	}
	return b.Bytes()
}

// Current is the ID id has now: the table's new ID for it, followed
// through any later change that renamed that one too, or id itself.
func (r Renames) Current(id c4.ID) c4.ID {
	for range len(r) {
		next, ok := r[id]
		if !ok {
			break
		}
		id = next
	}
	return id
}

// renames caches tables by their C4 ID: an object's bytes never change,
// so a table read once is the table under that ID for good.
var renames sync.Map // c4.ID -> Renames

// ReadRenames reads the table stored under table, checked, and caches it.
// A nil table is the empty table: nothing has been renamed.
func ReadRenames(st Store, table c4.ID) (Renames, error) {
	if table.IsNil() {
		return nil, nil
	}
	if r, ok := renames.Load(table); ok {
		return r.(Renames), nil
	}
	data, err := ReadObject(st, table)
	if err != nil {
		return nil, fmt.Errorf("renames %s: %w", table, err)
	}
	r, err := ParseRenames(data)
	if err != nil {
		return nil, fmt.Errorf("renames %s: %w", table, err)
	}
	renames.Store(table, r)
	return r, nil
}

// checked caches what ResolveOld found of a table line, by table and old
// ID: a line is checked once, true or false, and objects never change. It
// is per process, not per store: a table byte-identical in two stores is
// checked against the first, and a store lacking the new records then
// fails its load, loudly.
var checked sync.Map // [2]c4.ID -> checkedLine

type checkedLine struct {
	cur c4.ID
	err error
}

// ResolveOld is the one place an ID an earlier library gave a view is
// turned into the ID it has now: every reader that takes a view's ID from
// outside what it wrote itself (a spec, a signed record's subject, a
// tracker's link) asks it before it loads the view. table is the renames
// object the project's status names; a nil one renames nothing. A line is
// not taken on trust: the first time one is used, the old ID's records
// are identified again and must give the new ID, or the line is an error
// and id is returned as it is.
func ResolveOld(ctx context.Context, st Store, table, id c4.ID) (c4.ID, error) {
	r, err := ReadRenames(st, table)
	if err != nil {
		return id, err
	}
	cur := r.Current(id)
	if cur == id {
		return id, nil
	}
	key := [2]c4.ID{table, id}
	if v, ok := checked.Load(key); ok {
		l := v.(checkedLine)
		if l.err != nil {
			return id, l.err
		}
		return l.cur, nil
	}
	v, err := Reidentify(ctx, id, StoreRecords(st), 16)
	if err != nil {
		err = fmt.Errorf("renames %s: %s: %w", table, id, err)
		// An old ID that is not a view's is so every time; a store that
		// could not be read, or a cancelled reader, says nothing about the
		// line, and is not kept.
		if errors.Is(err, ErrNotAView) {
			checked.Store(key, checkedLine{err: err})
		}
		return id, err
	}
	if v.ID != cur {
		err := fmt.Errorf("renames %s says %s is now %s, and its records identify as %s", table, id, cur, v.ID)
		checked.Store(key, checkedLine{err: err})
		return id, err
	}
	checked.Store(key, checkedLine{cur: cur})
	return cur, nil
}
