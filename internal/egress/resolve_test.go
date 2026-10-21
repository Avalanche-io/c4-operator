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

// An address is the unit pod that has it; its tool is found in the
// pod's namespace, else the operator's. A pod with no tool, or no unit
// at the address, gets nothing.
func TestClusterResolves(t *testing.T) {
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	pod := func(name, ip, tool string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name,
			Labels: map[string]string{c4v1.LabelJob: "sh040", c4v1.LabelUnit: "3"}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip}}
		if tool != "" {
			p.Labels[c4v1.LabelTool] = tool
		}
		return p
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&corev1.Pod{}, "status.podIP", func(o client.Object) []string { return []string{o.(*corev1.Pod).Status.PodIP} }).
		WithObjects(pod("u3", "10.0.0.3", "search"), pod("u4", "10.0.0.4", ""),
			&c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: "search"},
				Spec: c4v1.ToolSpec{Egress: []c4v1.ToolEgress{{Host: "api.search.example"}}}}).Build()
	r := &Cluster{Client: c, Namespace: "c4-system"}
	ctx := context.Background()
	u, rules, err := r.Rules(ctx, "10.0.0.3")
	if err != nil || u.Job != "sh040" || u.Pod != "u3" || u.Tool != "search" || len(rules) != 1 || rules[0].Host != "api.search.example" {
		t.Fatalf("10.0.0.3: %+v %v %v", u, rules, err)
	}
	if _, _, err := r.Rules(ctx, "10.0.0.4"); err == nil || !strings.Contains(err.Error(), "no tool") {
		t.Fatalf("10.0.0.4: %v", err)
	}
	if _, _, err := r.Rules(ctx, "10.0.0.9"); err == nil || !strings.Contains(err.Error(), "no unit pod") {
		t.Fatalf("10.0.0.9: %v", err)
	}
}

// A tool job's unit reaches the networks its job's tool declares, as the
// farm defines them, and nothing else; the same tool declaring none is
// sealed. A refusal names the line that would allow the host.
func TestToolJobNetworks(t *testing.T) {
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	pod := func(name, ip, job string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name,
			Labels: map[string]string{c4v1.LabelJob: job, c4v1.LabelUnit: "0", c4v1.LabelTool: "fetch", c4v1.LabelJobTool: "true"}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip}}
	}
	job := func(name string, network ...string) *c4v1.WorkJob {
		return &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name},
			Spec: c4v1.WorkJobSpec{Tool: &c4v1.WorkTool{Name: "fetch", Network: network}}}
	}
	nets := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: c4v1.NetworksConfigMap},
		Data: map[string]string{"studio-api": "- {host: api.studio.example, ports: [443]}", "pypi": "- {host: pypi.org}\n- {host: files.pythonhosted.org}"}}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&corev1.Pod{}, "status.podIP", func(o client.Object) []string { return []string{o.(*corev1.Pod).Status.PodIP} }).
		WithObjects(nets, pod("d0", "10.0.1.1", "fetch-declared"), pod("s0", "10.0.1.2", "fetch-sealed"), pod("m0", "10.0.1.3", "fetch-missing"),
			job("fetch-declared", "studio-api"), job("fetch-sealed"), job("fetch-missing", "studio-db"),
			// a Tool object of the name allows more: it does not apply
			&c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "fetch"}, Spec: c4v1.ToolSpec{Egress: []c4v1.ToolEgress{{Host: "*.example"}}}}).Build()
	r := &Cluster{Client: c, Namespace: "c4-system"}
	ctx := context.Background()
	u, rules, err := r.Rules(ctx, "10.0.1.1")
	if err != nil || !u.JobTool || !Allowed(rules, "api.studio.example", 443) || Allowed(rules, "pypi.org", 443) || Allowed(rules, "evil.example", 443) {
		t.Fatalf("declared: %+v %v %v", u, rules, err)
	}
	if u, rules, err := r.Rules(ctx, "10.0.1.2"); err != nil || len(rules) != 0 {
		t.Fatalf("sealed: %+v %v %v", u, rules, err)
	}
	if _, _, err := r.Rules(ctx, "10.0.1.3"); err == nil || !strings.Contains(err.Error(), "fetch declares network studio-db, which this farm does not define: a farm admin adds it to the c4-networks ConfigMap in c4-system") {
		t.Fatalf("missing: %v", err)
	}
	if a := r.Advise(ctx, u, "pypi.org", 443); a != "add needs: {network: [pypi]} to fetch's tool.yaml and publish it again" {
		t.Fatalf("advice: %q", a)
	}
	if a := r.Advise(ctx, u, "evil.example", 443); !strings.Contains(a, "ask a farm admin to define a network for evil.example:443 in the c4-networks ConfigMap") {
		t.Fatalf("advice: %q", a)
	}
	if a := r.Advise(ctx, Unit{Tool: "search"}, "evil.example", 443); a != "add {host: evil.example, ports: [443]} to Tool search's spec.egress" {
		t.Fatalf("agent advice: %q", a)
	}
}

// The ConfigMap's values are rule lists, each rule one host, cidr or
// service; a bad one is an error naming the network, not a looser rule.
func TestParseNetworks(t *testing.T) {
	n, err := ParseNetworks(map[string]string{"pypi": "- {host: pypi.org}\n- {host: files.pythonhosted.org, ports: [443]}", "db": "- {service: pipeline/db}"})
	if err != nil || len(n["pypi"]) != 2 || n["db"][0].Service != "pipeline/db" {
		t.Fatalf("%v %v", n, err)
	}
	for _, bad := range []map[string]string{{"x": "host: a"}, {"x": "- {host: a, cidr: 10.0.0.0/8}"}, {"x": "- {ports: [1]}"}, {"Bad": "- {host: a}"}, {"licences": "- {host: a}"}, {"x": "- {hostname: a}"}} {
		if _, err := ParseNetworks(bad); err == nil {
			t.Errorf("%v parsed", bad)
		}
	}
}
