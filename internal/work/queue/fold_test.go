package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/go-logr/logr"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/work/sched"
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

// resolveHead resolves a chain's head through the c4m library, every block
// read checked, and returns the view it describes.
func resolveHead(t *testing.T, st *storeurl.Sharded, head c4.ID) c4.ID {
	t.Helper()
	m, err := c4m.ResolveBaseID(head, checkedSource{st})
	if err != nil {
		t.Fatalf("resolve %s: %v", head, err)
	}
	return m.ComputeC4ID()
}

type checkedSource struct{ st c4view.Store }

func (s checkedSource) Open(id c4.ID) (io.ReadCloser, error) {
	b, err := c4view.ReadObject(s.st, id)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
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
	var outs, heads []c4.ID
	for i, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: writeChangeset(t, st, name, name)}); err != nil {
			t.Fatal(err)
		}
		outs = append(outs, o.view)
		data, ch := readChain(t, st, o.id)
		on := c4view.EmptyID() // the first rests on the job's input
		if i > 0 {
			on = heads[i-1]
		}
		if !ch.Seal.IsNil() || len(ch.Sections) != 1 || ch.Base != on {
			t.Fatalf("after %d folds the head is not one block on the one before\n%s", i+1, data)
		}
		if got := resolveHead(t, st, o.id); got != o.view {
			t.Fatalf("after %d folds the head resolves to %s, the output is %s", i+1, got, o.view)
		}
		heads = append(heads, o.id)
	}
	if len(outputNames(t, st, o.view)) != 3 {
		t.Fatal("the output does not hold the three files")
	}

	// A restart: the chain and, beside it, the view from the job's status.
	again := newOutput()
	if err := again.load(st, o.id.String(), o.view.String()); err != nil {
		t.Fatal(err)
	}
	if again.view != o.view || again.id != o.id {
		t.Fatal("a loaded output is not the output that was stored")
	}
	if err := q.fold(ctx, st, again, Fold{Unit: 3, Changeset: writeChangeset(t, st, "d.txt", "d")}); err != nil {
		t.Fatal(err)
	}
	// After a restart the next block rests on the view the status kept.
	_, ch := readChain(t, st, again.id)
	if len(ch.Sections) != 1 || ch.Base != o.view || resolveHead(t, st, again.id) != again.view {
		t.Fatalf("after a restart and a fold the head rests on %s (want %s) and resolves elsewhere", ch.Base, o.view)
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
	var outs, wantHeads []c4.ID
	for i, name := range []string{"a.txt", "b.txt"} {
		csIDs = append(csIDs, writeChangeset(t, st, name, name))
		if err := q.fold(ctx, st, want, Fold{Unit: i, Changeset: csIDs[i]}); err != nil {
			t.Fatal(err)
		}
		outs = append(outs, want.view)
		wantHeads = append(wantHeads, want.id)
	}
	// The chain as it was stored before blocks: the folds' sections joined
	// over the job's input (here the empty view).
	wantChain := &c4view.Chain{Base: c4view.EmptyID()}
	for _, h := range []c4.ID{wantHeads[0], want.id} {
		_, blk := readChain(t, st, h)
		wantChain.Sections = append(wantChain.Sections, blk.Sections...)
	}

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
	if o.view != want.view || o.id != want.id {
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
	if re.view != want.view || re.loadedBase != c4view.EmptyID() {
		t.Fatal("an old chain does not load: its view, or its base (the job's input)")
	}
	if err := q.fold(ctx, st, re, Fold{Unit: 2, Changeset: writeChangeset(t, st, "c.txt", "c")}); err != nil {
		t.Fatal(err)
	}
	data, ch := readChain(t, st, re.id)
	if !ch.Seal.IsNil() || len(ch.Sections) != 1 || ch.Base != want.view {
		t.Fatalf("the block after an old chain was loaded does not rest on its view\n%s", data)
	}
	if got := resolveHead(t, st, re.id); got != re.view {
		t.Fatalf("it resolves to %s, the output is %s", got, re.view)
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
	beforeView, beforeID, beforeBlocks := o.view, o.id, o.blocks
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
	if o.blocks != beforeBlocks || o.view != beforeView || o.id != beforeID {
		t.Fatal("a failed fold changed the output")
	}
	if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: cs}); err != nil {
		t.Fatal(err)
	}
	stored := logicalChain(t, st, o.id)
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
	ch := logicalChain(t, st, o.id)
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
	if !strings.HasPrefix(got.Status.Output.Chain, chainField) {
		t.Fatalf("the status's chain %q is not marked as a head of blocks", got.Status.Output.Chain)
	}
	id, err := c4.Parse(strings.TrimPrefix(got.Status.Output.Chain, chainField))
	if err != nil {
		t.Fatal(err)
	}
	// After the restart the next block rests on the view the status kept:
	// the head resolves to the status's output, and its chain verifies.
	if r := resolveHead(t, st, id); r.String() != got.Status.Output.View {
		t.Fatalf("the stored chain resolves to %s, the status's output is %s", r, got.Status.Output.View)
	}
	views, err := logicalChain(t, st, id).Verify(ctx, c4view.StoreRecords(st))
	if err != nil || len(views) == 0 || views[len(views)-1].String() != got.Status.Output.View {
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

// logicalChain is the chain a head stands for, in the shape it had before
// blocks: walked back block by block to the view the oldest rests on (an
// object that is a listing, not a chain), its sections in order.
func logicalChain(t *testing.T, st *storeurl.Sharded, head c4.ID) *c4view.Chain {
	t.Helper()
	var secs []c4view.Section
	var blocks []c4.ID
	cur := head
	for {
		data, err := c4view.ReadObject(st, cur)
		if err != nil {
			t.Fatal(err)
		}
		ch, err := c4view.ReadChain(data)
		if err != nil {
			break // a view's listing: the base
		}
		if len(ch.Sections) == 0 {
			cur = ch.Base // a chain from before blocks that is its base alone
			break
		}
		if len(ch.Sections) != 1 {
			t.Fatalf("block %s holds %d sections, not one", cur, len(ch.Sections))
		}
		secs = append(secs, ch.Sections[0])
		blocks = append(blocks, cur)
		cur = ch.Base
	}
	out := &c4view.Chain{Base: cur}
	for i := len(secs) - 1; i >= 0; i-- {
		sec := secs[i]
		if i < len(secs)-1 {
			// It rests on the block before, which stands for the view it
			// resolves to.
			sec.Base = resolveHead(t, st, blocks[i+1])
		}
		out.Sections = append(out.Sections, sec)
	}
	return out
}

// checkChain reads the output's stored chain and checks it verifies, by
// the plain patch rule, to the output, and that its head resolves to it.
func checkChain(t *testing.T, st *storeurl.Sharded, o *output, sections int) {
	t.Helper()
	if sections > 0 && resolveHead(t, st, o.id) != o.view {
		t.Fatalf("the head %s does not resolve to the output %s", o.id, o.view)
	}
	ch := logicalChain(t, st, o.id)
	data := []byte(fmt.Sprintf("head %s", o.id))
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
	if _, err := re.loadIndex(ctx, st, o.index.String(), "job", nil, map[string]recordRun{}, nil); err != nil {
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
	if o.view != jobIn || o.id != aloneID {
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
			conflict: "d/ removed by unit 1: unit 0 wrote or removed 2 of the paths under it"},
		{name: "a directory replaced by a file",
			first: map[string]string{"d/a.txt": "a"}, second: map[string]string{"d": "now a file"},
			want: map[string]string{"d": "now a file"}, gone: []string{"d/a.txt", "d/in.txt"},
			conflict: "d/ removed by unit 1: unit 0 wrote or removed 1 of the paths under it"},
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
		return fmt.Sprint(o.view, o.id, o.count, o.folded, o.writer, o.records, o.units, o.conflicts, o.fresh, o.folds, o.dirty, o.blocks, o.input)
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

// A job's status names its output's chain and the index holding its fold
// log together. When the index cannot be saved the status waits, so a
// restart never finds a chain whose folds the log does not know. The log
// names each fold's run by its pod.
func TestStatusWaitsForTheFoldLogToBeSaved(t *testing.T) {
	t.Run("the store answers with an error", func(t *testing.T) {
		statusWaits(t, func() (*storeurl.Sharded, error) { return nil, errors.New("the store is away") }, "the store is away")
	})
	// What the manager's store function answers while a Project is not
	// found: no store, and no error.
	t.Run("there is no store", func(t *testing.T) {
		statusWaits(t, func() (*storeurl.Sharded, error) { return nil, nil }, "no store")
	})
}

func statusWaits(t *testing.T, refuse func() (*storeurl.Sharded, error), says string) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "bon", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a1"}, {"name": "b"}]}`)
	finishUnit(t, now, ns, "bon", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	uid := string(podOfUnit(t, ns, "bon", 1).UID)
	finishUnit(t, now, ns, "bon", 1, map[string]string{c4v1.AnnotationOutputChangeset: writeChangeset(t, st, "a1.txt", "unit one's file")})

	// The store is away for the status write only: the fold itself lands.
	away, refused := true, 0
	q.Stores = func(context.Context, string) (*storeurl.Sharded, error) {
		pc := make([]uintptr, 32)
		frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
		for away {
			f, more := frames.Next()
			if strings.HasSuffix(f.Function, "(*Queue).report") {
				refused++
				return refuse()
			}
			if !more {
				break
			}
		}
		return st, nil
	}
	tick(t, q)
	if refused == 0 {
		t.Fatal("the status write never asked for the store, so this test shows nothing")
	}
	held := get(t, ns, "bon")
	if held.Status.Output != nil {
		t.Fatalf("the status names an output whose fold log was not saved: %+v", held.Status.Output)
	}
	// And it says why it waits, so a store that never takes the index is
	// a condition someone can read and not a job that silently stands.
	if c := meta.FindStatusCondition(held.Status.Conditions, ConditionOutputRecorded); c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, says) {
		t.Fatalf("the waiting status does not say so: %+v", held.Status.Conditions)
	}
	// It is the last status with that added, not a new one: what else the
	// status said is still said.
	if c := meta.FindStatusCondition(held.Status.Conditions, "Planned"); c == nil || c.Status != metav1.ConditionTrue || held.Status.Phase == "" {
		t.Fatalf("the waiting status lost what the last one said: phase %q, conditions %+v", held.Status.Phase, held.Status.Conditions)
	}
	if len(decode(held.Status.UnitStates["succeeded"])) != 1 {
		t.Fatalf("the waiting status moved on without the output: units %v", held.Status.UnitStates)
	}
	// Still away a tick later: still waiting, still said.
	tick(t, q)
	if again := get(t, ns, "bon"); again.Status.Output != nil || meta.FindStatusCondition(again.Status.Conditions, ConditionOutputRecorded) == nil {
		t.Fatalf("a second tick with the store away: %+v", again.Status)
	}
	var pods corev1.PodList
	if err := k8s.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{c4v1.LabelJob: "bon"}); err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, p := range pods.Items {
		kept = kept || (string(p.UID) == uid && p.DeletionTimestamp == nil)
	}
	if !kept {
		t.Fatal("the unit's pod was deleted before the status that records it was written")
	}

	away = false
	tick(t, q)
	got := get(t, ns, "bon")
	if got.Status.Output == nil || got.Status.Output.Changesets != 1 || got.Status.Output.Provenance == "" {
		t.Fatalf("once the store is back: %+v", got.Status.Output)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, ConditionOutputRecorded); c != nil {
		t.Fatalf("the status still says it waits: %+v", c)
	}
	if files := outputFiles(t, st, got); len(files) != 1 || files[0] != "a1.txt" {
		t.Fatalf("the output holds %v", files)
	}
	id, _ := c4.Parse(got.Status.Output.Provenance)
	data, err := c4view.ReadObject(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var ix Index
	if err := json.Unmarshal(data, &ix); err != nil {
		t.Fatal(err)
	}
	if len(ix.Folds) != 1 || ix.Folds[0].Run != uid || ix.Folds[0].Unit != 1 {
		t.Fatalf("the fold log is %+v, want one fold of unit 1 by pod %s", ix.Folds, uid)
	}
}

// The index is not marked saved until it is: a save that fails leaves the
// output dirty, so the next tick tries again and the status goes on waiting.
func TestAFailedIndexSaveIsTriedAgain(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	ctx := context.Background()
	st, dir := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: unitChangeset(t, st, in, map[string]string{"a.txt": "a"}), Run: "pod-0"}); err != nil {
		t.Fatal(err)
	}
	var dirs []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for _, d := range dirs {
		os.Chmod(d, 0o555)
	}
	err := o.saveIndex(ctx, st, "job")
	for _, d := range dirs {
		os.Chmod(d, 0o755)
	}
	if err == nil {
		t.Fatal("the index was saved in a read-only store")
	}
	if !o.dirty || !o.index.IsNil() {
		t.Fatalf("after a failed save the output is marked saved: dirty %v, index %s", o.dirty, o.index)
	}
	if err := o.saveIndex(ctx, st, "job"); err != nil || o.dirty || o.index.IsNil() {
		t.Fatalf("the save tried again: %v, dirty %v", err, o.dirty)
	}
}

// Rebuilding the output without its superseded units keeps each remaining
// fold's run in the log, so a run already folded is still not folded again.
func TestRefoldKeepsTheRuns(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	m := &jobMem{out: newOutput(), superseded: map[int]bool{0: true}}
	// Two folds are kept, and they wrote one path: rebuilt in the order
	// they were folded, the later one still stands.
	earlier := Fold{Unit: 3, Changeset: unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 3, earlier"}), Run: "pod-3"}
	kept := Fold{Unit: 1, Changeset: unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 1"}), Run: "pod-1"}
	for _, f := range []Fold{earlier, {Unit: 0, Changeset: unitChangeset(t, st, in, map[string]string{"gone.txt": "superseded"}), Run: "pod-0"}, kept} {
		if err := q.fold(ctx, st, m.out, f); err != nil {
			t.Fatal(err)
		}
	}
	if !q.refoldOutput(ctx, &view{key: "ns/job", project: "show", mem: m}) {
		t.Fatal("the refold asked to be tried again")
	}
	o := m.out
	if len(o.folds) != 2 || o.folds[0] != earlier || o.folds[1] != kept {
		t.Fatalf("the rebuilt log is %+v, want the two folds kept, in order, with their runs", o.folds)
	}
	if files := outputFilesOf(t, st, o.view); len(files) != 2 || files["out.txt"] != idOf("from unit 1") {
		t.Fatalf("the rebuilt output holds %v", files)
	}
	// Another unit writes the path; the kept run, seen again, stays out.
	if err := q.fold(ctx, st, o, Fold{Unit: 2, Changeset: unitChangeset(t, st, in, map[string]string{"out.txt": "from unit 2"}), Run: "pod-2"}); err != nil {
		t.Fatal(err)
	}
	if err := q.fold(ctx, st, o, kept); err != nil {
		t.Fatal(err)
	}
	if files := outputFilesOf(t, st, o.view); files["out.txt"] != idOf("from unit 2") || len(o.folds) != 3 {
		t.Fatalf("a run kept through a refold was folded again: %v, %d folds", files, len(o.folds))
	}
}

// The one changeset the fold refuses: a directory written by its C4 ID
// alone where a different directory already is, whose meaning is not ruled.
// Capture never writes it. It is refused by name whether it is the job's
// first fold or a later one, and leaves the output as it was.
func TestFoldRefusesADirectoryByIDAloneOverAnother(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	// byID is a changeset over base that names d/ by the ID of the listing
	// {b.txt} and lists nothing under it.
	byID := func(base c4.ID) string {
		t.Helper()
		id, _ := c4.Parse(unitChangeset(t, st, base, map[string]string{"d/b.txt": "b"}, "d/a.txt"))
		_, ch := readChain(t, st, id)
		m := c4m.NewManifest()
		for _, e := range ch.Sections[0].Patch.Entries {
			if e.IsDir() {
				m.AddEntry(e)
			}
		}
		if len(m.Entries) != 1 {
			t.Fatalf("not one directory entry: %v", m.Entries)
		}
		cs, err := c4view.EncodeChangeset(base, m)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := st.PutBytes(ctx, cs)
		if err != nil {
			t.Fatal(err)
		}
		return stored.String()
	}
	withDir := inputView(t, st, map[string]string{"in.txt": "input", "d/a.txt": "a"})

	// As a job's first fold, over the base that holds the other directory.
	o := newOutput()
	err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: byID(withDir), Run: "pod-0"})
	if err == nil || !strings.Contains(err.Error(), "by its C4 ID alone") {
		t.Fatalf("as the first fold it was not refused by name: %v", err)
	}
	if !o.view.IsNil() || o.count != 0 || len(o.folds) != 0 {
		t.Fatalf("a refused fold moved the output: %+v", o)
	}

	// As a later fold, after another unit's.
	if err := q.fold(ctx, st, o, Fold{Unit: 1, Changeset: unitChangeset(t, st, withDir, map[string]string{"other.txt": "other"}), Run: "pod-1"}); err != nil {
		t.Fatal(err)
	}
	view, chain := o.view, o.id
	err = q.fold(ctx, st, o, Fold{Unit: 0, Changeset: byID(withDir), Run: "pod-0"})
	if err == nil || !strings.Contains(err.Error(), "by its C4 ID alone") {
		t.Fatalf("as a later fold it was not refused by name: %v", err)
	}
	if o.view != view || o.id != chain || o.count != 1 || len(o.folds) != 1 || len(o.conflicts) != 0 {
		t.Fatalf("a refused fold moved the output: count %d, %d folds, conflicts %v", o.count, len(o.folds), o.conflicts)
	}
}

// A fold that can never succeed is seen where an operator looks: the job's
// status says a finished unit's changes are not in its output, and why,
// for as long as that is so. Here the changeset is the one the fold
// refuses. The unit's pod stays and the other unit's work stays in the
// output. The condition follows what is failing now, and keeps the time
// the output first fell behind.
func TestAFoldThatFailsIsACondition(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "bon", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a1"}, {"name": "b"}]}`)
	finishUnit(t, now, ns, "bon", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)

	// Unit 1 writes d/a.txt. Unit 2's changeset names d/ by the C4 ID of
	// another listing and lists nothing under it.
	wrote := unitChangeset(t, st, c4view.EmptyID(), map[string]string{"d/a.txt": "a"})
	id, _ := c4.Parse(unitChangeset(t, st, c4view.EmptyID(), map[string]string{"d/b.txt": "b"}))
	_, ch := readChain(t, st, id)
	m := c4m.NewManifest()
	for _, e := range ch.Sections[0].Patch.Entries {
		if e.IsDir() {
			m.AddEntry(e)
		}
	}
	cs, err := c4view.EncodeChangeset(c4view.EmptyID(), m)
	if err != nil {
		t.Fatal(err)
	}
	byID, err := st.PutBytes(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	refused := string(podOfUnit(t, ns, "bon", 2).UID)
	finishUnit(t, now, ns, "bon", 1, map[string]string{c4v1.AnnotationOutputChangeset: wrote})
	finishUnit(t, now, ns, "bon", 2, map[string]string{c4v1.AnnotationOutputChangeset: byID.String()})

	// First both folds fail for want of the store. Then the store is back:
	// unit 1 folds, and unit 2's changeset is refused. The condition has to
	// follow, from the one reason to the other, and keep its time.
	away := true
	q.Stores = func(context.Context, string) (*storeurl.Sharded, error) {
		pc := make([]uintptr, 32)
		frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
		for away {
			f, more := frames.Next()
			if strings.HasSuffix(f.Function, "(*Queue).foldChangeset") {
				return nil, errors.New("the store is away")
			}
			if !more {
				break
			}
		}
		return st, nil
	}
	tick(t, q)
	late := get(t, ns, "bon")
	first := meta.FindStatusCondition(late.Status.Conditions, ConditionOutputRecorded)
	if first == nil || first.Reason != "FoldFailed" || !strings.Contains(first.Message, "the store is away") || late.Status.Output != nil {
		t.Fatalf("with the store away for the folds: output %+v, conditions %+v", late.Status.Output, late.Status.Conditions)
	}
	away = false

	since := first.LastTransitionTime
	for range 3 {
		// Long enough, by the queue's clock and the wall's, for a status
		// to be written again and for a condition made anew to show it.
		*now = now.Add(11 * time.Second)
		time.Sleep(1100 * time.Millisecond)
		tick(t, q)
		got := get(t, ns, "bon")
		c := meta.FindStatusCondition(got.Status.Conditions, ConditionOutputRecorded)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "FoldFailed" || !strings.Contains(c.Message, "unit 2") || !strings.Contains(c.Message, "by its C4 ID alone") {
			t.Fatalf("the status does not say the fold fails, or why: %+v", got.Status.Conditions)
		}
		// The condition says since when: an output that has been behind
		// for an hour must not read as one that has just fallen behind.
		if !c.LastTransitionTime.Equal(&since) {
			t.Fatalf("the condition's time moved from %s to %s while the output stayed behind", since.Time, c.LastTransitionTime.Time)
		}
		if got.Status.Output == nil || got.Status.Output.Changesets != 1 {
			t.Fatalf("the output moved, or lost the other unit's fold: %+v", got.Status.Output)
		}
		out, _ := c4.Parse(got.Status.Output.View)
		if files := outputFilesOf(t, st, out); len(files) != 1 || files["d/a.txt"] != idOf("a") {
			t.Fatalf("the output holds %v", files)
		}
		if len(decode(got.Status.UnitStates["succeeded"])) != 2 {
			t.Fatalf("the unit whose fold fails is recorded as done: %v", got.Status.UnitStates)
		}
	}
	var pods corev1.PodList
	if err := k8s.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{c4v1.LabelJob: "bon"}); err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, p := range pods.Items {
		kept = kept || (string(p.UID) == refused && p.DeletionTimestamp == nil)
	}
	if !kept {
		t.Fatal("the pod whose changes are not folded was deleted")
	}
}

// What is and is not a failed fold, at the call that makes one: a fold
// held over to the next tick by the per-tick limit is not; an output that
// cannot be loaded is, and the first failure of a tick is the one kept.
func TestWhatCountsAsAFoldThatFailed(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	cs := unitChangeset(t, st, in, map[string]string{"a.txt": "a"})

	v := &view{key: "ns/job", project: "show", mem: &jobMem{out: newOutput()}}
	q.folds = maxFoldsPerTick
	if q.foldChangeset(ctx, v, Fold{Unit: 1, Changeset: cs}) || v.foldWait != "" {
		t.Fatalf("a fold held over by the limit: said %q", v.foldWait)
	}
	q.folds = 0

	// The status names a chain that is not in the store.
	missing := c4.Identify(strings.NewReader("no such chain")).String()
	v = &view{key: "ns/job", project: "show", mem: &jobMem{out: newOutput(), outLoad: missing, outView: in.String()}}
	if q.foldChangeset(ctx, v, Fold{Unit: 3, Changeset: cs}) {
		t.Fatal("a fold onto an output that cannot be loaded succeeded")
	}
	if !strings.Contains(v.foldWait, "unit 3, load output") {
		t.Fatalf("an output that cannot be loaded: said %q", v.foldWait)
	}
	first := v.foldWait
	if q.foldChangeset(ctx, v, Fold{Unit: 4, Changeset: cs}) || v.foldWait != first {
		t.Fatalf("a second failure in the tick replaced the first: %q", v.foldWait)
	}
}

// A fold that is only late says so, and stops saying so once it is made.
func TestAFoldThatIsOnlyLateStopsBeingACondition(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "bon", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a1"}, {"name": "b"}]}`)
	finishUnit(t, now, ns, "bon", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	finishUnit(t, now, ns, "bon", 1, map[string]string{c4v1.AnnotationOutputChangeset: writeChangeset(t, st, "a1.txt", "unit one")})
	away := true
	q.Stores = func(context.Context, string) (*storeurl.Sharded, error) {
		pc := make([]uintptr, 32)
		frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
		for away {
			f, more := frames.Next()
			if strings.HasSuffix(f.Function, "(*Queue).foldChangeset") {
				return nil, errors.New("the store is away")
			}
			if !more {
				break
			}
		}
		return st, nil
	}
	tick(t, q)
	late := get(t, ns, "bon")
	if c := meta.FindStatusCondition(late.Status.Conditions, ConditionOutputRecorded); c == nil || c.Reason != "FoldFailed" || !strings.Contains(c.Message, "unit 1") || !strings.Contains(c.Message, "the store is away") || late.Status.Output != nil {
		t.Fatalf("with the store away for the fold: output %+v, conditions %+v", late.Status.Output, late.Status.Conditions)
	}
	away = false
	tick(t, q)
	if got := get(t, ns, "bon"); got.Status.Output == nil || got.Status.Output.Changesets != 1 || meta.FindStatusCondition(got.Status.Conditions, ConditionOutputRecorded) != nil {
		t.Fatalf("once the fold is made: output %+v, conditions %+v", got.Status.Output, got.Status.Conditions)
	}
}

// Computing a status never edits the job it is computed from. A condition
// already on the job, set again with another reason, must come back changed
// in the new status and unchanged on the job, or the two compare equal and
// the change is never written.
func TestAStatusDoesNotEditTheJobItIsComputedFrom(t *testing.T) {
	q := &Queue{}
	was := metav1.Condition{Type: ConditionOutputRecorded, Status: metav1.ConditionFalse, Reason: "IndexNotSaved", Message: "the index could not be saved", LastTransitionTime: metav1.NewTime(time.Unix(1700000000, 0))}
	j := &c4v1.WorkJob{}
	j.Status.Conditions = []metav1.Condition{was}
	v := &view{key: "ns/job", job: j, mem: &jobMem{}, foldWait: "unit 2, fold: refused"}
	st := q.status(time.Unix(1700000100, 0), sched.Decisions{}, v)
	got := meta.FindStatusCondition(st.Conditions, ConditionOutputRecorded)
	if got == nil || got.Reason != "FoldFailed" || !strings.Contains(got.Message, "unit 2, fold: refused") {
		t.Fatalf("the new status says %+v", got)
	}
	if !got.LastTransitionTime.Equal(&was.LastTransitionTime) {
		t.Errorf("the condition's time moved though the output stayed behind: %s", got.LastTransitionTime.Time)
	}
	if on := j.Status.Conditions[0]; on.Reason != "IndexNotSaved" || on.Message != was.Message {
		t.Fatalf("computing the status edited the job's own condition to %+v", on)
	}
}

// Every fold of a job is superseded. Its output is then the job's input
// view, and its chain that view alone: a stored object that reads, and
// that the next fold builds on after a restart. The second half is the
// state the rebuild once left with no view at all: an output restored from
// such a chain (a view, and no sections) whose fold log still lists the
// superseded folds.
func TestRefoldWithEveryFoldSuperseded(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }, Log: logr.Discard()}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	m := &jobMem{out: newOutput(), superseded: map[int]bool{0: true}}
	v := &view{key: "ns/job", project: "show", mem: m}
	cs := unitChangeset(t, st, in, map[string]string{"a.txt": "a"})
	if err := q.fold(ctx, st, m.out, Fold{Unit: 0, Changeset: cs, Run: "pod-0"}); err != nil {
		t.Fatal(err)
	}
	if !q.refoldOutput(ctx, v) {
		t.Fatal("the refold asked to be tried again")
	}
	o := m.out
	if o.view != in || len(o.folds) != 0 {
		t.Fatalf("with every fold superseded the output is %s with %d folds, want the input %s and none", o.view, len(o.folds), in)
	}
	checkChain(t, st, o, 0)

	// A restart, then another unit's fold: the chain is that changeset.
	re := newOutput()
	if err := re.load(st, o.id.String(), o.view.String()); err != nil {
		t.Fatalf("a chain that is its base alone does not load: %v", err)
	}
	next := unitChangeset(t, st, in, map[string]string{"b.txt": "b"})
	if err := q.fold(ctx, st, re, Fold{Unit: 1, Changeset: next, Run: "pod-1"}); err != nil {
		t.Fatal(err)
	}
	nextID, _ := c4.Parse(next)
	want, _ := readChain(t, st, nextID)
	if stored, _ := readChain(t, st, re.id); !bytes.Equal(stored, want) {
		t.Fatalf("the chain after a fold onto a base alone is not that changeset:\n%s", stored)
	}

	// Restored with no sections, and a log that still lists the superseded
	// fold: rebuilt, the output keeps its view.
	m2 := &jobMem{out: newOutput(), superseded: map[int]bool{0: true}}
	if err := m2.out.load(st, o.id.String(), o.view.String()); err != nil {
		t.Fatal(err)
	}
	m2.out.folds = []Fold{{Unit: 0, Changeset: cs, Run: "pod-0"}}
	if !q.refoldOutput(ctx, &view{key: "ns/job2", project: "show", mem: m2}) {
		t.Fatal("the refold asked to be tried again")
	}
	if m2.out.view != in || len(m2.out.folds) != 0 {
		t.Fatalf("rebuilt from a chain with no sections, the output is %s with %d folds, want %s and none", m2.out.view, len(m2.out.folds), in)
	}
	checkChain(t, st, m2.out, 0)
}

// The chain is stored in blocks, one fold's section each: past
// chainRestEvery blocks the next rests on the view before it, so a head
// resolves within that many reads. At every fold, the head resolved
// through the c4m library (every block's bytes checked) is exactly the
// view the whole chain of the same folds, as stored before blocks,
// resolves to.
func TestTheChainIsBlocksThatResolveAsTheWholeChain(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	const n = chainRestEvery + 6
	whole := &c4view.Chain{Base: in}
	for i := 0; i < n; i++ {
		cs := unitChangeset(t, st, in, map[string]string{fmt.Sprintf("d/e/f%03d.txt", i): fmt.Sprint(i)})
		before := o.view
		if before.IsNil() {
			before = in
		}
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: cs, Run: fmt.Sprintf("pod-%d", i)}); err != nil {
			t.Fatal(err)
		}
		// The whole chain as it was stored before blocks: this fold's
		// section joins the ones before.
		_, blk := readChain(t, st, o.id)
		if len(blk.Sections) != 1 {
			t.Fatalf("fold %d: the head holds %d sections", i, len(blk.Sections))
		}
		sec := blk.Sections[0]
		sec.Base = before
		whole.Sections = append(whole.Sections, sec)
		enc, err := whole.Encode()
		if err != nil {
			t.Fatal(err)
		}
		wholeID, err := st.PutBytes(ctx, enc)
		if err != nil {
			t.Fatal(err)
		}
		if h, w := resolveHead(t, st, o.id), resolveHead(t, st, wholeID); h != w || h != o.view {
			t.Fatalf("fold %d: the head resolves to %s, the whole chain to %s, the output is %s", i, h, w, o.view)
		}
	}
	// From the head back to a view: at most chainRestEvery blocks.
	blocks, cur := 0, o.id
	for {
		data, err := c4view.ReadObject(st, cur)
		if err != nil {
			t.Fatal(err)
		}
		ch, err := c4view.ReadChain(data)
		if err != nil {
			break
		}
		blocks++
		cur = ch.Base
	}
	if blocks < 1 || blocks > chainRestEvery {
		t.Fatalf("%d blocks from the head back to a view, want 1 to %d", blocks, chainRestEvery)
	}
}

// A block holding more than one section resolves each against the state
// the ones before it made. c4 before 80b7dda (19c97485, pinned until 2026-11-12) applied a
// block's later sections against its first section alone; c4 80b7dda
// (#136) fixed that, and the pin now carries it. The operator writes one
// section per block all the same.
func TestABlockOfTwoSectionsResolves(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	var secs []c4view.Section
	for i := 0; i < 2; i++ {
		before := o.view
		if before.IsNil() {
			before = in
		}
		// The first rewrites the base's in.txt, the second removes it (its
		// section restates the rewritten in.txt exactly). In order against
		// the base, in.txt is gone; the second against the first alone
		// removes the rewrite, and the base's in.txt comes back (#136's case).
		cs := unitChangeset(t, st, in, map[string]string{"in.txt": "rewritten"})
		if i == 1 {
			cs = unitChangeset(t, st, in, nil, "in.txt")
		}
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: cs, Run: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		_, blk := readChain(t, st, o.id)
		sec := blk.Sections[0]
		sec.Base = before
		secs = append(secs, sec)
	}
	two, err := (&c4view.Chain{Base: in, Sections: secs}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.PutBytes(ctx, two)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveHead(t, st, id); got != o.view {
		t.Fatalf("a block of two sections resolves to %s, the output is %s", got, o.view)
	}
}

// The same two folds as the operator stores them, one block each, resolve
// right on the pinned library: each block's one section applies against
// the whole state the blocks before it make.
func TestOneSectionBlocksResolveTheCaseTwoSectionsGetWrong(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	for i, cs := range []string{
		unitChangeset(t, st, in, map[string]string{"in.txt": "rewritten"}),
		unitChangeset(t, st, in, nil, "in.txt"),
	} {
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: cs, Run: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := resolveHead(t, st, o.id); got != o.view || o.view != c4view.EmptyID() {
		t.Fatalf("the head resolves to %s, the output is %s (want the empty view)", got, o.view)
	}
}

// A refold after a restart, with every fold superseded, starts from the
// job's input view: the chain's head no longer reaches it (blocks rest on
// views), so the index keeps it (Index.Input).
func TestARefoldAfterARestartStartsFromTheInput(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }, Log: logr.Discard()}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	for i, name := range []string{"a.txt", "b.txt"} {
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: unitChangeset(t, st, in, map[string]string{name: name}), Run: fmt.Sprint("pod-", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.saveIndex(ctx, st, "ns/job"); err != nil {
		t.Fatal(err)
	}
	// The restart: the chain and view from the status, then the index.
	re := newOutput()
	if err := re.load(st, o.id.String(), o.view.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := re.loadIndex(ctx, st, o.index.String(), "ns/job", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if re.input != in {
		t.Fatalf("after a restart the input is %s, want %s", re.input, in)
	}
	m := &jobMem{out: re, superseded: map[int]bool{0: true, 1: true}}
	v := &view{key: "ns/job", project: "show", mem: m}
	if !q.refoldOutput(ctx, v) {
		t.Fatal("the refold asked to be tried again")
	}
	if m.out.view != in || len(m.out.folds) != 0 {
		t.Fatalf("with every fold superseded after a restart the output is %s, want the input %s", m.out.view, in)
	}
}

// The job record's chain is the whole history: every block a job's folds
// ever stored is reachable from it (each head a rest cut off, then the
// head, each walked back to the view it rests on), across restarts, which
// rest the chain; and the head alone, the read path, resolves within
// chainRestEvery blocks.
func TestTheHistoryReachesEveryBlock(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	stored := map[c4.ID]bool{}
	for i := 0; i < 200; i++ {
		cs := unitChangeset(t, st, in, map[string]string{fmt.Sprintf("d%d/f%03d.txt", i/50, i): fmt.Sprint(i)})
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: cs, Run: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		stored[o.id] = true
		if i%37 == 36 { // a restart: the chain and view from the status, then the index
			if err := o.saveIndex(ctx, st, "ns/job"); err != nil {
				t.Fatal(err)
			}
			re := newOutput()
			if err := re.load(st, chainField+o.id.String(), o.view.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := re.loadIndex(ctx, st, o.index.String(), "ns/job", nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			re.count, re.folded, re.writer = o.count, o.folded, o.writer
			o = re
		}
	}
	histID, err := st.PutBytes(ctx, o.historyBytes())
	if err != nil {
		t.Fatal(err)
	}
	data, err := c4view.ReadObject(st, histID)
	if err != nil || !c4m.IsIDListContent(data) {
		t.Fatalf("the history is not an ID list: %v\n%s", err, data)
	}
	heads, err := readIDList(st, histID)
	if err != nil {
		t.Fatal(err)
	}
	reached := map[c4.ID]bool{}
	for _, h := range heads {
		for cur := h; ; {
			d, err := c4view.ReadObject(st, cur)
			if err != nil {
				t.Fatal(err)
			}
			ch, err := c4view.ReadChain(d)
			if err != nil {
				break // a view
			}
			reached[cur] = true
			cur = ch.Base
		}
	}
	for b := range stored {
		if !reached[b] {
			t.Fatalf("block %s is not reachable from the job's history (%d heads)", b, len(heads))
		}
	}
	n := 0
	for cur := o.id; ; n++ {
		d, _ := c4view.ReadObject(st, cur)
		ch, err := c4view.ReadChain(d)
		if err != nil {
			break
		}
		cur = ch.Base
	}
	if n < 1 || n > chainRestEvery || resolveHead(t, st, o.id) != o.view {
		t.Fatalf("the head reads %d blocks back to a view (at most %d), and must resolve to the output", n, chainRestEvery)
	}
}

// A manager from before blocks reads the status's chain as a C4 ID: the
// field is marked so it refuses it (its fold waits, FoldFailed) rather than
// taking a block for a whole chain and appending to it, which resolves
// wrong. The current reader takes the field as it is written.
func TestAManagerFromBeforeBlocksRefusesTheChain(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	o := newOutput()
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: unitChangeset(t, st, in, map[string]string{"a.txt": "a"}), Run: "0"}); err != nil {
		t.Fatal(err)
	}
	field := chainField + o.id.String()
	if _, err := c4.Parse(field); err == nil {
		t.Fatalf("the status's chain %q parses as a C4 ID, so a manager from before blocks would take it", field)
	}
	re := newOutput()
	if err := re.load(st, field, o.view.String()); err != nil || re.id != o.id {
		t.Fatalf("the current reader does not take the field it writes: %v", err)
	}
}
