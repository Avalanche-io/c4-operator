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

// The views that hold a link. The fixture wrote every link with a null
// mode; a view now records a link's type (LinkMode, lrwxrwxrwx), so each
// of these records, and the view's ID, changes too.
var holdsALink = map[string]bool{"link-depth1": true, "link-depth2": true, "mixed-deep": true, "chain1": true, "chain2": true, "tool": true, "toolout": true}

// renamed are the fixture's views Reidentify gives new IDs: those c4 #107
// changes and those holding a link. Only plain is left alone.
var renamed = func() map[string]bool {
	out := map[string]bool{}
	for _, set := range []map[string]bool{renamedByPin, holdsALink} {
		for name := range set {
			out[name] = true
		}
	}
	return out
}()

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
		if renamed[name] != (v.ID != old) {
			t.Errorf("%s: renamed %v, want %v", name, v.ID != old, renamed[name])
		}
		if (loadErr == nil) != (v.ID == old) {
			t.Errorf("%s: Load of the old ID: %v, and the ID now is %s", name, loadErr, v.ID)
		}
		recs, err := v.Records()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Every link is recorded with its type now: the cause of the
		// views holdsALink names being renamed.
		links := 0
		for _, r := range recs {
			raw, err := parseRecordRaw(r.Data)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, e := range raw {
				if !IsLink(e) {
					continue
				}
				links++
				if e.Mode != LinkMode {
					t.Errorf("%s: the link %q is recorded with mode %v", name, e.Name, e.Mode)
				}
			}
		}
		if (links > 0) != holdsALink[name] {
			t.Errorf("%s: %d links recorded, holds a link: %v", name, links, holdsALink[name])
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
	if len(table) != len(renamed) {
		t.Fatalf("%d renames, want %d", len(table), len(renamed))
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
		if want := table.Current(old); got != want || (renamed[name] && got == old) {
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

// A view stored before views kept a link's type holds every link with a
// null mode, and one to a location (host:path, C:foo) as a line the c4m
// library decodes as an outbound flow link. The migration's reader
// (Reidentify) reads it as the link it was written as, since a view never
// holds a flow link, and gives the view an ID whose records hold the link
// with its type. The ordinary reader does not take that line as a link:
// an edit over the old ID is refused, and the old ID does not load.
func TestAPreGuardLinkIsReidentifiedAsALink(t *testing.T) {
	ctx := context.Background()
	ram := store.NewRAM()
	src := StoreRecords(ram)
	// The projection before LinkMode: SymlinkEntry with a null mode.
	oldLink := func(name, target string) *c4m.Entry {
		return &c4m.Entry{Name: name, Size: -1, Target: target, Timestamp: c4m.NullTimestamp()}
	}
	root := NewNode("")
	root.Files["top"] = oldLink("top", "C:foo")
	root.Files["keep.txt"] = FileEntry("keep.txt", 4, c4.Identify(strings.NewReader("keep")), false)
	a := root.Dir("a/")
	a.Files["l"] = oldLink("l", "host:path")
	a.Files["near"] = oldLink("near", "t.txt")
	a.Files["t.txt"] = FileEntry("t.txt", 1, c4.Identify(strings.NewReader("t")), false)
	old := root.Identify()
	aOld := a.Entry.C4ID
	// Stored as the old writer stored them, each directory's canonical
	// listing. Records refuses them now, as the read-back guard does.
	root.Walk("", func(_ string, n *Node) error { putBytes(t, ram, n.Entry.C4ID, n.Record()); return nil })
	var nr *NotReadBack
	if _, err := root.Records(); !errors.As(err, &nr) {
		t.Fatalf("the old records are taken by the guard: %v", err)
	}
	// The premise: the library decodes the old lines as flow links.
	for id, want := range map[c4.ID][2]string{old: {"top", "C:foo"}, aOld: {"l", "host:path"}} {
		data, err := ReadObject(ram, id)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := parseRecordRaw(data)
		if err != nil {
			t.Fatal(err)
		}
		flows := 0
		for _, e := range raw {
			if e.Name != want[0] {
				continue
			}
			if e.FlowDirection != c4m.FlowOutbound || e.FlowTarget != want[1] || e.Target != "" || e.Mode != 0 {
				t.Fatalf("%s: the old line decodes as mode %v, target %q, flow %v %q, not as a flow link", want[0], e.Mode, e.Target, e.FlowDirection, e.FlowTarget)
			}
			flows++
		}
		if flows != 1 {
			t.Fatalf("%s: not in the old record:\n%s", want[0], data)
		}
	}

	v, err := Reidentify(ctx, old, src, 4)
	if err != nil {
		t.Fatal(err)
	}
	if v.ID == old {
		t.Fatal("the view keeps its old ID")
	}
	wantLinks := []Symlink{{"a/l", "host:path"}, {"a/near", "t.txt"}, {"top", "C:foo"}}
	if len(v.Symlinks) != len(wantLinks) || len(v.Files) != 2 {
		t.Fatalf("re-identified as links %v and %d files", v.Symlinks, len(v.Files))
	}
	for i, l := range wantLinks {
		if v.Symlinks[i] != l {
			t.Errorf("link %d: %v, want %v", i, v.Symlinks[i], l)
		}
	}
	recs, err := v.Records()
	if err != nil {
		t.Fatal(err)
	}
	readsBackAsLink(t, recordOf(t, recs, "a/"), "l", "host:path")
	readsBackAsLink(t, recordOf(t, recs, "a/"), "near", "t.txt")
	readsBackAsLink(t, recordOf(t, recs, ""), "top", "C:foo")
	for _, r := range recs {
		putBytes(t, ram, r.ID, r.Data)
	}
	now, err := Load(ctx, v.ID, src, 4)
	if err != nil || !sameTree(now, v) {
		t.Fatalf("the new ID loads as another tree: %v", err)
	}
	if _, err := Apply(ctx, v.ID, []Edit{fileEdit("a/new.txt", "n")}, src); err != nil {
		t.Fatalf("an edit over the new ID: %v", err)
	}

	// The ordinary reader: not a view's record, not a link.
	for _, id := range []c4.ID{old, aOld} {
		if entries, err := fetchViewRecord(ctx, src, id); !errors.Is(err, ErrNotAView) {
			t.Errorf("fetchViewRecord of an old record: %v, %v", entries, err)
		}
	}
	for _, path := range []string{"x.txt", "a/new.txt"} {
		if res, err := Apply(ctx, old, []Edit{fileEdit(path, "n")}, src); !errors.Is(err, ErrNotAView) || res != nil {
			t.Errorf("an edit of %s over the old ID: %v", path, err)
		}
	}
	if lv, err := Load(ctx, old, src, 4); err == nil {
		t.Errorf("the old ID loads, with links %v", lv.Symlinks)
	}
}
