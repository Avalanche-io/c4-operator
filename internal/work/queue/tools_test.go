package queue

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
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
		e[1].To[0].PodSelector.MatchLabels["app"] != "searcher" || e[2].Ports[0].Port.IntValue() != 53 ||
		len(e[2].To) != 1 || e[2].To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" ||
		e[2].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
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
	var proxySvc corev1.Service
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: farmNS, Name: "c4-egress"}, &proxySvc); err != nil || proxySvc.Spec.ClusterIP == "" {
		t.Fatalf("the proxy's service: %v", err)
	}
	// the proxy by its cluster IP: a unit that reaches only hosts needs no DNS
	want := "http://" + proxySvc.Spec.ClusterIP + ":3128"
	if envOf(p.Spec.Containers[0], "HTTPS_PROXY") != want || envOf(p.Spec.Containers[0], "https_proxy") != want || envOf(p.Spec.Containers[0], "NO_PROXY") == "" {
		t.Fatalf("lookup's env %v", p.Spec.Containers[0].Env)
	}
	var np networkingv1.NetworkPolicy
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "c4-tools-search"}, &np); err != nil {
		t.Fatal(err)
	}
	e := np.Spec.Egress
	if len(e) != 1 || e[0].To[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "egress" ||
		e[0].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != farmNS || e[0].Ports[0].Port.IntValue() != 3128 {
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

// A tool job's units run under its tool (spec.tool): labelled with it and
// selected by the job's own NetworkPolicy, which allows nothing when the
// tool declares no network, so the unit is sealed, and only the egress
// proxy and DNS when it declares one, the proxy then allowing only that
// network's hosts. A Tool object of the same name, and its wider policy,
// do not apply. A tool's test job runs the same way. A network the farm
// does not define holds the units, and the job says why.
func TestToolJobsRunUnderTheirTool(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	farmNS := ns + "-farm"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: farmNS}}); err != nil {
		t.Fatal(err)
	}
	q.Namespace, q.EgressProxy = farmNS, "c4-egress:3128"
	for _, o := range []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: farmNS, Name: "c4-egress"},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app.kubernetes.io/component": "egress"}, Ports: []corev1.ServicePort{{Port: 3128}}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: farmNS, Name: c4v1.NetworksConfigMap},
			Data: map[string]string{"studio-api": "- {host: api.studio.example, ports: [443]}"}},
		&c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "fetch"},
			Spec: c4v1.ToolSpec{Egress: []c4v1.ToolEgress{{CIDR: "0.0.0.0/0"}}}},
	} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	tree := publishVersion(t, st, ns, "fetch", "v1", "needs: {network: [studio-api, studio-db]}")
	mk := func(name string, network ...string) {
		t.Helper()
		j := job(ns, name, "render", "cpu-"+ns, map[string]string{"frames": "1-1"}, now.Add(4*time.Hour))
		j.Spec.Tool = &c4v1.WorkTool{Name: "fetch", Network: network, View: tree}
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	mk("fetch-sealed")
	mk("fetch-declared", "studio-api")
	mk("fetch-test-1a2b3c4d", "studio-api") // a tool's test job, as LowerTest makes it
	mk("fetch-missing", "studio-db")
	tick(t, q)

	policy := func(job string) networkingv1.NetworkPolicy {
		t.Helper()
		var np networkingv1.NetworkPolicy
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "c4-job-tool-" + job}, &np); err != nil {
			t.Fatal(err)
		}
		if np.Spec.PodSelector.MatchLabels[c4v1.LabelJob] != job || np.Spec.PodSelector.MatchLabels[c4v1.LabelJobTool] != "true" ||
			len(np.OwnerReferences) != 1 || np.OwnerReferences[0].Name != job {
			t.Fatalf("%s's policy %+v", job, np.ObjectMeta)
		}
		return np
	}
	sealed := podOfUnit(t, ns, "fetch-sealed", 0)
	if sealed.Labels[c4v1.LabelTool] != "fetch" || sealed.Labels[c4v1.LabelJobTool] != "true" || envOf(sealed.Spec.Containers[0], "HTTPS_PROXY") != "" {
		t.Fatalf("sealed pod %v %v", sealed.Labels, sealed.Spec.Containers[0].Env)
	}
	if np := policy("fetch-sealed"); len(np.Spec.Egress) != 0 || !q.egressDenied(ctx, ns, sealed.Labels) {
		t.Fatalf("not sealed: %+v", np.Spec)
	}
	var proxySvc corev1.Service
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: farmNS, Name: "c4-egress"}, &proxySvc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fetch-declared", "fetch-test-1a2b3c4d"} {
		p := podOfUnit(t, ns, name, 0)
		e := policy(name).Spec.Egress
		if p.Labels[c4v1.LabelTool] != "fetch" || envOf(p.Spec.Containers[0], "HTTPS_PROXY") != "http://"+proxySvc.Spec.ClusterIP+":3128" ||
			len(e) != 1 || e[0].To[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "egress" || e[0].Ports[0].Port.IntValue() != 3128 {
			t.Fatalf("%s: labels %v, egress %+v", name, p.Labels, e)
		}
	}
	if hasPod(t, ns, "fetch-missing", 0) {
		t.Fatal("a unit ran with a network the farm does not define")
	}
	if c := meta.FindStatusCondition(get(t, ns, "fetch-missing").Status.Conditions, "Tools"); c == nil || !strings.Contains(c.Message, "declares network studio-db, which this farm does not define") {
		t.Fatalf("Tools condition %+v", c)
	}
	// The Tool object's own policy, made for agents, leaves tool jobs' units out.
	if err := q.ensureToolsPolicy(ctx, ns, &c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "fetch"}, Spec: c4v1.ToolSpec{Egress: []c4v1.ToolEgress{{CIDR: "0.0.0.0/0"}}}}); err != nil {
		t.Fatal(err)
	}
	var wide networkingv1.NetworkPolicy
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "c4-tools-fetch"}, &wide); err != nil {
		t.Fatal(err)
	}
	sel, _ := metav1.LabelSelectorAsSelector(&wide.Spec.PodSelector)
	if sel.Matches(labels.Set(sealed.Labels)) {
		t.Fatal("the Tool's policy selects a tool job's unit")
	}
}

// Every namespace with units denies the unit pods egress by default: a
// job with no tool reaches nothing, a tool's policy only adds to that, a
// tool job's unit stays sealed once its job's own policy is gone (the job
// being deleted), and the queue puts the default back if it is removed.
func TestUnitsDenyEgressByDefault(t *testing.T) {
	ns, q, now, _ := graphFarm(t)
	ctx := context.Background()
	farmNS := ns + "-farm"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: farmNS}}); err != nil {
		t.Fatal(err)
	}
	q.Namespace = farmNS
	if err := k8s.Create(ctx, job(ns, "plain", "render", "cpu-"+ns, map[string]string{"frames": "1-1"}, now.Add(4*time.Hour))); err != nil {
		t.Fatal(err)
	}
	tj := job(ns, "fetch-sealed", "render", "cpu-"+ns, map[string]string{"frames": "1-1"}, now.Add(4*time.Hour))
	tj.Spec.Tool = &c4v1.WorkTool{Name: "fetch"}
	if err := k8s.Create(ctx, tj); err != nil {
		t.Fatal(err)
	}
	tick(t, q)

	deny := func() networkingv1.NetworkPolicy {
		t.Helper()
		var np networkingv1.NetworkPolicy
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: UnitsPolicyName}, &np); err != nil {
			t.Fatal(err)
		}
		return np
	}
	np := deny()
	want := []metav1.LabelSelectorRequirement{{Key: c4v1.LabelJob, Operator: metav1.LabelSelectorOpExists},
		{Key: c4v1.LabelUnit, Operator: metav1.LabelSelectorOpExists}}
	if len(np.Spec.PodSelector.MatchLabels) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 2 ||
		!equality.Semantic.DeepEqual(np.Spec.PodSelector.MatchExpressions, want) ||
		len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress || len(np.Spec.Egress) != 0 ||
		len(np.OwnerReferences) != 0 {
		t.Fatalf("default policy %+v %+v", np.ObjectMeta, np.Spec)
	}
	plain := podOfUnit(t, ns, "plain", 0)
	if sel, _ := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector); !sel.Matches(labels.Set(plain.Labels)) || !q.egressDenied(ctx, ns, plain.Labels) {
		t.Fatalf("a unit with no tool is not sealed: %v", plain.Labels)
	}
	// The job says so where a person looks, in one sentence; a tool job
	// does not.
	if c := meta.FindStatusCondition(get(t, ns, "plain").Status.Conditions, c4v1.ConditionNetwork); c == nil ||
		c.Reason != "NoTool" || c.Message != "this job names no tool, so it has no network: name a tool that declares one" {
		t.Fatalf("plain's Network condition %+v", c)
	}
	if c := meta.FindStatusCondition(get(t, ns, "fetch-sealed").Status.Conditions, c4v1.ConditionNetwork); c != nil {
		t.Fatalf("a tool job says it names no tool: %+v", c)
	}
	// Not a unit: the default leaves other pods in the namespace alone.
	if sel, _ := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector); sel.Matches(labels.Set{"app": "asset-db"}) {
		t.Fatal("the default selects a pod that is not a unit")
	}

	// The job's own policy goes with the job; its unit is still sealed.
	sealed := podOfUnit(t, ns, "fetch-sealed", 0)
	if err := k8s.Delete(ctx, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: jobToolPolicyName("fetch-sealed")}}); err != nil {
		t.Fatal(err)
	}
	if !q.egressDenied(ctx, ns, sealed.Labels) {
		t.Fatal("a tool job's unit opened when its job's policy went")
	}

	// Removed or widened, the default comes back as it was.
	if err := k8s.Delete(ctx, &np); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if np := deny(); len(np.Spec.Egress) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 2 {
		t.Fatalf("not put back: %+v", np.Spec)
	}
	np = deny()
	np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	if err := k8s.Update(ctx, &np); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if np := deny(); len(np.Spec.Egress) != 0 {
		t.Fatalf("widened default kept: %+v", np.Spec)
	}
}

// A policy that allows a unit some egress makes it unsealed, whatever
// other policy denies it: policies add up.
func TestEgressDeniedReadsEveryPolicy(t *testing.T) {
	ns, q, _ := farm(t, "cpu", 1, 1)
	ctx := context.Background()
	unit := map[string]string{c4v1.LabelJob: "sh010", c4v1.LabelUnit: "0", c4v1.LabelTool: "fetch"}
	if q.egressDenied(ctx, ns, unit) {
		t.Fatal("sealed with no policy at all")
	}
	if err := q.ensurePolicy(ctx, unitsPolicy(ns)); err != nil {
		t.Fatal(err)
	}
	if !q.egressDenied(ctx, ns, unit) {
		t.Fatal("the default did not seal a unit")
	}
	// An ingress-only policy changes nothing.
	ingress := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ingress-only"},
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}}
	if err := k8s.Create(ctx, ingress); err != nil {
		t.Fatal(err)
	}
	if !q.egressDenied(ctx, ns, unit) {
		t.Fatal("an ingress policy unsealed a unit")
	}
	allow := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "allow-fetch"},
		Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{c4v1.LabelTool: "fetch"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.0/24"}}}}}}}
	if err := k8s.Create(ctx, allow); err != nil {
		t.Fatal(err)
	}
	if q.egressDenied(ctx, ns, unit) {
		t.Fatal("counted sealed while another policy allows it egress")
	}
	if !q.egressDenied(ctx, ns, map[string]string{c4v1.LabelJob: "sh010", c4v1.LabelUnit: "1"}) {
		t.Fatal("a unit the allowing policy does not select is not sealed")
	}
}

// Inside a tool job a unit cannot run under another tool: one an
// expansion names waits, and the job says why; one naming the job's own
// tool runs under it, as the job's.
func TestToolJobUnitCannotNameAnotherTool(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	farmNS := ns + "-farm"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: farmNS}}); err != nil {
		t.Fatal(err)
	}
	q.Namespace = farmNS
	if err := k8s.Create(ctx, &c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "wide"},
		Spec: c4v1.ToolSpec{Egress: []c4v1.ToolEgress{{CIDR: "0.0.0.0/0"}}}}); err != nil {
		t.Fatal(err)
	}
	j := job(ns, "fetch-graph", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))
	j.Spec.Tool = &c4v1.WorkTool{Name: "fetch"}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	doc := putDoc(t, st, `{"version": 1, "units": [
		{"name": "same", "reasoning": {"tools": "fetch"}},
		{"name": "sneak", "reasoning": {"tools": "wide"}}]}`)
	finishUnit(t, now, ns, "fetch-graph", 0, map[string]string{c4v1.AnnotationOutputExpand: doc})
	tick(t, q)
	if hasPod(t, ns, "fetch-graph", 2) {
		t.Fatal("a tool job's unit ran under another tool")
	}
	if c := meta.FindStatusCondition(get(t, ns, "fetch-graph").Status.Conditions, "Tools"); c == nil ||
		!strings.Contains(c.Message, "names tool wide, but fetch-graph runs under tool fetch") {
		t.Fatalf("Tools condition %+v", c)
	}
	p := podOfUnit(t, ns, "fetch-graph", 1)
	if p.Labels[c4v1.LabelTool] != "fetch" || p.Labels[c4v1.LabelJobTool] != "true" || !q.egressDenied(ctx, ns, p.Labels) {
		t.Fatalf("same: labels %v", p.Labels)
	}
	var np networkingv1.NetworkPolicy
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "c4-tools-wide"}, &np); err == nil {
		t.Fatal("the other tool's policy was written")
	}
}

// publishVersion records version ver of Tool name in ns (made already) as
// a tree whose tool.yaml is doc, stored in st, as publishing does, and
// returns the tree's C4 ID.
func publishVersion(t *testing.T, st *storeurl.Sharded, ns, name, ver, doc string) string {
	t.Helper()
	tree := storeToolTree(t, st, doc)
	ctx := context.Background()
	var p c4v1.Tool
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	p.Status.Versions = append(p.Status.Versions, c4v1.ToolVersion{Version: ver, View: tree, By: "noor", At: metav1.Now(),
		Test: c4v1.ToolTest{State: c4v1.ToolTestUntested}})
	if err := k8s.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	return tree
}

// storeToolTree stores a tree holding only tool.yaml (doc) and returns
// its C4 ID.
func storeToolTree(t *testing.T, st *storeurl.Sharded, doc string) string {
	t.Helper()
	ctx := context.Background()
	id, err := st.PutBytes(ctx, []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	root := c4view.NewNode("")
	root.Files["tool.yaml"] = c4view.FileEntry("tool.yaml", int64(len(doc)), id, false)
	v := c4view.FromNode(root)
	for _, rec := range v.Records() {
		if _, err := st.PutBytes(ctx, rec.Data); err != nil {
			t.Fatal(err)
		}
	}
	return v.ID.String()
}

// A tool job's networks are checked against what its tool's version
// declares, as the Tool records the version, not the job's own copy: a
// job written directly claiming a network the version does not declare is
// held, and says so; one naming no version, or a tree that is no version
// of the tool, is held too; a job claiming what the version declares is
// released.
func TestToolJobClaimsOnlyDeclaredNetworks(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	farmNS := ns + "-farm"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: farmNS}}); err != nil {
		t.Fatal(err)
	}
	q.Namespace, q.EgressProxy = farmNS, "c4-egress:3128"
	for _, o := range []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: farmNS, Name: "c4-egress"},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app.kubernetes.io/component": "egress"}, Ports: []corev1.ServicePort{{Port: 3128}}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: farmNS, Name: c4v1.NetworksConfigMap},
			Data: map[string]string{"studio-api": "- {host: api.studio.example, ports: [443]}", "pypi": "- {host: pypi.org}"}},
		&c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "fetch"}},
	} {
		if err := k8s.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	tree := publishVersion(t, st, ns, "fetch", "v2", "c4: tool/1\nname: fetch\nneeds: {network: [studio-api]}\n")
	forged := storeToolTree(t, st, "needs: {network: [studio-api, pypi]}")
	mk := func(name, view string, network ...string) {
		t.Helper()
		j := job(ns, name, "render", "cpu-"+ns, map[string]string{"frames": "1-1"}, now.Add(4*time.Hour))
		j.Spec.Tool = &c4v1.WorkTool{Name: "fetch", Network: network, View: view}
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	mk("fetch-matches", tree, "studio-api")
	mk("fetch-claims", tree, "studio-api", "pypi")
	mk("fetch-noview", "", "studio-api")
	mk("fetch-forged", forged, "studio-api", "pypi")
	tick(t, q)

	if !hasPod(t, ns, "fetch-matches", 0) {
		t.Fatal("a job claiming what its version declares was held")
	}
	for job, want := range map[string]string{
		"fetch-claims": "this job claims network pypi that tool fetch version v2 does not declare",
		"fetch-noview": "this job claims network studio-api under tool fetch but names no version of it",
		"fetch-forged": "is not a version of fetch",
	} {
		if hasPod(t, ns, job, 0) {
			t.Errorf("%s ran", job)
		}
		if c := meta.FindStatusCondition(get(t, ns, job).Status.Conditions, "Tools"); c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, want) {
			t.Errorf("%s: Tools condition %+v, want %q", job, c, want)
		}
	}
}
