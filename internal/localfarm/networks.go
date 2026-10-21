package localfarm

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4-operator/internal/source/huggingface"
)

// NetworksConfigMap is where a farm defines the networks tools declare: in
// the operator's namespace, each key a network's name and each value a
// YAML list of egress rules (internal/hud/API.md, Networks).
const NetworksConfigMap = "c4-networks"

// Networks are the networks a local farm defines for tools to declare
// (needs.network), as the c4-networks ConfigMap takes them: model-api, the
// model provider's API the agent tool reaches; huggingface, the Hub and
// the CDN hosts its downloads redirect to, as the Hugging Face source
// follows them. A farm defines no other network until a tool needs it.
func Networks() map[string]string {
	var hf []string
	for _, h := range huggingface.Hosts {
		hf = append(hf, fmt.Sprintf("- {host: %q, ports: [443]}", h))
	}
	return map[string]string{
		"model-api":   "- {host: api.anthropic.com, ports: [443]}\n",
		"huggingface": strings.Join(hf, "\n") + "\n",
	}
}

// NetworksObject is the c4-networks ConfigMap with the local farm's
// networks, for --plan.
func NetworksObject() obj {
	return obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta(NetworksConfigMap, "namespace", "c4-system"),
		"data": Networks()}
}

// ensureNetworks gives the farm the local farm's networks that it does not
// define yet, and leaves every network it defines as it is: a farm admin's
// own, and their changes to these, survive up running again. It reports
// the networks it added.
func ensureNetworks(ctx context.Context, c client.Client) ([]string, error) {
	want := Networks()
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: "c4-system", Name: NetworksConfigMap}
	err := c.Get(ctx, key, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Data: want}
		if err := c.Create(ctx, &cm); err != nil {
			return nil, fmt.Errorf("the farm's networks: %w", err)
		}
		return sortedKeys(want), nil
	}
	if err != nil {
		return nil, fmt.Errorf("the farm's networks: %w", err)
	}
	before := cm.DeepCopy()
	var added []string
	for name, rules := range want {
		if _, ok := cm.Data[name]; ok {
			continue
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[name] = rules
		added = append(added, name)
	}
	if len(added) == 0 {
		return nil, nil
	}
	sort.Strings(added)
	if err := c.Patch(ctx, &cm, client.MergeFrom(before)); err != nil {
		return nil, fmt.Errorf("the farm's networks: %w", err)
	}
	return added, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
