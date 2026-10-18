package localfarm

import (
	"encoding/json"
	"strings"
	"testing"

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
	if err := json.Unmarshal(Site(Options{Lat: 40.68, Lon: -73.94}), &site); err != nil || site[0]["label"] != "This machine" {
		t.Fatalf("site %v %v", site, err)
	}
}

func TestViewersAndManager(t *testing.T) {
	if got := DevViewers(); got != "artist=saltmarsh;supervisor=saltmarsh;wrangler=*;admin@farm-admin=*;facilities=*;td@pipeline-td=saltmarsh" {
		t.Fatalf("viewers %s", got)
	}
	v, err := LookupViewer("pipeline-td")
	if err != nil || v.Name != "td" || Link(v) != "http://127.0.0.1:30080/?as=td&level=5" {
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
