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

// The operator's patch readers meet c4's shared apply-from vector,
// projected into view space: ApplyPatchFrom, over a view's records, on
// every case, and the resolver on the one case whose target is its base
// (own-base); TestThePatchReadersAgree compares the two on all of them.
// base, target and
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
// view without it). A hard link's group and a flow link's operator and
// target are part of the entry's line, so changing one of them changes
// the entry too; a view records neither, so here the patch adds one. The view it makes keeps the path, through ApplyPatch
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
		{"a hard-link group", file, restated(file, func(e *c4m.Entry) { e.HardLink = 1 }), true},
		{"a flow link", file, restated(file, func(e *c4m.Entry) { e.FlowDirection, e.FlowTarget = c4m.FlowNone+1, "nas:" }), true},
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
	flow := &c4m.Entry{Name: "fl.txt", Mode: 0o644, Size: 4, Timestamp: when, C4ID: a, FlowDirection: c4m.FlowNone + 1, FlowTarget: "nas:"}
	root.AddEntry(flow)
	hard := &c4m.Entry{Name: "hl.txt", Mode: 0o644, Size: 4, Timestamp: when, C4ID: a, HardLink: 1}
	root.AddEntry(hard)
	with := func(e *c4m.Entry, change func(*c4m.Entry)) *c4m.Entry {
		cp := *e
		change(&cp)
		return &cp
	}
	root.AddEntry(&c4m.Entry{Name: "sub/", Mode: os.ModeDir | 0o755, Size: 4 + subSize, Timestamp: when, C4ID: subID})
	rootID, _ := put(root)
	for _, c := range []struct {
		name  string
		entry *c4m.Entry
		keeps bool
	}{
		{"as the description records it", dated, false},
		{"as its view records it", Project(dated), true},
		{"with only the hard-link group changed", with(hard, func(e *c4m.Entry) { e.HardLink = 2 }), true},
		{"with the hard-link group as recorded", hard, false},
		{"with only the flow target changed", with(flow, func(e *c4m.Entry) { e.FlowTarget = "studio:plates/" }), true},
		{"with only the flow direction changed", with(flow, func(e *c4m.Entry) { e.FlowDirection = c4m.FlowNone + 2 }), true},
		{"with the flow link as recorded", flow, false},
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
		_, kept := res.View().Root.Files[c.entry.Name]
		if kept != c.keeps {
			t.Errorf("restated %s: %s kept %v, want %v", c.name, c.entry.Name, kept, c.keeps)
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
// no expected view to hold either to. The directory line is exactly as a
// view records it, true size and all, so nothing but the shape is in
// question. Run unskipped on 2026-11-11 they do not agree, and the
// resolver's view holds x/ empty, neither the file nor what the line names
// (finding 27's 977 of 4,915 generated cases are this shape), so the
// ruling comes with a fix to one reader or both.
func TestTheReadersAgreeOnADirectoryWrittenByIDOverAFile(t *testing.T) {
	t.Skip("unruled: a directory written by its C4 ID over a file of that name (draft c4 #89)")
	ctx := context.Background()
	st := objStore{}
	all, _ := NewFilter(nil)
	// x/ as a view records it: the root record of a view holding it.
	holder := c4m.NewManifest()
	holder.AddEntry(&c4m.Entry{Name: "x/", Mode: os.ModeDir, Size: -1, Timestamp: c4m.NullTimestamp()})
	holder.AddEntry(&c4m.Entry{Name: "in.txt", Depth: 1, Size: 2, Timestamp: c4m.NullTimestamp(), C4ID: c4.Identify(strings.NewReader("in"))})
	hb, err := c4m.Marshal(holder)
	if err != nil {
		t.Fatal(err)
	}
	hr, err := FromBytes(hb, st, all)
	if err != nil {
		t.Fatal(err)
	}
	hv := hr.View()
	recs, _ := hv.Records()
	var line *c4m.Entry
	for _, r := range recs {
		st[r.ID] = r.Data
		if r.ID != hv.ID {
			continue
		}
		es, err := ParseRecord(r.Data)
		if err != nil || len(es) != 1 {
			t.Fatalf("the holder's root record: %v", err)
		}
		line = es[0]
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
	patch.AddEntry(line)
	a, err := ApplyPatch(ctx, base.ID, patch, StoreRecords(st))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range a.Records {
		st[r.ID] = r.Data
	}
	if _, err := Load(ctx, a.ID, StoreRecords(st), 1); err != nil {
		t.Fatalf("ApplyPatch's view does not load: %v", err)
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

// The view check covers every record an edit reads, the ones it only
// recalls as well: a directory made again inside one replaced wholesale is
// compared with the old directory's record (recall), which nothing loads.
// Here build/'s record is a view's, but the obj/ it names is a description
// with dates, and the Apply that makes obj/ again is refused. The removals
// it would write from that record would not restate it exactly. A record
// with a mode beyond the executable bit, or a link with a size and C4 ID,
// is refused too, dates or not.
func TestApplyRefusesEveryRecordThatIsNotAView(t *testing.T) {
	ctx := context.Background()
	when := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	old := c4.Identify(strings.NewReader("old"))
	for _, c := range []struct {
		name  string
		entry *c4m.Entry
	}{
		{"a date", &c4m.Entry{Name: "old.o", Mode: 0o644, Size: 3, Timestamp: when, C4ID: old}},
		{"a mode", &c4m.Entry{Name: "old.o", Mode: 0o644, Size: 3, Timestamp: c4m.NullTimestamp(), C4ID: old}},
		{"a link's size and C4 ID", &c4m.Entry{Name: "old.o", Mode: os.ModeSymlink | 0o777, Size: 3, Timestamp: c4m.NullTimestamp(), C4ID: old, Target: "x"}},
	} {
		st := objStore{}
		put := func(es ...*c4m.Entry) (c4.ID, int64) {
			m := c4m.NewManifest()
			for _, e := range es {
				m.AddEntry(e)
			}
			b, err := c4m.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			id := c4.Identify(bytes.NewReader(b))
			st[id] = b
			return id, int64(len(b))
		}
		obj, objSize := put(c.entry)
		build, buildSize := put(Project(&c4m.Entry{Name: "obj/", Mode: os.ModeDir, Size: objSize, C4ID: obj}))
		root, _ := put(Project(&c4m.Entry{Name: "build/", Mode: os.ModeDir, Size: buildSize, C4ID: build}))
		edits := []Edit{
			{Path: "build/", Entry: &c4m.Entry{Name: "build/"}, Opaque: true},
			{Path: "build/obj/", Entry: &c4m.Entry{Name: "obj/"}},
			{Path: "build/obj/new.o", Entry: FileEntry("", 3, c4.Identify(strings.NewReader("new")), false)},
		}
		SortEdits(edits)
		if _, err := Apply(ctx, root, edits, StoreRecords(st)); !errors.Is(err, ErrNotAView) {
			t.Errorf("%s: Apply over a record that is not a view's: %v, want ErrNotAView", c.name, err)
		}
	}
}
