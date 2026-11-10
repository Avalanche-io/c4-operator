package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

var projectGVK = schema.GroupVersionKind{Group: "c4.avalanche.io", Version: "v1alpha1", Kind: "Project"}

// verifyFillSchema is the verifyFill property of each served version of
// the Project CRD.
func verifyFillSchema(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) []*apiextensionsv1.JSONSchemaProps {
	t.Helper()
	var out []*apiextensionsv1.JSONSchemaProps
	for i := range crd.Spec.Versions {
		spec := crd.Spec.Versions[i].Schema.OpenAPIV3Schema.Properties["spec"]
		store := spec.Properties["store"]
		p, ok := store.Properties["verifyFill"]
		if !ok {
			t.Fatal("the Project CRD has no spec.store.verifyFill")
		}
		out = append(out, &p)
	}
	return out
}

// rawProject builds a Project as a client that knows nothing of this
// operator's Go types would: verifyFill only when given.
func rawProject(name string, verifyFill *bool) *unstructured.Unstructured {
	store := map[string]any{"shards": []any{map[string]any{"name": "s0", "url": "c4d://store.example:17433"}}}
	if verifyFill != nil {
		store["verifyFill"] = *verifyFill
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"namespaces": []any{name}, "store": store},
	}}
	u.SetGroupVersionKind(projectGVK)
	return u
}

// storedVerifyFill reads a Project as the API serves it, and returns its
// spec.store.verifyFill and whether the field is there at all.
func storedVerifyFill(t *testing.T, c client.Client, name string) (value, present bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(projectGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, u); err != nil {
		t.Fatal(err)
	}
	v, present, err := unstructured.NestedBool(u.Object, "spec", "store", "verifyFill")
	if err != nil {
		t.Fatal(err)
	}
	return v, present
}

// verifyFill became a field that is on unless a Project says false. This
// is the upgrade, against a real API server: Projects are stored under the
// CRD as it was (a plain boolean, no default), then the CRD is replaced by
// today's.
//   - A Project stored without the field verifies afterwards.
//   - A Project that said false still says false, and a write through the
//     operator's own types keeps it.
//   - A Project that said true still does.
func TestVerifyFillAcrossTheUpgrade(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob("../../bin/k8s/k8s/*")
		if len(matches) == 0 {
			t.Skip("envtest binaries not found; run `make envtest`")
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		t.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "c4.avalanche.io_projects.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	now := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, now); err != nil {
		t.Fatal(err)
	}
	for _, p := range verifyFillSchema(t, now) {
		if p.Default == nil || string(p.Default.Raw) != "true" {
			t.Fatalf("today's CRD does not default verifyFill to true: %v", p.Default)
		}
	}
	// The CRD as it was: the same, with no default on the field.
	before := now.DeepCopy()
	for i := range before.Spec.Versions {
		spec := before.Spec.Versions[i].Schema.OpenAPIV3Schema.Properties["spec"]
		store := spec.Properties["store"]
		p := store.Properties["verifyFill"]
		p.Default = nil
		store.Properties["verifyFill"] = p
		spec.Properties["store"] = store
		before.Spec.Versions[i].Schema.OpenAPIV3Schema.Properties["spec"] = spec
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Stop() })
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{before}}); err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	c4v1.AddToScheme(s)
	apiextensionsv1.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	no, yes := false, true
	for name, v := range map[string]*bool{"absent": nil, "says-false": &no, "says-true": &yes} {
		if err := c.Create(ctx, rawProject(name, v)); err != nil {
			t.Fatalf("create %s under the old CRD: %v", name, err)
		}
	}
	if _, present := storedVerifyFill(t, c, "absent"); present {
		t.Fatal("under the old CRD a Project created without verifyFill has one; the test's premise is wrong")
	}

	// The upgrade: today's schema replaces the old one.
	live := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, client.ObjectKey{Name: now.Name}, live); err != nil {
		t.Fatal(err)
	}
	live.Spec.Versions = now.Spec.Versions
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	// The API server takes up the new schema in its own time.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if v, present := storedVerifyFill(t, c, "absent"); present && v {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("after the upgrade, a Project stored without verifyFill is still served without it")
		}
		time.Sleep(200 * time.Millisecond)
	}

	typed := func(name string) *c4v1.Project {
		t.Helper()
		p := &c4v1.Project{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, want := range map[string]bool{"absent": true, "says-false": false, "says-true": true} {
		if got := typed(name).Spec.Store.VerifiesFills(); got != want {
			t.Errorf("after the upgrade %s verifies fills: %v, want %v", name, got, want)
		}
	}

	// A write through the operator's own types must not drop a stated
	// false (a plain bool with omitempty would, and the default would
	// then turn it into true).
	p := typed("says-false")
	p.Labels = map[string]string{"touched": "yes"}
	if err := c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if v, present := storedVerifyFill(t, c, "says-false"); !present || v {
		t.Errorf("after a typed update, says-false is stored as verifyFill=%v (present %v)", v, present)
	}
	// Created through the types: a stated false is stored; nothing stated
	// is stored as true.
	for name, v := range map[string]*bool{"typed-false": &no, "typed-absent": nil} {
		np := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}}
		np.Spec.Namespaces = []string{name}
		np.Spec.Store.Shards = []c4v1.StoreShard{{Name: "s0", URL: "c4d://store.example:17433"}}
		np.Spec.Store.VerifyFill = v
		if err := c.Create(ctx, np); err != nil {
			t.Fatal(err)
		}
		got, present := storedVerifyFill(t, c, name)
		if want := v == nil; !present || got != want {
			t.Errorf("%s is stored as verifyFill=%v (present %v), want %v", name, got, present, want)
		}
	}
}
