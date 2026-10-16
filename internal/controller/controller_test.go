package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/scan"
	"github.com/Avalanche-io/c4/store"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

var k8s client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// Fall back to the binaries `make envtest` downloads.
		matches, _ := filepath.Glob("../../bin/k8s/k8s/*")
		if len(matches) == 0 {
			println("envtest binaries not found; run `make envtest`")
			os.Exit(0)
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		os.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

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
	r := &MaterializationReconciler{Client: mgr.GetClient(), Retry: 2 * time.Second}
	if err := r.SetupWithManager(mgr); err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go mgr.Start(ctx)
	k8s = mgr.GetClient()

	code := m.Run()
	cancel()
	env.Stop()
	os.Exit(code)
}

// seed writes a tree, stores its files in a TreeStore, and returns the
// store path and the tree's full c4m text.
func seed(t *testing.T, files map[string]string) (string, string, *c4m.Manifest) {
	t.Helper()
	src := t.TempDir()
	for p, content := range files {
		full := filepath.Join(src, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	storeDir := t.TempDir()
	ts, err := store.NewTreeStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range files {
		if _, err := ts.Put(strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	m, err := scan.Dir(src, scan.WithMode(scan.ModeFull))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := c4m.Marshal(m)
	return storeDir, string(text), m
}

func waitPhase(t *testing.T, name, phase string) *c4v1.Materialization {
	t.Helper()
	var mat c4v1.Materialization
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8s.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &mat); err == nil && mat.Status.Phase == phase && mat.Status.ObservedGeneration == mat.Generation {
			return &mat
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not reach %s: %+v", name, phase, mat.Status)
	return nil
}

func create(t *testing.T, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		if err := k8s.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
}

var files = map[string]string{
	"shots/sh010/frame.0001.exr": "plate one",
	"shots/sh010/frame.0002.exr": "plate two",
	"shots/sh020/frame.0001.exr": "plate two",
	"weights/model.bin":          "weights!",
}

func TestConfigMapSourceResolves(t *testing.T) {
	storeDir, text, m := seed(t, files)
	create(t,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm-a", Namespace: "default"}, Data: map[string]string{"show.c4m": text}},
		&c4v1.Materialization{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec: c4v1.MaterializationSpec{
				Source:  c4v1.ManifestSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "cm-a"}, Key: "show.c4m"}},
				Store:   storeDir,
				Include: []string{"shots/"},
			},
		})
	mat := waitPhase(t, "a", c4v1.PhaseReady)
	st := mat.Status
	if st.Files != 3 || st.Bytes != 27 || st.UniqueObjects != 2 || st.UniqueBytes != 18 {
		t.Fatalf("totals: %+v", st)
	}
	if st.RootID != m.ComputeC4ID().String() {
		t.Fatalf("rootID %s, want snapshot ID %s", st.RootID, m.ComputeC4ID())
	}
	if _, err := c4.Parse(st.ViewID); err != nil {
		t.Fatalf("viewID: %v", err)
	}

	// Changing the include re-resolves to a different view.
	oldView := st.ViewID
	mat.Spec.Include = []string{"weights/"}
	if err := k8s.Update(context.Background(), mat); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		k8s.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "a"}, mat)
		if mat.Status.ViewID != oldView && mat.Status.Phase == c4v1.PhaseReady {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if mat.Status.Files != 1 || mat.Status.Bytes != 8 {
		t.Fatalf("after include change: %+v", mat.Status)
	}
}

func TestMissingContentFails(t *testing.T) {
	storeDir, text, _ := seed(t, files)
	ts, _ := store.NewTreeStore(storeDir)
	gone := c4.Identify(strings.NewReader("weights!"))
	if err := ts.Remove(gone); err != nil {
		t.Fatal(err)
	}
	create(t,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm-b", Namespace: "default"}, Data: map[string]string{"c4m": text}},
		&c4v1.Materialization{
			ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default"},
			Spec: c4v1.MaterializationSpec{
				Source: c4v1.ManifestSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "cm-b"}, Key: "c4m"}},
				Store:  storeDir,
			},
		})
	mat := waitPhase(t, "b", c4v1.PhaseFailed)
	if mat.Status.Missing != 1 || len(mat.Status.MissingPaths) != 1 || !strings.HasPrefix(mat.Status.MissingPaths[0], "weights/model.bin") {
		t.Fatalf("status: %+v", mat.Status)
	}

	// Uploading the content heals it on the next retry.
	ts.Put(strings.NewReader("weights!"))
	waitPhase(t, "b", c4v1.PhaseReady)
}

func TestIDSourceResolves(t *testing.T) {
	storeDir, text, m := seed(t, files)
	ts, _ := store.NewTreeStore(storeDir)
	id, err := ts.Put(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	create(t, &c4v1.Materialization{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
		Spec: c4v1.MaterializationSpec{
			Source:  c4v1.ManifestSource{ID: id.String()},
			Store:   "file://" + storeDir,
			Include: []string{"shots/sh020"},
		},
	})
	mat := waitPhase(t, "c", c4v1.PhaseReady)
	if mat.Status.Files != 1 || mat.Status.RootID != id.String() || mat.Status.SourceID != id.String() {
		t.Fatalf("status: %+v (snapshot %s)", mat.Status, m.ComputeC4ID())
	}
}

func TestValidation(t *testing.T) {
	bad := &c4v1.Materialization{
		ObjectMeta: metav1.ObjectMeta{Name: "both", Namespace: "default"},
		Spec: c4v1.MaterializationSpec{
			Source: c4v1.ManifestSource{ID: "c4notanid"},
			Store:  "/x",
		},
	}
	if err := k8s.Create(context.Background(), bad); err == nil {
		t.Fatal("API accepted a malformed C4 ID")
	}
	neither := &c4v1.Materialization{
		ObjectMeta: metav1.ObjectMeta{Name: "neither", Namespace: "default"},
		Spec:       c4v1.MaterializationSpec{Store: "/x"},
	}
	if err := k8s.Create(context.Background(), neither); err == nil {
		t.Fatal("API accepted a source with neither field")
	}
	create(t, &c4v1.Materialization{
		ObjectMeta: metav1.ObjectMeta{Name: "badinclude", Namespace: "default"},
		Spec: c4v1.MaterializationSpec{
			Source:  c4v1.ManifestSource{ID: c4.Identify(strings.NewReader("x")).String()},
			Store:   t.TempDir(),
			Include: []string{"../escape"},
		},
	})
	mat := waitPhase(t, "badinclude", c4v1.PhaseFailed)
	if c := mat.Status.Conditions[0]; c.Reason != "InvalidInclude" {
		t.Fatalf("condition %+v", c)
	}
}
