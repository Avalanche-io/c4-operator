package queue

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// oldIndex and oldFold are the provenance index as it was stored before
// folds named their runs: no version, and a fold is its unit, changeset
// and task record.
type oldIndex struct {
	Job     string            `json:"job"`
	Output  string            `json:"output"`
	Records map[string]string `json:"records"`
	Units   map[int]string    `json:"units"`
	Folds   []oldFold         `json:"folds,omitempty"`
}

type oldFold struct {
	Unit      int    `json:"unit"`
	Changeset string `json:"changeset"`
	Record    string `json:"record,omitempty"`
}

// recordSigner signs task records as a node of a test farm.
type recordSigner struct {
	ca     *provenance.CA
	roots  *x509.CertPool
	signer *provenance.Signer
	now    time.Time
}

// as is the same farm signing under another identity.
func (r *recordSigner) as(t *testing.T, identity string) *recordSigner {
	t.Helper()
	s, err := provenance.NewSigner(context.Background(), provenance.LocalIssuer(r.ca, identity, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return &recordSigner{ca: r.ca, roots: r.roots, signer: s, now: r.now}
}

func newRecordSigner(t *testing.T) *recordSigner {
	t.Helper()
	now := time.Now()
	ca, err := provenance.NewCA(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s, err := provenance.NewSigner(context.Background(), provenance.LocalIssuer(ca, provenance.NodeIdentity("node-1"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	return &recordSigner{ca: ca, roots: roots, signer: s, now: now}
}

// record stores the task record a node signs for the pod uid's capture of
// changeset, as the node does, and returns its C4 ID.
func (r *recordSigner) record(t *testing.T, st *storeurl.Sharded, uid, changeset string) string {
	t.Helper()
	pred := provenance.TaskPredicate{
		Invocation: provenance.Invocation{Project: "show", Namespace: "ns", Pod: "pod-of-" + uid, PodUID: uid},
		Output:     provenance.Output{Changeset: changeset},
		Runtime:    provenance.Runtime{Node: "node-1"},
	}
	env, err := provenance.SignStatement(context.Background(), r.signer, provenance.PredicateTask, nil, pred, r.now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.PutBytes(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

// storeOld stores o's index in the old form and returns its ID. (Its chain
// is in the store already.)
func storeOld(t *testing.T, st *storeurl.Sharded, o *output) (indexID string) {
	t.Helper()
	old := oldIndex{Job: "job", Output: o.view.String(), Records: o.records, Units: o.units}
	for _, f := range o.folds {
		old.Folds = append(old.Folds, oldFold{Unit: f.Unit, Changeset: f.Changeset, Record: f.Record})
	}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "version") || strings.Contains(string(b), `"run"`) {
		t.Fatalf("the old form carries a version or a run: %s", b)
	}
	id, err := st.PutBytes(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

// restored is a new output with o's chain and view, as a manager started
// after the upgrade restores one before it loads the index.
func restored(t *testing.T, st *storeurl.Sharded, o *output) *output {
	t.Helper()
	re := newOutput()
	if err := re.load(st, o.id.String(), o.view.String()); err != nil {
		t.Fatal(err)
	}
	return re
}

// history folds A (unit 0) then B (unit 1), which write one path
// differently, the way the manager before the upgrade did: with task
// records and no runs. It returns the output and the two folds as their
// pods would be seen again.
func history(t *testing.T, st *storeurl.Sharded, q *Queue, recA, recB func(cs string) string) (o *output, a, b Fold) {
	t.Helper()
	ctx := context.Background()
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	csA := unitChangeset(t, st, in, map[string]string{"f.txt": "from A", "a.txt": "a"})
	csB := unitChangeset(t, st, in, map[string]string{"f.txt": "from B"})
	a = Fold{Unit: 0, Changeset: csA, Record: recA(csA), Run: "pod-a"}
	b = Fold{Unit: 1, Changeset: csB, Record: recB(csB), Run: "pod-b"}
	o = newOutput()
	for _, f := range []Fold{a, b} {
		f.Run = "" // the old manager knew no runs
		if err := q.fold(ctx, st, o, f); err != nil {
			t.Fatal(err)
		}
	}
	if files := outputFilesOf(t, st, o.view); files["f.txt"] != idOf("from B") {
		t.Fatalf("before the upgrade f.txt is not B's: %v", files)
	}
	return o, a, b
}

// An index from before folds named their runs gets them from its own task
// records. After the upgrade, a finished pod that is still listed is known
// as folded and is not folded again: B's later write stands, nothing is
// reported, and the index is saved once with its version and its runs.
// That is so with the farm's roots (the record verified) and without.
func TestAnIndexFromBeforeRunsTakesThemFromItsRecords(t *testing.T) {
	ctx := context.Background()
	for _, verified := range []bool{true, false} {
		name := "records verified"
		if !verified {
			name = "no roots to verify with"
		}
		t.Run(name, func(t *testing.T) {
			st, _ := foldStore(t)
			q := &Queue{}
			rs := newRecordSigner(t)
			roots := rs.roots
			if !verified {
				roots = nil
			}
			o, a, b := history(t, st, q,
				func(cs string) string { return rs.record(t, st, "pod-a", cs) },
				func(cs string) string { return rs.record(t, st, "pod-b", cs) })
			old := storeOld(t, st, o)

			re := restored(t, st, o)
			says, err := re.loadIndex(ctx, st, old, "job", roots)
			if err != nil || len(says) != 0 {
				t.Fatalf("loading the old index: %v, said %v", err, says)
			}
			if len(re.folds) != 2 || re.folds[0].Run != "pod-a" || re.folds[1].Run != "pod-b" || re.folds[0].BeforeRuns || re.folds[1].BeforeRuns {
				t.Fatalf("the folds did not get their runs: %+v", re.folds)
			}
			// Saved at once, in the new form, before any fold.
			if re.dirty || re.index.String() == old {
				t.Fatalf("the converted index was not saved: dirty %v, index %s", re.dirty, re.index)
			}
			data, err := c4view.ReadObject(st, re.index)
			if err != nil {
				t.Fatal(err)
			}
			var saved Index
			if err := json.Unmarshal(data, &saved); err != nil || saved.Version != indexVersion || len(saved.Folds) != 2 || saved.Folds[0].Run != "pod-a" {
				t.Fatalf("the saved index: %v, %s", err, data)
			}

			// Pod A, still listed, is seen again.
			if err := q.fold(ctx, st, re, a); err != nil {
				t.Fatal(err)
			}
			if files := outputFilesOf(t, st, re.view); files["f.txt"] != idOf("from B") || re.view != o.view {
				t.Fatalf("pod A was folded again over B's write: %v", files)
			}
			if len(re.folds) != 2 || len(re.conflicts) != 0 {
				t.Fatalf("%d folds, conflicts %v", len(re.folds), re.conflicts)
			}
			// So is pod B. And a later run of A is a fold like any other.
			if err := q.fold(ctx, st, re, b); err != nil || len(re.folds) != 2 {
				t.Fatalf("pod B seen again: %v, %d folds", err, len(re.folds))
			}
			later := a
			later.Run, later.Record = "pod-a2", rs.record(t, st, "pod-a2", a.Changeset)
			if err := q.fold(ctx, st, re, later); err != nil {
				t.Fatal(err)
			}
			if files := outputFilesOf(t, st, re.view); files["f.txt"] != idOf("from A") || len(re.folds) != 3 {
				t.Fatalf("a later run of A was not folded: %v, %d folds", files, len(re.folds))
			}

			// Loaded again, the index is in the new form and is left alone.
			if err := re.saveIndex(ctx, st, "job"); err != nil {
				t.Fatal(err)
			}
			again := restored(t, st, re)
			if says, err := again.loadIndex(ctx, st, re.index.String(), "job", roots); err != nil || len(says) != 0 || again.dirty || again.index != re.index {
				t.Fatalf("an index in the new form was converted again: %v, %v, dirty %v", err, says, again.dirty)
			}
		})
	}
}

// Where an old fold has no task record to say which pod it was of, nothing
// says whether a listed pod of that unit and changeset is its run or a
// later one. It is folded, and reported, once. The cases: no record at all
// (the node had no signer, or the signing failed); a record the store does
// not hold; an object under the record's ID that is not that object; a
// record signed by nobody the farm knows, or by a component of the farm
// that does not sign task records; a record of another changeset.
func TestAnOldFoldWithNoRecordToSayIsFoldedAndReported(t *testing.T) {
	ctx := context.Background()
	rs := newRecordSigner(t)
	other := newRecordSigner(t) // another farm's node
	for _, c := range []struct {
		name string
		rec  func(t *testing.T, st *storeurl.Sharded, dir, cs string) string
		says string // what loading says of the record, "" for nothing
	}{
		{"no record", func(*testing.T, *storeurl.Sharded, string, string) string { return "" }, ""},
		{"a record the store does not hold", func(*testing.T, *storeurl.Sharded, string, string) string {
			return c4.Identify(strings.NewReader("never stored")).String()
		}, ""},
		{"a wrong object under the record's ID", func(t *testing.T, st *storeurl.Sharded, dir, cs string) string {
			id := rs.record(t, st, "pod-a", cs)
			overwriteObject(t, dir, id, []byte("not the record"))
			return id
		}, "a wrong object under its task record"},
		{"a record no key of the farm signed", func(t *testing.T, st *storeurl.Sharded, _, cs string) string {
			return other.record(t, st, "pod-a", cs)
		}, "does not verify"},
		{"a record the farm's own HUD signed, which is not a node", func(t *testing.T, st *storeurl.Sharded, _, cs string) string {
			return rs.as(t, provenance.ComponentIdentity("hud")).record(t, st, "pod-a", cs)
		}, "its task record"},
		{"a record of another changeset", func(t *testing.T, st *storeurl.Sharded, _, cs string) string {
			return rs.record(t, st, "pod-a", c4.Identify(strings.NewReader("another changeset")).String())
		}, "is of another changeset"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, dir := foldStore(t)
			q := &Queue{}
			o, a, _ := history(t, st, q,
				func(cs string) string { return c.rec(t, st, dir, cs) },
				func(cs string) string { return rs.record(t, st, "pod-b", cs) })
			old := storeOld(t, st, o)
			re := restored(t, st, o)
			says, err := re.loadIndex(ctx, st, old, "job", rs.roots)
			if err != nil {
				t.Fatal(err)
			}
			if c.says == "" && len(says) != 0 || c.says != "" && (len(says) != 1 || !strings.Contains(says[0], c.says) || !strings.Contains(says[0], "unit 0")) {
				t.Fatalf("loading said %q, want it to say %q of unit 0", says, c.says)
			}
			if !re.folds[0].BeforeRuns || re.folds[0].Run != "" || re.folds[1].Run != "pod-b" {
				t.Fatalf("the folds: %+v", re.folds)
			}
			// Pod A seen again: folded, since nothing says it was, and said.
			if err := q.fold(ctx, st, re, a); err != nil {
				t.Fatal(err)
			}
			if files := outputFilesOf(t, st, re.view); files["f.txt"] != idOf("from A") || len(re.folds) != 3 {
				t.Fatalf("pod A was not folded: %v, %d folds", files, len(re.folds))
			}
			upgrade := 0
			for _, line := range re.conflicts {
				if strings.Contains(line, "unit 0 was folded again after an upgrade") {
					upgrade++
				}
			}
			if upgrade != 1 {
				t.Fatalf("reported %d times, want once: %q", upgrade, re.conflicts)
			}
			// A run after that is not reported again, and the same pod is
			// not folded a third time.
			n := len(re.conflicts)
			if err := q.fold(ctx, st, re, a); err != nil || len(re.folds) != 3 {
				t.Fatalf("the same pod again: %v, %d folds", err, len(re.folds))
			}
			a.Run = "pod-a3"
			if err := q.fold(ctx, st, re, a); err != nil || len(re.folds) != 4 || len(re.conflicts) != n {
				t.Fatalf("a later run: %v, %d folds, conflicts %q", err, len(re.folds), re.conflicts[n:])
			}
		})
	}
}

// overwriteObject replaces the stored object id with other bytes, as a
// disk fault or a wrong write would leave it.
func overwriteObject(t *testing.T, dir, id string, data []byte) {
	t.Helper()
	p := objectFile(t, dir, id)
	os.Chmod(p, 0o644)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// objectFile finds the file a tree store keeps the object id in.
func objectFile(t *testing.T, dir, id string) string {
	t.Helper()
	want, _ := c4.Parse(id)
	found := ""
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || found != "" {
			return nil
		}
		if f, err := os.Open(p); err == nil {
			if c4.Identify(f) == want {
				found = p
			}
			f.Close()
		}
		return nil
	})
	if found == "" {
		t.Fatalf("the store holds no object %s", id)
	}
	return found
}

// A store that cannot be asked for a record settles nothing. The index is
// not converted, nothing of it is taken (so no fold of the job can be made
// on it), and nothing is saved; asked again once the store answers, it is
// converted whole, with the same result as if nothing had gone wrong. The
// same holds for a manager that stopped part way: it starts over from the
// old index. A save that fails is the same again.
func TestAnIndexIsConvertedWholeOrNotAtAll(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a file it has no permission to read")
	}
	ctx := context.Background()
	st, dir := foldStore(t)
	q := &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	rs := newRecordSigner(t)
	var recB string
	o, a, _ := history(t, st, q,
		func(cs string) string { return rs.record(t, st, "pod-a", cs) },
		func(cs string) string { recB = rs.record(t, st, "pod-b", cs); return recB })
	old := storeOld(t, st, o)

	// The first record reads; the second cannot be read at all.
	locked := objectFile(t, dir, recB)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	re := restored(t, st, o)
	says, err := re.loadIndex(ctx, st, old, "job", rs.roots)
	if !errors.Is(err, errIndexNotConverted) || len(says) != 0 {
		t.Fatalf("with a record that cannot be read: %v, said %v", err, says)
	}
	if len(re.folds) != 0 || !re.index.IsNil() || re.dirty {
		t.Fatalf("something of the index was taken: %d folds, index %s, dirty %v", len(re.folds), re.index, re.dirty)
	}
	// Through the queue: the fold of a finished pod is not made, and the
	// job says why with a reason of its own.
	m := &jobMem{out: newOutput(), outLoad: o.id.String(), outView: o.view.String(), outIndex: old}
	v := &view{key: "ns/job", project: "show", mem: m}
	q.Roots = rs.roots
	if q.foldChangeset(ctx, v, a) {
		t.Fatal("a fold was made on an index that is not converted")
	}
	if v.foldReason != "IndexNotConverted" || !strings.Contains(v.foldWait, "task record") || m.outIndex != old {
		t.Fatalf("the job says %q (%s), and still has the old index to load: %v", v.foldWait, v.foldReason, m.outIndex == old)
	}
	if files := outputFilesOf(t, st, m.out.view); files["f.txt"] != idOf("from B") {
		t.Fatalf("the output moved: %v", files)
	}

	// The store answers again: the same job, the next tick.
	if err := os.Chmod(locked, 0o644); err != nil {
		t.Fatal(err)
	}
	v = &view{key: "ns/job", project: "show", mem: m}
	if !q.foldChangeset(ctx, v, a) || v.foldWait != "" {
		t.Fatalf("once the store answers: %q", v.foldWait)
	}
	if len(m.out.folds) != 2 || m.out.folds[0].Run != "pod-a" || m.out.folds[1].Run != "pod-b" || m.outIndex != "" {
		t.Fatalf("the index was not converted whole: %+v", m.out.folds)
	}
	if files := outputFilesOf(t, st, m.out.view); files["f.txt"] != idOf("from B") {
		t.Fatalf("pod A was folded again: %v", files)
	}
	first := m.out.index

	// A manager that starts over from the old index converts it to the
	// same index, byte for byte.
	re = restored(t, st, o)
	if _, err := re.loadIndex(ctx, st, old, "ns/job", rs.roots); err != nil || re.index != first {
		t.Fatalf("converted again: %v, index %s, was %s", err, re.index, first)
	}

	// A save that fails: nothing is taken either. (The converted index is
	// in the store already, so the job is given another name to make the
	// save a new object.)
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
	re = restored(t, st, o)
	_, err = re.loadIndex(ctx, st, old, "another-job", rs.roots)
	for _, d := range dirs {
		os.Chmod(d, 0o755)
	}
	if !errors.Is(err, errIndexNotConverted) || len(re.folds) != 0 || !re.index.IsNil() || re.dirty {
		t.Fatalf("with a save that fails: %v, %d folds, index %s, dirty %v", err, len(re.folds), re.index, re.dirty)
	}
}

// An index in the new form is never converted: a reused computation's
// fold, which has a record and no run of its own on purpose, keeps none.
func TestAnIndexInTheNewFormIsNotConverted(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	rs := newRecordSigner(t)
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	cs := unitChangeset(t, st, in, map[string]string{"a.txt": "a"})
	o := newOutput()
	// A reuse: the record is the earlier run's, and there is no run.
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: cs, Record: rs.record(t, st, "pod-elsewhere", cs)}); err != nil {
		t.Fatal(err)
	}
	if err := o.saveIndex(ctx, st, "job"); err != nil {
		t.Fatal(err)
	}
	data, _ := c4view.ReadObject(st, o.index)
	if !bytes.Contains(data, []byte(`"version":2`)) {
		t.Fatalf("a saved index has no version: %s", data)
	}
	re := restored(t, st, o)
	if says, err := re.loadIndex(ctx, st, o.index.String(), "job", rs.roots); err != nil || len(says) != 0 {
		t.Fatal(err, says)
	}
	if len(re.folds) != 1 || re.folds[0].Run != "" || re.folds[0].BeforeRuns || re.dirty || re.index != o.index {
		t.Fatalf("a new-form index was touched: %+v, dirty %v", re.folds, re.dirty)
	}
}
