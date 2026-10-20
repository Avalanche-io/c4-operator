package queue

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A unit that names a tool waits for the tool's view, then
// runs with it mounted at $C4_TOOLS, labelled for the tool's
// NetworkPolicy, which allows what the tool lists, and DNS, and
// nothing else. A tool with no egress seals its units; the farm's
// tools live in the operator's namespace. A unit naming no tool
// runs as before; one naming a missing tool waits, and the job says
// why.
func TestTools(t *testing.T) {
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
	for _, o := range []*c4v1.Tool{
		{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "search"}, Spec: c4v1.ToolSpec{View: view,
			Egress:   []c4v1.ToolEgress{{CIDR: "203.0.113.0/24", Ports: []int32{443}}, {Service: ns + "/searcher"}},
			Sidecars: []c4v1.ToolSidecar{{Name: "web-search", Image: "mcp-search:1", Args: []string{"--port", "8811"}, Port: 8811, Env: map[string]string{"MODE": "mcp"}}}}},
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
		t.Fatal("lookup waits for its view and ghost for its tool; plain and quiet run")
	}
	if c := meta.FindStatusCondition(get(t, ns, "tooled").Status.Conditions, "Tools"); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Tools condition %+v", c)
	}
	if p := podOfUnit(t, ns, "tooled", 2); p.Labels[c4v1.LabelTool] != "" {
		t.Fatalf("plain carries a tool: %v", p.Labels)
	}
	if p := podOfUnit(t, ns, "tooled", 3); p.Labels[c4v1.LabelTool] != "none" || !q.egressDenied(ctx, ns, p.Labels) {
		t.Fatalf("quiet: labels %v, sealed %v", p.Labels, q.egressDenied(ctx, ns, p.Labels))
	}

	readyMaterialization(t, ns, toolsName("tooled", "search"), view)
	tick(t, q)
	p := podOfUnit(t, ns, "tooled", 1)
	var mounted bool
	for _, vol := range p.Spec.Volumes {
		mounted = mounted || (vol.CSI != nil && vol.CSI.VolumeAttributes[c4v1.VolumeAttrMaterialization] == toolsName("tooled", "search"))
	}
	if p.Labels[c4v1.LabelTool] != "search" || !mounted || envOf(p.Spec.Containers[0], "C4_TOOLS") != ToolsMount {
		t.Fatalf("lookup's pod: labels %v, view mounted %v", p.Labels, mounted)
	}
	// Its sidecar runs beside it, a native sidecar, and the unit knows
	// where.
	var sc *corev1.Container
	for k := range p.Spec.InitContainers {
		if p.Spec.InitContainers[k].Name == "tool-web-search" {
			sc = &p.Spec.InitContainers[k]
		}
	}
	if sc == nil || sc.Image != "mcp-search:1" || sc.RestartPolicy == nil || *sc.RestartPolicy != corev1.ContainerRestartPolicyAlways ||
		envOf(*sc, "MODE") != "mcp" || envOf(p.Spec.Containers[0], "C4_TOOL_WEB_SEARCH") != "http://localhost:8811" {
		t.Fatalf("sidecar %+v, unit env %v", sc, p.Spec.Containers[0].Env)
	}
	var np networkingv1.NetworkPolicy
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "c4-tools-search"}, &np); err != nil {
		t.Fatal(err)
	}
	e := np.Spec.Egress
	if np.Spec.PodSelector.MatchLabels[c4v1.LabelTool] != "search" || len(e) != 3 ||
		e[0].To[0].IPBlock == nil || e[0].To[0].IPBlock.CIDR != "203.0.113.0/24" || e[0].Ports[0].Port.IntValue() != 443 ||
		e[1].To[0].PodSelector.MatchLabels["app"] != "searcher" || e[2].Ports[0].Port.IntValue() != 53 {
		t.Fatalf("policy %+v", np.Spec)
	}
	if q.egressDenied(ctx, ns, p.Labels) {
		t.Fatal("a unit with egress counted as sealed")
	}
	if hasPod(t, ns, "tooled", 4) {
		t.Fatal("ghost runs without its tool")
	}
}

// A tool that allows hosts sends its units through the farm's egress
// proxy: the policy allows the proxy and DNS, and the pod's HTTPS_PROXY
// names it. Without a proxy such a unit waits.
func TestToolHostsGoThroughTheProxy(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	farmNS := ns + "-farm"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: farmNS}}); err != nil {
		t.Fatal(err)
	}
	q.Namespace = farmNS
	for _, o := range []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: farmNS, Name: "c4-egress"},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app.kubernetes.io/component": "egress"}, Ports: []corev1.ServicePort{{Port: 3128}}}},
		&c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: farmNS, Name: "search"},
			Spec: c4v1.ToolSpec{Egress: []c4v1.ToolEgress{{Host: "api.search.example", Ports: []int32{443}}}}},
	} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := k8s.Create(ctx, job(ns, "hosts", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "lookup", "reasoning": {"does": "model", "tools": "search"}}]}`)
	finishUnit(t, now, ns, "hosts", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	if hasPod(t, ns, "hosts", 1) {
		t.Fatal("a unit with hosts ran with no egress proxy")
	}
	if c := meta.FindStatusCondition(get(t, ns, "hosts").Status.Conditions, "Tools"); c == nil || !strings.Contains(c.Message, "egress proxy") {
		t.Fatalf("Tools condition %+v", c)
	}
	q.EgressProxy = "c4-egress:3128"
	tick(t, q)
	p := podOfUnit(t, ns, "hosts", 1)
	want := "http://c4-egress." + farmNS + ".svc:3128"
	if envOf(p.Spec.Containers[0], "HTTPS_PROXY") != want || envOf(p.Spec.Containers[0], "https_proxy") != want || envOf(p.Spec.Containers[0], "NO_PROXY") == "" {
		t.Fatalf("lookup's env %v", p.Spec.Containers[0].Env)
	}
	var np networkingv1.NetworkPolicy
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "c4-tools-search"}, &np); err != nil {
		t.Fatal(err)
	}
	e := np.Spec.Egress
	if len(e) != 2 || e[0].To[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "egress" ||
		e[0].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != farmNS || e[0].Ports[0].Port.IntValue() != 3128 ||
		e[1].Ports[0].Port.IntValue() != 53 {
		t.Fatalf("policy %+v", np.Spec)
	}
}

// A tool that grants c4 gives its units a token for their pod alone,
// with the HUD's audience, and the HUD's address; its policy allows the
// HUD's pods on the port they listen on. Without a HUD such a unit waits.
func TestToolGrantsC4(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	farmNS := ns + "-farm"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: farmNS}}); err != nil {
		t.Fatal(err)
	}
	q.Namespace = farmNS
	for _, o := range []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: farmNS, Name: "c4-hud"},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app.kubernetes.io/component": "manager"},
				Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8090)}}}},
		&c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: farmNS, Name: "lead"}, Spec: c4v1.ToolSpec{C4: "job"}},
	} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := k8s.Create(ctx, job(ns, "granted", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [{"name": "lead", "reasoning": {"does": "model", "tools": "lead"}}]}`)
	finishUnit(t, now, ns, "granted", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	if hasPod(t, ns, "granted", 1) {
		t.Fatal("a unit granted c4 ran with no HUD")
	}
	q.HUDService = "c4-hud:80"
	tick(t, q)
	p := podOfUnit(t, ns, "granted", 1)
	var tok *corev1.ServiceAccountTokenProjection
	for _, v := range p.Spec.Volumes {
		if v.Projected != nil && len(v.Projected.Sources) == 1 {
			tok = v.Projected.Sources[0].ServiceAccountToken
		}
	}
	if tok == nil || tok.Audience != c4v1.HUDAudience || envOf(p.Spec.Containers[0], "C4_HUD") != "http://c4-hud."+farmNS+".svc:80/api/v1" ||
		envOf(p.Spec.Containers[0], "C4_HUD_TOKEN_FILE") != "/etc/c4/hud/token" || p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken {
		t.Fatalf("lead's pod: token %+v, env %v", tok, p.Spec.Containers[0].Env)
	}
	var np networkingv1.NetworkPolicy
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "c4-tools-lead"}, &np); err != nil {
		t.Fatal(err)
	}
	if e := np.Spec.Egress; len(e) != 2 || e[0].To[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "manager" || e[0].Ports[0].Port.IntValue() != 8090 {
		t.Fatalf("policy %+v", np.Spec)
	}
}
