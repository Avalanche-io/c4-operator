// Package policy_test checks the admission policies in this directory
// against a real API server (envtest): what the node daemon may write to
// pods, and that a WorkJob's owner never changes.
package policy_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

const nodeSA = "system:serviceaccount:c4-system:c4-node"

func start(t *testing.T) (*rest.Config, client.Client) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob("../../bin/k8s/k8s/*")
		if len(matches) == 0 {
			t.Skip("envtest binaries not found; run `make envtest`")
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		t.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Stop() })
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	c4v1.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	files := []string{"node-writes.yaml", "workjob-owner.yaml"}
	if v := os.Getenv("POLICY_FILES"); v != "" {
		files = strings.Split(v, ",")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		r := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
		for {
			var u unstructured.Unstructured
			if err := r.Decode(&u.Object); err != nil {
				break
			}
			if len(u.Object) == 0 {
				continue
			}
			if err := c.Create(context.Background(), &u); err != nil {
				t.Fatalf("%s %s: %v", u.GetKind(), u.GetName(), err)
			}
		}
	}
	return cfg, c
}

// eventually retries f until it returns nil: a new policy takes a moment
// to reach the admission plugin.
func eventually(t *testing.T, what string, f func() error) {
	t.Helper()
	var err error
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if err = f(); err == nil {
			return
		}
	}
	t.Fatalf("%s: %v", what, err)
}

func TestNodeDaemonWrites(t *testing.T) {
	cfg, admin := start(t)
	ctx := context.Background()
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "saltmarsh"}})
	admin.Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "node-test"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "patch"}}}})
	admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "node-test"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "node-test"},
		Subjects: []rbacv1.Subject{{Kind: "User", Name: nodeSA}}})
	pod := func(name, node string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "saltmarsh", Name: name},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "u", Image: "x"}}}}
		if err := admin.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mine, theirs := pod("u3", "w1"), pod("u4", "w2")

	nc := rest.CopyConfig(cfg)
	nc.Impersonate = rest.ImpersonationConfig{UserName: nodeSA, Extra: map[string][]string{"authentication.kubernetes.io/node-name": {"w1"}}}
	node, err := client.New(nc, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	annotate := func(p *corev1.Pod, k, v string) error {
		return node.Patch(ctx, p, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+k+`":"`+v+`"}}}`)))
	}
	// the policy is in force once an unrelated annotation is refused; each
	// try writes a new value, since rewriting an unchanged one is allowed
	try := 0
	eventually(t, "the policy refusing another annotation", func() error {
		try++
		if err := annotate(mine, "example.com/other", fmt.Sprint(try)); err == nil {
			return errNotYet
		}
		return nil
	})
	for _, k := range []string{c4v1.AnnotationOutputTree, c4v1.AnnotationPreflight, c4v1.AnnotationEgress} {
		if err := annotate(mine, k, "v"); err != nil {
			t.Errorf("%s on its own node's pod: %v", k, err)
		}
	}
	if err := annotate(theirs, c4v1.AnnotationPreflight, "v"); err == nil {
		t.Error("the node wrote to a pod on another node")
	}
	if err := node.Patch(ctx, mine, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"a":"b"}}}`))); err == nil {
		t.Error("the node changed a label")
	}
}

func TestWorkJobOwnerIsFixed(t *testing.T) {
	_, c := start(t)
	ctx := context.Background()
	c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "saltmarsh"}})
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "saltmarsh", Name: "sm0140-light-v001",
		Annotations: map[string]string{c4v1.AnnotationOwner: "noor", c4v1.AnnotationSubmittedVia: "hud"}},
		Spec: c4v1.WorkJobSpec{Class: "render", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "u", Image: "x"}}}}}}
	if err := c.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	try := 0
	eventually(t, "the owner refusing to change", func() error {
		try++
		err := c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"metadata":{"annotations":{"c4.avalanche.io/owner":"ivo%d"}}}`, try))))
		if err == nil {
			return errNotYet
		}
		return nil
	})
	if err := c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"c4.avalanche.io/owner":null}}}`))); err == nil {
		t.Error("the owner was removed")
	}
	if err := c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"nudge":{"hold":true}}}`))); err != nil {
		t.Errorf("an ordinary change was refused: %v", err)
	}
}

type notYet struct{}

func (notYet) Error() string { return "not in force yet" }

var errNotYet error = notYet{}
