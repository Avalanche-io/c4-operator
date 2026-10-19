package provreq

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

var k8s client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob(filepath.Join("..", "..", "..", "bin", "k8s", "k8s", "*"))
		if len(matches) == 0 {
			println("envtest binaries not found; run `make envtest`")
			os.Exit(0)
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		os.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd"), filepath.Join("testdata", "crd")},
		ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}
	code := m.Run()
	env.Stop()
	os.Exit(code)
}

func requests(t *testing.T, ns string) map[string]*unstructured.Unstructured {
	t.Helper()
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(GVK.GroupVersion().WithKind("ProvisioningRequestList"))
	if err := k8s.List(context.Background(), l, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	out := map[string]*unstructured.Unstructured{}
	for i := range l.Items {
		out[l.Items[i].GetName()] = &l.Items[i]
	}
	return out
}

func names(m map[string]*unstructured.Unstructured) []string {
	var out []string
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// answer plays Cluster Autoscaler: sets a condition on a request.
func answer(t *testing.T, pr *unstructured.Unstructured, typ string, status metav1.ConditionStatus, at time.Time) {
	t.Helper()
	conds, _, _ := unstructured.NestedSlice(pr.Object, "status", "conditions")
	conds = append(conds, map[string]any{"type": typ, "status": string(status), "reason": typ, "message": typ + " by the test",
		"lastTransitionTime": at.UTC().Format(time.RFC3339)})
	unstructured.SetNestedSlice(pr.Object, conds, "status", "conditions")
	if err := k8s.Status().Update(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
}

func readyNode(t *testing.T, name string, lbl map[string]string) {
	t.Helper()
	ctx := context.Background()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lbl}}
	if err := k8s.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := k8s.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(ctx, n) })
}

// A burst decision for a provisioning-request pool asks Cluster Autoscaler
// first whether the room exists (check-capacity), then for a scale-up; a
// booking that expires while the pool is short starts a fresh round, and
// a new decision gets fresh requests.
func TestProvisioningRequestRounds(t *testing.T) {
	ctx := context.Background()
	ns := fmt.Sprintf("pr-%d", time.Now().UnixNano()%1e9)
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	a := &Actuator{Client: k8s, Log: logr.Discard(), Namespace: ns, Now: func() time.Time { return now }}
	lbl := map[string]string{"pool": ns + "-spot"}
	pool := &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: ns + "-spot"}, Spec: c4v1.PoolSpec{NodeSelector: lbl, SlotsPerNode: 4,
		Burst: &c4v1.PoolBurst{MaxNodes: 5, ProvisioningSeconds: 300, CostPerSlotHour: "1.00", Provider: Provider}}}
	if err := k8s.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(ctx, pool) })
	readyNode(t, ns+"-spot-0", lbl)
	cr := &c4v1.CapacityRequest{ObjectMeta: metav1.ObjectMeta{Name: pool.Name}, Spec: c4v1.CapacityRequestSpec{Pool: pool.Name, Slots: 12, Approved: true}}
	if err := k8s.Create(ctx, cr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k8s.Delete(ctx, cr) })
	tick := func() map[string]*unstructured.Unstructured {
		t.Helper()
		if err := a.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		return requests(t, ns)
	}
	state := func() c4v1.CapacityRequestStatus {
		t.Helper()
		var got c4v1.CapacityRequest
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(cr), &got); err != nil {
			t.Fatal(err)
		}
		return got.Status
	}

	// 12 slots are 3 nodes: first, is there room?
	prs := tick()
	g := cr.Generation
	check := fmt.Sprintf("c4-%s-g%d-r0-check", pool.Name, g)
	pr := prs[check]
	if len(prs) != 1 || pr == nil {
		t.Fatalf("requests %v", names(prs))
	}
	class, _, _ := unstructured.NestedString(pr.Object, "spec", "provisioningClassName")
	sets, _, _ := unstructured.NestedSlice(pr.Object, "spec", "podSets")
	if class != ClassCheck || len(sets) != 1 || sets[0].(map[string]any)["count"].(int64) != 3 {
		t.Fatalf("check request %v", pr.Object["spec"])
	}
	var pt corev1.PodTemplate
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: TemplateName(pool.Name)}, &pt); err != nil || pt.Template.Spec.Affinity.PodAntiAffinity == nil {
		t.Fatalf("pod template %v %v", pt.Template.Spec, err)
	}

	// No room: scale up, atomically.
	answer(t, pr, CondProvisioned, metav1.ConditionFalse, now)
	prs = tick()
	scale := prs[fmt.Sprintf("c4-%s-g%d-r0-scale", pool.Name, g)]
	if scale == nil {
		t.Fatalf("requests %v", names(prs))
	}
	class, _, _ = unstructured.NestedString(scale.Object, "spec", "provisioningClassName")
	if valid, _, _ := unstructured.NestedString(scale.Object, "spec", "parameters", "ValidUntilSeconds"); class != ClassScale || valid != "1200" {
		t.Fatalf("scale request %v", scale.Object["spec"])
	}
	answer(t, scale, CondProvisioned, metav1.ConditionTrue, now)
	tick()
	if st := state(); st.State != "Provisioning" || !strings.Contains(st.Message, "booked until") || st.ProvisionedSlots != 4 {
		t.Fatalf("after the scale-up: %+v", st)
	}

	// The booking expires before the nodes came: after the backoff, a
	// fresh round, and the old requests go.
	answer(t, scale, CondBookingExpired, metav1.ConditionTrue, now)
	tick()
	now = now.Add(2 * time.Minute)
	prs = tick()
	if prs[fmt.Sprintf("c4-%s-g%d-r1-check", pool.Name, g)] == nil {
		t.Fatalf("no new round: %v", names(prs))
	}
	prs = tick()
	if len(prs) != 1 {
		t.Fatalf("the expired round's requests remain: %v", names(prs))
	}

	// A new decision (8 slots, 2 nodes) gets requests of its own.
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(cr), cr); err != nil {
		t.Fatal(err)
	}
	cr.Spec.Slots = 8
	if err := k8s.Update(ctx, cr); err != nil {
		t.Fatal(err)
	}
	prs = tick()
	if len(prs) != 1 || prs[fmt.Sprintf("c4-%s-g%d-r0-check", pool.Name, cr.Generation)] == nil {
		t.Fatalf("after a new decision: %v", names(prs))
	}

	// The nodes arrive: no request is needed any more.
	readyNode(t, ns+"-spot-1", lbl)
	prs = tick()
	if st := state(); len(prs) != 0 || st.State != "Provisioned" || st.ProvisionedSlots != 8 {
		t.Fatalf("with the nodes ready: %v %+v", names(prs), st)
	}
}
