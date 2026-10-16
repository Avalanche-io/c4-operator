package view

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/scan"
	"github.com/Avalanche-io/c4/store"
)

// writeTree creates files under root; content is the map value. A key
// ending in "/" is an empty directory; a value starting with "->" is a
// symlink; a key ending in "*" is an executable file.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		exec := strings.HasSuffix(p, "*")
		p = strings.TrimSuffix(p, "*")
		full := filepath.Join(root, filepath.FromSlash(p))
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(content, "->"); ok {
			if err := os.Symlink(target, full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		mode := os.FileMode(0o644)
		if exec {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(full, mode)
	}
}

var show = map[string]string{
	"shots/sh010/plates/frame.0001.exr": "plate-a-1",
	"shots/sh010/plates/frame.0002.exr": "plate-a-2",
	"shots/sh010/notes.txt":             "notes 010",
	"shots/sh020/plates/frame.0001.exr": "plate-a-2", // same bytes as sh010 frame 2
	"shots/sh020/plates/frame.0002.exr": "plate-b-2",
	"shots/sh020/empty/":                "",
	"shots/sh030/plates/frame.0001.exr": "never wanted",
	"weights/model.bin":                 "weights",
}

func scanMode(t *testing.T, dir string, mode scan.ScanMode) *c4m.Manifest {
	t.Helper()
	m, err := scan.Dir(dir, scan.WithMode(mode), scan.WithHidden(true))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestViewIDMatchesContentScan(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, show)
	data, err := c4m.Marshal(scanMode(t, src, scan.ModeFull))
	if err != nil {
		t.Fatal(err)
	}
	r, err := FromBytes(data, nil, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	content := scanMode(t, src, scan.ModeContent).ComputeC4ID()
	if r.View().ID != content {
		t.Fatalf("view ID %s, c4 content scan %s", r.View().ID, content)
	}
	if r.RootID != scanMode(t, src, scan.ModeFull).ComputeC4ID() {
		t.Fatalf("root ID is not the snapshot ID")
	}
	got, err := ScanID(src)
	if err != nil {
		t.Fatal(err)
	}
	if got != content {
		t.Fatalf("ScanID %s, c4 content scan %s", got, content)
	}
	tot := r.View().Totals()
	if tot.Files != 7 || tot.UniqueObjects != 6 {
		t.Fatalf("totals %+v", tot)
	}
}

func TestExecBitIsPartOfTheView(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	tree := map[string]string{"bin/build.sh*": "#!/bin/sh\necho hi\n", "src/main.go": "package main\n", "link": "->src/main.go"}
	writeTree(t, a, tree)
	plain := map[string]string{"bin/build.sh": "#!/bin/sh\necho hi\n", "src/main.go": "package main\n", "link": "->src/main.go"}
	writeTree(t, b, plain)
	va, err := ScanDir(a)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := ScanDir(b)
	if err != nil {
		t.Fatal(err)
	}
	if va.ID == vb.ID {
		t.Fatal("executable bit did not change the view ID")
	}
	if !va.Files[0].Exec || vb.Files[0].Exec {
		t.Fatalf("exec flags %+v %+v", va.Files[0], vb.Files[0])
	}
	if len(va.Symlinks) != 1 || va.Symlinks[0].Target != "src/main.go" {
		t.Fatalf("symlinks %+v", va.Symlinks)
	}
	// The full c4m of the same tree resolves to the same view.
	data, _ := c4m.Marshal(scanMode(t, a, scan.ModeFull))
	r, err := FromBytes(data, nil, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if r.View().ID != va.ID {
		t.Fatalf("c4m view %s != scanned view %s", r.View().ID, va.ID)
	}
}

func TestRecordsRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, show)
	writeTree(t, src, map[string]string{"tools/run*": "#!/bin/sh\n", "tools/latest": "->run"})
	v, err := ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	ram := store.NewRAM()
	recs := v.Records()
	if recs[len(recs)-1].ID != v.ID {
		t.Fatal("the root record is not last")
	}
	for _, r := range recs {
		if c4.Identify(bytes.NewReader(r.Data)) != r.ID {
			t.Fatalf("record %s does not hash to its ID", r.ID)
		}
		w, _ := ram.Create(r.ID)
		w.Write(r.Data)
		w.Close()
	}
	got, err := Load(context.Background(), v.ID, StoreRecords(ram), 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != v.ID || len(got.Files) != len(v.Files) || len(got.Dirs) != len(v.Dirs) || len(got.Symlinks) != 1 {
		t.Fatalf("loaded %d files %d dirs, want %d %d", len(got.Files), len(got.Dirs), len(v.Files), len(v.Dirs))
	}
	// FromID treats a view ID like any stored description.
	r, err := FromID(v.ID, ram, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if r.View().ID != v.ID {
		t.Fatal("FromID on a view ID changed the view")
	}
	// The nested form parses back to the same view.
	b, err := v.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(b, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(b, c4.Identify(strings.NewReader("x"))); err == nil {
		t.Fatal("Parse accepted a mismatched ID")
	}
}

func TestFilteredViewMatchesFilteredTree(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, show)
	data, _ := c4m.Marshal(scanMode(t, src, scan.ModeFull))
	f, err := NewFilter([]string{"shots/sh020/", "weights"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := FromBytes(data, nil, f)
	if err != nil {
		t.Fatal(err)
	}
	sub := t.TempDir()
	subset := map[string]string{}
	for p, c := range show {
		if strings.HasPrefix(p, "shots/sh020/") || strings.HasPrefix(p, "weights/") {
			subset[p] = c
		}
	}
	writeTree(t, sub, subset)
	want, _ := ScanID(sub)
	if r.View().ID != want {
		t.Fatalf("filtered view %s, subset scan %s", r.View().ID, want)
	}
	wantDirs := []string{"shots/", "shots/sh020/", "shots/sh020/empty/", "shots/sh020/plates/", "weights/"}
	if fmt.Sprint(r.View().Dirs) != fmt.Sprint(wantDirs) {
		t.Fatalf("dirs %v", r.View().Dirs)
	}
}

// countingStore records which IDs were opened.
type countingStore struct {
	store.Store
	mu     sync.Mutex
	opened map[c4.ID]int
}

func (s *countingStore) Open(id c4.ID) (io.ReadCloser, error) {
	s.mu.Lock()
	s.opened[id]++
	s.mu.Unlock()
	return s.Store.Open(id)
}

// storeSnapshot puts every directory's one-level record into s, the way
// `c4 id -s` does, and returns the records by path and the root ID.
func storeSnapshot(t *testing.T, s store.Store, m *c4m.Manifest) (map[string]c4.ID, c4.ID) {
	t.Helper()
	ids := map[string]c4.ID{}
	for _, e := range m.Entries {
		if !e.IsDir() {
			continue
		}
		listing := c4m.NewManifest()
		for _, c := range m.Children(e) {
			cp := *c
			cp.Depth = 0
			listing.AddEntry(&cp)
		}
		id, err := s.Put(strings.NewReader(listing.Canonical()))
		if err != nil {
			t.Fatal(err)
		}
		if id != e.C4ID {
			t.Fatalf("record for %s hashes to %s, entry says %s", e.Name, id, e.C4ID)
		}
		ids[m.EntryPath(e)] = id
	}
	root := c4m.NewManifest()
	for _, e := range m.Entries {
		if e.Depth == 0 {
			cp := *e
			root.AddEntry(&cp)
		}
	}
	id, err := s.Put(strings.NewReader(root.Canonical()))
	if err != nil {
		t.Fatal(err)
	}
	return ids, id
}

func TestFromIDExpandsRecordsSparsely(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, show)
	m := scanMode(t, src, scan.ModeFull)
	ts, err := store.NewTreeStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := &countingStore{Store: ts, opened: map[c4.ID]int{}}
	dirIDs, rootID := storeSnapshot(t, ts, m)

	f, _ := NewFilter([]string{"shots/sh020/"})
	r, err := FromID(rootID, cs, f)
	if err != nil {
		t.Fatal(err)
	}
	if cs.opened[dirIDs["shots/sh030/"]] != 0 || cs.opened[dirIDs["shots/sh010/"]] != 0 || cs.opened[dirIDs["weights/"]] != 0 {
		t.Fatalf("resolution fetched records outside the filter: %v", cs.opened)
	}
	if cs.opened[dirIDs["shots/sh020/plates/"]] != 1 {
		t.Fatal("did not expand the included directory")
	}
	data, _ := c4m.Marshal(m)
	r2, err := FromBytes(data, nil, f)
	if err != nil {
		t.Fatal(err)
	}
	if r2.View().ID != r.View().ID {
		t.Fatalf("record view %s != text view %s", r.View().ID, r2.View().ID)
	}
}

func TestMissingRecordIsIncomplete(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, show)
	m := scanMode(t, src, scan.ModeFull)
	ts, _ := store.NewTreeStore(t.TempDir())
	dirIDs, rootID := storeSnapshot(t, ts, m)
	if err := ts.Remove(dirIDs["weights/"]); err != nil {
		t.Fatal(err)
	}
	if _, err := FromID(rootID, ts, Filter{}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	f, _ := NewFilter([]string{"shots/"})
	if _, err := FromID(rootID, ts, f); err != nil {
		t.Fatalf("sparse resolution failed: %v", err)
	}
}

func TestFilter(t *testing.T) {
	f, err := NewFilter([]string{"shots/*/plates", "weights/model.bin", "editorial/"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		p        string
		inc, may bool
		isDir    bool
	}{
		{"shots/sh010/plates/frame.0001.exr", true, true, false},
		{"shots/sh010/notes.txt", false, true, false},
		{"shots/", false, true, true},
		{"weights/model.bin", true, true, false},
		{"weights/other.bin", false, true, false},
		{"editorial/cut.mov", true, true, false},
		{"audio/", false, false, true},
	}
	for _, c := range cases {
		if got := f.Includes(c.p); got != c.inc {
			t.Errorf("Includes(%q) = %v", c.p, got)
		}
		if c.isDir && f.MayContain(c.p) != c.may {
			t.Errorf("MayContain(%q) = %v", c.p, !c.may)
		}
	}
	for _, bad := range []string{"", "../x", "a/[", "a//b/.."} {
		if _, err := NewFilter([]string{bad}); err == nil {
			t.Errorf("NewFilter(%q) accepted", bad)
		}
	}
	// A grant narrows an include; it can never widen it.
	grant, _ := NewFilter([]string{"shots/sh010/"})
	g := Intersect(f, grant)
	if !g.Includes("shots/sh010/plates/frame.0001.exr") || g.Includes("shots/sh020/plates/frame.0001.exr") || g.Includes("editorial/cut.mov") {
		t.Fatal("intersection is wrong")
	}
	if g.MayContain("shots/sh020/") || !g.MayContain("shots/") {
		t.Fatal("intersection MayContain is wrong")
	}
	if !Intersect(Filter{}, Filter{}).All() {
		t.Fatal("intersection of empty filters is not All")
	}
}

func TestUnsafeNamesRefused(t *testing.T) {
	bad := "- - 1 .. " + c4.Identify(strings.NewReader("x")).String() + "\n"
	if _, err := ParseRecord([]byte(bad)); err == nil {
		t.Fatal("ParseRecord accepted ..")
	}
	m := c4m.NewManifest()
	m.AddEntry(&c4m.Entry{Name: "..", Size: 1, C4ID: c4.Identify(strings.NewReader("x"))})
	data, _ := c4m.Marshal(m)
	if _, err := FromBytes(data, nil, Filter{}); err == nil {
		t.Fatal("FromBytes accepted ..")
	}
}

// publish stores a view's records in a RAM store.
func publish(t *testing.T, s *store.RAM, v *View) {
	t.Helper()
	for _, r := range v.Records() {
		putBytes(t, s, r.ID, r.Data)
	}
}

func putBytes(t *testing.T, s *store.RAM, id c4.ID, data []byte) {
	t.Helper()
	if s.Has(id) {
		return
	}
	w, err := s.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	w.Close()
}

// mutation is a random edit made on disk and described as an Edit.
func mutate(t *testing.T, rng *rand.Rand, dir string, step int) []Edit {
	t.Helper()
	var files, dirs []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			dirs = append(dirs, rel+"/")
		} else {
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	sort.Strings(dirs)
	write := func(rel, content string, mode os.FileMode) Edit {
		full := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.Remove(full)
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(full, mode)
		return Edit{Path: rel, Entry: FileEntry("", int64(len(content)), c4.Identify(strings.NewReader(content)), mode&0o111 != 0)}
	}
	switch k := rng.IntN(7); {
	case k == 0 && len(files) > 0: // modify
		f := files[rng.IntN(len(files))]
		return []Edit{write(f, fmt.Sprintf("modified %d", step), 0o644)}
	case k == 1 && len(files) > 0: // delete a file
		f := files[rng.IntN(len(files))]
		os.Remove(filepath.Join(dir, f))
		return []Edit{{Path: f}}
	case k == 2 && len(dirs) > 0: // delete a directory
		d := dirs[rng.IntN(len(dirs))]
		os.RemoveAll(filepath.Join(dir, d))
		return []Edit{{Path: d}}
	case k == 3 && len(files) > 0: // make executable
		f := files[rng.IntN(len(files))]
		full := filepath.Join(dir, f)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil
		}
		return []Edit{write(f, string(data), 0o755)}
	case k == 4 && len(dirs) > 0: // replace a directory's contents (opaque)
		d := dirs[rng.IntN(len(dirs))]
		os.RemoveAll(filepath.Join(dir, d))
		os.MkdirAll(filepath.Join(dir, d), 0o755)
		e := write(d+"fresh.txt", fmt.Sprintf("fresh %d", step), 0o644)
		return []Edit{{Path: d, Entry: &c4m.Entry{Name: lastName(d)}, Opaque: true}, e}
	case k == 5: // new empty directory
		d := fmt.Sprintf("new%d/", step)
		os.MkdirAll(filepath.Join(dir, d), 0o755)
		return []Edit{{Path: d, Entry: &c4m.Entry{Name: d}}}
	default: // add a file in a new or existing directory
		parent := ""
		if len(dirs) > 0 && rng.IntN(2) == 0 {
			parent = dirs[rng.IntN(len(dirs))]
		}
		return []Edit{write(fmt.Sprintf("%sadded-%d.exr", parent, step), fmt.Sprintf("frame %d", step), 0o644)}
	}
}

func TestApplyMatchesTheFilesystem(t *testing.T) {
	for seed := range 25 {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(seed), 7))
			dir := t.TempDir()
			writeTree(t, dir, show)
			base, err := ScanDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			ram := store.NewRAM()
			publish(t, ram, base)

			var edits []Edit
			for step := range 1 + rng.IntN(8) {
				edits = append(edits, mutate(t, rng, dir, step)...)
			}
			got, err := Apply(context.Background(), base.ID, edits, StoreRecords(ram))
			if err != nil {
				t.Fatal(err)
			}
			want, err := ScanDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != want.ID {
				t.Fatalf("Apply gives %s, the filesystem %s", got.ID, want.ID)
			}
			if got.ID == base.ID {
				return
			}
			// Only records new to the view are returned, and with them the
			// whole view loads from the store.
			for _, r := range got.Records {
				if c4.Identify(bytes.NewReader(r.Data)) != r.ID {
					t.Fatal("record does not hash to its ID")
				}
				putBytes(t, ram, r.ID, r.Data)
			}
			loaded, err := Load(context.Background(), got.ID, StoreRecords(ram), 4)
			if err != nil {
				t.Fatalf("load new view: %v", err)
			}
			if loaded.ID != want.ID {
				t.Fatal("loaded view differs")
			}
			// The changeset resolves to the same view through the store...
			putBytes(t, ram, got.ChangesetID, got.Changeset)
			r, err := FromID(got.ChangesetID, ram, Filter{})
			if err != nil {
				t.Fatalf("resolve changeset: %v\n%s", err, got.Changeset)
			}
			if r.View().ID != want.ID || len(r.Chain) != 2 || r.Chain[0].ID != base.ID {
				t.Fatalf("changeset resolves to %s (chain %d), want %s\n%s", r.View().ID, len(r.Chain), want.ID, got.Changeset)
			}
			// ...and through the c4 library's own patch semantics.
			pm, err := c4m.NewDecoder(bytes.NewReader(got.Changeset)).Decode()
			if err != nil {
				t.Fatal(err)
			}
			if pm.Base != base.ID {
				t.Fatal("changeset does not name its parent")
			}
			applied := c4m.ApplyPatch(base.Manifest(), pm)
			root, err := nodeFromManifest(applied)
			if err != nil {
				t.Fatal(err)
			}
			if id := root.Identify(); id != want.ID {
				t.Fatalf("c4m.ApplyPatch gives %s, want %s\n%s", id, want.ID, got.Changeset)
			}
		})
	}
}

func TestApplyNoChange(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, show)
	base, _ := ScanDir(dir)
	ram := store.NewRAM()
	publish(t, ram, base)
	same := Edit{Path: "weights/model.bin", Entry: FileEntry("", 7, c4.Identify(strings.NewReader("weights")), false)}
	got, err := Apply(context.Background(), base.ID, []Edit{same}, StoreRecords(ram))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != base.ID || got.Changeset != nil || got.Records != nil {
		t.Fatalf("a no-op edit produced a change: %+v", got)
	}
}

// overlayApply applies Diff's changes to a copy of the tree the way
// overlay layers stack: removals hide, opaque directories hide everything
// below, adds replace.
func overlayApply(t *testing.T, base *View, changes []Change) c4.ID {
	t.Helper()
	b, _ := base.Marshal()
	v, err := Parse(b, base.ID)
	if err != nil {
		t.Fatal(err)
	}
	root := v.Root
	for _, c := range changes {
		dir, name := SplitPath(c.Path)
		parent := root.Dir(dir)
		switch c.Op {
		case OpRemove:
			delete(parent.Files, strings.TrimSuffix(name, "/"))
			delete(parent.Dirs, strings.TrimSuffix(name, "/")+"/")
		case OpOpaque:
			parent.Dirs[name] = NewNode(name)
		case OpAdd:
			if c.Entry.IsDir() {
				delete(parent.Files, strings.TrimSuffix(name, "/"))
				if _, ok := parent.Dirs[name]; !ok {
					parent.Dirs[name] = NewNode(name)
				}
				continue
			}
			delete(parent.Dirs, name+"/")
			cp := *c.Entry
			cp.Name = name
			parent.Files[name] = &cp
		}
	}
	return root.Identify()
}

func TestDiffStacksToTheTarget(t *testing.T) {
	for seed := range 25 {
		rng := rand.New(rand.NewPCG(uint64(seed), 11))
		dir := t.TempDir()
		writeTree(t, dir, show)
		from, _ := ScanDir(dir)
		for step := range 1 + rng.IntN(8) {
			mutate(t, rng, dir, step)
		}
		to, _ := ScanDir(dir)
		ram := store.NewRAM()
		publish(t, ram, from)
		publish(t, ram, to)
		changes, st, err := Diff(context.Background(), from.ID, to.ID, StoreRecords(ram))
		if err != nil {
			t.Fatal(err)
		}
		if got := overlayApply(t, from, changes); got != to.ID {
			t.Fatalf("seed %d: stacking %d changes (%+v) gives %s, want %s", seed, len(changes), st, got, to.ID)
		}
	}
}
