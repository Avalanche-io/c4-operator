package localfarm

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4-operator/internal/source/huggingface"
)

// Each network is a list of egress rules in the format the farm reads
// (one host each, its ports), and huggingface is exactly the hosts the
// Hugging Face source follows redirects to.
func TestNetworksFormat(t *testing.T) {
	for name, v := range Networks() {
		var rules []struct {
			Host  string  `json:"host"`
			Ports []int32 `json:"ports"`
		}
		if err := yaml.UnmarshalStrict([]byte(v), &rules); err != nil || len(rules) == 0 {
			t.Fatalf("%s: %v", name, err)
		}
		var hosts []string
		for _, r := range rules {
			if r.Host == "" || !slices.Equal(r.Ports, []int32{443}) {
				t.Errorf("%s: rule %+v", name, r)
			}
			hosts = append(hosts, r.Host)
		}
		if name == "huggingface" && !slices.Equal(hosts, huggingface.Hosts) {
			t.Errorf("huggingface is %v, the source follows %v", hosts, huggingface.Hosts)
		}
	}
	if _, ok := Networks()["model-api"]; !ok {
		t.Error("no model-api, which the agent tool declares")
	}
}

// up gives a farm the networks it does not define and leaves the ones it
// does, a farm admin's own and their edits to these, as they are.
func TestEnsureNetworks(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(s).Build()
	added, err := ensureNetworks(ctx, c)
	if err != nil || !slices.Equal(added, []string{"huggingface", "model-api"}) {
		t.Fatalf("a new farm: %v %v", added, err)
	}

	edited := "- {host: gateway.studio.example, ports: [443]}\n"
	c = fake.NewClientBuilder().WithScheme(s).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: NetworksConfigMap},
		Data:       map[string]string{"model-api": edited, "studio-api": "- {host: api.studio.example}\n"},
	}).Build()
	if added, err := ensureNetworks(ctx, c); err != nil || !slices.Equal(added, []string{"huggingface"}) {
		t.Fatalf("a farm with its own: %v %v", added, err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "c4-system", Name: NetworksConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["model-api"] != edited || cm.Data["studio-api"] == "" || cm.Data["huggingface"] != Networks()["huggingface"] {
		t.Fatalf("data %v", cm.Data)
	}
	if added, err := ensureNetworks(ctx, c); err != nil || len(added) != 0 {
		t.Fatalf("up again: %v %v", added, err)
	}
}
