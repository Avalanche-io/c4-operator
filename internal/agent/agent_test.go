package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/c4m"
	"github.com/Avalanche-io/c4/scan"
	"github.com/Avalanche-io/c4/store"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/cache"
	"github.com/Avalanche-io/c4-operator/internal/manifest"
)

type fixture struct {
	agent  *Agent
	store  *store.TreeStore
	viewID string
	src    string
}

func setup(t *testing.T, files map[string]string, include []string) *fixture {
	t.Helper()
	src := t.TempDir()
	for p, content := range files {
		full := filepath.Join(src, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	m, err := scan.Dir(src, scan.WithMode(scan.ModeFull))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := c4m.Marshal(m)
	storeDir := t.TempDir()
	ts, _ := store.NewTreeStore(storeDir)
	for _, c := range files {
		ts.Put(strings.NewReader(c))
	}
	f, _ := manifest.NewFilter(include)
	res, err := manifest.FromBytes(text, nil, f)
	if err != nil {
		t.Fatal(err)
	}

	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	c4v1.AddToScheme(s)
	mat := &c4v1.Materialization{
		ObjectMeta: metav1.ObjectMeta{Name: "plates", Namespace: "render"},
		Spec: c4v1.MaterializationSpec{
			Source:  c4v1.ManifestSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "plates"}, Key: "c4m"}},
			Store:   storeDir,
			Include: include,
		},
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "plates", Namespace: "render"}, Data: map[string]string{"c4m": string(text)}}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(mat, cm).WithStatusSubresource(mat).Build()

	c, err := cache.Open(t.TempDir(), 1<<30, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		agent:  &Agent{Client: cl, Cache: c, Node: "node-a", Log: logr.Discard()},
		store:  ts,
		viewID: res.View.ID.String(),
		src:    src,
	}
}

func (f *fixture) ensure(t *testing.T) (Response, error) {
	t.Helper()
	req := Request{Namespace: "render", Name: "plates", View: f.viewID}
	resp, err := f.agent.Ensure(context.Background(), req)
	f.agent.Report(context.Background(), req, resp, err)
	return resp, err
}

var show = map[string]string{
	"plates/frame.0001.exr": "frame one bytes",
	"plates/frame.0002.exr": "frame two bytes",
	"plates/frame.0003.exr": "frame one bytes", // duplicate
	"weights/model.bin":     "weights",
	"audio/mix.wav":         "not wanted",
}

func TestColdWarmCorrupt(t *testing.T) {
	f := setup(t, show, []string{"plates", "weights"})

	cold, err := f.ensure(t)
	if err != nil {
		t.Fatal(err)
	}
	if cold.Misses != 3 || cold.Hits != 0 || cold.BytesFetched != 37 || !cold.Built || cold.Files != 4 {
		t.Fatalf("cold: %+v", cold)
	}
	dir := f.agent.Cache.ViewDir(mustParse(f.viewID))
	if got, _ := manifest.ScanContentID(dir); got.String() != f.viewID {
		t.Fatalf("tree %s, want %s", got, f.viewID)
	}

	warm, err := f.ensure(t)
	if err != nil {
		t.Fatal(err)
	}
	if warm.Misses != 0 || warm.Hits != 3 || warm.Built || warm.BytesFetched != 0 {
		t.Fatalf("warm: %+v", warm)
	}

	p := filepath.Join(dir, "plates/frame.0002.exr")
	os.Chmod(p, 0o644)
	os.WriteFile(p, []byte("frame TWO bytes"), 0o644)
	healed, err := f.ensure(t)
	if err != nil {
		t.Fatal(err)
	}
	if healed.Refetched != 1 || !healed.Built || healed.BytesFetched != 15 {
		t.Fatalf("after corruption: %+v", healed)
	}
	if got, _ := manifest.ScanContentID(dir); got.String() != f.viewID {
		t.Fatal("healed tree does not match")
	}

	var mat c4v1.Materialization
	f.agent.Client.Get(context.Background(), types.NamespacedName{Namespace: "render", Name: "plates"}, &mat)
	ns := mat.Status.Nodes[0]
	if ns.Node != "node-a" || ns.Requests != 3 || ns.Misses != 4 || ns.Hits != 5 || ns.Refetched != 1 || ns.BytesFetched != 52 {
		t.Fatalf("node status %+v", ns)
	}
}

func TestMissingFailsBeforeAnyFetch(t *testing.T) {
	f := setup(t, show, nil)
	gone := c4.Identify(strings.NewReader("weights"))
	f.store.Remove(gone)

	resp, err := f.ensure(t)
	if !errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), "weights/model.bin") {
		t.Fatalf("err = %v", err)
	}
	if resp.BytesFetched != 0 || f.agent.Cache.Bytes() != 0 {
		t.Fatal("fetched content although the preflight failed")
	}
	if _, err := os.Stat(f.agent.Cache.ViewDir(mustParse(f.viewID))); !os.IsNotExist(err) {
		t.Fatal("a view exists for a failed request")
	}
}

func TestStaleAdmissionRefused(t *testing.T) {
	f := setup(t, show, []string{"plates"})
	f.viewID = c4.Identify(strings.NewReader("some other view")).String()
	if _, err := f.ensure(t); err == nil || !strings.Contains(err.Error(), "recreate the pod") {
		t.Fatalf("err = %v", err)
	}
}

func mustParse(s string) c4.ID {
	id, err := c4.Parse(s)
	if err != nil {
		panic(err)
	}
	return id
}
