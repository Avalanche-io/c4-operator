package gitimport

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// remakeOptions are the options a service makes a lost changeset again
// with: its store, the c4git store beside the repository, and nothing
// reported.
func (r *repo) remakeOptions(t *testing.T, sink *TreeSink, m []Commit) Options {
	t.Helper()
	src, err := OpenTreeSource(r.c4git)
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Repo: r.dir, Sink: sink, Records: view.StoreRecords(sink), C4Git: src,
		Known: KnownViews(m), KnownFiles: KnownFiles(m),
		Progress: func(c Commit) { t.Errorf("commit %s imported again was reported as imported", c.Commit) },
	}
}

// The records of the view a lost changeset applies to are lost as well:
// the parent is imported again in full first, which puts every record of
// its view back, and then the commit. Both come back as they were.
func TestRemakeWhenTheParentsRecordsAreLostToo(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	r.write("d/a.txt", "a\n")
	r.write("d/e/b.txt", "b\n")
	r.commit("one")
	r.write("d/a.txt", "a2\n")
	r.commit("two")
	r.write("d/e/b.txt", "b3\n")
	r.commit("three")
	sink := newTreeSink(t)
	sum := r.importInto(t, sink, Options{Ref: "refs/heads/main", Records: view.StoreRecords(sink)})
	line := sum.Commits[2]
	parent, err := view.Load(ctx, line.ParentView, view.StoreRecords(sink), 0)
	if err != nil {
		t.Fatal(err)
	}
	// The changeset, the root record of the view it applies to and the
	// record of the subdirectory it changes.
	var sub c4.ID
	for _, d := range parent.Root.Dirs {
		sub = d.Entry.C4ID
	}
	if len(parent.Root.Dirs) != 1 || sub.IsNil() {
		t.Fatalf("the parent view has %d subdirectories", len(parent.Root.Dirs))
	}
	lost := []c4.ID{line.Changeset, line.ParentView, sub}
	for _, id := range lost {
		if err := sink.TS.Remove(id); err != nil {
			t.Fatalf("remove %s: %v", id, err)
		}
	}
	if err := Remake(ctx, r.remakeOptions(t, sink, sum.Commits), line); err != nil {
		t.Fatal(err)
	}
	for _, id := range lost {
		if !sink.TS.Has(id) {
			t.Errorf("%s is not back in the store", id)
		}
	}
	if _, err := view.Load(ctx, line.View, view.StoreRecords(sink), 0); err != nil {
		t.Fatalf("the commit's view does not load: %v", err)
	}
}

// When the parent cannot be put back as the line records it (here a c4git
// object of its view is gone from both stores, so in full it comes to
// another view), the error says so, names the parent, and says why a
// c4git file can give another view.
func TestRemakeWhenTheParentCannotBePutBack(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	r.write(".gitattributes", "*.exr filter=c4\n")
	r.write("a.txt", "a\n")
	r.commit("one")
	content := strings.Repeat("frame ", 500)
	r.write("shot.exr", content)
	r.commit("two")
	r.write("a.txt", "a3\n")
	r.commit("three")
	sink := newTreeSink(t)
	sum := r.importInto(t, sink, Options{Ref: "refs/heads/main", Records: view.StoreRecords(sink)})
	if sum.Commits[1].C4GitObjects != 1 {
		t.Fatalf("the second commit has no c4git file: %+v", sum.Commits[1])
	}
	line := sum.Commits[2]
	obj := c4.Identify(strings.NewReader(content))
	for _, id := range []c4.ID{line.Changeset, line.ParentView, obj} {
		if err := sink.TS.Remove(id); err != nil {
			t.Fatal(err)
		}
	}
	src, _ := OpenTreeSource(r.c4git)
	if err := src.TS.Remove(obj); err != nil {
		t.Fatal(err)
	}
	opts := r.remakeOptions(t, sink, sum.Commits)
	opts.Warn = func(string) {}
	err := Remake(ctx, opts, line)
	var pn *ParentNotRemade
	if !errors.As(err, &pn) || pn.Parent != sum.Commits[1].Commit {
		t.Fatalf("Remake: %v; want a *ParentNotRemade naming %s", err, sum.Commits[1].Commit)
	}
	for _, want := range []string{"lost from the store as well", "it comes to the view", "c4git"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Remake: %v\nwant %q in it", err, want)
		}
	}
	if sink.TS.Has(line.Changeset) {
		t.Error("a changeset was stored for a commit that could not be imported again")
	}
}

// A commit with a c4git file whose object has arrived since it was
// imported (it was recorded with a null size) comes to another view. That
// is refused, and the error says why, so it is not taken for a wrong line.
func TestRemakeOfAC4GitCommitThatComesToAnotherView(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	r.write(".gitattributes", "*.exr filter=c4\n")
	content := strings.Repeat("frame ", 500)
	r.write("shot.exr", content)
	r.commit("one")
	obj := c4.Identify(strings.NewReader(content))
	src, _ := OpenTreeSource(r.c4git)
	rc, err := src.Open(obj)
	if err != nil {
		t.Fatal(err)
	}
	kept := make([]byte, len(content)+1)
	n, _ := rc.Read(kept)
	rc.Close()
	kept = kept[:n]
	if err := src.TS.Remove(obj); err != nil {
		t.Fatal(err)
	}
	sink := newTreeSink(t)
	sum := r.importInto(t, sink, Options{Ref: "refs/heads/main", Records: view.StoreRecords(sink), Warn: func(string) {}})
	line := sum.Commits[0]
	if len(line.C4GitMissing) != 1 {
		t.Fatalf("the object was not missing at the import: %+v", line)
	}
	if id, err := src.TS.Put(strings.NewReader(string(kept))); err != nil || id != obj {
		t.Fatalf("put the object back: %s, %v", id, err)
	}
	if err := sink.TS.Remove(line.Changeset); err != nil {
		t.Fatal(err)
	}
	err = Remake(ctx, r.remakeOptions(t, sink, sum.Commits), line)
	if err == nil || !strings.Contains(err.Error(), "and the mapping records the view") || !strings.Contains(err.Error(), "has arrived or been lost since") {
		t.Fatalf("Remake: %v", err)
	}
}

// A line is compared with what the commit comes to whichever way it was
// imported: a commit imported in full as well as one imported as a diff.
// And a line in the form written before 2026-11-09 is said to be in that
// form only when the commit comes to the view it records; otherwise it is
// a line that is not a record of its commit, like any other.
func TestRemakeComparesEveryKindOfLine(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	r.write("a.txt", "a\n")
	r.commit("one")
	r.write("a.txt", "a2\n")
	r.commit("two")
	sink := newTreeSink(t)
	sum := r.importInto(t, sink, Options{Ref: "refs/heads/main", Records: view.StoreRecords(sink)})
	opts := r.remakeOptions(t, sink, sum.Commits)
	if !sum.Commits[0].Base {
		t.Fatal("the first commit was not imported in full")
	}
	for i, line := range sum.Commits {
		if err := Remake(ctx, opts, line); err != nil {
			t.Fatalf("line %d as it was: %v", i+1, err)
		}
		other := line
		other.View = sum.Commits[1-i].View
		if err := Remake(ctx, opts, other); err == nil || !strings.Contains(err.Error(), "and the mapping records the view") {
			t.Fatalf("line %d (imported in full: %v) with another view: %v", i+1, line.Base, err)
		}
	}
	data, err := view.StoreRecords(sink).Record(ctx, sum.Commits[1].Changeset)
	if err != nil {
		t.Fatal(err)
	}
	old := sum.Commits[1]
	old.Changeset = c4.Identify(strings.NewReader(string(data) + old.View.String() + "\n"))
	if err := Remake(ctx, opts, old); err == nil || !strings.Contains(err.Error(), "before 2026-11-09") {
		t.Fatalf("a line in the earlier form: %v", err)
	}
	old.View = sum.Commits[0].View
	if err := Remake(ctx, opts, old); err == nil || strings.Contains(err.Error(), "before 2026-11-09") || !strings.Contains(err.Error(), "and the mapping records the view") {
		t.Fatalf("a line in the earlier form with another view: %v", err)
	}
	// A line whose view is not the commit's, with a changeset in the
	// earlier form over that same wrong view, is not said to be in the
	// earlier form: the commit does not come to the view it records.
	wrongView := sum.Commits[1]
	wrongView.View = sum.Commits[0].View
	wrongView.Changeset = c4.Identify(strings.NewReader(string(data) + wrongView.View.String() + "\n"))
	if err := Remake(ctx, opts, wrongView); err == nil || strings.Contains(err.Error(), "before 2026-11-09") || !strings.Contains(err.Error(), "and the mapping records the view") {
		t.Fatalf("a line in the earlier form over a view its commit does not come to: %v", err)
	}
	// The earlier form had its view's C4 ID on a line of its own; the
	// same text without the newline is not that form.
	old = sum.Commits[1]
	old.Changeset = c4.Identify(strings.NewReader(string(data) + old.View.String()))
	if err := Remake(ctx, opts, old); err == nil || strings.Contains(err.Error(), "before 2026-11-09") {
		t.Fatalf("the earlier form without its last newline: %v", err)
	}
}

// A ref stopped before the round-trip guard: a changeset the c4m library
// wrote and does not read, stored, with its mapping line written. The
// chain cannot be made from it, and the error says so and what gets the
// ref out: a c4m library that reads it, with no edit, or the cut.
func TestAChangesetStoredBeforeTheGuardThatDoesNotRead(t *testing.T) {
	ctx := context.Background()
	h := newHistory(t, 1)
	link := c4m.NewManifest()
	link.Entries = []*c4m.Entry{view.SymlinkEntry("link", c4.Identify(strings.NewReader("x")).String())}
	poisoned, err := view.EncodeChangeset(h.views[0], link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.ReadChain(poisoned); err == nil {
		t.Skip("this c4m library reads the changeset back: nothing is stopped")
	}
	h.put(t, poisoned)
	m := []Commit{{Commit: "c1", Ref: "main", View: h.views[1], ParentView: h.views[0],
		Changeset: c4.Identify(strings.NewReader(string(poisoned))), ChainBase: h.views[0], ChainSeq: 1}}
	p := filepath.Join(t.TempDir(), "main.c4m")
	_, _, err = OpenRecordedChain(ctx, p, h.src, m, "main")
	for _, want := range []string{"(mapping line 1) is the object its C4 ID names and does not read with this c4m library", "a c4m library that reads it makes the chain again with no edit", "cut the mapping back to before line 1 and"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%v\nwant %q in it", err, want)
		}
	}
	if _, err := RecordedChainBytes(ctx, p, h.src, m, "main"); err == nil {
		t.Fatal("a reader was served a chain")
	}
}
