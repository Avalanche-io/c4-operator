package egress

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// An address is the unit pod that has it; its profile is found in the
// pod's namespace, else the operator's. A pod with no profile, or no unit
// at the address, gets nothing.
func TestClusterResolves(t *testing.T) {
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	pod := func(name, ip, profile string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name,
			Labels: map[string]string{c4v1.LabelJob: "sh040", c4v1.LabelUnit: "3"}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip}}
		if profile != "" {
			p.Labels[c4v1.LabelToolProfile] = profile
		}
		return p
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&corev1.Pod{}, "status.podIP", func(o client.Object) []string { return []string{o.(*corev1.Pod).Status.PodIP} }).
		WithObjects(pod("u3", "10.0.0.3", "search"), pod("u4", "10.0.0.4", ""),
			&c4v1.ToolProfile{ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: "search"},
				Spec: c4v1.ToolProfileSpec{Egress: []c4v1.ToolEgress{{Host: "api.search.example"}}}}).Build()
	r := &Cluster{Client: c, Namespace: "c4-system"}
	ctx := context.Background()
	u, rules, err := r.Rules(ctx, "10.0.0.3")
	if err != nil || u.Job != "sh040" || u.Pod != "u3" || u.Profile != "search" || len(rules) != 1 || rules[0].Host != "api.search.example" {
		t.Fatalf("10.0.0.3: %+v %v %v", u, rules, err)
	}
	if _, _, err := r.Rules(ctx, "10.0.0.4"); err == nil || !strings.Contains(err.Error(), "no tool profile") {
		t.Fatalf("10.0.0.4: %v", err)
	}
	if _, _, err := r.Rules(ctx, "10.0.0.9"); err == nil || !strings.Contains(err.Error(), "no unit pod") {
		t.Fatalf("10.0.0.9: %v", err)
	}
}
