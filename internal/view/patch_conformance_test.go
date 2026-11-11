package view

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
)

// The operator's one patch reader (ApplyPatch, ApplyPatchFrom, and the
// resolver, which applies changesets through it) meets c4's shared
// apply-from vector, projected into view space: base, target and expected
// are each case's views, and the patch is each entry projected, as a
// changeset over a view is written. testdata/apply-from-vector/COPIED.md
// says where the vector came from and when it goes.
func TestTheOnePatchReaderMeetsTheSharedVector(t *testing.T) {
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

// An entry restated with another mode and date is a write, never a
// removal: c4m SPECIFICATION.md, "Patch Entry Semantics": "'All fields
// identical' means: same name, mode, timestamp, size, C4 ID, and target."
// A view B holding secret.mov, and a changeset over it restating secret.mov
// with its ID and size and another mode and date: the view it makes keeps
// secret.mov, through ApplyPatch and through the resolver alike; an exact
// restatement removes it.
func TestARestatedEntryIsAWrite(t *testing.T) {
	ctx := context.Background()
	st := objStore{}
	all, _ := NewFilter(nil)
	secret := c4.Identify(strings.NewReader("the unreleased cut"))
	kept := c4.Identify(strings.NewReader("kept"))
	bm := c4m.NewManifest()
	bm.AddEntry(&c4m.Entry{Name: "kept.txt", Mode: 0o644, Size: 4, Timestamp: c4m.NullTimestamp(), C4ID: kept})
	bm.AddEntry(&c4m.Entry{Name: "secret.mov", Mode: 0o644, Size: 18, Timestamp: c4m.NullTimestamp(), C4ID: secret})
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
	hasSecret := func(v *View) bool {
		_, ok := v.Root.Files["secret.mov"]
		return ok
	}
	restated := func(mode os.FileMode, when time.Time) *c4m.Manifest {
		m := c4m.NewManifest()
		m.AddEntry(&c4m.Entry{Name: "secret.mov", Mode: mode, Size: 18, Timestamp: when, C4ID: secret})
		return m
	}
	for _, c := range []struct {
		name  string
		patch *c4m.Manifest
		keeps bool
	}{
		{"another mode and date", restated(0o600, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)), true},
		{"another date", restated(0, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)), true},
		{"exactly as recorded", restated(0, c4m.NullTimestamp()), false},
	} {
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
		if hasSecret(made) != c.keeps {
			t.Errorf("%s: ApplyPatch kept secret.mov %v, want %v", c.name, hasSecret(made), c.keeps)
		}
		cs, err := EncodeChangeset(b.ID, c.patch)
		if err != nil {
			t.Fatal(err)
		}
		rr, err := FromBytes(cs, st, all)
		if err != nil {
			t.Fatal(err)
		}
		if hasSecret(rr.View()) != c.keeps || rr.View().ID != a.ID {
			t.Errorf("%s: the resolver kept secret.mov %v (view %s), ApplyPatch made %s", c.name, hasSecret(rr.View()), rr.View().ID, a.ID)
		}
	}
}
