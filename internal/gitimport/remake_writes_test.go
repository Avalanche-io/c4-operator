package gitimport

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/view"
)

// A first attempt at a commit whose writes are still in flight when its
// parent is remade (the shape these tests share, after the independent
// reader's probes): three commits; the third's changeset, its parent's
// view, that view's subdirectory record and the third commit's new blob are
// gone from the store.
func lostWithWrites(t *testing.T) (*repo, *TreeSink, *Summary, Commit, c4.ID) {
	t.Helper()
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
	var sub c4.ID
	for _, d := range parent.Root.Dirs {
		sub = d.Entry.C4ID
	}
	b3 := c4.Identify(strings.NewReader("b3\n"))
	for _, id := range []c4.ID{line.Changeset, line.ParentView, sub, b3} {
		if err := sink.TS.Remove(id); err != nil {
			t.Fatalf("remove %s: %v", id, err)
		}
	}
	return r, sink, sum, line, b3
}

// failingPut fails the Put of one object, every time or the first time.
type failingPut struct {
	*TreeSink
	bad   c4.ID
	once  bool
	tries int
}

func (s *failingPut) Put(ctx context.Context, id c4.ID, size int64, r io.Reader) (bool, error) {
	if id == s.bad {
		s.tries++
		if !s.once || s.tries == 1 {
			return false, errors.New("the store is full")
		}
	}
	return s.TreeSink.Put(ctx, id, size, r)
}

// The parent, imported again in full, counts its own objects only, not the
// first attempt's uploads still finishing.
func TestRemadeParentCountsItsOwnObjects(t *testing.T) {
	r, sink, sum, line, _ := lostWithWrites(t)
	opts := r.remakeOptions(t, sink, sum.Commits)
	opts.Progress = nil
	if err := opts.defaults(); err != nil {
		t.Fatal(err)
	}
	s := &Summary{}
	im, err := newImporter(context.Background(), opts, s)
	if err != nil {
		t.Fatal(err)
	}
	defer im.close()
	c := commit{sha: line.Commit, parent: line.Parent}
	if _, err := im.importDiffs([]commit{c}, line.ParentView); err == nil || !lostFromStore(err) {
		t.Fatalf("first attempt: %v", err)
	}
	if err := im.remakeParent(line); err != nil {
		t.Fatal(err)
	}
	if _, err := im.importDiffs([]commit{c}, line.ParentView); err != nil {
		t.Fatal(err)
	}
	if p := s.Commits[0]; p.ObjectsUploaded != 0 || p.ObjectsPresent != 2 {
		t.Errorf("the remade parent counted %d uploaded and %d present; it has 0 and 2", p.ObjectsUploaded, p.ObjectsPresent)
	}
}

// An upload of the commit's own that fails in the first attempt is the
// commit's error, not its parent's, and Remake never reports success with
// a blob its view names missing from the store.
func TestRemakeKeepsTheCommitsOwnFailedWrite(t *testing.T) {
	for _, once := range []bool{false, true} {
		r, sink, sum, line, b3 := lostWithWrites(t)
		opts := r.remakeOptions(t, sink, sum.Commits)
		fp := &failingPut{TreeSink: sink, bad: b3, once: once}
		opts.Sink = fp
		err := Remake(context.Background(), opts, line)
		var pn *ParentNotRemade
		if errors.As(err, &pn) {
			t.Errorf("once=%v: a failed write of the commit's own blamed on its parent: %v", once, err)
		}
		if err == nil && !sink.TS.Has(b3) {
			t.Errorf("once=%v: Remake reported success and the view's blob %s is not in the store", once, b3)
		}
		if err == nil {
			t.Errorf("once=%v: Remake succeeded although its first attempt's write failed", once)
		} else if !strings.Contains(err.Error(), "the store is full") {
			t.Errorf("once=%v: Remake: %v; want the write's own error", once, err)
		}
	}
}
