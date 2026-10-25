package egress

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
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
	n, err := ParseNetworks(cm.Data)
	if err != nil {
		return nil, &MalformedError{Namespace: ns, Err: err}
	}
	return n, nil
}

// MalformedError is a c4-networks ConfigMap that cannot be read as
// networks: a farm admin's to fix, not something a retry mends.
type MalformedError struct {
	Namespace string
	Err       error
}

func (e *MalformedError) Error() string {
	return fmt.Sprintf("the %s ConfigMap in %s is malformed: %v", c4v1.NetworksConfigMap, e.Namespace, e.Err)
}

func (e *MalformedError) Unwrap() error { return e.Err }

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
			if err := checkRule(r); err != nil {
				return nil, fmt.Errorf("network %s: %v", name, err)
			}
		}
		out[name] = rules
	}
	return out, nil
}

// checkRule checks a rule's syntax: a host is a DNS name (or an
// address), or *.domain; a cidr an address block; a service
// <namespace>/<name>; each port 1 to 65535.
func checkRule(r c4v1.ToolEgress) error {
	switch {
	case r.Host != "":
		h := strings.TrimSuffix(strings.ToLower(r.Host), ".")
		d, wild := strings.CutPrefix(h, "*.")
		if net.ParseIP(h) == nil && (strings.Contains(d, "*") || len(validation.IsDNS1123Subdomain(d)) > 0) {
			return fmt.Errorf("host %q: write a host name, or *.domain for the names under it", r.Host)
		}
		if wild && !strings.Contains(d, ".") {
			return fmt.Errorf("host %q: a wildcard is under a domain with a dot in it (*.studio.example), not a whole top-level domain", r.Host)
		}
	case r.CIDR != "":
		if _, _, err := net.ParseCIDR(r.CIDR); err != nil {
			return fmt.Errorf("cidr %q: write an address block, as 203.0.113.0/24", r.CIDR)
		}
	case r.Service != "":
		ns, name, ok := strings.Cut(r.Service, "/")
		if !ok || len(validation.IsDNS1123Label(ns)) > 0 || len(validation.IsDNS1123Label(name)) > 0 {
			return fmt.Errorf("service %q: write <namespace>/<name>", r.Service)
		}
	}
	for _, p := range r.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("port %d: a port is 1 to 65535", p)
		}
	}
	return nil
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

// Offering are the networks that allow host on port, narrowest first:
// one whose rule names the host itself before one that allows it by a
// wildcard (a longer domain before a shorter), a rule limited to ports
// before one allowing any, then the network with fewer rules, then by
// name.
func (n Networks) Offering(host string, port int) []string {
	type offer struct {
		name        string
		exact       bool
		suffix      int
		ports, size int
	}
	var offers []offer
	for name, rules := range n {
		best, found := offer{name: name, size: len(rules)}, false
		for _, r := range rules {
			if !Allowed([]c4v1.ToolEgress{r}, host, port) {
				continue
			}
			h := strings.TrimSuffix(strings.ToLower(r.Host), ".")
			o := offer{name: name, exact: !strings.HasPrefix(h, "*."), suffix: len(h), ports: len(r.Ports), size: len(rules)}
			if !found || narrower(o.exact, o.suffix, o.ports, best.exact, best.suffix, best.ports) {
				best, found = o, true
			}
		}
		if found {
			offers = append(offers, best)
		}
	}
	sort.Slice(offers, func(i, j int) bool {
		a, b := offers[i], offers[j]
		if a.exact != b.exact || a.suffix != b.suffix || (a.ports == 0) != (b.ports == 0) {
			return narrower(a.exact, a.suffix, a.ports, b.exact, b.suffix, b.ports)
		}
		if a.size != b.size {
			return a.size < b.size
		}
		return a.name < b.name
	})
	out := make([]string, len(offers))
	for i, o := range offers {
		out[i] = o.name
	}
	return out
}

// narrower says whether a rule (exact host or not, its host's length,
// its ports) allows less than another that allows the same request.
func narrower(exact bool, suffix, ports int, exact2 bool, suffix2, ports2 int) bool {
	if exact != exact2 {
		return exact
	}
	if !exact && suffix != suffix2 {
		return suffix > suffix2
	}
	if (ports == 0) != (ports2 == 0) {
		return ports > 0
	}
	return false
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
