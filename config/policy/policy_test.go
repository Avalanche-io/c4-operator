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
	files := []string{"node-writes.yaml", "workjob-owner.yaml", "workjob-controls.yaml", "host-runner.yaml", "lease-mounts.yaml", "submit-grants.yaml", "tracker-people.yaml", "host-pods.yaml", "deployment-bindings.yaml", "telemetry-setting.yaml", "../rbac/host-runner.yaml"}
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

// enforced waits until policy refuses a write it must refuse. A policy and
// its binding take a moment to reach the admission plugin, and a case run
// before then would pass or fail only because the policy is not yet in
// force. probe makes the write, with a value or name of its own on each
// try, since rewriting an unchanged value is allowed and a second create
// of one name is refused for another reason. Only a refusal from policy
// counts, not one from RBAC still settling.
func enforced(t *testing.T, policy string, probe func(try int) error) {
	t.Helper()
	try := 0
	eventually(t, policy+" in force", func() error {
		try++
		err := probe(try)
		if err == nil {
			return errNotYet
		}
		if !strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+policy+"'") {
			return fmt.Errorf("refused, but not by the policy: %w", err)
		}
		return nil
	})
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
	enforced(t, "c4-node-writes", func(try int) error {
		return annotate(mine, "example.com/other", fmt.Sprint(try))
	})
	// the unit's notes the daemon relays: its questions and answers, and
	// how far it read its messages
	for _, k := range []string{c4v1.AnnotationOutputTree, c4v1.AnnotationPreflight, c4v1.AnnotationEgress,
		c4v1.AnnotationAsks, c4v1.AnnotationAnswers, c4v1.AnnotationSteerSeen} {
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
	enforced(t, "c4-workjob-owner", func(try int) error {
		return c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"metadata":{"annotations":{"c4.avalanche.io/owner":"ivo%d"}}}`, try))))
	})
	if err := c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"c4.avalanche.io/owner":null}}}`))); err == nil {
		t.Error("the owner was removed")
	}
	if err := c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"c4.avalanche.io/grant":"c4forged"}}}`))); err != nil {
		t.Fatalf("adding a grant where there was none is a creation-time field the policy leaves to admission: %v", err)
	}
	if err := c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"c4.avalanche.io/grant":"c4other"}}}`))); err == nil {
		t.Error("the grant a job was submitted under was changed")
	}
	if err := c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"c4.avalanche.io/submitted-by-unit":"saltmarsh/other/0"}}}`))); err == nil {
		t.Error("a job was said, after its creation, to be a unit's")
	}
	if err := c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"nudge":{"hold":true}}}`))); err != nil {
		t.Errorf("an ordinary change was refused: %v", err)
	}
	// A job's review is set when it is created (DESIGN.md 10.6, The job's
	// review): added later, it
	// would not fit the plan the queue has replayed.
	if err := c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"review":{"server":"person:noor"}}}`))); err == nil {
		t.Error("a review was added to a running job")
	}
	// A job is a forecast or not from its creation (DESIGN.md 10.12,
	// Forecast jobs): a real job
	// replaces a forecast; neither turns into the other.
	if err := c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"forecast":{"slotHours":"4"}}}`))); err == nil {
		t.Error("a running job became a forecast")
	}

	// a bundle's owner is fixed the same way: its member job carries it
	b := &c4v1.AgentBundle{ObjectMeta: metav1.ObjectMeta{Namespace: "saltmarsh", Name: "team",
		Annotations: map[string]string{c4v1.AnnotationOwner: "noor", c4v1.AnnotationSubmittedVia: "api"}},
		Spec: c4v1.AgentBundleSpec{Pools: []string{"team"}, Members: []c4v1.AgentMemberSpec{{Name: "lead"}}}}
	if err := c.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := c.Patch(ctx, b, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"c4.avalanche.io/owner":"ivo"}}}`))); err == nil {
		t.Error("a bundle's owner was changed")
	}
	if err := c.Patch(ctx, b, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":true}}`))); err != nil {
		t.Errorf("an ordinary change to a bundle was refused: %v", err)
	}

	// and a lease's creator: its holder is checked against it
	l := &c4v1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Namespace: "saltmarsh", Name: "mine",
		Annotations: map[string]string{c4v1.AnnotationOwner: "noor"}}, Spec: c4v1.CredentialLeaseSpec{
		Account: "anthropic-max", Holder: "noor", BoundTo: c4v1.LeaseBinding{Owners: []string{"noor"}},
		Mode: c4v1.LeaseHolderTransfer, Target: c4v1.LeaseTarget{Pools: []string{"blocks"}},
		TTL: metav1.Duration{Duration: time.Hour}, RenewBefore: metav1.Duration{Duration: 10 * time.Minute}}}
	if err := c.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := c.Patch(ctx, l, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"c4.avalanche.io/owner":"ivo"}}}`))); err == nil {
		t.Error("a lease's creator was changed")
	}
}

// TestSubmitGrantsOnlyFromTheHUD: a person's grant to a Tracker is written
// only by the HUD (the manager's account), never directly, so a Tracker's
// author cannot forge or erase one.
func TestSubmitGrantsOnlyFromTheHUD(t *testing.T) {
	cfg, admin := start(t)
	ctx := context.Background()
	admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "widgets"}})
	admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "manager-admin"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "c4-manager", Namespace: "c4-system"}}})
	mc := rest.CopyConfig(cfg)
	mc.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:c4-system:c4-manager",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:c4-system", "system:authenticated"}}
	s := runtime.NewScheme()
	c4v1.AddToScheme(s)
	hud, err := client.New(mc, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	grant := func(name string) *c4v1.SubmitGrant {
		return &c4v1.SubmitGrant{ObjectMeta: metav1.ObjectMeta{Namespace: "widgets", Name: name},
			Spec: c4v1.SubmitGrantSpec{Person: "noor", Tracker: "github", Until: metav1.NewTime(time.Now().Add(time.Hour))}}
	}
	enforced(t, "c4-submit-grants", func(try int) error {
		return admin.Create(ctx, grant(fmt.Sprintf("forged-%d", try)))
	})
	// the manager's binding may still be settling
	g := grant("github-noor")
	eventually(t, "the HUD's grant", func() error { return hud.Create(ctx, g) })
	if err := admin.Patch(ctx, g, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"person":"mallory"}}`))); err == nil {
		t.Error("a grant was changed directly")
	}
	if err := admin.Delete(ctx, g); err == nil {
		t.Error("a grant was erased directly")
	}
	g.Spec.Revoked = &c4v1.GrantRevocation{By: "noor", At: metav1.Now()}
	if err := hud.Update(ctx, g); err != nil {
		t.Errorf("the HUD's revocation: %v", err)
	}
	// A grant goes with its namespace, or the namespace could never be
	// deleted: once it is terminating, its grants may be deleted.
	if err := admin.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "widgets"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a grant deleted with its namespace", func() error { return admin.Delete(ctx, g) })
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
	enforced(t, "c4-host-runner-nodes", func(int) error {
		err := laptop.Create(ctx, node("someone-else", taint))
		if err == nil {
			admin.Delete(ctx, node("someone-else", nil))
		}
		return err
	})
	if err := laptop.Create(ctx, node("laptop-1", nil)); err == nil {
		t.Fatal("a Node without the host-runner taint was created")
	}
	owned := node("laptop-1", taint)
	owned.Annotations = map[string]string{c4v1.AnnotationNodeOwners: "noor"}
	if err := laptop.Create(ctx, owned); err == nil {
		t.Fatal("a runner named its own owners")
	}
	mine := node("laptop-1", taint)
	if err := laptop.Create(ctx, mine); err != nil {
		t.Fatalf("its own Node: %v", err)
	}
	patch := func(c client.Client, o client.Object, body string) error {
		return c.Patch(ctx, o, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	if err := patch(laptop, mine, `{"metadata":{"annotations":{"c4.avalanche.io/node-owners":"noor"}}}`); err == nil {
		t.Error("a runner added its own owners after registering")
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
	// the pods policy (which also covers Leases) comes into force apart
	// from the Node one
	enforced(t, "c4-host-runner-pods", func(try int) error {
		return patch(laptop, onW1, fmt.Sprintf(`{"metadata":{"annotations":{"example.com/probe":"%d"}}}`, try))
	})
	if err := patch(laptop, onMe, `{"metadata":{"annotations":{"c4.avalanche.io/output-checkpoint":"c4abc","c4.avalanche.io/output-epoch":"3"}}}`); err != nil {
		t.Errorf("checkpoint annotations on its pod: %v", err)
	}
	// the safe point it relays, and a generator's expansion document
	if err := patch(laptop, onMe, `{"metadata":{"annotations":{"c4.avalanche.io/safepoint":"{\"seq\":1}","c4.avalanche.io/output-expand":"c4abc","c4.avalanche.io/output-verdict":"c4def","c4.avalanche.io/output-measure":"c4fed","c4.avalanche.io/output-usage":"c4bee","c4.avalanche.io/output-tree":"c4cab","c4.avalanche.io/output-provenance":"c4rec","c4.avalanche.io/asks":"[]","c4.avalanche.io/answers":"[]","c4.avalanche.io/steer-seen":"1"}}}`); err != nil {
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

// TestLeaseMounts checks who may mount a credential lease's Secret: only
// the work queue (the manager's account), as a Secret volume; nobody puts
// one in an environment variable or a projected volume.
func TestLeaseMounts(t *testing.T) {
	cfg, admin := start(t)
	ctx := context.Background()
	admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "practice"}})
	// the manager's account, with the rights to create pods
	admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "manager-pods"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "c4-manager", Namespace: "c4-system"}}})
	mc := rest.CopyConfig(cfg)
	mc.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:c4-system:c4-manager",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:c4-system", "system:authenticated"}}
	manager, err := client.New(mc, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	pod := func(edit func(*corev1.Pod)) *corev1.Pod {
		n++
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "practice", Name: fmt.Sprintf("unit-%d", n)},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "block", Image: "x"}}}}
		edit(p)
		return p
	}
	withLease := func(p *corev1.Pod) {
		p.Spec.Volumes = []corev1.Volume{{Name: "lease", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "c4-lease-noor-claude"}}}}
		p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "lease", MountPath: "/etc/c4/lease", ReadOnly: true}}
	}
	enforced(t, "c4-lease-mounts", func(int) error { return admin.Create(ctx, pod(withLease)) })
	// the manager's binding may still be settling
	unit := pod(withLease)
	eventually(t, "the queue's unit pod with its lease", func() error { return manager.Create(ctx, unit) })
	// others still annotate it (the node daemon's output annotations)
	if err := admin.Patch(ctx, unit, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"c4.avalanche.io/output-tree":"c4abc"}}}`))); err != nil {
		t.Errorf("annotating a unit pod with a lease: %v", err)
	}
	if err := admin.Create(ctx, pod(func(*corev1.Pod) {})); err != nil {
		t.Errorf("a pod with no lease: %v", err)
	}
	env := func(p *corev1.Pod) {
		p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "T", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "c4-lease-noor-claude"}, Key: ".credentials.json"}}}}
	}
	if err := manager.Create(ctx, pod(env)); err == nil {
		t.Error("a lease went into an environment variable")
	}
	envFrom := func(p *corev1.Pod) {
		p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "c4-lease-noor-claude"}}}}
	}
	if err := manager.Create(ctx, pod(envFrom)); err == nil {
		t.Error("a lease went into the environment")
	}
	projected := func(p *corev1.Pod) {
		p.Spec.Volumes = []corev1.Volume{{Name: "all", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "c4-lease-noor-claude"}}}}}}}}
	}
	if err := manager.Create(ctx, pod(projected)); err == nil {
		t.Error("a lease was projected")
	}
}

// Only the work queue makes a pod a WorkJob's unit: no one else creates a
// pod whose controller reference names a WorkJob, or adds one to a pod
// later, so a host runner may read a pod's owner from the job it names.
// Others still annotate unit pods, and anyone may drop the reference.
func TestOnlyTheQueueMakesUnitPods(t *testing.T) {
	cfg, admin := start(t)
	ctx := context.Background()
	admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agents"}})
	admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "manager-pods"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "c4-manager", Namespace: "c4-system"}}})
	mc := rest.CopyConfig(cfg)
	mc.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:c4-system:c4-manager",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:c4-system", "system:authenticated"}}
	manager, err := client.New(mc, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	ref := []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: "team",
		UID: "6b1f0d4e-0000-4000-8000-000000000001", Controller: &yes}}
	pod := func(name string, refs []metav1.OwnerReference) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: name, OwnerReferences: refs},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "member", Image: "c4-agent"}}}}
	}
	enforced(t, "c4-host-pods", func(try int) error { return admin.Create(ctx, pod(fmt.Sprintf("forged-%d", try), ref)) })
	unit := pod("lead-0", ref)
	eventually(t, "the queue's unit pod", func() error { return manager.Create(ctx, unit) })
	if err := admin.Patch(ctx, unit, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"c4.avalanche.io/output-epoch":"3"}}}`))); err != nil {
		t.Errorf("annotating a unit pod: %v", err)
	}

	plain := pod("plain", nil)
	if err := admin.Create(ctx, plain); err != nil {
		t.Fatalf("a pod of no job: %v", err)
	}
	forge := client.MergeFrom(plain.DeepCopy())
	plain.OwnerReferences = ref
	if err := admin.Patch(ctx, plain, forge); err == nil || !strings.Contains(err.Error(), "c4-host-pods") {
		t.Errorf("a WorkJob reference added to a pod later: %v", err)
	}
	orphan := client.MergeFrom(unit.DeepCopy())
	unit.OwnerReferences = nil
	if err := admin.Patch(ctx, unit, orphan); err != nil {
		t.Errorf("dropping the reference: %v", err)
	}
}

// TestControlsOnlyFromTheHUD: a job's controls (decide, supply, withdraw,
// steer, requeue and the rest) are written only by the manager's account,
// the HUD, which checks who may give each and signs it; a cluster admin
// patching one directly is refused, and other annotations are untouched.
func TestControlsOnlyFromTheHUD(t *testing.T) {
	cfg, admin := start(t)
	ctx := context.Background()
	admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "saltmarsh"}})
	admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "manager-admin"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "c4-manager", Namespace: "c4-system"}}})
	mc := rest.CopyConfig(cfg)
	mc.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:c4-system:c4-manager",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:c4-system", "system:authenticated"}}
	s := runtime.NewScheme()
	c4v1.AddToScheme(s)
	hud, err := client.New(mc, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, anno map[string]string) *c4v1.WorkJob {
		return &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "saltmarsh", Name: name, Annotations: anno},
			Spec: c4v1.WorkJobSpec{Class: "render", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "u", Image: "x"}}}}}}
	}
	j := mk("sm0140-light-v001", nil)
	if err := admin.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	patch := func(c client.Client, k, v string) error {
		return c.Patch(ctx, j, client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, k, v))))
	}
	enforced(t, "c4-workjob-controls", func(try int) error {
		return patch(admin, c4v1.AnnotationDecide, fmt.Sprintf(`{"token":"t%d"}`, try))
	})
	for _, k := range []string{c4v1.AnnotationSupply, c4v1.AnnotationSteer, c4v1.AnnotationRequeue, c4v1.AnnotationAcks, c4v1.AnnotationAllowNodes, c4v1.AnnotationAcceptValidated, c4v1.AnnotationWithdrawNeeds, c4v1.AnnotationControlRecords} {
		if err := patch(admin, k, "x"); err == nil {
			t.Errorf("%s written directly", k)
		}
	}
	if err := patch(hud, c4v1.AnnotationDecide, `{"token":"hud"}`); err != nil {
		t.Errorf("the HUD's decide: %v", err)
	}
	if err := patch(hud, c4v1.AnnotationWithdrawNeeds, `[{"token":"hud"}]`); err != nil {
		t.Errorf("the HUD's withdrawal: %v", err)
	}
	if err := patch(admin, "example.com/note", "fine"); err != nil {
		t.Errorf("another annotation: %v", err)
	}
	if err := admin.Create(ctx, mk("sm0140-light-v002", map[string]string{c4v1.AnnotationRequeue: "t:1"})); err == nil {
		t.Error("a job created with a control by someone other than the HUD")
	}
	if err := hud.Create(ctx, mk("sm0140-light-v003", map[string]string{c4v1.AnnotationRequeue: "t:1"})); err != nil {
		t.Errorf("the HUD creating a job with a control: %v", err)
	}
}

// TestTrackerPeopleOnlyFromTheHUD: a person's confirmation that a tracker
// login is them is written only by the HUD (the manager's account), so a
// Tracker's author cannot make a login decide as anyone; it goes with its
// namespace.
func TestTrackerPeopleOnlyFromTheHUD(t *testing.T) {
	cfg, admin := start(t)
	ctx := context.Background()
	admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "widgets"}})
	admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "manager-admin"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "c4-manager", Namespace: "c4-system"}}})
	mc := rest.CopyConfig(cfg)
	mc.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:c4-system:c4-manager",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:c4-system", "system:authenticated"}}
	s := runtime.NewScheme()
	c4v1.AddToScheme(s)
	hud, err := client.New(mc, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	entry := func(name, person string) *c4v1.TrackerPerson {
		return &c4v1.TrackerPerson{ObjectMeta: metav1.ObjectMeta{Namespace: "widgets", Name: name},
			Spec: c4v1.TrackerPersonSpec{Tracker: "github", Login: "dana", Person: person,
				Offer:    c4v1.TrackerPersonOffer{From: "spec.people", Generation: 1},
				Accepted: c4v1.TrackerPersonAct{By: person, At: metav1.Now()}}}
	}
	enforced(t, "c4-tracker-people", func(try int) error {
		return admin.Create(ctx, entry(fmt.Sprintf("forged-%d", try), "dana"))
	})
	e := entry("github-dana", "dana")
	eventually(t, "the HUD's entry", func() error { return hud.Create(ctx, e) })
	if err := admin.Patch(ctx, e, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"person":"mallory"}}`))); err == nil {
		t.Error("an entry was changed directly")
	}
	if err := admin.Delete(ctx, e); err == nil {
		t.Error("an entry was erased directly")
	}
	if err := admin.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "widgets"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "an entry deleted with its namespace", func() error { return admin.Delete(ctx, e) })
}

// Only the HUD (the manager's account) binds a farm to its owner: an
// admin cannot create, change or delete a binding directly, and the
// manager itself cannot change one, since a new owner is a new binding.
func TestBindingsOnlyFromTheHUD(t *testing.T) {
	cfg, admin := start(t)
	ctx := context.Background()
	admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "manager-admin"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "c4-manager", Namespace: "c4-system"}}})
	mc := rest.CopyConfig(cfg)
	mc.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:c4-system:c4-manager",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:c4-system", "system:authenticated"}}
	s := runtime.NewScheme()
	c4v1.AddToScheme(s)
	hud, err := client.New(mc, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name, owner string) *c4v1.DeploymentBinding {
		return &c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: c4v1.DeploymentBindingSpec{
			Owner: owner, To: "laptop-1/*", For: []string{"task/v1"}, Start: metav1.Now(), By: "noor"}}
	}
	enforced(t, "c4-deployment-bindings", func(try int) error {
		return admin.Create(ctx, mk(fmt.Sprintf("probe-%d", try), "mallory"))
	})
	b := mk(c4v1.FarmBindingName, "noor")
	if err := hud.Create(ctx, b); err != nil {
		t.Fatalf("the HUD binds the farm: %v", err)
	}
	changed := b.DeepCopy()
	changed.Spec.Owner = "mallory"
	if err := admin.Update(ctx, changed); err == nil {
		t.Error("an admin changed the farm's owner")
	}
	if err := hud.Update(ctx, changed); err == nil || !strings.Contains(err.Error(), "a new owner is a new binding") {
		t.Errorf("the HUD changed a binding in place: %v", err)
	}
	if err := admin.Delete(ctx, b); err == nil {
		t.Error("an admin deleted the farm binding")
	}
}

// The farm's telemetry setting is the manager's alone to write: an admin
// with ConfigMap access in c4-system cannot raise the tier around the
// HUD's custody rule, nor delete the setting; other ConfigMaps there are
// untouched by the policy.
func TestTelemetrySettingOnlyFromTheManager(t *testing.T) {
	cfg, admin := start(t)
	ctx := context.Background()
	admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "c4-system"}})
	admin.Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "manager-admin"},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "c4-manager", Namespace: "c4-system"}}})
	mc := rest.CopyConfig(cfg)
	mc.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:c4-system:c4-manager",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:c4-system", "system:authenticated"}}
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	manager, err := client.New(mc, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	setting := func(tier string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: "c4-telemetry-setting"}, Data: map[string]string{"tier": tier}}
	}
	enforced(t, "c4-telemetry-setting", func(try int) error {
		cm := setting(fmt.Sprint("b", try))
		err := admin.Create(ctx, cm)
		if err == nil {
			// made before the policy was in force: take it away again
			admin.Delete(ctx, cm)
		}
		return err
	})
	cm := setting("a")
	eventually(t, "the manager's setting", func() error { return manager.Create(ctx, cm) })
	if err := admin.Patch(ctx, cm, client.RawPatch(types.MergePatchType, []byte(`{"data":{"tier":"b"}}`))); err == nil {
		t.Error("the tier was raised directly")
	}
	if err := admin.Delete(ctx, cm); err == nil {
		t.Error("the setting was deleted directly")
	}
	if err := manager.Patch(ctx, cm, client.RawPatch(types.MergePatchType, []byte(`{"data":{"tier":"b"}}`))); err != nil {
		t.Errorf("the manager could not change the tier: %v", err)
	}
	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: "c4-other"}, Data: map[string]string{"x": "y"}}
	if err := admin.Create(ctx, other); err != nil {
		t.Errorf("another ConfigMap was refused: %v", err)
	}
}
