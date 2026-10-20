package queue

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A unit that names a tool profile waits for the profile's view, then
// runs with it mounted at $C4_TOOLS, labelled for the profile's
// NetworkPolicy, which allows what the profile lists, and DNS, and
// nothing else. A profile with no egress seals its units; the farm's
// profiles live in the operator's namespace. A unit naming no profile
// runs as before; one naming a missing profile waits, and the job says
// why.
func TestToolProfiles(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	farmNS := ns + "-farm"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: farmNS}}); err != nil {
		t.Fatal(err)
	}
	q.Namespace = farmNS
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "searcher"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "searcher"}, Ports: []corev1.ServicePort{{Port: 8080}}}}
	view := viewID("c4search-mcp-config")
	for _, o := range []*c4v1.ToolProfile{
		{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "search"}, Spec: c4v1.ToolProfileSpec{View: view,
			Egress: []c4v1.ToolEgress{{CIDR: "203.0.113.0/24", Ports: []int32{443}}, {Service: ns + "/searcher"}}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: farmNS, Name: "none"}},
	} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := k8s.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, job(ns, "tooled", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [
		{"name": "lookup", "reasoning": {"does": "model", "tools": "search"}},
		{"name": "plain"},
		{"name": "quiet", "reasoning": {"tools": "none"}},
		{"name": "ghost", "reasoning": {"tools": "missing"}}]}`)
	finishUnit(t, now, ns, "tooled", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	// units: 1 lookup, 2 plain, 3 quiet, 4 ghost
	if hasPod(t, ns, "tooled", 1) || hasPod(t, ns, "tooled", 4) || !hasPod(t, ns, "tooled", 2) || !hasPod(t, ns, "tooled", 3) {
		t.Fatal("lookup waits for its view and ghost for its profile; plain and quiet run")
	}
	if c := meta.FindStatusCondition(get(t, ns, "tooled").Status.Conditions, "Tools"); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Tools condition %+v", c)
	}
	if p := podOfUnit(t, ns, "tooled", 2); p.Labels[c4v1.LabelToolProfile] != "" {
		t.Fatalf("plain carries a profile: %v", p.Labels)
	}
	if p := podOfUnit(t, ns, "tooled", 3); p.Labels[c4v1.LabelToolProfile] != "none" || !q.egressDenied(ctx, ns, p.Labels) {
		t.Fatalf("quiet: labels %v, sealed %v", p.Labels, q.egressDenied(ctx, ns, p.Labels))
	}

	readyMaterialization(t, ns, toolsName("tooled", "search"), view)
	tick(t, q)
	p := podOfUnit(t, ns, "tooled", 1)
	var mounted bool
	for _, vol := range p.Spec.Volumes {
		mounted = mounted || (vol.CSI != nil && vol.CSI.VolumeAttributes[c4v1.VolumeAttrMaterialization] == toolsName("tooled", "search"))
	}
	if p.Labels[c4v1.LabelToolProfile] != "search" || !mounted || envOf(p.Spec.Containers[0], "C4_TOOLS") != ToolsMount {
		t.Fatalf("lookup's pod: labels %v, view mounted %v", p.Labels, mounted)
	}
	var np networkingv1.NetworkPolicy
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "c4-tools-search"}, &np); err != nil {
		t.Fatal(err)
	}
	e := np.Spec.Egress
	if np.Spec.PodSelector.MatchLabels[c4v1.LabelToolProfile] != "search" || len(e) != 3 ||
		e[0].To[0].IPBlock == nil || e[0].To[0].IPBlock.CIDR != "203.0.113.0/24" || e[0].Ports[0].Port.IntValue() != 443 ||
		e[1].To[0].PodSelector.MatchLabels["app"] != "searcher" || e[2].Ports[0].Port.IntValue() != 53 {
		t.Fatalf("policy %+v", np.Spec)
	}
	if q.egressDenied(ctx, ns, p.Labels) {
		t.Fatal("a unit with egress counted as sealed")
	}
	if hasPod(t, ns, "tooled", 4) {
		t.Fatal("ghost runs without its profile")
	}
}
