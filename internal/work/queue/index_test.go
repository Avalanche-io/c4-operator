package queue

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/work/sched"
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
	return r.recordOf(t, st, provenance.PredicateTask, provenance.Invocation{Project: "show", Namespace: "ns", Pod: "pod-of-" + uid, PodUID: uid}, changeset)
}

// recordOf stores a record of the given kind for an invocation, which may
// name the job and unit it ran as, the way a node does when it can still
// see the pod.
func (r *recordSigner) recordOf(t *testing.T, st *storeurl.Sharded, kind string, inv provenance.Invocation, changeset string) string {
	t.Helper()
	pred := provenance.TaskPredicate{
		Invocation: inv,
		Output:     provenance.Output{Changeset: changeset},
		Runtime:    provenance.Runtime{Node: "node-1"},
	}
	env, err := provenance.SignStatement(context.Background(), r.signer, kind, nil, pred, r.now)
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
			says, err := re.loadIndex(ctx, st, old, "job", roots, map[string]recordRun{}, nil)
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
			if says, err := again.loadIndex(ctx, st, re.index.String(), "job", roots, map[string]recordRun{}, nil); err != nil || len(says) != 0 || again.dirty || again.index != re.index {
				t.Fatalf("an index in the new form was converted again: %v, %v, dirty %v", err, says, again.dirty)
			}
		})
	}
}

// Where an old fold has no task record to say which pod it was of, nothing
// says whether a listed pod of that unit and changeset is its run or a
// later one. It is folded, and reported, once. The cases: no record at all
// (the node had no signer, or the signing failed); a record the store does
// not hold, and an object under the record's ID that is not that object,
// which is no record either (not present: the read is counted and logged,
// and nothing about it is said in the job); a record signed by nobody the
// farm knows, or by a component of the farm that does not sign task
// records; a record of another changeset.
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
		}, ""},
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
			var told []string
			was := c4view.OnMismatch
			t.Cleanup(func() { c4view.OnMismatch = was })
			c4view.OnMismatch = func(_ string, asked, _ c4.ID, _ error) { told = append(told, asked.String()) }
			defer func() {
				wrong := c.name == "a wrong object under the record's ID"
				if wrong != (len(told) > 0) {
					t.Errorf("reads told as wrong objects: %v (want some: %v)", told, wrong)
				}
			}()
			st, dir := foldStore(t)
			q := &Queue{}
			o, a, _ := history(t, st, q,
				func(cs string) string { return c.rec(t, st, dir, cs) },
				func(cs string) string { return rs.record(t, st, "pod-b", cs) })
			old := storeOld(t, st, o)
			re := restored(t, st, o)
			says, err := re.loadIndex(ctx, st, old, "job", rs.roots, map[string]recordRun{}, nil)
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
				if strings.Contains(line, "a run of unit 0 may have been folded twice after an upgrade") {
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
	says, err := re.loadIndex(ctx, st, old, "job", rs.roots, map[string]recordRun{}, nil)
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
	if _, err := re.loadIndex(ctx, st, old, "ns/job", rs.roots, map[string]recordRun{}, nil); err != nil || re.index != first {
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
	_, err = re.loadIndex(ctx, st, old, "another-job", rs.roots, map[string]recordRun{}, nil)
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
	if says, err := re.loadIndex(ctx, st, o.index.String(), "job", rs.roots, map[string]recordRun{}, nil); err != nil || len(says) != 0 {
		t.Fatal(err, says)
	}
	if len(re.folds) != 1 || re.folds[0].Run != "" || re.folds[0].BeforeRuns || re.dirty || re.index != o.index {
		t.Fatalf("a new-form index was touched: %+v, dirty %v", re.folds, re.dirty)
	}
}

// A fold made from an earlier run's record is a reused computation: it has
// no run of its own. In an old index it is told by its record, a good one
// that names another unit or another job, and it keeps no run. Given the
// earlier run's, a rebuild of the output would take it for that run folded
// twice and drop it.
func TestAReusedFoldInAnOldIndexKeepsNoRun(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	rs := newRecordSigner(t)
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	cs := unitChangeset(t, st, in, map[string]string{"f.txt": "computed once"})
	other := unitChangeset(t, st, in, map[string]string{"g.txt": "another"})
	// Unit 0 ran as pod-c; unit 1 reused that run; unit 2 reused a run of
	// another job; unit 3 ran as pod-d and is superseded later.
	recC := rs.recordOf(t, st, provenance.PredicateTask, provenance.Invocation{Namespace: "ns", Job: "job", Unit: "0", PodUID: "pod-c"}, cs)
	recElsewhere := rs.recordOf(t, st, provenance.PredicateTask, provenance.Invocation{Namespace: "ns", Job: "an-earlier-job", Unit: "2", PodUID: "pod-e"}, other)
	recD := rs.recordOf(t, st, provenance.PredicateTask, provenance.Invocation{Namespace: "ns", Job: "job", Unit: "3", PodUID: "pod-d"}, other)
	o := newOutput()
	for _, f := range []Fold{{Unit: 0, Changeset: cs, Record: recC}, {Unit: 1, Changeset: cs, Record: recC}, {Unit: 2, Changeset: other, Record: recElsewhere}, {Unit: 3, Changeset: other, Record: recD}} {
		if err := q.fold(ctx, st, o, f); err != nil {
			t.Fatal(err)
		}
	}
	old := storeOld(t, st, o)
	m := &jobMem{out: newOutput(), outLoad: o.id.String(), outView: o.view.String(), outIndex: old, superseded: map[int]bool{3: true}}
	v := &view{key: "ns/job", project: "show", mem: m}
	q.Roots = rs.roots
	if !q.refoldOutput(ctx, v) {
		t.Fatalf("the rebuild asked to be tried again: %s", v.foldWait)
	}
	var got []string
	for _, f := range m.out.folds {
		got = append(got, fmt.Sprintf("%d:%s:%v", f.Unit, f.Run, f.BeforeRuns))
	}
	if want := "[0:pod-c:false 1::false 2::false]"; fmt.Sprint(got) != want {
		t.Fatalf("after conversion and a rebuild the folds are %v, want %s", got, want)
	}
	if said := strings.Join(m.out.conflicts, "\n"); strings.Contains(said, "upgrade") {
		t.Fatalf("something was said of the upgrade: %q", m.out.conflicts)
	}
}

// An index with no version can hold both kinds of fold: an old manager's,
// with no run, and those a run-naming manager added before the index had a
// version. Every fold with no run is settled from its record, and one that
// has a run is left alone and its record not read: the old pod fold gets
// its run, and a reused computation's fold (its record names another unit)
// keeps none.
func TestAnIndexWithSomeRunsAndNoVersion(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	rs := newRecordSigner(t)
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	csA := unitChangeset(t, st, in, map[string]string{"f.txt": "from A"})
	csB := unitChangeset(t, st, in, map[string]string{"f.txt": "from B"})
	gone := c4.Identify(strings.NewReader("a record the store does not hold")).String()
	recA := rs.record(t, st, "pod-a", csA)
	reuse := rs.recordOf(t, st, provenance.PredicateTask, provenance.Invocation{Namespace: "ns", Job: "job", Unit: "1", PodUID: "pod-b"}, csB)
	o := newOutput()
	folds := []Fold{
		{Unit: 0, Changeset: csA, Record: recA},               // the old manager's: pod-a, no run
		{Unit: 1, Changeset: csB, Record: gone, Run: "pod-b"}, // named already; its record is not asked for
		{Unit: 2, Changeset: csB, Record: reuse},              // a reuse of unit 1's run
	}
	for _, f := range folds {
		if err := q.fold(ctx, st, o, f); err != nil {
			t.Fatal(err)
		}
	}
	// Stored as it stands: the folds, and no version.
	b, err := json.Marshal(struct {
		Job   string `json:"job"`
		Folds []Fold `json:"folds"`
	}{"ns/job", o.folds})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.PutBytes(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	re := restored(t, st, o)
	read, asked := map[string]recordRun{}, 0
	says, err := re.loadIndex(ctx, st, id.String(), "ns/job", rs.roots, read, &asked)
	if err != nil || len(says) != 0 || asked != 2 {
		t.Fatalf("%v, said %v, %d records asked for (want the two folds with no run)", err, says, asked)
	}
	var got []string
	for _, f := range re.folds {
		got = append(got, fmt.Sprintf("%d:%s:%v", f.Unit, f.Run, f.BeforeRuns))
	}
	if want := "[0:pod-a:false 1:pod-b:false 2::false]"; fmt.Sprint(got) != want || re.dirty || re.index == id {
		t.Fatalf("the folds are %v, want %s; dirty %v", got, want, re.dirty)
	}
	// Pod A, still listed, is not folded again over B's write.
	if err := q.fold(ctx, st, re, Fold{Unit: 0, Changeset: csA, Record: recA, Run: "pod-a"}); err != nil {
		t.Fatal(err)
	}
	if files := outputFilesOf(t, st, re.view); files["f.txt"] != idOf("from B") || len(re.folds) != 3 || len(re.conflicts) != 0 {
		t.Fatalf("pod A was folded again: %v, %d folds, conflicts %q", files, len(re.folds), re.conflicts)
	}
}

// The report of a fold made again after an upgrade is for a pod of that
// unit with that changeset, and for no other fold.
func TestTheUpgradeReportIsForThatUnitAndChangeset(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{}
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	cs := unitChangeset(t, st, in, map[string]string{"f.txt": "f"})
	another := unitChangeset(t, st, in, map[string]string{"g.txt": "g"})
	o := newOutput()
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: cs}); err != nil {
		t.Fatal(err)
	}
	o.folds[0].BeforeRuns = true
	said := func() int {
		n := 0
		for _, line := range o.conflicts {
			if strings.Contains(line, "after an upgrade") {
				n++
			}
		}
		return n
	}
	for _, f := range []Fold{
		{Unit: 1, Changeset: cs, Run: "pod-of-another-unit"},
		{Unit: 0, Changeset: another, Run: "pod-with-another-changeset"},
		{Unit: 0, Changeset: cs}, // a reused computation: no pod, so not that run
	} {
		if err := q.fold(ctx, st, o, f); err != nil {
			t.Fatal(err)
		}
		if said() != 0 || !o.folds[0].BeforeRuns {
			t.Fatalf("the fold %+v was taken for the old fold seen again", f)
		}
	}
	if err := q.fold(ctx, st, o, Fold{Unit: 0, Changeset: cs, Run: "pod-0"}); err != nil || said() != 1 || o.folds[0].BeforeRuns {
		t.Fatalf("the pod of that unit and changeset: %v, said %d times", err, said())
	}
}

// A record that is not a task record gives no run, with the farm's roots
// and without them. What is wrong with a record reaches the job's
// conflicts, and the wait has its reason in the job's status.
func TestARecordOfAnotherKindGivesNoRun(t *testing.T) {
	ctx := context.Background()
	for _, verified := range []bool{true, false} {
		st, _ := foldStore(t)
		q := &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
		rs := newRecordSigner(t)
		if verified {
			q.Roots = rs.roots
		}
		o, a, _ := history(t, st, q,
			func(cs string) string {
				return rs.recordOf(t, st, provenance.PredicateResolution, provenance.Invocation{PodUID: "pod-a"}, cs)
			},
			func(cs string) string { return rs.record(t, st, "pod-b", cs) })
		m := &jobMem{out: newOutput(), outLoad: o.id.String(), outView: o.view.String(), outIndex: storeOld(t, st, o)}
		v := &view{key: "ns/job", project: "show", mem: m}
		if !q.foldChangeset(ctx, v, a) {
			t.Fatalf("verified %v: %s", verified, v.foldWait)
		}
		if !strings.Contains(strings.Join(m.out.conflicts, "\n"), "not a task record") {
			t.Errorf("verified %v: the job's conflicts do not say what was wrong with the record: %q", verified, m.out.conflicts)
		}
		if len(m.out.folds) != 3 || m.out.folds[0].Run != "" {
			t.Errorf("verified %v: a record of another kind gave a run: %+v", verified, m.out.folds)
		}
	}

	// The reason reaches the status.
	q := &Queue{}
	j := &c4v1.WorkJob{}
	st := q.status(time.Unix(1700000100, 0), sched.Decisions{}, &view{key: "ns/job", job: j, mem: &jobMem{}, foldWait: "load output: not converted", foldReason: "IndexNotConverted"})
	if c := meta.FindStatusCondition(st.Conditions, ConditionOutputRecorded); c == nil || c.Reason != "IndexNotConverted" || !strings.Contains(c.Message, "not converted") {
		t.Fatalf("the status says %+v", c)
	}
}

// A long log is converted over several ticks: each reads a bounded number
// of records, what was read is kept, and the index is saved only when
// every fold is settled. The rebuild of the output waits the same way and
// says so.
func TestALongIndexIsConvertedOverSeveralTicks(t *testing.T) {
	ctx := context.Background()
	st, _ := foldStore(t)
	q := &Queue{Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	rs := newRecordSigner(t)
	in := inputView(t, st, map[string]string{"in.txt": "input"})
	n := 2*maxRecordReadsPerTick + 5
	o := newOutput()
	for i := range n {
		cs := unitChangeset(t, st, in, map[string]string{fmt.Sprintf("out/%03d.txt", i): fmt.Sprint(i)})
		if err := q.fold(ctx, st, o, Fold{Unit: i, Changeset: cs, Record: rs.record(t, st, fmt.Sprintf("pod-%03d", i), cs)}); err != nil {
			t.Fatal(err)
		}
	}
	old := storeOld(t, st, o)
	m := &jobMem{out: newOutput(), outLoad: o.id.String(), outView: o.view.String(), outIndex: old, superseded: map[int]bool{0: true}}
	q.Roots = rs.roots
	for tick := 1; ; tick++ {
		q.recordReads = 0 // as each tick begins
		v := &view{key: "ns/job", project: "show", mem: m}
		// However many folds of the tick ask, the tick reads its bound.
		if tick == 1 && (q.foldChangeset(ctx, v, Fold{Unit: 1, Changeset: o.folds[1].Changeset, Run: "pod-001"}) || len(m.outRuns) != maxRecordReadsPerTick) {
			t.Fatalf("a fold in the first tick: %d records kept", len(m.outRuns))
		}
		if q.refoldOutput(ctx, v) {
			if tick != 3 {
				t.Fatalf("converted in %d ticks, want 3", tick)
			}
			break
		}
		if v.foldReason != "IndexNotConverted" || !strings.Contains(v.foldWait, "settled so far") || m.outIndex != old || len(m.out.folds) != 0 {
			t.Fatalf("tick %d: %q (%s), %d folds taken", tick, v.foldWait, v.foldReason, len(m.out.folds))
		}
		if len(m.outRuns) != tick*maxRecordReadsPerTick {
			t.Fatalf("tick %d: %d records kept", tick, len(m.outRuns))
		}
		if tick > 5 {
			t.Fatal("it never finishes")
		}
	}
	if len(m.out.folds) != n-1 || m.out.folds[0].Run != "pod-001" || m.outRuns != nil {
		t.Fatalf("after the conversion and the rebuild: %d folds, first %+v", len(m.out.folds), m.out.folds[0])
	}
}

// Through the manager, across restarts: while an old index cannot be
// converted (a record cannot be read), the job's status goes on naming
// that index and says why it waits. A restart in that state finds the fold
// log where it was, and once the store answers, the pod still listed is
// not folded again: the later write stands.
func TestARestartWhileAnOldIndexWaits(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a file it has no permission to read")
	}
	for _, c := range []struct{ name, reason string }{
		{"a record cannot be read", "IndexNotConverted"},
		{"the index itself cannot be read", "FoldFailed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ns, q, now, _ := graphFarm(t)
			st, dir := foldStore(t)
			q.Stores = func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
			ctx := context.Background()
			if err := k8s.Create(ctx, job(ns, "bon", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
				t.Fatal(err)
			}
			tick(t, q)
			doc := putDoc(t, st, `{"version": 1, "units": [{"name": "a1"}, {"name": "b"}]}`)
			finishUnit(t, now, ns, "bon", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
			tick(t, q)

			// Unit 1's pod stays listed: a finalizer that is not the queue's.
			rs := newRecordSigner(t)
			p1 := podOfUnit(t, ns, "bon", 1)
			p1.Finalizers = append(p1.Finalizers, "test.c4.avalanche.io/hold")
			if err := k8s.Update(ctx, p1); err != nil {
				t.Fatal(err)
			}
			defer func() {
				var held corev1.Pod
				if k8s.Get(ctx, client.ObjectKeyFromObject(p1), &held) == nil {
					held.Finalizers = nil
					k8s.Update(ctx, &held)
				}
			}()
			csA := unitChangeset(t, st, c4view.EmptyID(), map[string]string{"f.txt": "from A"})
			csB := unitChangeset(t, st, c4view.EmptyID(), map[string]string{"f.txt": "from B"})
			recA := rs.record(t, st, string(p1.UID), csA)
			recB := rs.record(t, st, string(podOfUnit(t, ns, "bon", 2).UID), csB)
			finishUnit(t, now, ns, "bon", 1, map[string]string{c4v1.AnnotationOutputChangeset: csA, c4v1.AnnotationOutputProvenance: recA})
			tick(t, q)
			finishUnit(t, now, ns, "bon", 2, map[string]string{c4v1.AnnotationOutputChangeset: csB, c4v1.AnnotationOutputProvenance: recB})
			tick(t, q)
			tick(t, q)
			got := get(t, ns, "bon")
			if got.Status.Output == nil || got.Status.Output.Changesets != 2 || got.Status.Output.Provenance == "" {
				t.Fatalf("before the upgrade: %+v", got.Status.Output)
			}

			// The status's index as the manager before the upgrade stored it.
			ixID, _ := c4.Parse(got.Status.Output.Provenance)
			head, err := ReadIndex(st, ixID) // the whole index, its segments folded
			if err != nil {
				t.Fatal(err)
			}
			cur := *head
			old := oldIndex{Job: cur.Job, Output: cur.Output, Records: cur.Records, Units: cur.Units}
			for _, f := range cur.Folds {
				old.Folds = append(old.Folds, oldFold{Unit: f.Unit, Changeset: f.Changeset, Record: f.Record})
			}
			ob, _ := json.Marshal(old)
			oldID, err := st.PutBytes(ctx, ob)
			if err != nil {
				t.Fatal(err)
			}
			got.Status.Output.Provenance = oldID.String()
			if err := k8s.Status().Update(ctx, got); err != nil {
				t.Fatal(err)
			}

			locked := objectFile(t, dir, recB)
			if c.reason == "FoldFailed" {
				locked = objectFile(t, dir, oldID.String())
			}
			if err := os.Chmod(locked, 0); err != nil {
				t.Fatal(err)
			}
			restart := func() {
				q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores, Roots: rs.roots}
			}
			// The upgraded manager starts while the store cannot answer.
			restart()
			tick(t, q)
			tick(t, q)
			mid := get(t, ns, "bon")
			if mid.Status.Output == nil || mid.Status.Output.Provenance != oldID.String() {
				t.Fatalf("while the index waits the status names %+v, want the old index %s", mid.Status.Output, oldID)
			}
			if cond := meta.FindStatusCondition(mid.Status.Conditions, ConditionOutputRecorded); cond == nil || cond.Reason != c.reason {
				t.Fatalf("while the index waits the status says %+v, want %s", mid.Status.Conditions, c.reason)
			}

			// It restarts again in that state, and then the store answers.
			if err := os.Chmod(locked, 0o644); err != nil {
				t.Fatal(err)
			}
			restart()
			// As if the tick before had spent the whole bound on record
			// reads: each tick starts its count again, or a conversion
			// would stop for good after its first 64.
			q.recordReads = maxRecordReadsPerTick
			tick(t, q)
			tick(t, q)
			end := get(t, ns, "bon")
			if end.Status.Output == nil || end.Status.Output.Changesets != 2 {
				t.Fatalf("at the end: %+v", end.Status.Output)
			}
			out, _ := c4.Parse(end.Status.Output.View)
			if files := outputFilesOf(t, st, out); files["f.txt"] != idOf("from B") {
				t.Fatalf("the pod still listed was folded again over the later write: %v", files)
			}
			if meta.FindStatusCondition(end.Status.Conditions, ConditionOutputRecorded) != nil {
				t.Fatalf("the status still says it waits: %+v", end.Status.Conditions)
			}
			id, _ := c4.Parse(end.Status.Output.Provenance)
			ix, err := ReadIndex(st, id)
			if err != nil || ix.Version != indexVersion || len(ix.Folds) != 2 || ix.Folds[0].Run != string(p1.UID) {
				t.Fatalf("the index the status names at the end: %v, %+v", err, ix)
			}
		})
	}
}

// A job's provenance index is stored as segments, each holding what one
// save changed and naming the one before, with the whole index again every
// indexCheckpointEvery segments: read from its head it is the whole index,
// every fold and every path's record, and a reader follows at most
// indexCheckpointEvery segments. Before, every fold stored the whole index
// again (a job folded n times stored about n²/2 folds' worth).
func TestTheIndexIsStoredInSegments(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, job(ns, "segs", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	const n = indexCheckpointEvery + 6
	var units []string
	for i := 1; i <= n; i++ {
		units = append(units, fmt.Sprintf(`{"name": "u%d"}`, i))
	}
	doc := putDoc(t, st, `{"version": 1, "units": [`+strings.Join(units, ", ")+`]}`)
	finishUnit(t, now, ns, "segs", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	for i := 1; i <= n; i++ {
		for try := 0; ; try++ {
			running := false
			for _, p := range unitPods(t, ns, "segs") {
				running = running || (unitOf(p) == i && (p.Status.Phase == "" || p.Status.Phase == corev1.PodPending))
			}
			if running {
				break
			}
			if try > 200 {
				t.Fatalf("unit %d never launched", i)
			}
			tick(t, q)
		}
		finishUnit(t, now, ns, "segs", i, map[string]string{c4v1.AnnotationOutputChangeset: writeChangeset(t, st, fmt.Sprintf("out/f%03d.txt", i), fmt.Sprintf("unit %d\n", i))})
		tick(t, q)
	}
	tick(t, q)
	o := get(t, ns, "segs").Status.Output
	head, err := c4.Parse(o.Provenance)
	if err != nil {
		t.Fatal(err)
	}
	ix, segs, err := readIndex(st, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Folds) != n || ix.Output != o.View || ix.Prev != "" {
		t.Fatalf("read from its head: %d folds, output %s (the job's %s)", len(ix.Folds), ix.Output, o.View)
	}
	for i := 1; i <= n; i++ {
		if ix.Folds[i-1].Unit != i {
			t.Fatalf("fold %d is unit %d's", i-1, ix.Folds[i-1].Unit)
		}
	}
	if segs < 1 || segs > indexCheckpointEvery {
		t.Fatalf("%d segments after the whole index; a checkpoint comes every %d", segs, indexCheckpointEvery)
	}
	// The head itself holds one fold, not all of them.
	data, err := c4view.ReadObject(st, head)
	if err != nil {
		t.Fatal(err)
	}
	var one Index
	if err := json.Unmarshal(data, &one); err != nil || one.Prev == "" || len(one.Folds) != 1 {
		t.Fatalf("the head segment: prev %q, %d folds, %v", one.Prev, len(one.Folds), err)
	}
}
