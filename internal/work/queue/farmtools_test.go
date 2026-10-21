package queue

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// The manager makes the farm's none tool, so the install manifest need not
// create a Tool beside its CRD; one an admin changed is left as it is.
func TestFarmToolsMadeByTheManager(t *testing.T) {
	ctx := context.Background()
	ns, q, _, _ := graphFarm(t)
	farmNS := ns + "-farm"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: farmNS}}); err != nil {
		t.Fatal(err)
	}
	q.Namespace = farmNS
	if !q.ensureFarmTools(ctx) {
		t.Fatal("the farm's tools were not made")
	}
	var none c4v1.Tool
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: farmNS, Name: NoneTool}, &none); err != nil {
		t.Fatalf("no none tool: %v", err)
	}
	// An admin's change survives the next start.
	none.Spec.View = viewID("admin-config")
	if err := k8s.Update(ctx, &none); err != nil {
		t.Fatal(err)
	}
	if !q.ensureFarmTools(ctx) {
		t.Fatal("an existing none tool counted as missing")
	}
	var again c4v1.Tool
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: farmNS, Name: NoneTool}, &again); err != nil {
		t.Fatal(err)
	}
	if again.Spec.View != none.Spec.View {
		t.Fatalf("the manager overwrote the admin's none tool: %q", again.Spec.View)
	}
}

// A farm tool's name resolves to the farm's tool, never to a Tool of that
// name in the job's namespace; any other name, the project's first.
func TestFarmToolsCannotBeReplaced(t *testing.T) {
	ctx := context.Background()
	ns, q, _, _ := graphFarm(t)
	farmNS := ns + "-farm"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: farmNS}}); err != nil {
		t.Fatal(err)
	}
	q.Namespace = farmNS
	if !q.ensureFarmTools(ctx) {
		t.Fatal("the farm's tools were not made")
	}
	for _, t2 := range []*c4v1.Tool{
		{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: NoneTool}, Spec: c4v1.ToolSpec{Egress: []c4v1.ToolEgress{{CIDR: "0.0.0.0/0"}}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: farmNS, Name: "search"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "search"}, Spec: c4v1.ToolSpec{C4: "read"}},
	} {
		if err := k8s.Create(ctx, t2); err != nil {
			t.Fatal(err)
		}
	}
	got, err := q.toolObject(ctx, ns, NoneTool)
	if err != nil || got.Namespace != farmNS || len(got.Spec.Egress) != 0 {
		t.Fatalf("none: %+v %v", got, err)
	}
	if got, err := q.toolObject(ctx, ns, "search"); err != nil || got.Namespace != ns {
		t.Fatalf("search: %+v %v", got, err)
	}
}
