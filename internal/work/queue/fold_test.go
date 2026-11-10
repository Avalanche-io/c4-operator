package queue

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

func foldStore(t *testing.T) (*storeurl.Sharded, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: dir}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	return st, dir
}

func readChain(t *testing.T, st *storeurl.Sharded, id c4.ID) ([]byte, *c4view.Chain) {
	t.Helper()
	data, err := c4view.ReadObject(st, id)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := c4view.ReadChain(data)
	if err != nil {
		t.Fatal(err)
	}
	return data, ch
}

// oldChain writes a chain as the queue stored it before 2026-11-09: the
// base, then each section's entries followed by the view they produce.
func oldChain(t *testing.T, ch *c4view.Chain, views []c4.ID) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString(ch.Base.String() + "\n")
	for i, sec := range ch.Sections {
		if err := c4m.NewEncoder(&out).Encode(sec.Patch); err != nil {
			t.Fatal(err)
		}
		out.WriteString(views[i].String() + "\n")
	}
	return out.Bytes()
}

// A job's chain is its folds as changesets, one after another: it never
// ends with a bare C4 ID, it verifies to the output after each fold, and
// the output after the last is kept beside it.
func TestFoldChainIsChangesetsJoined(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	o := newOutput()
	var outs []c4.ID
	for i, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := q.fold(ctx, st, o, i, writeChangeset(t, st, name, name), ""); err != nil {
			t.Fatal(err)
		}
		outs = append(outs, o.view)
		data, ch := readChain(t, st, o.id)
		if !bytes.Equal(data, o.chain) || !ch.Seal.IsNil() || len(ch.Sections) != i+1 || ch.Base != c4view.EmptyID() {
			t.Fatalf("after %d folds the stored chain is not the folds joined\n%s", i+1, data)
		}
		got, err := ch.Verify(ctx, c4view.StoreRecords(st))
		if err != nil || len(got) != i+1 || got[i] != o.view {
			t.Fatalf("after %d folds the chain verifies to %v: %v", i+1, got, err)
		}
		for k, v := range got {
			if v != outs[k] {
				t.Fatalf("view %d of the chain is not the output after fold %d", k, k+1)
			}
		}
	}
	if len(outputNames(t, st, o.view)) != 3 {
		t.Fatal("the output does not hold the three files")
	}

	// A restart: the chain and, beside it, the view from the job's status.
	again := newOutput()
	if err := again.load(st, o.id.String(), o.view.String()); err != nil {
		t.Fatal(err)
	}
	if again.view != o.view || !bytes.Equal(again.chain, o.chain) {
		t.Fatal("a loaded output is not the output that was stored")
	}
	if err := q.fold(ctx, st, again, 3, writeChangeset(t, st, "d.txt", "d"), ""); err != nil {
		t.Fatal(err)
	}
	_, ch := readChain(t, st, again.id)
	if got, err := ch.Verify(ctx, c4view.StoreRecords(st)); err != nil || len(got) != 4 || got[3] != again.view {
		t.Fatalf("after a restart and a fold the chain verifies to %d views: %v", len(got), err)
	}
}

func outputNames(t *testing.T, st *storeurl.Sharded, id c4.ID) []string {
	t.Helper()
	v, err := c4view.Load(context.Background(), id, c4view.StoreRecords(st), 1)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for name := range v.Root.Files {
		out = append(out, name)
	}
	return out
}

// Changesets and chains stored before 2026-11-09 end with a bare C4 ID.
// A unit's old changeset still folds, and an output restored from an old
// chain carries on in the current form.
func TestFoldReadsTheOldForm(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	// The same two folds, in the current form, for comparison.
	want := newOutput()
	var csIDs []string
	var outs []c4.ID
	for i, name := range []string{"a.txt", "b.txt"} {
		csIDs = append(csIDs, writeChangeset(t, st, name, name))
		if err := q.fold(ctx, st, want, i, csIDs[i], ""); err != nil {
			t.Fatal(err)
		}
		outs = append(outs, want.view)
	}
	_, wantChain := readChain(t, st, want.id)

	// The units' changesets as they were stored before: each with the view
	// it produces after it. (Every changeset here is over the empty view.)
	o := newOutput()
	for i, cs := range csIDs {
		id, _ := c4.Parse(cs)
		_, ch := readChain(t, st, id)
		res, err := c4view.ApplyPatch(ctx, ch.Base, ch.Sections[0].Patch, c4view.StoreRecords(st))
		if err != nil {
			t.Fatal(err)
		}
		oldID, err := st.PutBytes(ctx, oldChain(t, ch, []c4.ID{res.ID}))
		if err != nil {
			t.Fatal(err)
		}
		if err := q.fold(ctx, st, o, i, oldID.String(), ""); err != nil {
			t.Fatalf("fold an old changeset: %v", err)
		}
	}
	if o.view != want.view || !bytes.Equal(o.chain, want.chain) {
		t.Fatal("old changesets do not fold to the same output and chain")
	}

	// A job's chain as it was stored before, restored after a restart.
	old := oldChain(t, wantChain, outs)
	oldID, err := st.PutBytes(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	re := newOutput()
	if err := re.load(st, oldID.String(), want.view.String()); err != nil {
		t.Fatal(err)
	}
	if re.view != want.view || !bytes.Equal(re.chain, want.chain) {
		t.Fatal("an old chain does not load as the chain in the current form")
	}
	if err := q.fold(ctx, st, re, 2, writeChangeset(t, st, "c.txt", "c"), ""); err != nil {
		t.Fatal(err)
	}
	data, ch := readChain(t, st, re.id)
	if !ch.Seal.IsNil() || !bytes.Equal(data, re.chain) {
		t.Fatalf("the chain stored after an old one was loaded ends with a bare ID\n%s", data)
	}
	if got, err := ch.Verify(ctx, c4view.StoreRecords(st)); err != nil || len(got) != 3 || got[2] != re.view {
		t.Fatalf("it verifies to %d views: %v", len(got), err)
	}
	// An old chain that ends with another view than the status names is
	// refused.
	if err := newOutput().load(st, oldID.String(), outs[0].String()); err == nil {
		t.Fatal("loaded an old chain whose last line is not the job's output")
	}
}

// A fold that fails while storing leaves the output as it was, so trying
// it again does not put its section in the chain twice.
func TestFoldRetriedLeavesOneSection(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	ctx := context.Background()
	st, dir := foldStore(t)
	q := &Queue{}
	o := newOutput()
	if err := q.fold(ctx, st, o, 0, writeChangeset(t, st, "a.txt", "a"), ""); err != nil {
		t.Fatal(err)
	}
	cs := writeChangeset(t, st, "b.txt", "b")
	// The records the fold will write are stored first, so the one thing
	// left to store is the chain; then the store is made read-only.
	id, _ := c4.Parse(cs)
	_, ch := readChain(t, st, id)
	res, err := c4view.ApplyPatch(ctx, o.view, ch.Sections[0].Patch, c4view.StoreRecords(st))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Records {
		if _, err := st.PutBytes(ctx, r.Data); err != nil {
			t.Fatal(err)
		}
	}
	before, beforeView, beforeID := append([]byte(nil), o.chain...), o.view, o.id
	var dirs []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	chmod := func(mode os.FileMode) {
		for _, d := range dirs {
			os.Chmod(d, mode)
		}
	}
	chmod(0o555)
	err = q.fold(ctx, st, o, 1, cs, "")
	chmod(0o755)
	if err == nil {
		t.Fatal("the fold stored its chain in a read-only store")
	}
	if !bytes.Equal(o.chain, before) || o.view != beforeView || o.id != beforeID {
		t.Fatal("a failed fold changed the output")
	}
	if err := q.fold(ctx, st, o, 1, cs, ""); err != nil {
		t.Fatal(err)
	}
	_, stored := readChain(t, st, o.id)
	got, err := stored.Verify(ctx, c4view.StoreRecords(st))
	if err != nil || len(stored.Sections) != 2 || got[1] != o.view || o.view != res.ID {
		t.Fatalf("after a retry the chain has %d sections and verifies: %v", len(stored.Sections), err)
	}
}

// A unit that turns a directory into a file: the job's output changes. The
// changeset states the file, then the directory restated, and the fold
// once applied that to nothing.
func TestFoldADirectoryBecomesAFile(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	src := c4view.StoreRecords(st)
	put := func(data []byte) c4.ID {
		id, err := st.PutBytes(ctx, data)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	put(nil)
	apply := func(parent c4.ID, edits ...c4view.Edit) (c4.ID, string) {
		c4view.SortEdits(edits)
		a, err := c4view.Apply(ctx, parent, edits, src)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range a.Records {
			put(r.Data)
		}
		return a.ID, put(a.Changeset).String()
	}
	in, first := apply(c4view.EmptyID(), c4view.Edit{Path: "lib/a.txt", Entry: c4view.FileEntry("", 1, put([]byte("a")), false)})
	want, second := apply(in, c4view.Edit{Path: "lib/"}, c4view.Edit{Path: "lib", Entry: c4view.FileEntry("", 10, put([]byte("now a file")), false)})
	o := newOutput()
	if err := q.fold(ctx, st, o, 0, first, ""); err != nil {
		t.Fatal(err)
	}
	if err := q.fold(ctx, st, o, 1, second, ""); err != nil {
		t.Fatal(err)
	}
	if o.view != want {
		t.Fatalf("after a unit turned a directory into a file the output is %v, want lib as a file", outputNames(t, st, o.view))
	}
	_, ch := readChain(t, st, o.id)
	if got, err := ch.Verify(ctx, src); err != nil || len(got) != 2 || got[1] != want {
		t.Fatalf("the chain verifies to %v: %v", got, err)
	}
}

// A manager restarts between two units of a job. The output it restores
// is the chain with the view the status keeps beside it, and the next
// unit folds onto that.
func TestOutputIsRestoredAfterARestartAndFoldedOnto(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "bon", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a1"}, {"name": "a2"}]}`)
	finishUnit(t, now, ns, "bon", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	finishUnit(t, now, ns, "bon", 1, map[string]string{c4v1.AnnotationOutputChangeset: writeChangeset(t, st, "a1.txt", "one")})
	tick(t, q)
	got := get(t, ns, "bon")
	if got.Status.Output == nil || got.Status.Output.Changesets != 1 {
		t.Fatalf("after the first unit: %+v", got.Status.Output)
	}
	// The restart: a new queue with no memory.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	finishUnit(t, now, ns, "bon", 2, map[string]string{c4v1.AnnotationOutputChangeset: writeChangeset(t, st, "a2.txt", "two")})
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "bon")
	files := outputFiles(t, st, got)
	if len(files) != 2 || files[0] != "a1.txt" || files[1] != "a2.txt" || got.Status.Output.Changesets != 2 {
		t.Fatalf("after a restart and a second unit the output holds %v, %d changesets", files, got.Status.Output.Changesets)
	}
	id, err := c4.Parse(got.Status.Output.Chain)
	if err != nil {
		t.Fatal(err)
	}
	_, ch := readChain(t, st, id)
	views, err := ch.Verify(ctx, c4view.StoreRecords(st))
	if err != nil || len(views) != 2 || views[1].String() != got.Status.Output.View {
		t.Fatalf("the stored chain verifies to %d views, the last not the status's output: %v", len(views), err)
	}
}

// A unit replaces a directory and makes a subdirectory of the same name
// again inside it. The job's output is the unit's view: the files the unit
// no longer has are gone from it. The changeset once failed to say so, and
// the fold kept them.
func TestFoldADirectoryMadeAgainInsideAReplacedOne(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	src := c4view.StoreRecords(st)
	put := func(data []byte) c4.ID {
		id, err := st.PutBytes(ctx, data)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	put(nil)
	file := func(content string) *c4m.Entry {
		return c4view.FileEntry("", int64(len(content)), put([]byte(content)), false)
	}
	apply := func(parent c4.ID, edits ...c4view.Edit) (c4.ID, string) {
		c4view.SortEdits(edits)
		a, err := c4view.Apply(ctx, parent, edits, src)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range a.Records {
			put(r.Data)
		}
		return a.ID, put(a.Changeset).String()
	}
	in, first := apply(c4view.EmptyID(),
		c4view.Edit{Path: "build/obj/old.o", Entry: file("old")},
		c4view.Edit{Path: "build/obj/keep.o", Entry: file("keep")},
		c4view.Edit{Path: "build/top.txt", Entry: file("top")})
	want, second := apply(in,
		c4view.Edit{Path: "build/", Entry: &c4m.Entry{Name: "build/"}, Opaque: true},
		c4view.Edit{Path: "build/obj/", Entry: &c4m.Entry{Name: "obj/"}},
		c4view.Edit{Path: "build/obj/new.o", Entry: file("new")},
		c4view.Edit{Path: "build/obj/keep.o", Entry: file("keep")})
	o := newOutput()
	for i, cs := range []string{first, second} {
		if err := q.fold(ctx, st, o, i, cs, ""); err != nil {
			t.Fatal(err)
		}
	}
	if o.view != want {
		v, _ := c4view.Load(ctx, o.view, src, 2)
		var held []string
		if v != nil {
			for _, f := range v.Files {
				held = append(held, f.Path)
			}
		}
		t.Fatalf("the output is not the unit's view; it holds %v, want build/obj/keep.o and build/obj/new.o", held)
	}
}
