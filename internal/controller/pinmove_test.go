package controller

import (
	"bytes"
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/gitimport"
	"github.com/Avalanche-io/c4-operator/internal/pinmove"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// oldPinStore is a project store holding what the view package wrote at
// the c4 pin before the move (internal/view/testdata/pin19c9): its views
// and changesets by name.
func oldPinStore(t *testing.T) (*tree, map[string]c4.ID, map[string]c4.ID) {
	t.Helper()
	st := newStore(t)
	dir := filepath.Join("..", "view", "testdata", "pin19c9")
	files, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, "objects", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.store.PutBytes(context.Background(), data); err != nil {
			t.Fatal(err)
		}
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.txt"))
	if err != nil {
		t.Fatal(err)
	}
	views, changesets := map[string]c4.ID{}, map[string]c4.ID{}
	for _, l := range strings.Split(string(index), "\n") {
		w := strings.Fields(l)
		if len(w) < 3 || strings.HasPrefix(l, "#") {
			continue
		}
		id, err := c4.Parse(w[2])
		if err != nil {
			t.Fatal(err)
		}
		if w[0] == "view" {
			views[w[1]] = id
		} else {
			changesets[w[1]] = id
		}
	}
	return st, views, changesets
}

// The pin move's acceptance test. A project whose store, grants,
// Materializations, finished jobs and git-import mapping were written at
// the c4 pin before the move is migrated: every stored view is
// re-identified, each renamed one is in the table the Project's status
// names, old to new; its old ID still loads through the shared function;
// the grants match both IDs; and the Materializations resolve again to the
// new. A dry run before writes nothing, a live job refuses the run, and a
// second run does nothing.
func TestThePinMoveMigratesAProjectWrittenBeforeIt(t *testing.T) {
	ctx := context.Background()
	st, views, changesets := oldPinStore(t)
	const ns = "pinned"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	old := func(name string) string { return views[name].String() }
	putProject(t, "pinned", []string{ns}, st, c4v1.ScopeGrants, []c4v1.Grant{
		{Tree: old("link-depth2"), Paths: []string{"a/"}},
		{Tree: old("null-depth2")},
	})

	// A Materialization of an old ID, Ready as the operator before the move
	// left it: its status names the view as stored.
	putMat(t, ns, "frames", byID(views["null-depth2"]))
	m := waitMat(t, ns, "frames", c4v1.PhaseReady, "Ready")
	orig := m.DeepCopy()
	m.Status.ViewID, m.Status.Layers = old("null-depth2"), []string{old("null-depth2")}
	if err := k8s.Status().Patch(ctx, m, client.MergeFrom(orig)); err != nil {
		t.Fatal(err)
	}
	// A changeset over a granted view: its base is recomputed with the
	// library now, so the grant on the base's old ID does not cover it.
	putMat(t, ns, "change", byID(changesets["chain1"]))
	waitMat(t, ns, "change", c4v1.PhaseFailed, "NotGranted")

	mapping := filepath.Join(t.TempDir(), "mapping.jsonl")
	w, err := gitimport.AppendMapping(mapping)
	if err != nil {
		t.Fatal(err)
	}
	parent := view.EmptyID()
	for i, name := range []string{"link-depth2", "chain1", "chain2"} {
		w.Write(gitimport.Commit{Commit: strings.Repeat(string(rune('a'+i)), 40), Ref: "main", View: views[name], ParentView: parent,
			Changeset: changesets[name], ChainBase: view.EmptyID(), ChainSeq: i + 1})
		parent = views[name]
	}
	w.Close()
	mappingBefore, _ := os.ReadFile(mapping)

	// A farm tool with a version published from the project, and a tool of
	// the project's own with an earlier version: their trees as stored.
	putTool := func(namespace, name string, st c4v1.ToolStatus) {
		tool := &c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		if err := k8s.Create(ctx, tool); err != nil {
			t.Fatal(err)
		}
		tool.Status = st
		if err := k8s.Status().Update(ctx, tool); err != nil {
			t.Fatal(err)
		}
	}
	putTool("c4-system", "farm-tool", c4v1.ToolStatus{Versions: []c4v1.ToolVersion{{Version: "v1", View: old("link-depth2"), Project: "pinned", At: metav1.Now(), Test: c4v1.ToolTest{State: c4v1.ToolTestOK}}}})
	putTool(ns, "own-tool", c4v1.ToolStatus{Earlier: []c4v1.ToolVersionRef{{Version: "v1", View: old("null-depth2")}}})

	// A job that has not finished.
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "live"}, Spec: c4v1.WorkJobSpec{
		Class: "render", Pools: []string{"cpu"}, Retries: ptr.To[int32](1),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "work", Image: "busybox"}}}}}}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}

	stores := func(ctx context.Context, _ *c4v1.Project) (pinmove.Store, error) { return st.store, nil }
	run := func(write bool) (string, error) {
		var out, log bytes.Buffer
		// mixed-deep is a view no holder here names, named outside.
		err := pinmove.Migrate(ctx, pinmove.Options{Client: k8s, Project: "pinned", Store: stores, Mappings: []string{mapping},
			Named: []c4.ID{views["mixed-deep"]}, FarmNamespace: "c4-system", Write: write, Out: &out, Log: &log})
		t.Logf("write %v:\n%s%s", write, out.String(), log.String())
		return out.String(), err
	}

	// The dry run says what it would do, and writes nothing.
	report, err := run(false)
	if err == nil || !strings.Contains(err.Error(), ns+"/live") || !strings.Contains(err.Error(), "Drain these, then run again") {
		t.Fatalf("a dry run with a live job: %v", err)
	}
	renamed := []string{"chain1", "chain2", "link-depth2", "mixed-deep", "null-depth2"}
	for _, want := range []string{"unfinished " + ns + "/live", "rematerialize " + ns + "/frames",
		"tool c4-system/farm-tool " + old("link-depth2"), "tool " + ns + "/own-tool " + old("null-depth2"), "mapping " + mapping + " 3", "table - c4", "grant pinned "} {
		if !strings.Contains(report, want) {
			t.Errorf("the dry run's report has no %q", want)
		}
	}
	for _, name := range []string{"link-depth2", "null-depth2"} {
		if !strings.Contains(report, "rename "+old(name)+" ") {
			t.Errorf("the dry run's report does not rename %s", name)
		}
	}
	nothingWritten := func(when string) {
		t.Helper()
		var p c4v1.Project
		if err := k8s.Get(ctx, types.NamespacedName{Name: "pinned"}, &p); err != nil {
			t.Fatal(err)
		}
		if p.Status.Renames != "" || len(p.Spec.Grants) != 2 {
			t.Fatalf("%s: the project has renames %q and %d grants", when, p.Status.Renames, len(p.Spec.Grants))
		}
		if now, _ := os.ReadFile(mapping); !bytes.Equal(now, mappingBefore) {
			t.Fatalf("%s: the mapping was written", when)
		}
		var m c4v1.Materialization
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "frames"}, &m); err != nil || m.Status.ViewID != old("null-depth2") {
			t.Fatalf("%s: frames is at %s (%v)", when, m.Status.ViewID, err)
		}
	}
	nothingWritten("after the dry run")

	// A live job refuses a run that writes, before it writes anything.
	if _, err := run(true); err == nil || !strings.Contains(err.Error(), ns+"/live") {
		t.Fatalf("a run with a live job: %v", err)
	}
	nothingWritten("after the refused run")

	// Finished, the job is a holder like any other.
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "live"}, j); err != nil {
		t.Fatal(err)
	}
	jorig := j.DeepCopy()
	j.Status.Phase = "Succeeded"
	j.Status.Output = &c4v1.WorkOutput{View: old("chain2")}
	if err := k8s.Status().Patch(ctx, j, client.MergeFrom(jorig)); err != nil {
		t.Fatal(err)
	}
	report, err = run(true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "rename "+old("chain2")+" ") || !strings.Contains(report, "WorkJob "+ns+"/live .status.output.view") {
		t.Errorf("the finished job's output is not renamed in the report")
	}

	// Every stored view: re-identified, and each renamed one in the table
	// the status names, its old ID loading through the shared function.
	var p c4v1.Project
	if err := k8s.Get(ctx, types.NamespacedName{Name: "pinned"}, &p); err != nil {
		t.Fatal(err)
	}
	table, err := c4.Parse(p.Status.Renames)
	if err != nil {
		t.Fatalf("status.renames %q: %v", p.Status.Renames, err)
	}
	r, err := view.ReadRenames(st.store, table)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != len(renamed) {
		t.Fatalf("%d renames, want %d", len(r), len(renamed))
	}
	src := view.StoreRecords(st.store)
	newID := map[string]c4.ID{}
	for name, id := range views {
		v, err := view.Reidentify(ctx, id, src, 4)
		if err != nil {
			t.Fatal(err)
		}
		newID[name] = v.ID
		if cur := r.Current(id); cur != v.ID {
			t.Errorf("%s: the table says %s, its records identify as %s", name, cur, v.ID)
		}
		resolved, err := view.ResolveOld(st.store, table, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := view.Load(ctx, resolved, src, 4); err != nil {
			t.Errorf("%s: the old ID resolves to %s, which does not load: %v", name, resolved, err)
		}
	}
	for _, name := range renamed {
		if newID[name] == views[name] {
			t.Errorf("%s was not renamed", name)
		}
	}

	// The tools' versions name the new trees.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var farm, own c4v1.Tool
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: "c4-system", Name: "farm-tool"}, &farm); err != nil {
			t.Fatal(err)
		}
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "own-tool"}, &own); err != nil {
			t.Fatal(err)
		}
		if farm.Status.Versions[0].View == newID["link-depth2"].String() && own.Status.Earlier[0].View == newID["null-depth2"].String() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tools are at %s and %s", farm.Status.Versions[0].View, own.Status.Earlier[0].View)
		}
	}

	// The grants match both IDs. (The client here reads through a cache,
	// so the update is waited for.)
	for deadline := time.Now().Add(10 * time.Second); len(p.Spec.Grants) != 4; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("grants %+v", p.Spec.Grants)
		}
		if err := k8s.Get(ctx, types.NamespacedName{Name: "pinned"}, &p); err != nil {
			t.Fatal(err)
		}
	}
	putMat(t, ns, "by-old", byID(views["link-depth2"]))
	putMat(t, ns, "by-new", byID(newID["link-depth2"]))
	for _, name := range []string{"by-old", "by-new"} {
		if m := waitMat(t, ns, name, c4v1.PhaseReady, "Ready"); m.Status.ViewID != newID["link-depth2"].String() {
			t.Errorf("%s resolves to %s", name, m.Status.ViewID)
		}
	}
	if m := waitMat(t, ns, "change", c4v1.PhaseReady, "Ready"); m.Status.ViewID == "" {
		t.Errorf("the changeset over a granted view: %+v", m.Status)
	}
	// The Materialization left at the old view resolves again, to the new.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var m c4v1.Materialization
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "frames"}, &m); err != nil {
			t.Fatal(err)
		}
		if m.Status.ViewID == newID["null-depth2"].String() && m.Status.ObservedGeneration == m.Generation && m.Status.Phase == c4v1.PhaseReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("frames is at %s, phase %s", m.Status.ViewID, m.Status.Phase)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The mapping reads with its views renamed, and its chain is made again
	// from the same changesets to the new head.
	lines, err := gitimport.ReadMapping(mapping)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 4 || lines[2].View != newID["chain2"] {
		t.Fatalf("the mapping reads as %d lines, the last commit at %s", len(lines), lines[2].View)
	}
	ch, _, err := gitimport.OpenRecordedChain(ctx, filepath.Join(t.TempDir(), "main.c4m"), src, lines, "main")
	if err != nil || ch.Head != newID["chain2"] {
		t.Fatalf("the chain: %+v, %v", ch, err)
	}

	// A second run finds nothing to do.
	mappingAfter, _ := os.ReadFile(mapping)
	report, err = run(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"rename ", "table ", "grant ", "rematerialize ", "mapping ", "tool "} {
		if strings.Contains(report, word) {
			t.Errorf("the second run reports %q", word)
		}
	}
	if now, _ := os.ReadFile(mapping); !bytes.Equal(now, mappingAfter) {
		t.Error("the second run wrote the mapping")
	}
	var again c4v1.Project
	if err := k8s.Get(ctx, types.NamespacedName{Name: "pinned"}, &again); err != nil || again.Status.Renames != p.Status.Renames || len(again.Spec.Grants) != 4 {
		t.Fatalf("after the second run: %q, %d grants (%v)", again.Status.Renames, len(again.Spec.Grants), err)
	}
}

// A Materialization whose project's grants do not change is resolved again
// only when the operator restarts, on the new library: a restart's first
// reconcile leaves a Ready one as it is unless the migration cleared it.
// Before the migration that first reconcile keeps the view as stored; after
// it, the same reconcile gives the new.
func TestARestartAfterTheMigrationResolvesTheMaterializationAgain(t *testing.T) {
	ctx := context.Background()
	st, views, _ := oldPinStore(t)
	const ns = "pinned-store"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	putProject(t, "pinned-store", []string{ns}, st, c4v1.ScopeStore, nil)
	putMat(t, ns, "frames", byID(views["null-depth2"]))
	m := waitMat(t, ns, "frames", c4v1.PhaseReady, "Ready")
	orig := m.DeepCopy()
	m.Status.ViewID, m.Status.Layers = views["null-depth2"].String(), []string{views["null-depth2"].String()}
	if err := k8s.Status().Patch(ctx, m, client.MergeFrom(orig)); err != nil {
		t.Fatal(err)
	}
	signer, err := provenance.NewSigner(ctx, provenance.LocalIssuer(farmCA, provenance.ComponentIdentity("controller"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(farmCA.Cert)
	restarted := &MaterializationReconciler{Client: k8s, Projects: &project.Registry{Cache: k8s, Secrets: k8s, Namespace: "c4-system"},
		Retry: time.Second, Signer: signer, Roots: roots}
	viewAfterRestart := func() string {
		t.Helper()
		if _, err := restarted.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "frames"}}); err != nil {
			t.Fatal(err)
		}
		var m c4v1.Materialization
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "frames"}, &m); err != nil {
			t.Fatal(err)
		}
		return m.Status.ViewID
	}
	// The cache the client reads through has the patch once the status
	// says so; until then the reconcile would read the status before it.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var m c4v1.Materialization
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "frames"}, &m); err == nil && m.Status.ViewID == views["null-depth2"].String() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stale status never reached the cache")
		}
	}
	if got := viewAfterRestart(); got != views["null-depth2"].String() {
		t.Fatalf("before the migration a restart resolves frames to %s, so this test shows nothing", got)
	}

	var out, log bytes.Buffer
	err = pinmove.Migrate(ctx, pinmove.Options{Client: k8s, Project: "pinned-store",
		Store: func(context.Context, *c4v1.Project) (pinmove.Store, error) { return st.store, nil }, Write: true, Out: &out, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rematerialize "+ns+"/frames") {
		t.Fatalf("report:\n%s", out.String())
	}
	want, err := view.Reidentify(ctx, views["null-depth2"], view.StoreRecords(st.store), 4)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if got := viewAfterRestart(); got == want.ID.String() {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("after the migration a restart resolves frames to %s, want %s", got, want.ID)
		}
	}
}
