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

	coordv1 "k8s.io/api/coordination/v1"
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
	bootstrap, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// the namespace a real cluster has for node leases
	bootstrap.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-node-lease"}})
	t.Cleanup(func() { env.Stop() })
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	c4v1.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	files := []string{"node-writes.yaml", "workjob-owner.yaml", "host-runner.yaml", "../rbac/host-runner.yaml"}
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

// TestHostRunnerWrites checks what a host runner (service account
// c4-hosts/laptop-1) may write: its own Node, Lease and pods, nothing else.
func TestHostRunnerWrites(t *testing.T) {
	cfg, admin := start(t)
	ctx := context.Background()
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "saltmarsh"}})
	admin.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}})
	as := func(host string) client.Client {
		hc := rest.CopyConfig(cfg)
		hc.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:c4-hosts:" + host,
			Groups: []string{"system:serviceaccounts", "system:serviceaccounts:c4-hosts", "system:authenticated"}}
		c, err := client.New(hc, client.Options{Scheme: s})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	laptop := as("laptop-1")
	taint := []corev1.Taint{{Key: "c4.avalanche.io/host-runner", Value: "true", Effect: corev1.TaintEffectNoSchedule}}
	node := func(name string, taints []corev1.Taint) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"c4.avalanche.io/pool": "agent",
			"c4.avalanche.io/host": name}}, Spec: corev1.NodeSpec{Taints: taints}}
	}
	// policies reach admission after a moment: wait until a forbidden
	// create is refused, then run the cases
	eventually(t, "the node policy in force", func() error {
		if err := laptop.Create(ctx, node("someone-else", taint)); err == nil {
			admin.Delete(ctx, node("someone-else", nil))
			return errNotYet
		}
		return nil
	})
	if err := laptop.Create(ctx, node("laptop-1", nil)); err == nil {
		t.Fatal("a Node without the host-runner taint was created")
	}
	mine := node("laptop-1", taint)
	if err := laptop.Create(ctx, mine); err != nil {
		t.Fatalf("its own Node: %v", err)
	}
	patch := func(c client.Client, o client.Object, body string) error {
		return c.Patch(ctx, o, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	if err := patch(laptop, mine, `{"metadata":{"labels":{"c4.avalanche.io/slot-limit":"1"},"annotations":{"c4.avalanche.io/capacity-reason":"on battery"}}}`); err != nil {
		t.Errorf("slot-limit and capacity-reason: %v", err)
	}
	if err := patch(laptop, mine, `{"metadata":{"labels":{"c4.avalanche.io/pool":"gpu"}}}`); err == nil {
		t.Error("the runner moved its Node to another pool")
	}
	if err := patch(laptop, mine, `{"spec":{"taints":null}}`); err == nil {
		t.Error("the runner removed its taint")
	}
	status := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "laptop-1"}}
	if err := laptop.Status().Patch(ctx, status, client.RawPatch(types.MergePatchType,
		[]byte(`{"status":{"conditions":[{"type":"Ready","status":"True","reason":"HostRunnerReady"}]}}`))); err != nil {
		t.Errorf("its own status: %v", err)
	}
	other := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}}
	if err := laptop.Status().Patch(ctx, other, client.RawPatch(types.MergePatchType,
		[]byte(`{"status":{"conditions":[{"type":"Ready","status":"False","reason":"x"}]}}`))); err == nil {
		t.Error("the runner wrote another Node's status")
	}

	// pods: its own, bound to laptop-1, and one on w1
	pod := func(name, node string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "saltmarsh", Name: name},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "m", Image: "x"}}}}
		if err := admin.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	onMe, onW1, unbound := pod("member-0", "laptop-1"), pod("render-3", "w1"), pod("member-1", "")
	if err := patch(laptop, onMe, `{"metadata":{"annotations":{"c4.avalanche.io/output-checkpoint":"c4abc","c4.avalanche.io/output-epoch":"3"}}}`); err != nil {
		t.Errorf("checkpoint annotations on its pod: %v", err)
	}
	// the safe point it relays, and a generator's expansion document
	if err := patch(laptop, onMe, `{"metadata":{"annotations":{"c4.avalanche.io/safepoint":"{\"seq\":1}","c4.avalanche.io/output-expand":"c4abc","c4.avalanche.io/output-verdict":"c4def","c4.avalanche.io/output-measure":"c4fed","c4.avalanche.io/output-usage":"c4bee","c4.avalanche.io/output-tree":"c4cab"}}}`); err != nil {
		t.Fatalf("the runner relays a safe point and names its documents on its pod: %v", err)
	}
	// but not the queue's answer
	if err := patch(laptop, onMe, `{"metadata":{"annotations":{"c4.avalanche.io/safepoint-decision":"{\"seq\":1,\"do\":\"continue\"}"}}}`); err == nil {
		t.Fatal("the runner wrote the queue's safe-point decision")
	}
	if err := patch(laptop, onMe, `{"metadata":{"annotations":{"c4.avalanche.io/owner":"someone"}}}`); err == nil {
		t.Error("the runner set another annotation")
	}
	if err := patch(laptop, onW1, `{"metadata":{"annotations":{"c4.avalanche.io/output-epoch":"1"}}}`); err == nil {
		t.Error("the runner wrote to a pod on another node")
	}
	if err := laptop.Status().Patch(ctx, onMe, client.RawPatch(types.MergePatchType, []byte(`{"status":{"phase":"Running"}}`))); err != nil {
		t.Errorf("its pod's status: %v", err)
	}
	if err := laptop.Status().Patch(ctx, onW1, client.RawPatch(types.MergePatchType, []byte(`{"status":{"phase":"Failed"}}`))); err == nil {
		t.Error("the runner wrote another node's pod's status")
	}
	bind := func(target string) error {
		return laptop.SubResource("binding").Create(ctx, unbound, &corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Namespace: "saltmarsh", Name: unbound.Name},
			Target:     corev1.ObjectReference{Kind: "Node", Name: target}})
	}
	if err := bind("w1"); err == nil {
		t.Error("the runner bound a pod to another node")
	}
	if err := bind("laptop-1"); err != nil {
		t.Errorf("binding to itself: %v", err)
	}

	// it finishes the deletion of its pod the farm is deleting, and
	// deletes nothing else
	zero := int64(0)
	del := func(p *corev1.Pod) error {
		return laptop.Delete(ctx, p, &client.DeleteOptions{GracePeriodSeconds: &zero})
	}
	held := func(name, node string) *corev1.Pod {
		p := pod(name, node)
		p.Finalizers = []string{"c4.avalanche.io/unit"}
		if err := admin.Update(ctx, p); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			admin.Patch(ctx, p, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`)))
		})
		return p
	}
	if err := del(onMe); err == nil {
		t.Error("the runner deleted its pod the farm is not deleting")
	}
	ownPod, otherPod := held("member-2", "laptop-1"), held("render-4", "w1")
	for _, p := range []*corev1.Pod{ownPod, otherPod} {
		if err := admin.Delete(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// the queue's drain mark, only on a pod the farm is deleting, and no
	// other yield
	if err := patch(laptop, onMe, `{"metadata":{"annotations":{"c4.avalanche.io/yield":"disruption"}}}`); err == nil {
		t.Error("the runner marked a pod the farm is not deleting as drained")
	}
	if err := patch(laptop, ownPod, `{"metadata":{"annotations":{"c4.avalanche.io/yield":"reclaim"}}}`); err == nil {
		t.Error("the runner wrote a yield other than the drain mark")
	}
	if err := patch(laptop, ownPod, `{"metadata":{"annotations":{"c4.avalanche.io/yield":"disruption"}}}`); err != nil {
		t.Errorf("the drain mark on its pod the farm is deleting: %v", err)
	}
	if err := del(ownPod); err != nil {
		t.Errorf("finishing the deletion of its own pod: %v", err)
	}
	if err := del(otherPod); err == nil {
		t.Error("the runner deleted another node's pod")
	}

	// its Lease, and not another's
	micro := metav1.NewMicroTime(time.Now())
	lease := func(name string) *coordv1.Lease {
		return &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-node-lease", Name: name},
			Spec: coordv1.LeaseSpec{HolderIdentity: &name, RenewTime: &micro}}
	}
	ls := runtime.NewScheme()
	coordv1.AddToScheme(ls)
	hc := rest.CopyConfig(cfg)
	hc.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:c4-hosts:laptop-1",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:c4-hosts", "system:authenticated"}}
	lc, _ := client.New(hc, client.Options{Scheme: ls})
	if err := lc.Create(ctx, lease("laptop-1")); err != nil {
		t.Errorf("its own Lease: %v", err)
	}
	if err := lc.Create(ctx, lease("w1")); err == nil {
		t.Error("the runner made another node's Lease")
	}
}
