package controller

import (
	"context"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"go.uber.org/zap/zapcore"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

var (
	k8s client.Client
	// farmCA signs the test farm's certificates; the controller signs its
	// resolution records with one it issued.
	farmCA *provenance.CA
)

// TestMain runs the manager's two controllers against a real API server
// (envtest) with stores on local disk.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob("../../bin/k8s/k8s/*")
		if len(matches) == 0 {
			println("envtest binaries not found; run `make envtest`")
			os.Exit(0)
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		os.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	ctrl.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.Level(zapLevel())))
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Client:  client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
	})
	if err != nil {
		panic(err)
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &c4v1.Project{}, project.NamespaceIndex, project.IndexNamespaces); err != nil {
		panic(err)
	}
	reg := &project.Registry{Cache: mgr.GetClient(), Secrets: mgr.GetAPIReader(), Namespace: "c4-system"}
	if err := (&ProjectReconciler{Client: mgr.GetClient(), Projects: reg}).SetupWithManager(mgr); err != nil {
		panic(err)
	}
	if farmCA, err = provenance.NewCA(time.Now()); err != nil {
		panic(err)
	}
	signer, err := provenance.NewSigner(context.Background(), provenance.LocalIssuer(farmCA, provenance.ComponentIdentity("controller"), time.Hour))
	if err != nil {
		panic(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(farmCA.Cert)
	if err := (&MaterializationReconciler{Client: mgr.GetClient(), Projects: reg, Retry: time.Second,
		Signer: signer, Roots: roots}).SetupWithManager(mgr); err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go mgr.Start(ctx)
	k8s = mgr.GetClient()
	for _, ns := range []string{"c4-system", "a", "b", "vendor", "scoped", "dup-1", "signed"} {
		if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	cancel()
	env.Stop()
	os.Exit(code)
}

func zapLevel() zapcore.Level {
	if os.Getenv("TEST_VERBOSE") != "" {
		return zapcore.DebugLevel
	}
	return zapcore.ErrorLevel
}

// tree is a store on local disk holding one or more views.
type tree struct {
	dir   string
	store *storeurl.Sharded
}

func newStore(t *testing.T) *tree {
	t.Helper()
	dir := t.TempDir()
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: dir}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	return &tree{dir: dir, store: st}
}

// ingest writes files to disk, then puts their objects and the view's
// records into the store, as `c4-operator ingest` does.
func (s *tree) ingest(t *testing.T, files map[string]string) *view.View {
	t.Helper()
	src := t.TempDir()
	for p, content := range files {
		full := filepath.Join(src, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	v, err := view.ScanDir(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, content := range files {
		if _, err := s.store.PutBytes(ctx, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range v.Records() {
		if _, err := s.store.PutBytes(ctx, r.Data); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

// show is a small show: two shots and editorial.
func show(tag string) map[string]string {
	files := map[string]string{"editorial/cut.mov": "cut " + tag}
	for i := 1001; i <= 1004; i++ {
		files[fmt.Sprintf("shots/sh010/plates/sh010.%d.exr", i)] = fmt.Sprintf("sh010 %d %s", i, tag)
		files[fmt.Sprintf("shots/sh020/plates/sh020.%d.exr", i)] = fmt.Sprintf("sh020 %d %s", i, tag)
	}
	return files
}

func putProject(t *testing.T, name string, namespaces []string, st *tree, scope c4v1.Scope, grants []c4v1.Grant) {
	t.Helper()
	ctx := context.Background()
	p := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}}
	var existing c4v1.Project
	err := k8s.Get(ctx, types.NamespacedName{Name: name}, &existing)
	if err == nil {
		p = &existing
	}
	p.Spec = c4v1.ProjectSpec{
		Namespaces: namespaces,
		Store:      c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: st.dir}}},
		Scope:      scope,
		Grants:     grants,
	}
	if err == nil {
		err = k8s.Update(ctx, p)
	} else {
		err = k8s.Create(ctx, p)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func putMat(t *testing.T, ns, name string, src c4v1.ManifestSource, include ...string) {
	t.Helper()
	m := &c4v1.Materialization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       c4v1.MaterializationSpec{Source: src, Include: include},
	}
	if err := k8s.Create(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}

func byID(id c4.ID) c4v1.ManifestSource { return c4v1.ManifestSource{ID: id.String()} }

func byConfigMap(t *testing.T, ns, name string, c4mText []byte) c4v1.ManifestSource {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string]string{"tree.c4m": string(c4mText)}}
	if err := k8s.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	return c4v1.ManifestSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "tree.c4m"}}
}

// waitMat waits until the Materialization reports phase with the Ready
// condition's reason, and returns it.
func waitMat(t *testing.T, ns, name, phase, reason string) *c4v1.Materialization {
	t.Helper()
	var mat c4v1.Materialization
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		err := k8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &mat)
		if err == nil && mat.Status.Phase == phase && mat.Status.ObservedGeneration == mat.Generation {
			c := meta.FindStatusCondition(mat.Status.Conditions, c4v1.ConditionReady)
			if c != nil && c.Reason == reason {
				return &mat
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	c := meta.FindStatusCondition(mat.Status.Conditions, c4v1.ConditionReady)
	t.Fatalf("%s/%s: want %s (%s), have %s %+v", ns, name, phase, reason, mat.Status.Phase, c)
	return nil
}

func readyMessage(m *c4v1.Materialization) string {
	if c := meta.FindStatusCondition(m.Status.Conditions, c4v1.ConditionReady); c != nil {
		return c.Message
	}
	return ""
}

func countUnder(v *view.View, prefix string) int {
	n := 0
	for _, f := range v.Files {
		if strings.HasPrefix(f.Path, prefix) {
			n++
		}
	}
	return n
}

// TestIsolationBetweenProjects covers what DESIGN.md section 6.2 says
// stops project B from reading project A's data by ID.
func TestIsolationBetweenProjects(t *testing.T) {
	storeA, storeB := newStore(t), newStore(t)
	va := storeA.ingest(t, show("a"))
	storeB.ingest(t, map[string]string{"own.txt": "b's own"})
	putProject(t, "a", []string{"a"}, storeA, "", nil)
	putProject(t, "b", []string{"b"}, storeB, "", nil)

	t.Run("own view resolves", func(t *testing.T) {
		putMat(t, "a", "own", byID(va.ID))
		m := waitMat(t, "a", "own", c4v1.PhaseReady, "Ready")
		if m.Status.ViewID != va.ID.String() || m.Status.Files != int64(len(va.Files)) || m.Status.Project != "a" {
			t.Fatalf("status %+v", m.Status)
		}
	})
	t.Run("another project's view ID does not resolve", func(t *testing.T) {
		putMat(t, "b", "by-id", byID(va.ID))
		m := waitMat(t, "b", "by-id", c4v1.PhaseFailed, "ResolveFailed")
		if m.Status.ViewID != "" {
			t.Fatalf("resolved a view: %s", m.Status.ViewID)
		}
	})
	t.Run("a c4m listing another project's objects fails preflight", func(t *testing.T) {
		listing, err := va.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		putMat(t, "b", "by-c4m", byConfigMap(t, "b", "stolen", listing))
		m := waitMat(t, "b", "by-c4m", c4v1.PhaseFailed, "ContentMissing")
		if m.Status.Missing != int64(len(va.Objects())) {
			t.Fatalf("missing %d of %d objects", m.Status.Missing, len(va.Objects()))
		}
		// A refused description leaves nothing in B's store: by ID, A's
		// view still does not resolve there.
		if _, err := storeB.store.Head(context.Background(), va.ID); err == nil {
			t.Fatal("the refused view's records were written to B's store")
		}
	})
}

// TestGrants covers a vendor project that shares the show's store but may
// materialize only one shot of one version.
func TestGrants(t *testing.T) {
	st := newStore(t)
	v := st.ingest(t, show("g"))
	secret := st.ingest(t, map[string]string{"shots/sh900/plates/sh900.1001.exr": "unreleased"})
	sh010 := countUnder(v, "shots/sh010/")
	putProject(t, "vendor", []string{"vendor"}, st, c4v1.ScopeGrants, []c4v1.Grant{{Tree: v.ID.String(), Paths: []string{"shots/sh010/"}}})

	t.Run("granted tree is cut to the grant's paths", func(t *testing.T) {
		putMat(t, "vendor", "granted", byID(v.ID))
		m := waitMat(t, "vendor", "granted", c4v1.PhaseReady, "Ready")
		if m.Status.Files != int64(sh010) {
			t.Fatalf("%d files, want the %d under shots/sh010/", m.Status.Files, sh010)
		}
	})
	t.Run("an include outside the grant is refused, not mounted empty", func(t *testing.T) {
		putMat(t, "vendor", "outside", byID(v.ID), "shots/sh020/")
		waitMat(t, "vendor", "outside", c4v1.PhaseFailed, "NotGranted")
	})
	t.Run("a tree in the same store that is not granted is refused", func(t *testing.T) {
		putMat(t, "vendor", "secret", byID(secret.ID))
		waitMat(t, "vendor", "secret", c4v1.PhaseFailed, "NotGranted")
	})
	t.Run("a hand-written c4m of granted files is not the granted tree", func(t *testing.T) {
		sub, err := view.FromID(v.ID, st.store, mustFilter(t, "shots/sh010/"))
		if err != nil {
			t.Fatal(err)
		}
		listing, err := sub.View().Marshal()
		if err != nil {
			t.Fatal(err)
		}
		putMat(t, "vendor", "handmade", byConfigMap(t, "vendor", "handmade", listing))
		waitMat(t, "vendor", "handmade", c4v1.PhaseFailed, "NotGranted")
	})
	t.Run("a changeset on the granted tree is covered by its base's grant", func(t *testing.T) {
		ctx := context.Background()
		frame := []byte("sh010 1005 new")
		id, err := st.store.PutBytes(ctx, frame)
		if err != nil {
			t.Fatal(err)
		}
		edit := view.Edit{Path: "shots/sh010/plates/sh010.1005.exr", Entry: view.FileEntry("", int64(len(frame)), id, false)}
		applied, err := view.Apply(ctx, v.ID, []view.Edit{edit}, view.StoreRecords(st.store))
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
		putMat(t, "vendor", "changeset", byID(csID))
		m := waitMat(t, "vendor", "changeset", c4v1.PhaseReady, "Ready")
		if m.Status.Files != int64(sh010+1) || len(m.Status.Layers) != 2 {
			t.Fatalf("%d files in %d layers, want %d in 2", m.Status.Files, len(m.Status.Layers), sh010+1)
		}
	})
	t.Run("revoking the last grant refuses everything", func(t *testing.T) {
		putProject(t, "vendor", []string{"vendor"}, st, c4v1.ScopeGrants, nil)
		m := waitMat(t, "vendor", "granted", c4v1.PhaseFailed, "NotGranted")
		if !strings.Contains(readyMessage(m), "no grants") {
			t.Fatalf("message %q", readyMessage(m))
		}
		var p c4v1.Project
		if err := k8s.Get(context.Background(), types.NamespacedName{Name: "vendor"}, &p); err != nil {
			t.Fatal(err)
		}
		if m.Status.ProjectGeneration != p.Generation {
			t.Fatalf("status from project generation %d, project is at %d", m.Status.ProjectGeneration, p.Generation)
		}
	})
}

func mustFilter(t *testing.T, patterns ...string) view.Filter {
	t.Helper()
	f, err := view.NewFilter(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestProjectValidation covers the Ready condition of Projects.
func TestProjectValidation(t *testing.T) {
	st := newStore(t)
	v := st.ingest(t, show("v"))
	cases := []struct {
		name    string
		spec    func(p *c4v1.Project)
		problem string
	}{
		{"grants need scope Grants", func(p *c4v1.Project) {
			p.Spec.Namespaces = []string{"scoped"}
			p.Spec.Grants = []c4v1.Grant{{Tree: v.ID.String()}}
		}, "scope is not Grants"},
		{"a namespace belongs to one project", func(p *c4v1.Project) {
			p.Spec.Namespaces = []string{"a"}
		}, "already belongs to project a"},
		{"shared needs a group", func(p *c4v1.Project) {
			p.Spec.Namespaces = []string{"dup-1"}
			p.Spec.Isolation = c4v1.IsolationShared
		}, "needs a sharingGroup"},
		{"no credentials in shard URLs", func(p *c4v1.Project) {
			p.Spec.Namespaces = []string{"dup-1"}
			p.Spec.Store.Shards = []c4v1.StoreShard{{Name: "s0", URL: "s3://key:secret@bucket/c4/?endpoint=http://x"}}
		}, "carries credentials"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("invalid-%d", i)}}
			p.Spec.Store.Shards = []c4v1.StoreShard{{Name: "s0", URL: st.dir}}
			tc.spec(p)
			if err := k8s.Create(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				var got c4v1.Project
				k8s.Get(context.Background(), types.NamespacedName{Name: p.Name}, &got)
				c := meta.FindStatusCondition(got.Status.Conditions, "Ready")
				if c != nil && c.Status == metav1.ConditionFalse && strings.Contains(c.Message, tc.problem) {
					k8s.Delete(context.Background(), &got)
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatalf("project %s never reported %q", p.Name, tc.problem)
		})
	}
}

// signedAs signs a record as identity and stores it.
func signedAs(t *testing.T, st *tree, identity, predicateType string, subjects []provenance.Subject, pred any) string {
	t.Helper()
	ctx := context.Background()
	sg, err := provenance.NewSigner(ctx, provenance.LocalIssuer(farmCA, identity, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := provenance.SignStatement(ctx, sg, predicateType, subjects, pred, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.store.PutBytes(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func putMatWithRecord(t *testing.T, ns, name string, src c4v1.ManifestSource, record string) {
	t.Helper()
	m := &c4v1.Materialization{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: map[string]string{c4v1.AnnotationSourceRecord: record}},
		Spec:       c4v1.MaterializationSpec{Source: src},
	}
	if err := k8s.Create(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}

// TestResolutionRecords covers DESIGN.md section 9.2: the controller signs
// what it resolved, and checks who vouched for the source.
func TestResolutionRecords(t *testing.T) {
	st := newStore(t)
	v := st.ingest(t, show("signed"))
	other := st.ingest(t, map[string]string{"plates/x.exr": "another tree"})
	putProject(t, "signed", []string{"signed"}, st, "", nil)
	roots := x509.NewCertPool()
	roots.AddCert(farmCA.Cert)
	ingest := map[string]any{"source": "plates"}
	good := signedAs(t, st, provenance.ServiceAccountIdentity("signed", "ingest"), provenance.PredicateIngest,
		[]provenance.Subject{provenance.SubjectFor("view", v.ID)}, ingest)

	t.Run("a resolution is signed by the controller and links the ingest record", func(t *testing.T) {
		putMatWithRecord(t, "signed", "plates", byID(v.ID), good)
		m := waitMat(t, "signed", "plates", c4v1.PhaseReady, "Ready")
		if m.Status.SourceRecord != good || m.Status.Record == "" {
			t.Fatalf("status record %q, source record %q", m.Status.Record, m.Status.SourceRecord)
		}
		id, _ := c4.Parse(m.Status.Record)
		data, err := view.ReadObject(st.store, id)
		if err != nil {
			t.Fatal(err)
		}
		rv, err := provenance.VerifyBundle(data, roots)
		if err != nil {
			t.Fatal(err)
		}
		if rv.Identity != provenance.ComponentIdentity("controller") || provenance.CheckSigner(rv) != nil {
			t.Fatalf("signed by %s, policy %v", rv.Identity, provenance.CheckSigner(rv))
		}
		if rv.Subject("view") != v.ID.String() || !strings.Contains(string(rv.Predicate), good) {
			t.Fatalf("resolution record %s", rv.Predicate)
		}
	})
	t.Run("an ingest record from another project's workload is refused", func(t *testing.T) {
		foreign := signedAs(t, st, provenance.ServiceAccountIdentity("b", "ingest"), provenance.PredicateIngest,
			[]provenance.Subject{provenance.SubjectFor("view", v.ID)}, ingest)
		putMatWithRecord(t, "signed", "foreign", byID(v.ID), foreign)
		m := waitMat(t, "signed", "foreign", c4v1.PhaseFailed, "SourceRecordInvalid")
		if !strings.Contains(readyMessage(m), "not a workload of project signed") {
			t.Fatal(readyMessage(m))
		}
	})
	t.Run("a record for another tree does not vouch for this one", func(t *testing.T) {
		putMatWithRecord(t, "signed", "mismatch", byID(other.ID), good)
		m := waitMat(t, "signed", "mismatch", c4v1.PhaseFailed, "SourceRecordInvalid")
		if !strings.Contains(readyMessage(m), "does not name the source") {
			t.Fatal(readyMessage(m))
		}
	})
	t.Run("a node cannot vouch for a source", func(t *testing.T) {
		byNode := signedAs(t, st, provenance.NodeIdentity("w1"), provenance.PredicateIngest,
			[]provenance.Subject{provenance.SubjectFor("view", v.ID)}, ingest)
		putMatWithRecord(t, "signed", "by-node", byID(v.ID), byNode)
		waitMat(t, "signed", "by-node", c4v1.PhaseFailed, "SourceRecordInvalid")
	})
	t.Run("a record signed outside the farm is refused", func(t *testing.T) {
		otherCA, _ := provenance.NewCA(time.Now())
		sg, _ := provenance.NewSigner(context.Background(), provenance.LocalIssuer(otherCA, provenance.ServiceAccountIdentity("signed", "ingest"), time.Hour))
		rec, _ := provenance.SignStatement(context.Background(), sg, provenance.PredicateIngest,
			[]provenance.Subject{provenance.SubjectFor("view", v.ID)}, ingest, time.Now())
		id, err := st.store.PutBytes(context.Background(), rec)
		if err != nil {
			t.Fatal(err)
		}
		putMatWithRecord(t, "signed", "outsider", byID(v.ID), id.String())
		m := waitMat(t, "signed", "outsider", c4v1.PhaseFailed, "SourceRecordInvalid")
		if !strings.Contains(readyMessage(m), "chain") {
			t.Fatal(readyMessage(m))
		}
	})
}

func TestAdvanceClock(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)
	var c c4v1.AgentClock
	if c = advanceClock(c, "Pending", t0); c.Start != nil {
		t.Fatal("the clock started before the bundle ran")
	}
	c = advanceClock(c, "Running", t0)
	c = advanceClock(c, "Running", t0.Add(55*time.Minute))
	if !c.VirtualNow.Time.Equal(t0.Add(55*time.Minute)) || c.DriftSeconds != 0 {
		t.Fatalf("running: %+v", c)
	}
	// Held overnight by a person: the bundle's time stops.
	c = advanceClock(c, "Held", t0.Add(time.Hour))
	c = advanceClock(c, "Held", t0.Add(10*time.Hour+30*time.Minute))
	if c.SuspendedBy != "person" {
		t.Fatalf("suspended by %q", c.SuspendedBy)
	}
	if !c.VirtualNow.Time.Equal(t0.Add(time.Hour)) || c.DriftSeconds != int64((9*time.Hour+30*time.Minute).Seconds()) {
		t.Fatalf("suspended: virtual %v drift %d", c.VirtualNow, c.DriftSeconds)
	}
	// Resumed: it continues from where it stopped, without a jump.
	c = advanceClock(c, "Running", t0.Add(10*time.Hour+30*time.Minute))
	c = advanceClock(c, "Running", t0.Add(11*time.Hour))
	if !c.VirtualNow.Time.Equal(t0.Add(90*time.Minute)) || c.SuspendedSince != nil {
		t.Fatalf("resumed: %+v", c)
	}
	if len(c.Suspensions) != 1 || c.Suspensions[0].By != "person" || !c.Suspensions[0].From.Time.Equal(t0.Add(time.Hour)) || !c.Suspensions[0].To.Time.Equal(t0.Add(10*time.Hour+30*time.Minute)) {
		t.Fatalf("suspensions %+v", c.Suspensions)
	}
}

func TestAdvanceClockQueueSuspension(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 22, 55, 0, 0, time.UTC)
	c := advanceClock(c4v1.AgentClock{}, "Running", t0)
	c = advanceClock(c, "Suspended", t0.Add(time.Minute))
	if c.SuspendedBy != "queue" {
		t.Fatalf("by %q", c.SuspendedBy)
	}
	c = advanceClock(c, "Running", t0.Add(time.Hour))
	if len(c.Suspensions) != 1 || c.Suspensions[0].By != "queue" || c.DriftSeconds != 59*60 {
		t.Fatalf("%+v", c)
	}
}
