package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// newNamespace makes a namespace of the test's own.
// nsSeq numbers the namespaces the package's tests make.
var nsSeq atomic.Int64

// newNamespace makes a namespace for one test, named so that no two are
// alike in a process: envtest never deletes one, and a name from the clock
// alone (once, the nanoseconds within the second, which macOS counts in
// microseconds) was made twice in 2 of 750 runs under -race, and the test
// failed on "already exists".
func newNamespace(t *testing.T, prefix string) string {
	t.Helper()
	ns := fmt.Sprintf("%s-%d-%d", prefix, nsSeq.Add(1), time.Now().UnixNano()%1e6)
	if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	return ns
}

// marshal writes a c4m of entries with the library.
func marshal(t *testing.T, entries ...*c4m.Entry) []byte {
	t.Helper()
	m := c4m.NewManifest()
	for _, e := range entries {
		m.AddEntry(e)
	}
	b, err := c4m.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fileEntry(depth int, name string, id c4.ID, size int64) *c4m.Entry {
	return &c4m.Entry{Name: name, Mode: 0o644, Size: size, Timestamp: c4m.NullTimestamp(), C4ID: id, Depth: depth}
}

func dirEntry(depth int, name string, size int64, id c4.ID) *c4m.Entry {
	return &c4m.Entry{Name: name, Mode: 0o755 | os.ModeDir, Size: size, Timestamp: c4m.NullTimestamp(), C4ID: id, Depth: depth}
}

// A grant matches only IDs computed from content: the reader's two
// forgeries of a granted tree's root lines are refused, and a plain
// description is granted by the view it resolves to, whatever the include.
func TestGrantsMatchOnlyComputedIDs(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	v := st.ingest(t, show("forge"))
	secret := []byte("the unreleased cut")
	secretID, err := st.store.PutBytes(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a c4m copying a granted view's root lines over another file is refused", func(t *testing.T) {
		ns := newNamespace(t, "forge-a")
		putProject(t, "forge-a", []string{ns}, st, c4v1.ScopeGrants, []c4v1.Grant{{Tree: v.ID.String()}})
		var top []*c4m.Entry
		for _, e := range v.Manifest().Entries {
			if e.Depth == 0 {
				cp := *e
				top = append(top, &cp)
			}
		}
		var forged []*c4m.Entry
		for _, e := range top {
			forged = append(forged, e)
			if e.Name == "editorial/" {
				forged = append(forged, fileEntry(1, "cut.mov", secretID, int64(len(secret))))
			}
		}
		putMat(t, ns, "forged", byConfigMap(t, ns, "forged", marshal(t, forged...)))
		waitMat(t, ns, "forged", c4v1.PhaseFailed, "NotGranted")
	})

	// Two descriptions whose top-level lines are the same, a/ null, and
	// whose content under a/ differs.
	f1, f2 := []byte("one\n"), []byte("two\n")
	id1, _ := st.store.PutBytes(ctx, f1)
	id2, _ := st.store.PutBytes(ctx, f2)
	hID, _ := st.store.PutBytes(ctx, []byte("h\n"))
	one := marshal(t, fileEntry(0, "h.txt", hID, 2), dirEntry(0, "a/", -1, c4.ID{}), fileEntry(1, "f.txt", id1, 4))
	two := marshal(t, fileEntry(0, "h.txt", hID, 2), dirEntry(0, "a/", -1, c4.ID{}), fileEntry(1, "f.txt", id2, 4))
	all, _ := view.NewFilter(nil)
	resOne, err := view.FromBytes(one, st.store, all)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a grant of one description does not grant another with the same root lines", func(t *testing.T) {
		ns := newNamespace(t, "forge-b")
		putProject(t, "forge-b", []string{ns}, st, c4v1.ScopeGrants, []c4v1.Grant{{Tree: resOne.View().ID.String()}})
		putMat(t, ns, "one", byConfigMap(t, ns, "one", one))
		waitMat(t, ns, "one", c4v1.PhaseReady, "Ready")
		putMat(t, ns, "two", byConfigMap(t, ns, "two", two))
		waitMat(t, ns, "two", c4v1.PhaseFailed, "NotGranted")
	})

	t.Run("a grant naming the shared root lines' ID grants neither", func(t *testing.T) {
		ns := newNamespace(t, "forge-c")
		putProject(t, "forge-c", []string{ns}, st, c4v1.ScopeGrants, []c4v1.Grant{{Tree: resOne.RootID.String()}})
		putMat(t, ns, "one", byConfigMap(t, ns, "one", one))
		waitMat(t, ns, "one", c4v1.PhaseFailed, "NotGranted")
	})

	t.Run("a plain description is granted by the view it resolves to, under an include", func(t *testing.T) {
		ns := newNamespace(t, "plain")
		putProject(t, "plain", []string{ns}, st, c4v1.ScopeGrants, []c4v1.Grant{{Tree: resOne.View().ID.String()}})
		putMat(t, ns, "a-only", byConfigMap(t, ns, "a-only", one), "a/")
		m := waitMat(t, ns, "a-only", c4v1.PhaseReady, "Ready")
		if m.Status.Files != 1 {
			t.Fatalf("%d files under the include a/, want 1", m.Status.Files)
		}
	})
}

// A store that serves the wrong bytes for an ID fails the grant check (a
// resolve failure); it never matches.
func TestGrantCheckRefusesBytesThatAreNotTheirID(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	v := st.ingest(t, show("swap"))
	frame := []byte("sh010 1005 swap")
	id, err := st.store.PutBytes(ctx, frame)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := view.Apply(ctx, v.ID, []view.Edit{{Path: "shots/sh010/plates/sh010.1005.exr", Entry: view.FileEntry("", int64(len(frame)), id, false)}}, view.StoreRecords(st.store))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range applied.Records {
		st.store.PutBytes(ctx, r.Data)
	}
	csID, err := st.store.PutBytes(ctx, applied.Changeset)
	if err != nil {
		t.Fatal(err)
	}
	// The base's root record, on disk, made to hold another record's bytes.
	var root string
	filepath.WalkDir(st.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) == v.ID.String() {
			root = p
		}
		return nil
	})
	if root == "" {
		t.Fatalf("the base's root record %s is not under %s", v.ID, st.dir)
	}
	if err := os.WriteFile(root, []byte("drwxr-xr-x - - x/ -\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ns := newNamespace(t, "swap")
	putProject(t, "swap", []string{ns}, st, c4v1.ScopeGrants, []c4v1.Grant{{Tree: v.ID.String()}})
	putMat(t, ns, "swapped", byID(csID))
	// It fails as a resolve that read bad bytes does, not as a grant that
	// did not match. The read that found them re-filed them under their
	// own ID (Joshua, 2026-11-11), so a retry finds the base's root simply
	// missing: either way the resolve is incomplete.
	m := waitMat(t, ns, "swapped", c4v1.PhaseFailed, "ResolveFailed")
	if !strings.Contains(readyMessage(m), "description is incomplete") {
		t.Fatalf("message %q", readyMessage(m))
	}
	moved := c4.Identify(strings.NewReader("drwxr-xr-x - - x/ -\n"))
	if b, err := view.ReadObject(st.store, moved); err != nil || string(b) != "drwxr-xr-x - - x/ -\n" {
		t.Fatalf("the wrong bytes under their own ID: %q, %v", b, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the base's root ID still holds them: %v", err)
	}
}

// countingStore counts the objects read through it.
type countingStore struct {
	view.Store
	n int
}

func (c *countingStore) Open(id c4.ID) (io.ReadCloser, error) {
	c.n++
	return c.Store.Open(id)
}

// A plain description's view is resolved once for grant lookup; a
// changeset over the empty tree makes the view its one section makes.
func TestGrantIDsUnit(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	hID, _ := st.store.PutBytes(ctx, []byte("h\n"))
	// A view's root record: resolving it reads its directories' records.
	v := st.ingest(t, show("memo"))
	desc, err := view.ReadObject(st.store, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := &MaterializationReconciler{}
	cs := &countingStore{Store: st.store}
	first, err := r.grantIDs(ctx, "memo", cs, desc, c4.Identify(bytes.NewReader(desc)))
	if err != nil {
		t.Fatal(err)
	}
	reads := cs.n
	if reads == 0 || len(first) != 3 || first[2] != v.ID {
		t.Fatalf("the first lookup read %d objects and gave %v; want reads and the view %s", reads, first, v.ID)
	}
	second, err := r.grantIDs(ctx, "memo", cs, desc, c4.Identify(bytes.NewReader(desc)))
	if err != nil {
		t.Fatal(err)
	}
	if cs.n != reads || first[2] != second[2] {
		t.Fatalf("a second lookup read %d objects more and gave %v, after %v", cs.n-reads, second, first)
	}
	// Another project's lookup of the same source reads its own store.
	if _, err := r.grantIDs(ctx, "another", cs, desc, c4.Identify(bytes.NewReader(desc))); err != nil {
		t.Fatal(err)
	}
	if cs.n == reads {
		t.Fatal("another project's lookup was answered from the first project's memo")
	}

	patch := c4m.NewManifest()
	patch.Base = view.EmptyID()
	patch.AddEntry(fileEntry(0, "h.txt", hID, 2))
	changeset, err := view.EncodeChangeset(view.EmptyID(), patch)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := r.grantIDs(ctx, "memo", st.store, changeset, c4.Identify(bytes.NewReader(changeset)))
	if err != nil {
		t.Fatal(err)
	}
	all, _ := view.NewFilter(nil)
	res, err := view.FromBytes(marshal(t, fileEntry(0, "h.txt", hID, 2)), st.store, all)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[1] != view.EmptyID() || ids[2] != res.View().ID {
		t.Fatalf("a changeset over the empty tree gave %v; its base %s and the view %s", ids, view.EmptyID(), res.View().ID)
	}
}

// The grant is matched against what the resolver mounts. A changeset over
// an ungranted view B (a granted G and one more file) that restates that
// file with another mode and date is a write, never a removal (c4m
// SPECIFICATION.md, "Patch Entry Semantics"), so it makes B, not G, and it
// is refused. Before the two patch readers shared one comparison,
// view.ApplyPatch read it as a removal, which would have made G.
func TestAGrantIsMatchedAgainstWhatIsMounted(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	g := st.ingest(t, show("restate"))
	secret := []byte("the unreleased cut")
	sid, err := st.store.PutBytes(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	b, err := view.Apply(ctx, g.ID, []view.Edit{{Path: "secret.mov", Entry: view.FileEntry("", int64(len(secret)), sid, false)}}, view.StoreRecords(st.store))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range b.Records {
		st.store.PutBytes(ctx, r.Data)
	}
	patch := c4m.NewManifest()
	patch.Base = b.ID
	patch.AddEntry(&c4m.Entry{Name: "secret.mov", Mode: 0o644, Size: int64(len(secret)), Timestamp: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), C4ID: sid})
	cs, err := view.EncodeChangeset(b.ID, patch)
	if err != nil {
		t.Fatal(err)
	}
	ns := newNamespace(t, "restate")
	putProject(t, "restate", []string{ns}, st, c4v1.ScopeGrants, []c4v1.Grant{{Tree: g.ID.String()}})
	putMat(t, ns, "restated", byConfigMap(t, ns, "restated", cs))
	waitMat(t, ns, "restated", c4v1.PhaseFailed, "NotGranted")
}

// A changeset whose base is itself a stored changeset (the resolver
// follows such a chain) is granted by the view it makes, as it was before.
func TestAChangesetOverAStoredChangesetIsGrantedByItsView(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	g := st.ingest(t, show("stack"))
	put := func(base c4.ID, path, body string) *view.Applied {
		id, err := st.store.PutBytes(ctx, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		a, err := view.Apply(ctx, base, []view.Edit{{Path: path, Entry: view.FileEntry("", int64(len(body)), id, false)}}, view.StoreRecords(st.store))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range a.Records {
			st.store.PutBytes(ctx, r.Data)
		}
		return a
	}
	one := put(g.ID, "shots/sh010/plates/sh010.1005.exr", "frame 1005")
	c1, err := st.store.PutBytes(ctx, one.Changeset)
	if err != nil {
		t.Fatal(err)
	}
	// The second changeset names the first, stored, as its base: its patch,
	// written over that changeset's ID with the library's encoder.
	two, err := view.ReadChain(put(one.ID, "shots/sh010/plates/sh010.1006.exr", "frame 1006").Changeset)
	if err != nil {
		t.Fatal(err)
	}
	cs2, err := view.EncodeChangeset(c1, two.Sections[0].Patch)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := view.NewFilter(nil)
	res, err := view.FromBytes(cs2, st.store, all)
	if err != nil {
		t.Fatal(err)
	}
	ns := newNamespace(t, "stack")
	putProject(t, "stack", []string{ns}, st, c4v1.ScopeGrants, []c4v1.Grant{{Tree: res.View().ID.String()}})
	putMat(t, ns, "stacked", byConfigMap(t, ns, "stacked", cs2))
	m := waitMat(t, ns, "stacked", c4v1.PhaseReady, "Ready")
	if m.Status.ViewID != res.View().ID.String() {
		t.Fatalf("mounted %s, the view it makes is %s", m.Status.ViewID, res.View().ID)
	}
}
