package localfarm

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	kindv1 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/config/samples/quickstart"
)

func TestKindConfig(t *testing.T) {
	cfg := KindConfig(Options{})
	var c kindv1.Cluster
	if err := yaml.UnmarshalStrict(cfg, &c); err != nil {
		t.Fatalf("not a kind v1alpha4 config: %v\n%s", err, cfg)
	}
	if c.Kind != "Cluster" || c.Name != Name || len(c.Nodes) != 5 || c.Nodes[0].Role != kindv1.ControlPlaneRole {
		t.Fatalf("cluster %+v", c)
	}
	pools := map[string]int{}
	for _, n := range c.Nodes[1:] {
		pools[n.Labels["c4.avalanche.io/pool"]]++
		if n.Labels["topology.kubernetes.io/zone"] != Zone || n.Labels["topology.kubernetes.io/region"] != Region || n.Image != NodeImage {
			t.Errorf("a worker without its zone, region or image: %+v", n)
		}
		burst := n.Labels["c4.avalanche.io/pool"] == "cloud-gpu"
		if burst != (len(n.KubeadmConfigPatches) == 1 && strings.Contains(n.KubeadmConfigPatches[0], "c4.avalanche.io/unprovisioned")) {
			t.Errorf("%s: unprovisioned taint %v", n.Labels["c4.avalanche.io/pool"], n.KubeadmConfigPatches)
		}
	}
	if pools["cpu"] != 2 || pools["gpu"] != 1 || pools["cloud-gpu"] != 1 {
		t.Fatalf("pools %v", pools)
	}
	for _, m := range c.Nodes[0].ExtraPortMappings {
		if m.ListenAddress != "127.0.0.1" {
			t.Errorf("port %d open beyond this machine", m.HostPort)
		}
	}
	if err := yaml.UnmarshalStrict(KindConfig(Options{HUDHostPort: 30180, StoreHostPort: 30920}), &c); err != nil {
		t.Fatal(err)
	}
	ports := map[int32]int32{}
	for _, m := range c.Nodes[0].ExtraPortMappings {
		ports[m.ContainerPort] = m.HostPort
	}
	if ports[HUDPort] != 30180 || ports[StorePort] != 30920 {
		t.Fatalf("host ports %v", ports)
	}
}

func TestFarmObjects(t *testing.T) {
	objs := Farm(Options{Lat: 40.68, Lon: -73.94})
	data := YAML(objs)
	var kinds []string
	slots, burst := 0, 0
	for _, doc := range strings.Split(string(data), "\n---\n") {
		var head struct {
			Kind string `json:"kind"`
		}
		yaml.Unmarshal([]byte(doc), &head)
		kinds = append(kinds, head.Kind)
		switch head.Kind {
		case "Pool":
			var p c4v1.Pool
			if err := yaml.UnmarshalStrict([]byte(doc), &p); err != nil {
				t.Fatalf("pool: %v\n%s", err, doc)
			}
			if p.Spec.Burst != nil {
				burst += int(p.Spec.Burst.MaxNodes * p.Spec.SlotsPerNode)
				if p.Spec.Burst.Provider != "kind-sim" {
					t.Errorf("burst provider %s", p.Spec.Burst.Provider)
				}
			} else if p.Name == "cpu" {
				slots += 2 * int(p.Spec.SlotsPerNode)
			} else {
				slots += int(p.Spec.SlotsPerNode)
			}
		case "Project":
			var p c4v1.Project
			if err := yaml.UnmarshalStrict([]byte(doc), &p); err != nil {
				t.Fatalf("project: %v\n%s", err, doc)
			}
			if p.Name != Project || p.Spec.Store.CredentialsSecretRef.Name != "local-store" || !strings.Contains(p.Spec.Store.Shards[0].URL, "garage-s3.c4-store:3900") {
				t.Fatalf("project %+v", p.Spec)
			}
		}
	}
	if strings.Join(kinds, ",") != "Secret,Namespace,Project,Pool,Pool,Pool,Service,ConfigMap" {
		t.Fatalf("order %v", kinds)
	}
	if slots != 6 || burst != 4 {
		t.Fatalf("owned %d slots, burst %d: the guides say 6 and 4", slots, burst)
	}
	var site []map[string]any
	if err := json.Unmarshal(Site(Options{Lat: 40.68, Lon: -73.94, TZ: "America/New_York"}), &site); err != nil || site[0]["label"] != "This machine" || site[0]["tz"] != "America/New_York" {
		t.Fatalf("site %v %v", site, err)
	}
}

func TestViewersAndManager(t *testing.T) {
	if got := DevViewers(""); got != "artist=saltmarsh;supervisor=saltmarsh;wrangler=*;admin@farm-admin=*;facilities=*;td@pipeline-td=saltmarsh" {
		t.Fatalf("viewers %s", got)
	}
	v, err := LookupViewer("pipeline-td")
	if err != nil || v.Name != "td" || Link((Options{}).HUD(), v) != "http://127.0.0.1:30080/?as=td" || (Options{HUDHostPort: 30180}).HUD() != "http://127.0.0.1:30180" {
		t.Fatalf("td %+v %v", v, err)
	}
	if _, err := LookupViewer("director"); err == nil {
		t.Fatal("an unknown role was found")
	}
	p := ManagerPatch(Options{TZ: "America/New_York", OperatorImage: "c4-operator:dev"}, []string{"manager", "--hud-dev-viewers=me=*", "--work-policy=objective"})
	var m struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name, Image string
						Args        []string
					}
				}
			}
		}
	}
	if err := json.Unmarshal(p, &m); err != nil {
		t.Fatal(err)
	}
	c := m.Spec.Template.Spec.Containers[0]
	args := strings.Join(c.Args, " ")
	if c.Name != "manager" || c.Image != "c4-operator:dev" || strings.Contains(args, "me=*") || !strings.Contains(args, "--work-policy=objective") ||
		!strings.Contains(args, "--capacity-provider=kind-sim") || !strings.Contains(args, "--tz=America/New_York") || c.Args[0] != "manager" {
		t.Fatalf("manager %+v", c)
	}
}

func TestInstallManifest(t *testing.T) {
	in := []byte("apiVersion: v1\nkind: Namespace\nmetadata: {name: c4-system}\n---\n# a comment only\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata: {name: c4-manager, namespace: c4-system}\nspec:\n  template:\n    spec:\n      containers:\n      - name: manager\n        image: ghcr.io/avalanche-io/c4-operator:v0.1.0\n      - name: registrar\n        image: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.15.0\n")
	out := OverrideImage(in, "c4-operator:dev")
	if !strings.Contains(string(out), "image: c4-operator:dev\n") || !strings.Contains(string(out), "csi-node-driver-registrar:v2.15.0") {
		t.Fatalf("override:\n%s", out)
	}
	if string(OverrideImage(in, "")) != string(in) {
		t.Fatal("no image changed the manifest")
	}
	objs, err := Docs(out)
	if err != nil || len(objs) != 2 || objs[1].GetKind() != "Deployment" {
		t.Fatalf("docs %d %v", len(objs), err)
	}
	store, err := Docs(quickstart.Store)
	if err != nil || len(store) < 5 {
		t.Fatalf("the quickstart store: %d objects, %v", len(store), err)
	}
}

func TestPlace(t *testing.T) {
	if lat, lon := Place("America/New_York"); lat != 40.71 || lon != -74.01 {
		t.Fatalf("%v %v", lat, lon)
	}
	if lat, lon := Place("Mars/Olympus"); lat != 0 || lon != 0 {
		t.Fatal("an unknown zone was placed")
	}
}

// The farm's owner reaches the manager as --single-person-farm, and a
// second up replaces it rather than adding another.
func TestOwnerIsTheSinglePerson(t *testing.T) {
	o := Options{Owner: "noor@petrelfinch.example"}
	if !slices.Contains(ManagerArgs(o), "--single-person-farm=noor@petrelfinch.example") {
		t.Fatalf("args %v", ManagerArgs(o))
	}
	p := string(ManagerPatch(o, []string{"manager", "--single-person-farm=someone-else"}))
	if strings.Contains(p, "someone-else") || strings.Count(p, "--single-person-farm=") != 1 {
		t.Errorf("patch %s", p)
	}
	if slices.ContainsFunc(ManagerArgs(Options{}), func(a string) bool { return strings.HasPrefix(a, "--single-person-farm") }) {
		t.Error("a farm with no owner named one")
	}
}

// The owner's name is one the farm can record and no role's: an address's
// local part, a Windows user without its domain, GitHub's private address
// without its number, lowercase, with what else a name carries made '-'.
func TestOwnerName(t *testing.T) {
	for raw, want := range map[string]string{
		"lena@studio.example": "lena",
		"Noor Haddad":         "noor-haddad",
		"12345678+mrjoshuak@users.noreply.github.com": "mrjoshuak",
		`STUDIO\nhaddad`: "nhaddad",
		"  n.haddad  ":   "n.haddad",
		"o'brien--x":     "o-brien-x",
	} {
		if got, err := OwnerName(raw); err != nil || got != want {
			t.Errorf("%q: %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"td@studio.com", "Admin", "farm-admin", "", "@", "---"} {
		if got, err := OwnerName(raw); err == nil {
			t.Errorf("%q: %q, want refused", raw, got)
		}
	}
}

// The owner signs in as themselves, first, beside the role sign-ins.
func TestOwnerSignsInAsThemselves(t *testing.T) {
	args := strings.Join(ManagerArgs(Options{Owner: "noor"}), " ")
	if !strings.Contains(args, "--hud-dev-viewers=noor@farm-admin=*;artist=saltmarsh;") || !strings.Contains(args, "--single-person-farm=noor") {
		t.Errorf("manager args: %s", args)
	}
	if v := OwnerViewer("noor"); v.Name != "noor" || v.Projects != "*" {
		t.Errorf("owner viewer %+v", v)
	}
}

// up's last line says the whole and each step a second or more, a step
// marked twice adding its times, in the order steps first ran.
func TestTimings(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	tm := &Timings{Now: func() time.Time { return now }}
	step := func(name string, d time.Duration) { tm.Mark(name); now = now.Add(d) }
	step("checks", 300*time.Millisecond)
	step("cluster", 130*time.Second)
	step("operator", 20*time.Second)
	step("store", 15*time.Second)
	step("operator", 45*time.Second)
	step("tools", 40*time.Second)
	if got := tm.String(); got != "up in 4m10s: cluster 2m10s, operator 1m5s, store 15s, tools 40s" {
		t.Errorf("%q", got)
	}
	var none *Timings
	none.Mark("cluster")
	if none.String() != "" {
		t.Error("a nil Timings says something")
	}
}

// A new farm's owner is --owner or the default; an existing farm keeps its
// owner, and up refuses an --owner naming someone else.
func TestChooseOwner(t *testing.T) {
	if got, err := ChooseOwner("", "Noor@studio.example"); err != nil || got != "noor" {
		t.Errorf("new farm, --owner: %q %v", got, err)
	}
	if got, err := ChooseOwner("noor", ""); err != nil || got != "noor" {
		t.Errorf("kept: %q %v", got, err)
	}
	if got, err := ChooseOwner("noor", "NOOR"); err != nil || got != "noor" {
		t.Errorf("--owner naming the owner: %q %v", got, err)
	}
	if _, err := ChooseOwner("noor", "sam"); err == nil || !strings.Contains(err.Error(), "this farm's owner is noor; up does not change it (changing it will be c4 farm owner change)") {
		t.Errorf("change: %v", err)
	}
}
