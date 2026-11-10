package queue

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: writeChangeset(t, st, name, name)}); err != nil {
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
	if err := q.fold(ctx, st, again, Fold{Unit: 3, Changeset: writeChangeset(t, st, "d.txt", "d")}); err != nil {
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
		if err := q.fold(ctx, st, want, Fold{Unit: i, Changeset: csIDs[i]}); err != nil {
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
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: oldID.String()}); err != nil {
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
	if err := q.fold(ctx, st, re, Fold{Unit: 2, Changeset: writeChangeset(t, st, "c.txt", "c")}); err != nil {
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
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: writeChangeset(t, st, "a.txt", "a")}); err != nil {
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
	err = q.fold(ctx, st, o, Fold{Unit: 1, Changeset: cs})
	chmod(0o755)
	if err == nil {
		t.Fatal("the fold stored its chain in a read-only store")
	}
	if !bytes.Equal(o.chain, before) || o.view != beforeView || o.id != beforeID {
		t.Fatal("a failed fold changed the output")
	}
	if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: cs}); err != nil {
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
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: first}); err != nil {
		t.Fatal(err)
	}
	if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: second}); err != nil {
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
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: cs}); err != nil {
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

// unitChangeset stores what a unit's capture would: a changeset over the
// unit's input view that writes each file of puts (name to content) and
// removes each path of dels. It returns the changeset's ID.
func unitChangeset(t *testing.T, st *storeurl.Sharded, input c4.ID, puts map[string]string, dels ...string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := st.PutBytes(ctx, nil); err != nil { // the empty directory's record
		t.Fatal(err)
	}
	var edits []c4view.Edit
	for name, content := range puts {
		id, err := st.PutBytes(ctx, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		edits = append(edits, c4view.Edit{Path: name, Entry: c4view.FileEntry(name, int64(len(content)), id, false)})
	}
	for _, p := range dels {
		edits = append(edits, c4view.Edit{Path: p})
	}
	c4view.SortEdits(edits)
	a, err := c4view.Apply(ctx, input, edits, c4view.StoreRecords(st))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Changeset) == 0 {
		t.Fatal("the unit changed nothing")
	}
	for _, r := range a.Records {
		if _, err := st.PutBytes(ctx, r.Data); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := st.PutBytes(ctx, a.Changeset)
	if err != nil {
		t.Fatal(err)
	}
	return cs.String()
}

// inputView stores a job's input view holding files, and returns its ID.
func inputView(t *testing.T, st *storeurl.Sharded, files map[string]string) c4.ID {
	t.Helper()
	id, _ := c4.Parse(unitChangeset(t, st, c4view.EmptyID(), files))
	_, ch := readChain(t, st, id)
	got, err := ch.Verify(context.Background(), c4view.StoreRecords(st))
	if err != nil {
		t.Fatal(err)
	}
	return got[0]
}

// outputFilesOf maps each file of the output view to its content's ID.
func outputFilesOf(t *testing.T, st *storeurl.Sharded, id c4.ID) map[string]c4.ID {
	t.Helper()
	v, err := c4view.Load(context.Background(), id, c4view.StoreRecords(st), 2)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]c4.ID{}
	for _, f := range v.Files {
		out[f.Path] = f.ID
	}
	return out
}

func idOf(content string) c4.ID { return c4.Identify(strings.NewReader(content)) }

// checkChain reads the output's stored chain and checks it verifies, by
// the plain patch rule, to the output.
func checkChain(t *testing.T, st *storeurl.Sharded, o *output, sections int) {
	t.Helper()
	data, ch := readChain(t, st, o.id)
	if len(ch.Sections) != sections {
		t.Fatalf("the chain has %d sections, want %d\n%s", len(ch.Sections), sections, data)
	}
	got, err := ch.Verify(context.Background(), c4view.StoreRecords(st))
	if err != nil {
		t.Fatalf("the chain does not verify: %v\n%s", err, data)
	}
	head := ch.Base
	if len(got) > 0 {
		head = got[len(got)-1]
	}
	if head != o.view {
		t.Fatalf("the chain resolves to %s, the output is %s", head, o.view)
	}
}

// Two units write the same file with the same bytes (a licence, a lock
// file, a cache entry). The output holds it. Folded as a patch over the
// output, the second write read as a removal.
func TestFoldTwoUnitsWriteTheSameBytes(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: unitChangeset(t, st, in, map[string]string{"own0.txt": "unit 0", "lib/shared.txt": "the same bytes"})}); err != nil {
		t.Fatal(err)
	}
	if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: unitChangeset(t, st, in, map[string]string{"own1.txt": "unit 1", "lib/shared.txt": "the same bytes"})}); err != nil {
		t.Fatal(err)
	}
	files := outputFilesOf(t, st, o.view)
	for name, content := range map[string]string{"in.txt": "input", "own0.txt": "unit 0", "own1.txt": "unit 1", "lib/shared.txt": "the same bytes"} {
		if files[name] != idOf(content) {
			t.Errorf("%s is not in the output as written; the output holds %v", name, files)
		}
	}
	if len(o.conflicts) != 1 || !strings.Contains(o.conflicts[0], "lib/shared.txt") {
		t.Errorf("the path both wrote is reported as %v", o.conflicts)
	}
	checkChain(t, st, o, 2)
}

// Two units write one path differently: the later fold's write is kept,
// and the path is reported.
func TestFoldLaterWriteIsKept(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	a := unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 0"})
	b := unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 1"})
	for i, cs := range []string{a, b} {
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: cs}); err != nil {
			t.Fatal(err)
		}
	}
	if files := outputFilesOf(t, st, o.view); files["out.txt"] != idOf("from unit 1") || files["in.txt"] != idOf("input") {
		t.Fatalf("the later write is not what the output holds: %v", files)
	}
	if len(o.conflicts) != 1 || !strings.Contains(o.conflicts[0], "out.txt written by units 0 and 1") {
		t.Errorf("reported: %v", o.conflicts)
	}
	checkChain(t, st, o, 2)
}

// A run is folded once. Seen a second time (the queue restarted and the
// pod is still there) nothing moves: not the output, the chain, the count,
// the log or the conflicts.
func TestFoldARunOnce(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	cs := unitChangeset(t, st, in, map[string]string{"a.txt": "a", "d/b.txt": "b"}, "in.txt")
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: cs, Run: "pod-1"}); err != nil {
		t.Fatal(err)
	}
	view, chainID, fresh := o.view, o.id, len(o.fresh)
	want := outputFilesOf(t, st, o.view)
	if len(want) != 2 || want["a.txt"] != idOf("a") || want["d/b.txt"] != idOf("b") {
		t.Fatalf("after the first fold: %v", want)
	}
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: cs, Run: "pod-1"}); err != nil {
		t.Fatal(err)
	}
	if o.view != view || o.id != chainID || o.count != 1 || len(o.folds) != 1 || o.folded[0] != 1 || len(o.fresh) != fresh {
		t.Fatalf("a run seen again moved the output: count %d, %d folds, %d fresh paths (was %d)", o.count, len(o.folds), len(o.fresh), fresh)
	}
	// Another run of the unit that wrote the same is a fold like any
	// other, and has nothing to change. So has another unit that did the
	// same, and a fold with no run named (a reused computation).
	for i, f := range []Fold{{Unit: 0, Changeset: cs, Run: "pod-2"}, {Unit: 1, Changeset: cs, Run: "pod-3"}, {Unit: 2, Changeset: cs}, {Unit: 2, Changeset: cs}} {
		if err := q.fold(ctx, st, o, f); err != nil {
			t.Fatal(err)
		}
		if o.view != view || o.id != chainID {
			t.Fatalf("fold %d of the same changes moved the output: %v", i, outputFilesOf(t, st, o.view))
		}
		if o.count != 2+i || len(o.folds) != 2+i {
			t.Fatalf("fold %d was not counted: count %d, %d folds", i, o.count, len(o.folds))
		}
	}
	checkChain(t, st, o, 1)
}

// A, B, A: unit 0 writes a path, unit 1 writes it differently, and then
// unit 0's first run is seen again, as it is after a restart while its pod
// is still there. Unit 1's write is the later one and stays. Folded again,
// unit 0's would have gone back over it.
func TestFoldABATheSameRunSeenAgain(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	a := Fold{Unit: 0, Changeset: unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 0"}), Run: "pod-a"}
	b := Fold{Unit: 1, Changeset: unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 1"}), Run: "pod-b"}
	for _, f := range []Fold{a, b, a} {
		if err := q.fold(ctx, st, o, f); err != nil {
			t.Fatal(err)
		}
	}
	if files := outputFilesOf(t, st, o.view); files["out.txt"] != idOf("from unit 1") {
		t.Fatalf("a run seen again went back over a later write: %v", files)
	}
	if o.count != 2 || len(o.folds) != 2 || len(o.conflicts) != 1 {
		t.Fatalf("count %d, %d folds, conflicts %v", o.count, len(o.folds), o.conflicts)
	}
	checkChain(t, st, o, 2)

	// The same after a restart: the log comes back from the stored index,
	// and the run in it is still not folded again.
	if err := o.saveIndex(ctx, st, "job"); err != nil {
		t.Fatal(err)
	}
	re := newOutput()
	if err := re.load(st, o.id.String(), o.view.String()); err != nil {
		t.Fatal(err)
	}
	if err := re.loadIndex(st, o.index.String()); err != nil {
		t.Fatal(err)
	}
	if err := q.fold(ctx, st, re, a); err != nil {
		t.Fatal(err)
	}
	if files := outputFilesOf(t, st, re.view); re.view != o.view || files["out.txt"] != idOf("from unit 1") || len(re.folds) != 2 {
		t.Fatalf("after a restart the run was folded again: %v, %d folds", files, len(re.folds))
	}
}

// A unit runs again after another unit wrote the same path. The new run
// wrote exactly what the first did, so its changeset is the same object;
// it is another run all the same, its write is the later one, and it is
// kept and reported.
func TestFoldARerunAfterAnotherWriter(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	csA := unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 0"})
	csB := unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 1"})
	for _, f := range []Fold{{Unit: 0, Changeset: csA, Run: "pod-a1"}, {Unit: 1, Changeset: csB, Run: "pod-b"}, {Unit: 0, Changeset: csA, Run: "pod-a2"}} {
		if err := q.fold(ctx, st, o, f); err != nil {
			t.Fatal(err)
		}
	}
	if files := outputFilesOf(t, st, o.view); files["out.txt"] != idOf("from unit 0") || files["in.txt"] != idOf("input") {
		t.Fatalf("the rerun's write is not what the output holds: %v", files)
	}
	if o.count != 3 || len(o.folds) != 3 || o.folded[0] != 2 {
		t.Fatalf("count %d, %d folds, unit 0 folded %d times", o.count, len(o.folds), o.folded[0])
	}
	if len(o.conflicts) != 2 || !strings.Contains(o.conflicts[1], "out.txt written by units 1 and 0") {
		t.Fatalf("reported: %v", o.conflicts)
	}
	checkChain(t, st, o, 3)
}

// One unit folded twice with different changesets, as a unit that yields
// and is resumed is: each is a run, both are folded, and a unit is never in
// conflict with itself.
func TestFoldOneUnitTwiceWithDifferentChangesets(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	first := unitChangeset(t, st, in, map[string]string{"part.txt": "half", "log.txt": "one"})
	second := unitChangeset(t, st, in, map[string]string{"part.txt": "whole", "done.txt": "done"})
	for i, cs := range []string{first, second} {
		if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: cs, Run: fmt.Sprint("pod-", i)}); err != nil {
			t.Fatal(err)
		}
	}
	files := outputFilesOf(t, st, o.view)
	for name, content := range map[string]string{"part.txt": "whole", "log.txt": "one", "done.txt": "done", "in.txt": "input"} {
		if files[name] != idOf(content) {
			t.Errorf("%s is not %q in the output: %v", name, content, files)
		}
	}
	if o.folded[0] != 2 || o.count != 2 || len(o.conflicts) != 0 {
		t.Fatalf("unit 0 folded %d times, count %d, conflicts %v", o.folded[0], o.count, o.conflicts)
	}
	checkChain(t, st, o, 2)
}

// A changeset of two sections: the second is a patch over the view the
// first made, not over the unit's input. Its removal of what the first
// section wrote is a removal only when read against that view; read
// against the input, where the path is not, it would be a write.
func TestFoldEachSectionAgainstItsOwnBase(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	src := c4view.StoreRecords(st)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})

	one, _ := c4.Parse(unitChangeset(t, st, in, map[string]string{"scratch.txt": "scratch", "kept.txt": "kept"}))
	oneData, ch := readChain(t, st, one)
	mid, err := ch.Verify(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	two, _ := c4.Parse(unitChangeset(t, st, mid[0], map[string]string{"result.txt": "result"}, "scratch.txt"))
	twoData, _ := readChain(t, st, two)
	both, err := st.PutBytes(ctx, append(append([]byte(nil), oneData...), twoData...))
	if err != nil {
		t.Fatal(err)
	}
	if _, joined := readChain(t, st, both); len(joined.Sections) != 2 || joined.Sections[1].Base != mid[0] {
		t.Fatalf("the unit's changeset is not two sections, the second over the first's view")
	}

	o := newOutput()
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: unitChangeset(t, st, in, map[string]string{"other.txt": "another unit"})}); err != nil {
		t.Fatal(err)
	}
	if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: both.String()}); err != nil {
		t.Fatal(err)
	}
	files := outputFilesOf(t, st, o.view)
	if _, ok := files["scratch.txt"]; ok {
		t.Errorf("what the second section removed is in the output: %v", files)
	}
	for name, content := range map[string]string{"in.txt": "input", "other.txt": "another unit", "kept.txt": "kept", "result.txt": "result"} {
		if files[name] != idOf(content) {
			t.Errorf("%s is not %q in the output: %v", name, content, files)
		}
	}
	if o.count != 2 || len(o.conflicts) != 0 {
		t.Errorf("count %d, conflicts %v", o.count, o.conflicts)
	}
	checkChain(t, st, o, 3)
}

// A fold that changes nothing in an output no fold has changed yet: the
// stored chain is still the job's input view alone, a c4m file that can be
// read, and not an empty object.
func TestFoldThatChangesNothingStoresTheInputAlone(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	unitIn := inputView(t, st, map[string]string{"in.txt": "input", "tmp.txt": "tmp"})
	jobIn := inputView(t, st, map[string]string{"in.txt": "input"})
	alone, err := (&c4view.Chain{Base: jobIn}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	aloneID, err := st.PutBytes(ctx, alone)
	if err != nil {
		t.Fatal(err)
	}
	o := newOutput()
	if err := o.load(st, aloneID.String(), jobIn.String()); err != nil {
		t.Fatal(err)
	}
	// The unit removed a file the output never had.
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: unitChangeset(t, st, unitIn, nil, "tmp.txt"), Run: "pod-0"}); err != nil {
		t.Fatal(err)
	}
	if o.view != jobIn || o.id != aloneID || len(o.chain) != 0 {
		t.Fatalf("the output moved: view %s, chain object %s", o.view, o.id)
	}
	if o.count != 1 || len(o.folds) != 1 {
		t.Fatalf("the fold was not counted: %d, %d folds", o.count, len(o.folds))
	}
	checkChain(t, st, o, 0)
}

// A fold can take another unit's writes out of the output without naming
// them: it removes the directory they are in, writes a file where that
// directory was, or writes under a name where the other unit wrote a file.
// The later fold stands, as everywhere, and each loss is reported.
func TestFoldThatTakesAnotherUnitsWritesReportsThem(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		first    map[string]string
		second   map[string]string
		dels     []string
		want     map[string]string
		gone     []string
		conflict string
	}{
		{name: "a directory removed",
			first: map[string]string{"d/a.txt": "a", "d/sub/b.txt": "b", "e.txt": "e"}, dels: []string{"d/"},
			second: map[string]string{"mine.txt": "mine"},
			want:   map[string]string{"e.txt": "e", "mine.txt": "mine"}, gone: []string{"d/a.txt", "d/sub/b.txt", "d/in.txt"},
			conflict: "d/ removed by unit 1: unit 0 wrote 2 of the paths under it"},
		{name: "a directory replaced by a file",
			first: map[string]string{"d/a.txt": "a"}, second: map[string]string{"d": "now a file"},
			want: map[string]string{"d": "now a file"}, gone: []string{"d/a.txt", "d/in.txt"},
			conflict: "d/ removed by unit 1: unit 0 wrote 1 of the paths under it"},
		{name: "a file replaced by a directory",
			first: map[string]string{"x": "a file"}, second: map[string]string{"x/y.txt": "y"},
			want: map[string]string{"x/y.txt": "y"}, gone: []string{"x"},
			conflict: "x written by unit 0, replaced by a directory by unit 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, _ := foldStore(t)
			q := &Queue{}
			in := inputView(t, st, map[string]string{"d/in.txt": "in", "keep.txt": "keep"})
			o := newOutput()
			if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: unitChangeset(t, st, in, c.first)}); err != nil {
				t.Fatal(err)
			}
			if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: unitChangeset(t, st, in, c.second, c.dels...)}); err != nil {
				t.Fatal(err)
			}
			files := outputFilesOf(t, st, o.view)
			for name, content := range c.want {
				if files[name] != idOf(content) {
					t.Errorf("%s is not %q in the output: %v", name, content, files)
				}
			}
			for _, name := range c.gone {
				if _, ok := files[name]; ok {
					t.Errorf("%s is still in the output: %v", name, files)
				}
			}
			if len(o.conflicts) != 1 || o.conflicts[0] != c.conflict {
				t.Errorf("reported %q, want %q", o.conflicts, c.conflict)
			}
			checkChain(t, st, o, 2)
		})
	}

	// A unit that removes a directory nobody else wrote in is in conflict
	// with nobody.
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"d/in.txt": "in", "keep.txt": "keep"})
	o := newOutput()
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: unitChangeset(t, st, in, map[string]string{"a.txt": "a"})}); err != nil {
		t.Fatal(err)
	}
	if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: unitChangeset(t, st, in, nil, "d/")}); err != nil {
		t.Fatal(err)
	}
	if len(o.conflicts) != 0 {
		t.Errorf("reported %q for a directory only the input had filled", o.conflicts)
	}
	// Nor is a unit in conflict with itself: one run writes into a
	// directory, a later run of the same unit removes it.
	st, _ = foldStore(t)
	in = inputView(t, st, map[string]string{"d/in.txt": "in", "keep.txt": "keep"})
	o = newOutput()
	for i, cs := range []string{unitChangeset(t, st, in, map[string]string{"d/mine.txt": "mine"}), unitChangeset(t, st, in, nil, "d/")} {
		if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: cs, Run: fmt.Sprint("pod-", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if files := outputFilesOf(t, st, o.view); len(files) != 1 || len(o.conflicts) != 0 {
		t.Errorf("a unit removing what it wrote itself: output %v, reported %q", files, o.conflicts)
	}
	// The same for a file the unit wrote and then made a directory of.
	for i, cs := range []string{unitChangeset(t, st, in, map[string]string{"x": "a file"}), unitChangeset(t, st, in, map[string]string{"x/y.txt": "y"})} {
		if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: cs, Run: fmt.Sprint("pod-x", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if files := outputFilesOf(t, st, o.view); files["x/y.txt"] != idOf("y") || len(o.conflicts) != 0 {
		t.Errorf("a unit making a directory of its own file: output %v, reported %q", files, o.conflicts)
	}
}

// A fold that fails changes nothing: not the output, and not what is kept
// beside it (who wrote each path, the records, the conflicts, the paths to
// ask about). Tried again, it is one fold.
func TestAFailedFoldLeavesTheBooksAlone(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	ctx := context.Background()
	st, dir := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 0"}), Record: "record-0", Run: "pod-0"}); err != nil {
		t.Fatal(err)
	}
	if err := o.saveIndex(ctx, st, "job"); err != nil {
		t.Fatal(err)
	}
	second := Fold{Unit: 1, Changeset: unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 1", "new.txt": "new"}), Record: "record-1", Run: "pod-1"}
	// The records the fold will write are stored first, so the one thing
	// left to store is the chain; then the store is made read-only.
	id, _ := c4.Parse(second.Changeset)
	_, ch := readChain(t, st, id)
	res, err := c4view.ApplyPatchFrom(ctx, o.view, in, ch.Sections[0].Patch, c4view.StoreRecords(st))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Records {
		if _, err := st.PutBytes(ctx, r.Data); err != nil {
			t.Fatal(err)
		}
	}
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
	state := func() string {
		return fmt.Sprint(o.view, o.id, o.count, o.folded, o.writer, o.records, o.units, o.conflicts, o.fresh, o.folds, o.dirty, len(o.chain))
	}
	before := state()
	chmod(0o555)
	err = q.fold(ctx, st, o, second)
	chmod(0o755)
	if err == nil {
		t.Fatal("the fold stored its chain in a read-only store")
	}
	if after := state(); after != before {
		t.Fatalf("a failed fold changed what the output keeps:\n%s\n%s", before, after)
	}
	if err := q.fold(ctx, st, o, second); err != nil {
		t.Fatal(err)
	}
	if o.count != 2 || len(o.conflicts) != 1 || o.writer["out.txt"] != 1 || o.records["new.txt"] != "record-1" || o.units[1] != "record-1" || !o.dirty {
		t.Fatalf("after the retry: count %d, conflicts %v, writer %v, records %v", o.count, o.conflicts, o.writer, o.records)
	}
	checkChain(t, st, o, 2)
}

// Removals follow the same rule as writes: what each unit did to its own
// input is done to the output, and the later fold stands.
func TestFoldRemovals(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name          string
		first, second func(st *storeurl.Sharded, in c4.ID) string
		want          map[string]string
		gone          []string
		sections      int
	}{
		{name: "the second removes what the first changed",
			first: func(st *storeurl.Sharded, in c4.ID) string {
				return unitChangeset(t, st, in, map[string]string{"g.txt": "changed"})
			},
			second: func(st *storeurl.Sharded, in c4.ID) string { return unitChangeset(t, st, in, nil, "g.txt") },
			gone:   []string{"g.txt"}, want: map[string]string{"keep.txt": "keep"}, sections: 2},
		{name: "the second changes what the first removed",
			first: func(st *storeurl.Sharded, in c4.ID) string { return unitChangeset(t, st, in, nil, "g.txt") },
			second: func(st *storeurl.Sharded, in c4.ID) string {
				return unitChangeset(t, st, in, map[string]string{"g.txt": "changed"})
			},
			want: map[string]string{"g.txt": "changed", "keep.txt": "keep"}, sections: 2},
		{name: "both remove the same file",
			first:  func(st *storeurl.Sharded, in c4.ID) string { return unitChangeset(t, st, in, nil, "g.txt") },
			second: func(st *storeurl.Sharded, in c4.ID) string { return unitChangeset(t, st, in, nil, "g.txt") },
			gone:   []string{"g.txt"}, want: map[string]string{"keep.txt": "keep"}, sections: 1},
		{name: "the second removes a file the first never touched, and adds beside what the first added",
			first: func(st *storeurl.Sharded, in c4.ID) string {
				return unitChangeset(t, st, in, map[string]string{"out/a.txt": "a"})
			},
			second: func(st *storeurl.Sharded, in c4.ID) string {
				return unitChangeset(t, st, in, map[string]string{"out/b.txt": "b"}, "g.txt")
			},
			gone: []string{"g.txt"}, want: map[string]string{"out/a.txt": "a", "out/b.txt": "b", "keep.txt": "keep"}, sections: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, _ := foldStore(t)
			q := &Queue{}
			in := inputView(t, st, map[string]string{"g.txt": "g", "keep.txt": "keep"})
			o := newOutput()
			if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: c.first(st, in)}); err != nil {
				t.Fatal(err)
			}
			if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: c.second(st, in)}); err != nil {
				t.Fatal(err)
			}
			files := outputFilesOf(t, st, o.view)
			for name, content := range c.want {
				if files[name] != idOf(content) {
					t.Errorf("%s is not in the output as %q; it holds %v", name, content, files)
				}
			}
			for _, name := range c.gone {
				if _, ok := files[name]; ok {
					t.Errorf("%s is still in the output: %v", name, files)
				}
			}
			checkChain(t, st, o, c.sections)
		})
	}
}

// The manager restarts while a finished unit's pod still exists (it is
// being deleted, as a pod is for a moment on a real node). The new queue
// sees the pod again. The job's output and its count of changesets stay
// as they were.
func TestRestartDoesNotFoldAFinishedUnitAgain(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "bon", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a1"}, {"name": "b"}]}`)
	finishUnit(t, now, ns, "bon", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)

	// Unit 1's pod is held by a finalizer that isn't the queue's, so it
	// stays listed, terminating, after the queue lets go of it.
	p := podOfUnit(t, ns, "bon", 1)
	p.Finalizers = append(p.Finalizers, "test.c4.avalanche.io/hold")
	if err := k8s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	defer func() {
		var held corev1.Pod
		if k8s.Get(ctx, client.ObjectKeyFromObject(p), &held) == nil {
			held.Finalizers = nil
			k8s.Update(ctx, &held)
		}
	}()
	finishUnit(t, now, ns, "bon", 1, map[string]string{c4v1.AnnotationOutputChangeset: writeChangeset(t, st, "a1.txt", "unit one's file")})
	tick(t, q)
	got := get(t, ns, "bon")
	if got.Status.Output == nil || got.Status.Output.Changesets != 1 || len(outputFiles(t, st, got)) != 1 {
		t.Fatalf("after the fold: %+v", got.Status.Output)
	}
	before := *got.Status.Output
	var all corev1.PodList
	if err := k8s.List(ctx, &all, client.InNamespace(ns), client.MatchingLabels{c4v1.LabelJob: "bon"}); err != nil {
		t.Fatal(err)
	}
	still := false
	for _, pod := range all.Items {
		still = still || (unitOf(pod) == 1 && pod.Status.Phase == corev1.PodSucceeded)
	}
	if !still {
		t.Fatal("unit 1's finished pod is gone, so this test shows nothing")
	}

	// The restart: a new queue with no memory, seeing that pod again.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "bon")
	if got.Status.Output == nil {
		t.Fatal("no output after the restart")
	}
	if files := outputFiles(t, st, got); got.Status.Output.View != before.View || len(files) != 1 || files[0] != "a1.txt" {
		t.Errorf("the restart changed the job's output: files %v, view %s, was %s", files, got.Status.Output.View, before.View)
	}
	if got.Status.Output.Changesets != before.Changesets || got.Status.Output.Chain != before.Chain {
		t.Errorf("the restart moved the count or the chain: %d changesets (was %d), chain %s (was %s)", got.Status.Output.Changesets, before.Changesets, got.Status.Output.Chain, before.Chain)
	}
}
