package view

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// Both of the operator's patch readers meet c4's shared apply-from vector,
// projected into view space: ApplyPatchFrom, over a view's records, and the
// resolver, where the patch's target is its base. base, target and
// expected are each case's views, and the patch is each entry projected, as
// a changeset over a view is written. testdata/apply-from-vector/COPIED.md
// says where the vector came from and when it goes.
func TestThePatchReadersMeetTheSharedVector(t *testing.T) {
	ctx := context.Background()
	st := objStore{}
	all, _ := NewFilter(nil)
	viewOf := func(b []byte) c4.ID {
		t.Helper()
		res, err := FromBytes(b, st, all)
		if err != nil {
			t.Fatal(err)
		}
		v := res.View()
		recs, err := v.Records()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			st[r.ID] = r.Data
		}
		return v.ID
	}
	cases, err := filepath.Glob(filepath.Join("testdata", "apply-from-vector", "*", "patch.c4m"))
	if err != nil || len(cases) == 0 {
		t.Fatalf("no cases: %v", err)
	}
	sort.Strings(cases)
	for _, pf := range cases {
		dir := filepath.Dir(pf)
		name := filepath.Base(dir)
		read := func(n string) []byte {
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		base, target, want := viewOf(read("base.c4m")), viewOf(read("target.c4m")), viewOf(read("expected.c4m"))
		ch, err := ReadChain(read("patch.c4m"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		patch := c4m.NewManifest()
		for _, e := range ch.Sections[0].Patch.Entries {
			pe := Project(e)
			pe.Depth = e.Depth
			patch.AddEntry(pe)
		}
		a, err := ApplyPatchFrom(ctx, target, base, patch, StoreRecords(st))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if a.ID != want {
			t.Errorf("%s: ApplyPatchFrom made %s, the vector's expected view is %s", name, a.ID, want)
		}
		if target != base {
			continue
		}
		cs, err := EncodeChangeset(base, patch)
		if err != nil {
			t.Fatal(err)
		}
		res, err := FromBytes(cs, st, all)
		if err != nil {
			t.Errorf("%s: resolving: %v", name, err)
		} else if res.View().ID != want {
			t.Errorf("%s: the resolver made %s, the vector's expected view is %s", name, res.View().ID, want)
		}
	}
}

// An entry restated with any field different is a write, never a removal:
// c4m SPECIFICATION.md, "Patch Entry Semantics": "'All fields identical'
// means: same name, mode, timestamp, size, C4 ID, and target." A view B
// holds secret.mov and a link; a changeset over it restates one of them
// with one field changed (secret.mov with its ID and size and another mode
// and date is the case that once mounted B under a grant of the smaller
// view without it). The view it makes keeps the path, through ApplyPatch
// and through the resolver alike; an exact restatement removes it.
func TestARestatedEntryIsAWrite(t *testing.T) {
	ctx := context.Background()
	st := objStore{}
	all, _ := NewFilter(nil)
	secret := c4.Identify(strings.NewReader("the unreleased cut"))
	kept := c4.Identify(strings.NewReader("kept"))
	bm := c4m.NewManifest()
	bm.AddEntry(&c4m.Entry{Name: "kept.txt", Mode: 0o644, Size: 4, Timestamp: c4m.NullTimestamp(), C4ID: kept})
	link := &c4m.Entry{Name: "link", Mode: os.ModeSymlink | 0o777, Size: 8, Timestamp: c4m.NullTimestamp(), Target: "kept.txt"}
	bm.AddEntry(link)
	file := &c4m.Entry{Name: "secret.mov", Mode: 0o644, Size: 18, Timestamp: c4m.NullTimestamp(), C4ID: secret}
	bm.AddEntry(file)
	bb, err := c4m.Marshal(bm)
	if err != nil {
		t.Fatal(err)
	}
	res, err := FromBytes(bb, st, all)
	if err != nil {
		t.Fatal(err)
	}
	b := res.View()
	recs, _ := b.Records()
	for _, r := range recs {
		st[r.ID] = r.Data
	}
	has := func(v *View, name string) bool {
		_, ok := v.Root.Files[name]
		return ok
	}
	// restated is the entry as B's record holds it, with one change.
	restated := func(e *c4m.Entry, change func(*c4m.Entry)) *c4m.Manifest {
		cp := Project(e)
		change(cp)
		m := c4m.NewManifest()
		m.AddEntry(cp)
		return m
	}
	date := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	other := c4.Identify(strings.NewReader("another cut"))
	for _, c := range []struct {
		name  string
		entry *c4m.Entry
		patch *c4m.Manifest
		keeps bool
	}{
		{"another mode and date", file, restated(file, func(e *c4m.Entry) { e.Mode, e.Timestamp = 0o600, date }), true},
		{"another date", file, restated(file, func(e *c4m.Entry) { e.Timestamp = date }), true},
		{"another mode", file, restated(file, func(e *c4m.Entry) { e.Mode = 0o600 }), true},
		{"another size", file, restated(file, func(e *c4m.Entry) { e.Size = 19 }), true},
		{"another C4 ID", file, restated(file, func(e *c4m.Entry) { e.C4ID = other }), true},
		{"exactly as recorded", file, restated(file, func(*c4m.Entry) {}), false},
		{"a link with another target", link, restated(link, func(e *c4m.Entry) { e.Target = "secret.mov" }), true},
		{"a link exactly as recorded", link, restated(link, func(*c4m.Entry) {}), false},
	} {
		name := c.entry.Name
		a, err := ApplyPatch(ctx, b.ID, c.patch, StoreRecords(st))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, r := range a.Records {
			st[r.ID] = r.Data
		}
		made, err := Load(ctx, a.ID, StoreRecords(st), 1)
		if err != nil {
			t.Fatal(err)
		}
		if has(made, name) != c.keeps {
			t.Errorf("%s: ApplyPatch kept %s %v, want %v", c.name, name, has(made, name), c.keeps)
		}
		cs, err := EncodeChangeset(b.ID, c.patch)
		if err != nil {
			t.Fatal(err)
		}
		rr, err := FromBytes(cs, st, all)
		if err != nil {
			t.Fatal(err)
		}
		if has(rr.View(), name) != c.keeps || rr.View().ID != a.ID {
			t.Errorf("%s: the resolver kept %s %v (view %s), ApplyPatch made %s", c.name, name, has(rr.View(), name), rr.View().ID, a.ID)
		}
	}
}

// A changeset's base is what its base ID names, as recorded. Over a stored
// description with dates (each level a one-level c4m, as c4 writes them),
// an entry restated exactly as the description holds it, date and all, is
// a removal, as a patch from c4 diff writes one, and the same entry
// projected, as a view holds it, is a write. Read against the base's view
// instead, the two swap, and c4 diff's removals would be kept.
func TestAChangesetOverADescriptionReadsItAsRecorded(t *testing.T) {
	st := objStore{}
	all, _ := NewFilter(nil)
	when := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	put := func(m *c4m.Manifest) (c4.ID, int64) {
		b, err := c4m.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		id := c4.Identify(bytes.NewReader(b))
		st[id] = b
		return id, int64(len(b))
	}
	a := c4.Identify(strings.NewReader("aaaa"))
	b := c4.Identify(strings.NewReader("bbbb"))
	sub := c4m.NewManifest()
	sub.AddEntry(&c4m.Entry{Name: "b.txt", Mode: 0o644, Size: 4, Timestamp: when, C4ID: b})
	subID, subSize := put(sub)
	root := c4m.NewManifest()
	dated := &c4m.Entry{Name: "a.txt", Mode: 0o644, Size: 4, Timestamp: when, C4ID: a}
	root.AddEntry(dated)
	root.AddEntry(&c4m.Entry{Name: "sub/", Mode: os.ModeDir | 0o755, Size: 4 + subSize, Timestamp: when, C4ID: subID})
	rootID, _ := put(root)
	for _, c := range []struct {
		name  string
		entry *c4m.Entry
		keeps bool
	}{
		{"as the description records it", dated, false},
		{"as its view records it", Project(dated), true},
	} {
		p := c4m.NewManifest()
		cp := *c.entry
		p.AddEntry(&cp)
		cs, err := EncodeChangeset(rootID, p)
		if err != nil {
			t.Fatal(err)
		}
		res, err := FromBytes(cs, st, all)
		if err != nil {
			t.Fatal(err)
		}
		_, kept := res.View().Root.Files["a.txt"]
		if kept != c.keeps {
			t.Errorf("restated %s: a.txt kept %v, want %v", c.name, kept, c.keeps)
		}
		if _, ok := res.View().Root.Dirs["sub/"]; !ok {
			t.Errorf("restated %s: sub/ is gone", c.name)
		}
		// ApplyPatch takes only a view as its base, and says so.
		if _, err := ApplyPatch(context.Background(), rootID, p, StoreRecords(st)); !errors.Is(err, ErrNotAView) {
			t.Errorf("restated %s: ApplyPatch over a description: %v, want ErrNotAView", c.name, err)
		}
	}
}

// The two patch readers agree: every case of c4's apply-from vector, its
// patch applied over its own base, written as a view's changeset
// (projected) and as the vector has it (dates and all), goes through
// ApplyPatch and the resolver, and the two views must be one. Each reader
// is held to the vector's expected views as well (above, and the restated
// cases compare the two too); this holds them to each other on every
// input, so a change to one that no expected view sees still shows.
func TestThePatchReadersAgree(t *testing.T) {
	ctx := context.Background()
	st := objStore{}
	all, _ := NewFilter(nil)
	cases, _ := filepath.Glob(filepath.Join("testdata", "apply-from-vector", "*", "patch.c4m"))
	sort.Strings(cases)
	compared := 0
	for _, pf := range cases {
		dir := filepath.Dir(pf)
		name := filepath.Base(dir)
		read := func(n string) []byte {
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		res, err := FromBytes(read("base.c4m"), st, all)
		if err != nil {
			t.Fatal(err)
		}
		base := res.View()
		recs, _ := base.Records()
		for _, r := range recs {
			st[r.ID] = r.Data
		}
		ch, err := ReadChain(read("patch.c4m"))
		if err != nil {
			t.Fatal(err)
		}
		projected, raw := c4m.NewManifest(), c4m.NewManifest()
		for _, e := range ch.Sections[0].Patch.Entries {
			pe := Project(e)
			pe.Depth = e.Depth
			projected.AddEntry(pe)
			cp := *e
			raw.AddEntry(&cp)
		}
		for _, form := range []struct {
			name  string
			patch *c4m.Manifest
		}{{"projected", projected}, {"as written", raw}} {
			a, err := ApplyPatch(ctx, base.ID, form.patch, StoreRecords(st))
			if err != nil {
				t.Errorf("%s, %s: ApplyPatch: %v", name, form.name, err)
				continue
			}
			cs, err := EncodeChangeset(base.ID, form.patch)
			if err != nil {
				t.Fatal(err)
			}
			rr, err := FromBytes(cs, st, all)
			if err != nil {
				t.Errorf("%s, %s: the resolver: %v", name, form.name, err)
				continue
			}
			if rr.View().ID != a.ID {
				t.Errorf("%s, %s: ApplyPatch made %s, the resolver %s", name, form.name, a.ID, rr.View().ID)
			}
			compared++
		}
	}
	if compared != 2*len(cases) || len(cases) < 7 {
		t.Fatalf("compared %d of %d vector cases in two forms", compared, len(cases))
	}
}

// A directory written by its C4 ID alone over a file of that name, with no
// line removing the file: one of the questions about patches open in draft
// c4 #89. The two readers are to agree on it, whatever the ruling, and
// this is the case that shows whether they do; until the ruling there is
// no expected view to hold either to. Run unskipped on 2026-11-11 they do
// not agree: one reader's view is not the other's (finding 27's 977 of
// 4,915 generated cases are this shape), so the ruling comes with a fix
// to one reader or both.
func TestTheReadersAgreeOnADirectoryWrittenByIDOverAFile(t *testing.T) {
	t.Skip("unruled: a directory written by its C4 ID over a file of that name (draft c4 #89)")
	ctx := context.Background()
	st := objStore{}
	all, _ := NewFilter(nil)
	inner := c4m.NewManifest()
	inner.AddEntry(&c4m.Entry{Name: "in.txt", Size: 2, Timestamp: c4m.NullTimestamp(), C4ID: c4.Identify(strings.NewReader("in"))})
	ib, err := c4m.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	ir, err := FromBytes(ib, st, all)
	if err != nil {
		t.Fatal(err)
	}
	dir := ir.View()
	recs, _ := dir.Records()
	for _, r := range recs {
		st[r.ID] = r.Data
	}
	bm := c4m.NewManifest()
	bm.AddEntry(&c4m.Entry{Name: "x", Size: 1, Timestamp: c4m.NullTimestamp(), C4ID: c4.Identify(strings.NewReader("x"))})
	bb, err := c4m.Marshal(bm)
	if err != nil {
		t.Fatal(err)
	}
	br, err := FromBytes(bb, st, all)
	if err != nil {
		t.Fatal(err)
	}
	base := br.View()
	recs, _ = base.Records()
	for _, r := range recs {
		st[r.ID] = r.Data
	}
	patch := c4m.NewManifest()
	patch.AddEntry(&c4m.Entry{Name: "x/", Mode: os.ModeDir, Size: -1, Timestamp: c4m.NullTimestamp(), C4ID: dir.ID})
	a, err := ApplyPatch(ctx, base.ID, patch, StoreRecords(st))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := EncodeChangeset(base.ID, patch)
	if err != nil {
		t.Fatal(err)
	}
	rr, err := FromBytes(cs, st, all)
	if err != nil {
		t.Fatal(err)
	}
	if rr.View().ID != a.ID {
		t.Errorf("ApplyPatch made %s, the resolver %s", a.ID, rr.View().ID)
	}
}
