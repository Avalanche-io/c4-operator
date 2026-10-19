// Package localfarm builds `c4 farm up`'s farm on this machine: a kind
// cluster in Docker, the operator, a Garage store, Petrel & Finch's
// feature Saltmarsh as the project, a cpu, a gpu and a burst pool, one
// site, and one sign-in per role. Plan is pure (and tested without a
// cluster); Up carries it out.
package localfarm

import (
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4-operator/config/samples/quickstart"
)

// Defaults of the local farm.
const (
	Name      = "c4-local"
	NodeImage = "kindest/node:v1.36.1"
	Project   = "saltmarsh"
	Zone      = "local-a"
	Region    = "local"
	// HUDPort and StorePort are the NodePorts inside the cluster, and by
	// default the ports on this machine too.
	HUDPort   = 30080
	StorePort = 30900
)

// Viewers are the local sign-ins: one per role the HUD supports, as
// --hud-dev-viewers takes them (name@role=projects). The HUD opens each at
// its role's detail level.
var Viewers = []Viewer{
	{Name: "artist", Role: "artist", Projects: Project, Level: 1},
	{Name: "supervisor", Role: "supervisor", Projects: Project, Level: 2},
	{Name: "wrangler", Role: "wrangler", Projects: "*", Level: 3},
	{Name: "admin", Role: "farm-admin", Projects: "*", Level: 4},
	{Name: "facilities", Role: "facilities", Projects: "*", Level: 4},
	{Name: "td", Role: "pipeline-td", Projects: Project, Level: 5},
}

// Viewer is one local sign-in.
type Viewer struct {
	Name, Role, Projects string
	Level                int
}

// LookupViewer finds a sign-in by name or role.
func LookupViewer(s string) (Viewer, error) {
	for _, v := range Viewers {
		if v.Name == s || v.Role == s {
			return v, nil
		}
	}
	var names []string
	for _, v := range Viewers {
		names = append(names, v.Name)
	}
	return Viewer{}, fmt.Errorf("no role %q on the local farm; there are: %s", s, strings.Join(names, ", "))
}

// DevViewers is the manager's --hud-dev-viewers value.
func DevViewers() string {
	var parts []string
	for _, v := range Viewers {
		n := v.Name
		if v.Role != v.Name {
			n += "@" + v.Role
		}
		parts = append(parts, n+"="+v.Projects)
	}
	return strings.Join(parts, ";")
}

// Options shape the local farm.
type Options struct {
	Name string
	// OperatorImage replaces the image the install manifest names (a
	// development build loaded into the cluster).
	OperatorImage string
	// TZ is the farm's time zone, an IANA name.
	TZ string
	// Lat and Lon place "This machine" on the map.
	Lat, Lon float64
	// HUDHostPort and StoreHostPort are the HUD's and the store's ports on
	// 127.0.0.1 (default HUDPort and StorePort), for a machine where another
	// cluster already holds those.
	HUDHostPort, StoreHostPort int
}

// HUD is the HUD's address on this machine.
func (o Options) HUD() string {
	return fmt.Sprintf("http://127.0.0.1:%d", or(o.HUDHostPort, HUDPort))
}

func or(port, def int) int {
	if port == 0 {
		return def
	}
	return port
}

func (o Options) name() string {
	if o.Name == "" {
		return Name
	}
	return o.Name
}

type node struct {
	pool    string
	burst   bool
	comment string
}

// nodes are the kind workers: two cpu, one gpu (labelled as a GPU node;
// kind has none) and one burst node that starts unprovisioned until the
// kind-sim capacity provider "rents" it.
var nodes = []node{
	{pool: "cpu", comment: "cpu"}, {pool: "cpu", comment: "cpu"},
	{pool: "gpu", comment: "a GPU node (simulated)"},
	{pool: "cloud-gpu", burst: true, comment: "burst: unprovisioned until the farm rents it"},
}

// KindConfig is the cluster's kind configuration.
func KindConfig(o Options) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# c4 farm up: the local farm %s.\nkind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nname: %s\nnodes:\n", o.name(), o.name())
	fmt.Fprintf(&b, "- role: control-plane\n  image: %s\n  extraPortMappings:\n", NodeImage)
	fmt.Fprintf(&b, "  - {containerPort: %d, hostPort: %d, listenAddress: 127.0.0.1}   # the store\n", StorePort, or(o.StoreHostPort, StorePort))
	fmt.Fprintf(&b, "  - {containerPort: %d, hostPort: %d, listenAddress: 127.0.0.1}   # the HUD\n", HUDPort, or(o.HUDHostPort, HUDPort))
	for _, n := range nodes {
		fmt.Fprintf(&b, "- role: worker   # %s\n  image: %s\n", n.comment, NodeImage)
		fmt.Fprintf(&b, "  labels: {topology.kubernetes.io/zone: %s, topology.kubernetes.io/region: %s, c4.avalanche.io/pool: %s}\n", Zone, Region, n.pool)
		if n.burst {
			b.WriteString("  kubeadmConfigPatches:\n  - |\n    kind: JoinConfiguration\n    nodeRegistration:\n      taints:\n" +
				"      - {key: c4.avalanche.io/unprovisioned, value: \"true\", effect: NoSchedule}\n")
		}
	}
	return []byte(b.String())
}

// Pool is one pool of the local farm.
type Pool struct {
	Name, Class      string
	SlotsPerNode     int
	CostPerSlotHour  string
	Attributes       map[string]string
	BurstMaxNodes    int
	BurstProvisionS  int
	BurstCostPerSlot string
}

// Pools are the local farm's pools: 6 owned slots and 4 to rent.
var Pools = []Pool{
	{Name: "cpu", Class: "cpu", SlotsPerNode: 2, CostPerSlotHour: "0.12",
		Attributes: map[string]string{"class": "cpu", "capacityType": "on-demand"}},
	{Name: "gpu", Class: "gpu", SlotsPerNode: 2, CostPerSlotHour: "0.45",
		Attributes: map[string]string{"class": "gpu", "gpuModel": "sim-rtx-6000-ada", "vram": "48Gi", "capacityType": "on-demand"}},
	{Name: "cloud-gpu", Class: "gpu", SlotsPerNode: 4, CostPerSlotHour: "1.10",
		Attributes:    map[string]string{"class": "gpu", "gpuModel": "sim-l40s", "vram": "48Gi", "capacityType": "on-demand"},
		BurstMaxNodes: 1, BurstProvisionS: 120, BurstCostPerSlot: "1.10"},
}

type obj = map[string]any

func meta(name string, extra ...any) obj {
	m := obj{"name": name}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

// Site is the farm's one site, for the HUD's map.
func Site(o Options) []byte {
	site := obj{"name": Region, "label": "This machine", "region": Region, "lat": o.Lat, "lon": o.Lon,
		"kind": "onprem", "localities": []string{Zone}}
	if o.TZ != "" {
		site["tz"] = o.TZ
	}
	b, _ := json.Marshal([]obj{site})
	return b
}

// Farm is everything applied after the operator and the store, in order:
// the store's credentials, the project and its namespace, the pools, the
// HUD's NodePort and the site for the map.
func Farm(o Options) []obj {
	out := []obj{
		{"apiVersion": "v1", "kind": "Secret", "metadata": meta("local-store", "namespace", "c4-system"),
			"stringData": obj{"AWS_ACCESS_KEY_ID": quickstart.StoreKey, "AWS_SECRET_ACCESS_KEY": quickstart.StoreSecret}},
		{"apiVersion": "v1", "kind": "Namespace", "metadata": meta(Project, "labels",
			obj{"pod-security.kubernetes.io/enforce": "restricted", "c4.avalanche.io/studio": "petrel-finch"})},
		{"apiVersion": "c4.avalanche.io/v1alpha1", "kind": "Project", "metadata": meta(Project, "annotations",
			obj{"c4.avalanche.io/title": "Saltmarsh (feature), Petrel & Finch"}),
			"spec": obj{"namespaces": []string{Project}, "isolation": "Strict",
				"store": obj{"access": "Tiered", "credentialsSecretRef": obj{"name": "local-store"},
					"shards": []obj{{"name": "s0", "url": "s3://quickstart/c4/?endpoint=http://garage-s3.c4-store:3900"}}},
				"work": obj{"burstBudgetPerDay": "20", "lateCostPerHour": "40"}}},
	}
	for _, p := range Pools {
		spec := obj{"nodeSelector": obj{"c4.avalanche.io/pool": p.Name}, "slotsPerNode": p.SlotsPerNode,
			"costPerSlotHour": p.CostPerSlotHour, "attributes": p.Attributes}
		if p.BurstMaxNodes > 0 {
			spec["burst"] = obj{"maxNodes": p.BurstMaxNodes, "provisioningSeconds": p.BurstProvisionS,
				"costPerSlotHour": p.BurstCostPerSlot, "provider": "kind-sim"}
		}
		out = append(out, obj{"apiVersion": "c4.avalanche.io/v1alpha1", "kind": "Pool", "metadata": meta(p.Name), "spec": spec})
	}
	out = append(out,
		obj{"apiVersion": "v1", "kind": "Service", "metadata": meta("c4-hud-local", "namespace", "c4-system"),
			"spec": obj{"type": "NodePort", "selector": obj{"app.kubernetes.io/name": "c4-operator", "app.kubernetes.io/component": "manager"},
				"ports": []obj{{"port": 8090, "targetPort": 8090, "nodePort": HUDPort}}}},
		obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta("c4-sites", "namespace", "c4-system"),
			"data": obj{"sites.json": string(Site(o))}},
	)
	return out
}

// ManagerArgs are what the local farm adds to the manager's arguments.
func ManagerArgs(o Options) []string {
	args := []string{"--hud-dev-viewers=" + DevViewers(), "--capacity-provider=kind-sim", "--hud-sites=/etc/c4/sites/sites.json"}
	if o.TZ != "" {
		args = append(args, "--tz="+o.TZ)
	}
	return args
}

// ManagerPatch is the strategic merge patch for the manager Deployment:
// the local arguments, the sites ConfigMap, and the image if one is given.
// args is the manager's current argument list.
func ManagerPatch(o Options, args []string) []byte {
	keep := []string{}
	for _, a := range args {
		drop := false
		for _, f := range []string{"--hud-dev-viewers=", "--capacity-provider=", "--hud-sites=", "--tz="} {
			if strings.HasPrefix(a, f) {
				drop = true
			}
		}
		if !drop {
			keep = append(keep, a)
		}
	}
	c := obj{"name": "manager", "args": append(keep, ManagerArgs(o)...),
		"volumeMounts": []obj{{"name": "sites", "mountPath": "/etc/c4/sites", "readOnly": true}}}
	if o.OperatorImage != "" {
		c["image"] = o.OperatorImage
		c["imagePullPolicy"] = "IfNotPresent"
	}
	p := obj{"spec": obj{"template": obj{"spec": obj{
		"containers": []obj{c},
		"volumes":    []obj{{"name": "sites", "configMap": obj{"name": "c4-sites"}}},
	}}}}
	b, _ := json.Marshal(p)
	return b
}

// YAML renders objects as one multi-document file, for --plan.
func YAML(objs []obj) []byte {
	var b strings.Builder
	for i, o := range objs {
		if i > 0 {
			b.WriteString("---\n")
		}
		y, _ := yaml.Marshal(o)
		b.Write(y)
	}
	return []byte(b.String())
}

// Link is the address on the HUD at hud for a viewer at its role's level.
func Link(hud string, v Viewer) string {
	return fmt.Sprintf("%s/?as=%s&level=%d", hud, v.Name, v.Level)
}
