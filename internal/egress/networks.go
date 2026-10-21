package egress

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// networkName is a network's name, as needs.network writes it.
var networkName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Networks are the networks the farm admin defines for tools to declare
// (needs.network): the c4-networks ConfigMap in the operator's namespace,
// each key a network's name and each value its egress rules. A farm with
// no such ConfigMap defines none.
type Networks map[string][]c4v1.ToolEgress

// LoadNetworks reads the farm's networks from namespace ns.
func LoadNetworks(ctx context.Context, c client.Reader, ns string) (Networks, error) {
	var cm corev1.ConfigMap
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: c4v1.NetworksConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return Networks{}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseNetworks(cm.Data)
}

// ParseNetworks reads the ConfigMap's data: each value a YAML list of
// rules, each naming exactly one of a host, an address block or a service.
func ParseNetworks(data map[string]string) (Networks, error) {
	out := Networks{}
	for name, v := range data {
		if !networkName.MatchString(name) || name == "licences" {
			return nil, fmt.Errorf("network %q: a name is lower-case letters, digits and -, and not licences", name)
		}
		var rules []c4v1.ToolEgress
		if err := yaml.UnmarshalStrict([]byte(v), &rules); err != nil {
			return nil, fmt.Errorf("network %s: %v", name, err)
		}
		for _, r := range rules {
			n := 0
			for _, s := range []string{r.Host, r.CIDR, r.Service} {
				if s != "" {
					n++
				}
			}
			if n != 1 {
				return nil, fmt.Errorf("network %s: each rule names one host, cidr or service", name)
			}
		}
		out[name] = rules
	}
	return out, nil
}

// Expand is the egress rules of the named networks, and the names the
// farm does not define.
func (n Networks) Expand(names []string) (rules []c4v1.ToolEgress, missing []string) {
	for _, name := range names {
		r, ok := n[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		rules = append(rules, r...)
	}
	return rules, missing
}

// Offering are the networks that allow host on port, by name.
func (n Networks) Offering(host string, port int) []string {
	var out []string
	for name, rules := range n {
		if Allowed(rules, host, port) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Missing says, in the farm's words, which networks a tool declares that
// the farm does not define, and where an admin defines them: the
// c4-networks ConfigMap in the operator's namespace ns.
func Missing(tool string, missing []string, ns string) string {
	what, it := "network", "it"
	if len(missing) > 1 {
		what, it = "networks", "them"
	}
	return fmt.Sprintf("%s declares %s %s, which this farm does not define: a farm admin adds %s to the %s ConfigMap in %s (internal/hud/API.md, Networks)",
		tool, what, strings.Join(missing, ", "), it, c4v1.NetworksConfigMap, ns)
}
