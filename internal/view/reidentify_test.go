package view

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"
)

// oldPin is the fixture testdata/pin19c9: views, records and changesets
// written by this package at the c4 pin before the move (gen/main.go, run
// at 19c97485), loaded into a RAM store.
type oldPin struct {
	ram   *store.RAM
	views map[string]c4.ID
}

func loadOldPin(t *testing.T) oldPin {
	t.Helper()
	dir := filepath.Join("testdata", "pin19c9")
	p := oldPin{ram: store.NewRAM(), views: map[string]c4.ID{}}
	files, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, "objects", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		id, err := p.ram.Put(bytes.NewReader(data))
		if err != nil || id.String() != f.Name() {
			t.Fatalf("fixture object %s: %s, %v", f.Name(), id, err)
		}
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(index), "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[0] == "view" {
			id, err := c4.Parse(f[2])
			if err != nil {
				t.Fatal(err)
			}
			p.views[f[1]] = id
		}
	}
	return p
}

// The views whose IDs c4 #107 changes: a null-sized entry (a link, a
// sequence frame) two or more directories down.
var renamedByPin = map[string]bool{"link-depth2": true, "null-depth2": true, "mixed-deep": true, "chain1": true, "chain2": true, "tool": true, "toolout": true}

// A view stored before the pin moved no longer loads under its ID when its
// records identify differently; Reidentify gives the ID they have now, its
// records load under it, and the tree is the same tree, file for file.
func TestAViewStoredBeforeThePinMovedIsReidentified(t *testing.T) {
	ctx := context.Background()
	p := loadOldPin(t)
	src := StoreRecords(p.ram)
	if len(p.views) != 9 {
		t.Fatalf("the fixture names %d views", len(p.views))
	}
	for name, old := range p.views {
		_, loadErr := Load(ctx, old, src, 4)
		v, err := Reidentify(ctx, old, src, 4)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if renamedByPin[name] != (v.ID != old) {
			t.Errorf("%s: renamed %v, want %v", name, v.ID != old, renamedByPin[name])
		}
		if (loadErr == nil) != (v.ID == old) {
			t.Errorf("%s: Load of the old ID: %v, and the ID now is %s", name, loadErr, v.ID)
		}
		recs, err := v.Records()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, r := range recs {
			if !p.ram.Has(r.ID) {
				if _, err := p.ram.Put(bytes.NewReader(r.Data)); err != nil {
					t.Fatal(err)
				}
			}
		}
		now, err := Load(ctx, v.ID, src, 4)
		if err != nil {
			t.Fatalf("%s: the new records do not load: %v", name, err)
		}
		again, err := Reidentify(ctx, v.ID, src, 4)
		if err != nil || again.ID != v.ID {
			t.Fatalf("%s: the new ID re-identifies as %s (%v)", name, again.ID, err)
		}
		if !sameTree(now, v) {
			t.Errorf("%s: the loaded view is not the tree the old records hold", name)
		}
		// Files and links as the old records hold them: only directories'
		// derived fields change.
		if len(now.Files)+len(now.Symlinks) == 0 {
			t.Errorf("%s: an empty tree", name)
		}
	}
}

func sameTree(a, b *View) bool {
	if a.ID != b.ID || len(a.Files) != len(b.Files) || len(a.Symlinks) != len(b.Symlinks) || len(a.Dirs) != len(b.Dirs) {
		return false
	}
	for i := range a.Files {
		if a.Files[i] != b.Files[i] {
			return false
		}
	}
	for i := range a.Symlinks {
		if a.Symlinks[i] != b.Symlinks[i] {
			return false
		}
	}
	return true
}

// An old ID loads through the table the project's status names, and an ID
// the table does not hold is itself: a view the library change left alone,
// or one written since.
func TestAnOldIDResolvesThroughTheTable(t *testing.T) {
	ctx := context.Background()
	p := loadOldPin(t)
	src := StoreRecords(p.ram)
	table := Renames{}
	for _, old := range p.views {
		v, err := Reidentify(ctx, old, src, 4)
		if err != nil {
			t.Fatal(err)
		}
		if v.ID != old {
			table[old] = v.ID
		}
	}
	if len(table) != len(renamedByPin) {
		t.Fatalf("%d renames, want %d", len(table), len(renamedByPin))
	}
	tid, err := p.ram.Put(bytes.NewReader(table.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for name, old := range p.views {
		got, err := ResolveOld(context.Background(), p.ram, tid, old)
		if err != nil {
			t.Fatal(err)
		}
		if want := table.Current(old); got != want || (renamedByPin[name] && got == old) {
			t.Errorf("%s: resolves to %s, want %s", name, got, want)
		}
	}
	if got, err := ResolveOld(context.Background(), p.ram, c4.ID{}, p.views["null-depth2"]); err != nil || got != p.views["null-depth2"] {
		t.Fatalf("no table: %s, %v", got, err)
	}
	// The table's text is its identity: parsed and written again, the
	// same bytes.
	back, err := ParseRenames(table.Bytes())
	if err != nil || !bytes.Equal(back.Bytes(), table.Bytes()) {
		t.Fatalf("the table does not read back: %v", err)
	}
}

// A second library change renames a renamed view again; Current follows
// the table to the end.
func TestCurrentFollowsALaterRename(t *testing.T) {
	a, b, c := c4.Identify(strings.NewReader("a")), c4.Identify(strings.NewReader("b")), c4.Identify(strings.NewReader("c"))
	r := Renames{a: b, b: c}
	if r.Current(a) != c || r.Current(b) != c || r.Current(c) != c {
		t.Fatalf("Current: %s %s %s", r.Current(a), r.Current(b), r.Current(c))
	}
}

func TestATableThatIsNotOneIsRefused(t *testing.T) {
	a, b := c4.Identify(strings.NewReader("a")), c4.Identify(strings.NewReader("b"))
	for name, text := range map[string]string{
		"one field":          a.String() + "\n",
		"three fields":       a.String() + " " + b.String() + " " + b.String() + "\n",
		"not a C4 ID":        "c4nope " + b.String() + "\n",
		"renamed to itself":  a.String() + " " + a.String() + "\n",
		"a view named twice": a.String() + " " + b.String() + "\n" + a.String() + " " + b.String() + "\n",
	} {
		if _, err := ParseRenames([]byte(text)); err == nil || !strings.Contains(err.Error(), "line") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// What is not a view is not re-identified: a changeset (its first line is
// its base), and a description whose entries hold dates.
func TestOnlyAViewIsReidentified(t *testing.T) {
	ctx := context.Background()
	p := loadOldPin(t)
	src := StoreRecords(p.ram)
	index, err := os.ReadFile(filepath.Join("testdata", "pin19c9", "index.txt"))
	if err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, l := range strings.Split(string(index), "\n") {
		f := strings.Fields(l)
		if len(f) != 4 || f[0] != "changeset" {
			continue
		}
		cs, err := c4.Parse(f[2])
		if err != nil {
			t.Fatal(err)
		}
		if v, err := Reidentify(ctx, cs, src, 4); err == nil {
			t.Errorf("changeset %s re-identified as %s", f[1], v.ID)
		}
		refused++
	}
	if refused != 9 {
		t.Fatalf("%d changesets in the fixture", refused)
	}
	dated := c4m.NewManifest()
	dated.AddEntry(&c4m.Entry{Mode: 0o644, Timestamp: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), Size: 1, Name: "a.txt", C4ID: c4.Identify(strings.NewReader("a"))})
	data, err := c4m.Marshal(dated)
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.ram.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := Reidentify(ctx, id, src, 4); !errors.Is(err, ErrNotAView) {
		t.Fatalf("a description with dates: %v, %v", v, err)
	}
}

// A table line is checked, not trusted: one that names another view as the
// old one's new ID is an error, and the ID comes back as it was.
func TestAFalseTableLineIsRefused(t *testing.T) {
	p := loadOldPin(t)
	old, other := p.views["null-depth2"], p.views["plain"]
	table, err := p.ram.Put(bytes.NewReader(Renames{old: other}.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveOld(context.Background(), p.ram, table, old)
	if err == nil || got != old || !strings.Contains(err.Error(), "identify as") {
		t.Fatalf("a false line: %s, %v", got, err)
	}
}
