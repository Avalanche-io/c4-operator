package view

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"
)

// A changeset stored before the pin moved is not renamed: applied over its
// parent's new ID, it gives its head's new ID. The fixture
// testdata/pin19c9-removals (gen/main.go's removals, run at 19c97485) is a
// view V and changesets over it that remove, replace or add one thing each.
// Removal is spelled by restating the base's entry exactly, so these are
// the changesets whose meaning rests on V's entries as the new library
// records them. Removing a directory #107 renamed (a null-sized frame or a
// link two directories down) was the case that went wrong: the directory's
// size differs between the libraries, so the removal read as a write of
// the directory by its own ID and nothing was removed (restatesInView).
func TestAnOldChangesetGivesItsHeadsNewID(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join("testdata", "pin19c9-removals")
	ram := store.NewRAM()
	files, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, "objects", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if id, err := ram.Put(bytes.NewReader(data)); err != nil || id.String() != f.Name() {
			t.Fatalf("fixture object %s: %s, %v", f.Name(), id, err)
		}
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.txt"))
	if err != nil {
		t.Fatal(err)
	}
	src := StoreRecords(ram)
	// newID re-identifies a view stored at the old pin and stores its new
	// records, as the migration does.
	newID := func(old c4.ID) c4.ID {
		t.Helper()
		v, err := Reidentify(ctx, old, src, 4)
		if err != nil {
			t.Fatalf("reidentify %s: %v", old, err)
		}
		rs, err := v.Records()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if _, err := ram.Put(bytes.NewReader(r.Data)); err != nil {
				t.Fatal(err)
			}
		}
		return v.ID
	}
	var v c4.ID
	type step struct {
		name     string
		cs, head c4.ID
	}
	var steps []step
	for _, l := range strings.Split(string(index), "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 3 && f[0] == "view" && f[1] == "V":
			v = mustParse(t, f[2])
		case len(f) == 4 && f[0] == "changeset" && f[1] != "V": // V's own is over the empty view
			steps = append(steps, step{f[1], mustParse(t, f[2]), mustParse(t, f[3])})
		}
	}
	if v.IsNil() || len(steps) != 11 {
		t.Fatalf("the fixture names V %s and %d changesets", v, len(steps))
	}
	vNew := newID(v)
	if vNew == v {
		t.Fatal("V is not renamed by the pin, so this tests nothing")
	}
	// Each shape the fixture holds, so a regeneration that drops one fails.
	want := map[string]bool{
		"rm-top-link": true, "rm-deep-link": true, "rm-dir-of-link": true,
		"rm-dir-deep-link": true, "rm-dir-deep-null": true,
		"rm-nested-dir-deep-link": true, "rm-nested-dir-deep-null": true,
		"rm-dir-plain": true, "rm-file": true, "retarget-link": true, "add-file-deep": true,
	}
	for _, s := range steps {
		delete(want, s.name)
		data, err := ReadObject(ram, s.cs)
		if err != nil {
			t.Fatal(err)
		}
		ch, err := ReadChain(data)
		if err != nil || len(ch.Sections) != 1 || ch.Base != v {
			t.Fatalf("%s: %v", s.name, err)
		}
		headNew := newID(s.head)
		got, err := ApplyPatch(ctx, vNew, ch.Sections[0].Patch, src)
		switch {
		case err != nil:
			t.Errorf("%s over V's new ID: %v", s.name, err)
		case got.ID != headNew:
			t.Errorf("%s over V's new ID gives %s, and its head's new ID is %s", s.name, got.ID, headNew)
		}
	}
	for name := range want {
		t.Errorf("the fixture has no %s", name)
	}
}

func mustParse(t *testing.T, s string) c4.ID {
	t.Helper()
	id, err := c4.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
