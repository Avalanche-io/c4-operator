// Package localfarm builds `c4 farm up`'s farm on this machine: a kind
// cluster in Docker, the operator, a Garage store, Petrel & Finch's
// feature Saltmarsh as the project, a cpu, a gpu and a burst pool, one
// site, and one sign-in per role. Plan is pure (and tested without a
// cluster); Up carries it out.
package localfarm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/Avalanche-io/c4-operator/config/samples/quickstart"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Name is the local farm's cluster: c4-local, or what --name gives, so two farms
// can run on one machine on different ports.
var Name = "c4-local"

// Defaults of the local farm.
const (
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
// --hud-dev-viewers takes them (name@role=projects). The HUD opens each in
// its role's mode and at its role's level.
var Viewers = []Viewer{
	{Name: "artist", Role: "artist", Projects: Project},
	{Name: "supervisor", Role: "supervisor", Projects: Project},
	{Name: "wrangler", Role: "wrangler", Projects: "*"},
	{Name: "admin", Role: "farm-admin", Projects: "*"},
	{Name: "facilities", Role: "facilities", Projects: "*"},
	{Name: "td", Role: "pipeline-td", Projects: Project},
}

// OwnerViewer is the farm's owner's own sign-in: every project, at the
// farm admin's level, and, as the manager's --single-person-farm, every
// role's decisions.
func OwnerViewer(owner string) Viewer {
	return Viewer{Name: owner, Role: "farm-admin", Projects: "*"}
}

// Viewer is one local sign-in.
type Viewer struct {
	Name, Role, Projects string
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
// The owner, when named, signs in as themselves first, and the role
// sign-ins stay for trying the other views.
func DevViewers(owner string) string {
	var parts []string
	vs := Viewers
	if owner != "" {
		vs = append([]Viewer{OwnerViewer(owner)}, Viewers...)
	}
	for _, v := range vs {
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
	// RegistryHostPort is the farm's registry on 127.0.0.1 (default
	// RegistryPort), where c4 farm image load pushes.
	RegistryHostPort int
	// LocalImages says the farm runs a development build: a tool's
	// c4/<name> is c4-<name>:dev in the farm's registry, not the release's
	// image on ghcr.io.
	LocalImages bool
	// Owner is the person who owns the farm and plays every role on it: the
	// installer, recorded as standing in for a role when they decide a wait
	// addressed to it (the manager's --single-person-farm).
	Owner string
	// Tracker adds the stand-in GitHub and c4-sync (c4 farm up --tracker):
	// c4-sync's sign-in, and the sandbox's page at the HUD's /sandbox/.
	Tracker bool
	// InstallerToken is the farm's installer token (InstallerToken), which
	// the manager's HUD asks of every development viewer; empty in a plan,
	// which shows where it goes and never the token.
	InstallerToken string
}

// BindRequest is the farm binding a local farm asks its HUD for
// (POST /api/v1/farm/binding): every deployment of the farm bound to its
// owner, said to be one person's, so the owner holds every role there. A
// local farm is the only place that is said: a shared farm's binding
// leaves onePerson unset.
func (o Options) BindRequest() api.FarmBindingRequest {
	return api.FarmBindingRequest{Owner: o.Owner, To: o.name() + "/*", OnePerson: true}
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
	fmt.Fprintf(&b, "# c4 farm up: the local farm %s.\nkind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nname: %s\n", o.name(), o.name())
	// every node pulls the farm registry's images by name (registry.go)
	fmt.Fprintf(&b, "containerdConfigPatches:\n- |-\n%s\nnodes:\n", indentLines(registryPatch, "  "))
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

// ProjectSpec is one project of the local farm.
type ProjectSpec struct {
	Name, Title string
	// BurstBudgetPerDay and LateCostPerHour are dollars; empty takes the
	// local farm's (20 and 40).
	BurstBudgetPerDay, LateCostPerHour string
}

// Store is where a project's objects go: one Garage bucket, a key prefix
// per project (Saltmarsh's is c4/).
func (p ProjectSpec) Store() string {
	prefix := p.Name
	if p.Name == Project {
		prefix = "c4"
	}
	return "s3://quickstart/" + prefix + "/?endpoint=http://garage-s3.c4-store:3900"
}

// ProjectObjects are a project's namespace and Project.
func ProjectObjects(p ProjectSpec) []obj {
	budget, late := p.BurstBudgetPerDay, p.LateCostPerHour
	if budget == "" {
		budget = "20"
	}
	if late == "" {
		late = "40"
	}
	return []obj{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": meta(p.Name, "labels",
			obj{"pod-security.kubernetes.io/enforce": "restricted", "c4.avalanche.io/studio": "petrel-finch"})},
		{"apiVersion": "c4.avalanche.io/v1alpha1", "kind": "Project", "metadata": meta(p.Name, "annotations",
			obj{"c4.avalanche.io/title": p.Title}),
			"spec": obj{"namespaces": []string{p.Name}, "isolation": "Strict",
				"store": obj{"access": "Tiered", "credentialsSecretRef": obj{"name": "local-store"},
					"shards": []obj{{"name": "s0", "url": p.Store()}}},
				"work": obj{"burstBudgetPerDay": budget, "lateCostPerHour": late}}},
	}
}

// Farm is everything applied after the operator and the store, in order:
// the store's credentials, the project and its namespace, the pools, the
// HUD's NodePort and the site for the map.
func Farm(o Options) []obj {
	// the installer token, for the manager's pod alone
	token := o.InstallerToken
	if token == "" {
		token = "(made by up, kept in " + InstallerTokenFile(o.name()) + ")"
	}
	out := []obj{
		{"apiVersion": "v1", "kind": "Secret", "metadata": meta("local-store", "namespace", "c4-system"),
			"stringData": obj{"AWS_ACCESS_KEY_ID": quickstart.StoreKey, "AWS_SECRET_ACCESS_KEY": quickstart.StoreSecret}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": meta(InstallerSecret, "namespace", "c4-system"),
			"stringData": obj{"token": token}},
	}
	out = append(out, ProjectObjects(ProjectSpec{Name: Project, Title: "Saltmarsh (feature), Petrel & Finch"})...)
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
	args := []string{"--hud-dev-viewers=" + DevViewers(o.Owner), "--capacity-provider=kind-sim", "--hud-sites=/etc/c4/sites/sites.json"}
	if o.TZ != "" {
		args = append(args, "--tz="+o.TZ)
	}
	// the farm's registry is pinned from without TLS; a development farm's
	// own images (c4/<name>) are in it, at dev
	args = append(args, "--hud-plain-registries="+RegistryInFarm)
	if o.LocalImages {
		args = append(args, "--hud-image-registry="+RegistryInFarm, "--hud-image-tag=dev")
	}
	if o.Owner != "" {
		args = append(args, "--single-person-farm="+o.Owner)
	}
	if o.Tracker {
		args = append(args, "--hud-sandbox="+sandboxURL)
	}
	// development viewers only with the installer's token
	args = append(args, "--hud-installer-token-file="+installerMount+"/token")
	return args
}

func indentLines(s, by string) string {
	return by + strings.ReplaceAll(s, "\n", "\n"+by)
}

// ManagerPatch is the strategic merge patch for the manager Deployment:
// the local arguments, the sites ConfigMap, and the image if one is given.
// args is the manager's current argument list.
func ManagerPatch(o Options, args []string) []byte {
	keep := []string{}
	for _, a := range args {
		drop := false
		for _, f := range []string{"--hud-dev-viewers=", "--capacity-provider=", "--hud-sites=", "--tz=", "--hud-plain-registries=", "--single-person-farm=",
			"--hud-image-registry=", "--hud-image-tag=", "--hud-sandbox=", "--hud-installer-token-file="} {
			if strings.HasPrefix(a, f) {
				drop = true
			}
		}
		if !drop {
			keep = append(keep, a)
		}
	}
	c := obj{"name": "manager", "args": append(keep, ManagerArgs(o)...),
		"volumeMounts": []obj{{"name": "sites", "mountPath": "/etc/c4/sites", "readOnly": true},
			{"name": "installer", "mountPath": installerMount, "readOnly": true}}}
	if o.OperatorImage != "" {
		c["image"] = o.OperatorImage
		c["imagePullPolicy"] = "IfNotPresent"
	}
	// the token's file is the manager's group's to read: the manager runs
	// as 65532 (config/manager), and without fsGroup a Secret's files are
	// root's alone, so 0400 or 0440 is unreadable to it (as the regional
	// replicas do, config/regional)
	p := obj{"spec": obj{"template": obj{"spec": obj{
		"securityContext": obj{"fsGroup": managerUser},
		"containers":      []obj{c},
		"volumes": []obj{{"name": "sites", "configMap": obj{"name": "c4-sites"}},
			{"name": "installer", "secret": obj{"secretName": InstallerSecret, "defaultMode": 0o440}}},
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

// Link is the address on the HUD at hud for a viewer; the HUD starts them
// in their role's mode and level.
func Link(hud string, v Viewer) string {
	return fmt.Sprintf("%s/?as=%s", hud, v.Name)
}

// NeedsLink is the HUD's Needs-you view as v: only the questions and
// decisions waiting on the person, where a new farm's owner starts.
func NeedsLink(hud string, v Viewer) string { return Link(hud, v) + "#/needs" }

// DefaultOwner is who owns a farm made on this machine unless --owner
// says: the local part of git's user.email, as git reads it here (this
// repository's, else the global one), else the login name, made a name
// OwnerName takes. A bare name keeps addresses out of every record. It is
// empty when neither gives one.
func DefaultOwner() string {
	var tries []string
	if out, err := exec.Command("git", "config", "user.email").Output(); err == nil {
		tries = append(tries, strings.TrimSpace(string(out)))
	}
	if u, err := user.Current(); err == nil {
		tries = append(tries, u.Username)
	}
	tries = append(tries, os.Getenv("USER"))
	for _, t := range tries {
		if name, err := OwnerName(t); err == nil {
			return name
		}
	}
	return ""
}

// ChooseOwner is the farm's owner as up settles it: a new farm's is
// --owner, else DefaultOwner; an existing farm keeps the one it has, and
// up refuses an --owner naming someone else, since changing a farm's
// owner will be a recorded act of its own (c4 farm owner change).
func ChooseOwner(was, flag string) (string, error) {
	if flag != "" {
		name, err := OwnerName(flag)
		if err != nil {
			return "", err
		}
		if was != "" && name != was {
			return "", fmt.Errorf("this farm's owner is %s; up does not change it (changing it will be c4 farm owner change); c4 farm up without --owner keeps %s", was, was)
		}
		return name, nil
	}
	if was != "" {
		return was, nil
	}
	if name := DefaultOwner(); name != "" {
		return name, nil
	}
	return "", errors.New("no name for the farm's owner from git's user.email or the login; give one with --owner NAME")
}

// OwnerName is a person's name as the farm records its owner: an
// address's local part, a Windows DOMAIN\user's user, GitHub's private
// address without its numeric prefix (12345678+name), in lowercase, with
// anything outside a-z, 0-9, '.' and '-' made '-'. A role's sign-in name
// (artist, td, admin...) is refused: the owner is a person, and taking a
// role's name would quietly give that sign-in every power.
func OwnerName(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if i := strings.LastIndexByte(s, '\\'); i >= 0 {
		s = s[i+1:]
	}
	s, _, _ = strings.Cut(s, "@")
	if num, rest, ok := strings.Cut(s, "+"); ok && num != "" && strings.Trim(num, "0123456789") == "" {
		s = rest
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := b.String()
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	name = strings.Trim(name, "-.")
	if name == "" {
		return "", fmt.Errorf("%q gives no name for the farm's owner; give one with --owner NAME", raw)
	}
	for _, v := range Viewers {
		if name == v.Name || name == v.Role {
			return "", fmt.Errorf("%q is the %s sign-in's name; the owner is a person: give your own name with --owner NAME", name, v.Role)
		}
	}
	return name, nil
}

// Capacity is the farm's capacity as up says it: its nodes and slots, and
// the burst pool it rents only when a deadline needs it, simulated here.
func Capacity() string {
	slots := map[string]int{}
	burst := 0
	for _, p := range Pools {
		slots[p.Name] = p.SlotsPerNode
		burst += p.BurstMaxNodes * p.SlotsPerNode
	}
	owned, n := 0, 0
	for _, nd := range nodes {
		if nd.burst {
			continue
		}
		n++
		owned += slots[nd.pool]
	}
	line := fmt.Sprintf("%d nodes · %d slots", n, owned)
	if burst > 0 {
		line += fmt.Sprintf(", and %d more it rents only when a deadline needs them (a simulated cloud)", burst)
	}
	return line
}
