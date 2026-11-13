package gitimport

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/store"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// oldPinImport is the view package's fixture written at the c4 pin before
// the move (internal/view/testdata/pin19c9), read as an import of three
// commits on one ref: link-depth2 over the empty view, then chain1 and
// chain2, each changeset made by view.Apply at that pin as the importer
// makes them.
func oldPinImport(t *testing.T) (*store.RAM, []Commit) {
	t.Helper()
	return fixtureImport(t, "pin19c9", "link-depth2", "chain1", "chain2")
}

// fixtureImport reads one of the view package's fixtures written at the
// old pin as an import of one ref, main, whose commits are the named
// changesets in order, each over the view the one before produced.
func fixtureImport(t *testing.T, fixture string, names ...string) (*store.RAM, []Commit) {
	t.Helper()
	dir := filepath.Join("..", "view", "testdata", fixture)
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
		if _, err := ram.Put(bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.txt"))
	if err != nil {
		t.Fatal(err)
	}
	views, changesets := map[string]c4.ID{}, map[string]c4.ID{}
	for _, l := range strings.Split(string(index), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || strings.HasPrefix(l, "#") {
			continue
		}
		id, err := c4.Parse(f[2])
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case f[0] == "view":
			views[f[1]] = id
		case len(f) == 4: // the removals fixture names each changeset's head
			changesets[f[1]] = id
			if views[f[1]], err = c4.Parse(f[3]); err != nil {
				t.Fatal(err)
			}
		default:
			changesets[f[1]] = id
		}
	}
	var m []Commit
	parent := view.EmptyID()
	for i, name := range names {
		m = append(m, Commit{
			Commit: strings.Repeat(string(rune('a'+i)), 40), Ref: "main",
			View: views[name], ParentView: parent, Changeset: changesets[name],
			ChainBase: view.EmptyID(), ChainSeq: i + 1,
		})
		parent = views[name]
	}
	return ram, m
}

// A mapping written before the pin moved records views by the IDs the
// library then gave them, and its chain does not verify under the library
// now. One renamed line appended, the mapping reads with every view as it
// is now: the chain is made again from the same stored changesets and
// verifies, a continued import starts from the new view, and the lines
// written before it stay in the file as they were.
func TestARenamedLineReadsAMappingWithItsViewsAsTheyAreNow(t *testing.T) {
	ctx := context.Background()
	ram, m := oldPinImport(t)
	src := view.StoreRecords(ram)
	dir := t.TempDir()
	p := filepath.Join(dir, "mapping.jsonl")
	w, err := AppendMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range m {
		if err := w.Write(c); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(dir, "main.c4m")
	read, err := ReadMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenRecordedChain(ctx, chain, src, read, "main"); err == nil {
		t.Fatal("the old mapping's chain verifies under this library, so this test shows nothing")
	}

	// The migration's work: each view re-identified, its records stored.
	table := view.Renames{}
	for _, c := range m {
		v, err := view.Reidentify(ctx, c.View, src, 4)
		if err != nil {
			t.Fatal(err)
		}
		recs, err := v.Records()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if !ram.Has(r.ID) {
				ram.Put(bytes.NewReader(r.Data))
			}
		}
		if v.ID != c.View {
			table[c.View] = v.ID
		}
	}
	if len(table) != 3 {
		t.Fatalf("%d of the mapping's views renamed, want 3", len(table))
	}
	// Every line's view is renamed, so each changeset is carried: the
	// line names the 3 views and the 3 changesets.
	line := migrated(t, read, table, ram)
	if len(line.Renamed) != 6 {
		t.Fatalf("the renamed line names %d pairs, want 3 views and 3 changesets", len(line.Renamed))
	}
	pairs, err := view.ParseRenames([]byte(strings.Join(line.Renamed, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	w, err = AppendMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(line)
	w.Close()

	now, err := ReadMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(now) != len(m)+1 {
		t.Fatalf("%d lines", len(now))
	}
	for i, c := range m {
		if now[i].View != table[c.View] || now[i].ParentView != table.Current(c.ParentView) || now[i].Changeset == c.Changeset || now[i].Changeset != pairs[c.Changeset] {
			t.Fatalf("line %d reads as %+v", i+1, now[i])
		}
	}
	if known := KnownViews(now); known[m[2].Commit] != table[m[2].View] {
		t.Fatalf("an import continues from %s, want %s", known[m[2].Commit], table[m[2].View])
	}
	if LastByRef(now)["main"] != m[2].Commit {
		t.Fatalf("main is at %q", LastByRef(now)["main"])
	}
	ch, repaired, err := OpenRecordedChain(ctx, chain, src, now, "main")
	if err != nil {
		t.Fatal(err)
	}
	if repaired == "" || ch.Head != table[m[2].View] || ch.Sections != 3 {
		t.Fatalf("the chain: %+v (%q)", ch, repaired)
	}
	// Made again, the file is the chain the mapping records: the next open
	// finds nothing to repair, each section the changeset the mapping
	// records for it.
	if ch, repaired, err := OpenRecordedChain(ctx, chain, src, now, "main"); err != nil || repaired != "" || ch.Head != table[m[2].View] {
		t.Fatalf("the chain opened again: %+v (%q, %v)", ch, repaired, err)
	}
	if again := Renamed(now, table, nil); len(again.Renamed) != 0 {
		t.Fatalf("a second run appends %v", again.Renamed)
	}
	after, _ := os.ReadFile(p)
	if !bytes.HasPrefix(after, before) {
		t.Fatal("the lines written before the renamed line changed")
	}
}

// A renamed line applies to the lines before it, never to a line after.
func TestARenamedLineRenamesOnlyTheLinesBeforeIt(t *testing.T) {
	a, b := c4.Identify(strings.NewReader("a")), c4.Identify(strings.NewReader("b"))
	var buf bytes.Buffer
	for _, text := range []string{
		`{"commit":"` + strings.Repeat("1", 40) + `","view":"` + a.String() + `","parentView":"` + a.String() + `"}`,
		`{"renamed":["` + a.String() + " " + b.String() + `"]}`,
		`{"commit":"` + strings.Repeat("2", 40) + `","view":"` + a.String() + `","parentView":"` + a.String() + `"}`,
	} {
		buf.WriteString(text + "\n")
	}
	m, err := DecodeMapping(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if m[0].View != b || m[0].ParentView != b || m[2].View != a {
		t.Fatalf("read as %s, %s; %s", m[0].View, m[0].ParentView, m[2].View)
	}
	bad := `{"renamed":["` + a.String() + " " + a.String() + `"]}`
	for _, end := range []string{"\n", ""} {
		if _, err := DecodeMapping(strings.NewReader(bad + end)); err == nil || !strings.Contains(err.Error(), "mapping line 1") {
			t.Fatalf("a renamed line that renames a view to itself (ending %q): %v", end, err)
		}
	}
}

// A changeset stored before 2026-11-09 ends with the view it produces, as
// the library then gave it. With a renamed line after it, the chain is
// still made again: the seal is checked against the view as the line was
// written, and the view produced against the line as renamed.
func TestAChangesetEndingWithItsViewIsMadeAgainAfterARenamedLine(t *testing.T) {
	ctx := context.Background()
	ram, m := oldPinImport(t)
	for i := range m {
		data, err := view.ReadObject(ram, m[i].Changeset)
		if err != nil {
			t.Fatal(err)
		}
		sealed := append(append([]byte(nil), data...), m[i].View.String()+"\n"...)
		id, err := ram.Put(bytes.NewReader(sealed))
		if err != nil {
			t.Fatal(err)
		}
		m[i].Changeset = id
	}
	src := view.StoreRecords(ram)
	dir := t.TempDir()
	p := filepath.Join(dir, "mapping.jsonl")
	w, err := AppendMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range m {
		w.Write(c)
	}
	w.Close()
	table := view.Renames{}
	for _, c := range m {
		v, err := view.Reidentify(ctx, c.View, src, 4)
		if err != nil {
			t.Fatal(err)
		}
		recs, err := v.Records()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if !ram.Has(r.ID) {
				ram.Put(bytes.NewReader(r.Data))
			}
		}
		if v.ID != c.View {
			table[c.View] = v.ID
		}
	}
	read, err := ReadMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	w, err = AppendMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(migrated(t, read, table, ram))
	w.Close()
	now, err := ReadMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(dir, "main.c4m")
	ch, _, err := OpenRecordedChain(ctx, chain, src, now, "main")
	if err != nil || ch.Head != table[m[2].View] {
		t.Fatalf("the chain of sealed changesets: %+v, %v", ch, err)
	}
	if ch, repaired, err := OpenRecordedChain(ctx, chain, src, now, "main"); err != nil || repaired != "" || ch.Head != table[m[2].View] {
		t.Fatalf("opened again: %+v (%q, %v)", ch, repaired, err)
	}
}

// A commit whose line a renamed line renamed cannot be made again: the
// changeset recorded was made by the library before the change, and this
// one makes another for the same step. The error says so, and what to do.
func TestARenamedCommitIsNotMadeAgainAndSaysWhy(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	r.write("d/a.txt", "a\n")
	r.commit("one")
	r.write("d/a.txt", "a2\n")
	r.commit("two")
	sink := newTreeSink(t)
	sum := r.importInto(t, sink, Options{Ref: "refs/heads/main", Records: view.StoreRecords(sink)})
	// The lines as an earlier library would have written them: other IDs
	// for the same views, and a changeset this library never made.
	table := view.Renames{}
	old := func(id c4.ID) c4.ID {
		for o, n := range table {
			if n == id {
				return o
			}
		}
		o := c4.Identify(strings.NewReader("as the library before gave " + id.String()))
		table[o] = id
		return o
	}
	var lines []Commit
	for _, c := range sum.Commits {
		c.View, c.ParentView = old(c.View), old(c.ParentView)
		lines = append(lines, c)
	}
	lines[1].Changeset = c4.Identify(strings.NewReader("a changeset the library before made"))
	p := filepath.Join(t.TempDir(), "mapping.jsonl")
	w, err := AppendMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range lines {
		w.Write(c)
	}
	w.Write(Renamed(lines, table, nil)) // synthetic old IDs, with no records to carry from
	w.Close()
	m, err := ReadMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	err = Remake(ctx, r.remakeOptions(t, sink, m), m[1])
	for _, want := range []string{"renamed from", "Cut the mapping back", "run migrate-pin again"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("remaking a renamed commit: %v\nwant %q in it", err, want)
		}
	}
}

// A renamed commit whose step passes through no directory the change
// renamed is made again: its changeset is the same patch, and with its
// first line naming the parent as written it is the changeset recorded.
func TestARenamedCommitWhosePatchIsTheSameIsMadeAgain(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	r.write("d/a.txt", "a\n")
	r.commit("one")
	r.write("top.txt", "top\n")
	r.commit("two")
	sink := newTreeSink(t)
	sum := r.importInto(t, sink, Options{Ref: "refs/heads/main", Records: view.StoreRecords(sink)})
	table := view.Renames{}
	old := func(id c4.ID) c4.ID {
		for o, n := range table {
			if n == id {
				return o
			}
		}
		o := c4.Identify(strings.NewReader("as the library before gave " + id.String()))
		table[o] = id
		return o
	}
	var lines []Commit
	for _, c := range sum.Commits {
		c.View, c.ParentView = old(c.View), old(c.ParentView)
		lines = append(lines, c)
	}
	// The changeset as the library before stored it: this patch, over the
	// parent as it was named then. It is not in the store.
	made, err := view.ReadObject(sink, sum.Commits[1].Changeset)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := asWrittenOver(made, lines[1].ParentView)
	if !ok {
		t.Fatal("the changeset has no first line")
	}
	lines[1].Changeset = c4.Identify(bytes.NewReader(stored))
	if sink.TS.Has(lines[1].Changeset) {
		t.Fatal("the store holds the changeset as stored before, so this test shows nothing")
	}
	p := filepath.Join(t.TempDir(), "mapping.jsonl")
	w, err := AppendMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range lines {
		w.Write(c)
	}
	w.Write(Renamed(lines, table, nil)) // synthetic old IDs, with no records to carry from
	w.Close()
	m, err := ReadMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Remake(ctx, r.remakeOptions(t, sink, m), m[1]); err != nil {
		t.Fatal(err)
	}
	if got, err := view.ReadObject(sink, lines[1].Changeset); err != nil || !bytes.Equal(got, stored) {
		t.Fatalf("the changeset made again: %v", err)
	}
}

// A commit over a renamed parent whose own view the change left alone (it
// removed what the change renamed, say) is made again the same way.
func TestACommitOverARenamedParentIsMadeAgain(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	r.write("d/a.txt", "a\n")
	r.commit("one")
	r.write("top.txt", "top\n")
	r.commit("two")
	sink := newTreeSink(t)
	sum := r.importInto(t, sink, Options{Ref: "refs/heads/main", Records: view.StoreRecords(sink)})
	parent := sum.Commits[1].ParentView
	old := c4.Identify(strings.NewReader("as the library before gave " + parent.String()))
	table := view.Renames{old: parent}
	lines := append([]Commit(nil), sum.Commits...)
	lines[0].View, lines[1].ParentView = old, old
	made, err := view.ReadObject(sink, sum.Commits[1].Changeset)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := asWrittenOver(made, old)
	if !ok {
		t.Fatal("the changeset has no first line")
	}
	lines[1].Changeset = c4.Identify(bytes.NewReader(stored))
	p := filepath.Join(t.TempDir(), "mapping.jsonl")
	w, err := AppendMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range lines {
		w.Write(c)
	}
	w.Write(Renamed(lines, table, nil)) // synthetic old IDs, with no records to carry from
	w.Close()
	m, err := ReadMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	if m[1].View != sum.Commits[1].View || m[1].ParentView != parent {
		t.Fatalf("line 2 reads as %+v", m[1])
	}
	if err := Remake(ctx, r.remakeOptions(t, sink, m), m[1]); err != nil {
		t.Fatal(err)
	}
	if got, err := view.ReadObject(sink, lines[1].Changeset); err != nil || !bytes.Equal(got, stored) {
		t.Fatalf("the changeset made again: %v", err)
	}
}

// A ref whose history removes a directory c4 #107 renamed (one holding a
// null-sized frame or a link two directories down), migrated, is served
// as a chain any reader resolves to the ref's head: the chain the import
// service serves (RecordedChainBytes, then ExportChain), read by the c4m
// library alone, holds the head's tree. Its stored changeset restates the
// directory with the size the old library summed, and the view as it is
// now records that size null: as text over the parent's new ID it reads,
// to any reader that compares every field as the spec says, as a write of
// the directory, and nothing is removed. So the migration carries it
// (Carry): each entry read against the stored base, a new changeset over
// the new parent, named in the renamed line.
func TestARefThatRemovedARenamedDirectoryIsServedAsItsReadersReadIt(t *testing.T) {
	for _, removal := range []string{"rm-dir-deep-null", "rm-dir-deep-link", "rm-nested-dir-deep-null", "rm-nested-dir-deep-link",
		"rm-top-link", "rm-deep-link", "rm-dir-of-link", "rm-dir-plain", "rm-file", "retarget-link", "add-file-deep"} {
		t.Run(removal, func(t *testing.T) {
			ctx := context.Background()
			ram, m := fixtureImport(t, "pin19c9-removals", "V", removal)
			src := view.StoreRecords(ram)
			table := view.Renames{}
			for _, c := range m {
				v, err := view.Reidentify(ctx, c.View, src, 4)
				if err != nil {
					t.Fatal(err)
				}
				recs, err := v.Records()
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range recs {
					if !ram.Has(r.ID) {
						ram.Put(bytes.NewReader(r.Data))
					}
				}
				if v.ID != c.View {
					table[c.View] = v.ID
				}
			}
			if len(table) != 2 {
				t.Fatalf("%d of the mapping's views renamed, want 2", len(table))
			}
			p := filepath.Join(t.TempDir(), "mapping.jsonl")
			w, err := AppendMapping(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range m {
				w.Write(c)
			}
			w.Write(migrated(t, m, table, ram))
			w.Close()
			now, err := ReadMapping(p)
			if err != nil {
				t.Fatal(err)
			}
			head := table[m[1].View]
			chain := filepath.Join(filepath.Dir(p), "main.c4m")
			served, err := RecordedChainBytes(ctx, chain, src, now, "main")
			if err != nil {
				t.Fatal(err)
			}
			full, err := view.ExportChain(ctx, served, src)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c4m.NewDecoder(bytes.NewReader(full)).Decode()
			if err != nil {
				t.Fatal(err)
			}
			want, err := view.Load(ctx, head, src, 4)
			if err != nil {
				t.Fatal(err)
			}
			if g, w := manifestEntries(got), manifestEntries(want.Manifest()); g != w {
				t.Fatalf("the served chain, read by the c4m library, holds\n  %s\nand the head holds\n  %s", g, w)
			}
			ch, _, err := OpenRecordedChain(ctx, chain, src, now, "main")
			if err != nil {
				t.Fatal(err)
			}
			if ch.Head != head || ch.Sections != 2 {
				t.Fatalf("the chain: %+v, want head %s", ch, head)
			}
		})
	}
}

// manifestEntries is every entry a c4m manifest holds, in its order: its
// path, size, C4 ID, mode and link target.
func manifestEntries(m *c4m.Manifest) string {
	var out, stack []string
	for _, e := range m.Entries {
		stack = stack[:e.Depth]
		out = append(out, fmt.Sprintf("%s %d %s %v %q", strings.Join(stack, "")+e.Name, e.Size, e.C4ID, e.Mode, e.Target))
		if e.IsDir() {
			stack = append(stack, e.Name)
		}
	}
	return strings.Join(out, "\n  ")
}

// A mapping renamed by a build that carried no changesets (its renamed
// line names views only) is carried by the next run: its lines read with
// their views renamed and their changesets over the old parents, which
// Carry finds by what each changeset was written over, and once the line
// it makes is appended, the served chain reads as the head.
func TestAMappingRenamedBeforeCarryingIsCarried(t *testing.T) {
	ctx := context.Background()
	ram, m := fixtureImport(t, "pin19c9-removals", "V", "rm-dir-deep-null")
	src := view.StoreRecords(ram)
	table := view.Renames{}
	for _, c := range m {
		v, err := view.Reidentify(ctx, c.View, src, 4)
		if err != nil {
			t.Fatal(err)
		}
		recs, _ := v.Records()
		for _, r := range recs {
			ram.Put(bytes.NewReader(r.Data))
		}
		table[c.View] = v.ID
	}
	p := filepath.Join(t.TempDir(), "mapping.jsonl")
	w, err := AppendMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range m {
		w.Write(c)
	}
	w.Write(Renamed(m, table, nil)) // as a build before carrying wrote it
	w.Close()
	now, err := ReadMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := Uncarried(now, table, nil); len(got) != 2 {
		t.Fatalf("lines needing a carry: %v, want both commits", got)
	}
	cr, err := Carry(ctx, now, table, src)
	if err != nil || cr.Lines != 2 || len(cr.Changesets) != 2 {
		t.Fatalf("carried %+v, %v", cr, err)
	}
	if got := Uncarried(now, table, cr.Changesets); len(got) != 0 {
		t.Fatalf("lines left uncarried: %v", got)
	}
	for _, data := range cr.Objects {
		ram.Put(bytes.NewReader(data))
	}
	line := Renamed(now, table, cr.Changesets)
	if len(line.Renamed) != 2 {
		t.Fatalf("the line names %d pairs, want the 2 changesets", len(line.Renamed))
	}
	w, err = AppendMapping(p)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(line)
	w.Close()
	if now, err = ReadMapping(p); err != nil {
		t.Fatal(err)
	}
	if again, _ := Carry(ctx, now, table, src); again.Lines != 0 {
		t.Fatalf("a run after it carries %d lines again", again.Lines)
	}
	served, err := RecordedChainBytes(ctx, filepath.Join(filepath.Dir(p), "main.c4m"), src, now, "main")
	if err != nil {
		t.Fatal(err)
	}
	full, err := view.ExportChain(ctx, served, src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c4m.NewDecoder(bytes.NewReader(full)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	want, err := view.Load(ctx, table[m[1].View], src, 4)
	if err != nil {
		t.Fatal(err)
	}
	if g, w := manifestEntries(got), manifestEntries(want.Manifest()); g != w {
		t.Fatalf("the served chain holds\n  %s\nand the head\n  %s", g, w)
	}
}

// migrated is the renamed line the migration appends to a mapping m for
// the renames table: each changeset over a renamed parent carried to its
// new ID, stored in ram, and the pairs for the views and the changesets.
func migrated(t *testing.T, m []Commit, table view.Renames, ram *store.RAM) Commit {
	t.Helper()
	cr, err := Carry(context.Background(), m, table, view.StoreRecords(ram))
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range cr.Objects {
		if _, err := ram.Put(bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	return Renamed(m, table, cr.Changesets)
}
