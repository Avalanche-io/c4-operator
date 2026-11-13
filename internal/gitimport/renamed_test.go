package gitimport

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
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
	dir := filepath.Join("..", "view", "testdata", "pin19c9")
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
		if f[0] == "view" {
			views[f[1]] = id
		} else {
			changesets[f[1]] = id
		}
	}
	var m []Commit
	parent := view.EmptyID()
	for i, name := range []string{"link-depth2", "chain1", "chain2"} {
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
	line := Renamed(read, table)
	if len(line.Renamed) != 3 {
		t.Fatalf("the renamed line names %d views", len(line.Renamed))
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
		if now[i].View != table[c.View] || now[i].ParentView != table.Current(c.ParentView) || now[i].Changeset != c.Changeset {
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
	if again := Renamed(now, table); len(again.Renamed) != 0 {
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
	w.Write(Renamed(read, table))
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
	w.Write(Renamed(lines, table))
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
	w.Write(Renamed(lines, table))
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
	w.Write(Renamed(lines, table))
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
