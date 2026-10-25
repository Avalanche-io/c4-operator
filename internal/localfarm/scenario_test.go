package localfarm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
)

// Every scenario fits the local farm: its jobs' sign-ins exist and see
// their projects, their projects are the farm's or the scenarios', and a
// node a job fails on is one of the farm's cpu workers.
func TestScenariosFitTheLocalFarm(t *testing.T) {
	all, err := farmcli.Scenarios()
	if err != nil {
		t.Fatal(err)
	}
	projects := map[string]bool{Project: true}
	for _, sc := range all {
		for _, p := range sc.Projects {
			projects[p.Name] = true
		}
	}
	// kind names workers <cluster>-worker, -worker2, ... in nodes' order,
	// and a scenario names them without the cluster's name
	cpu := map[string]bool{}
	for i, n := range nodes {
		name := "worker"
		if i > 0 {
			name += fmt.Sprint(i + 1)
		}
		if n.pool == "cpu" {
			cpu[name] = true
		}
	}
	for _, sc := range all {
		for p := range sc.Budgets {
			if !projects[p] {
				t.Errorf("%s: a budget for %s, no project of the farm's", sc.Name, p)
			}
		}
		for _, j := range sc.Jobs {
			if !projects[j.Project] {
				t.Errorf("%s/%s: project %s is not on the local farm", sc.Name, j.Name, j.Project)
			}
			v, err := LookupViewer(j.As)
			if err != nil || (v.Projects != "*" && !strings.Contains(","+v.Projects+",", ","+j.Project+",")) {
				t.Errorf("%s/%s: %s cannot submit to %s (%v)", sc.Name, j.Name, j.As, j.Project, err)
			}
			if j.Fail != nil && !cpu[j.Fail.Node] {
				t.Errorf("%s/%s: fails on %s, not a cpu worker of the local farm", sc.Name, j.Name, j.Fail.Node)
			}
		}
	}
}

// A scenario's failing node is named as the farm loading it has it, and
// the scenario itself keeps its own name for the next farm.
func TestOnFarm(t *testing.T) {
	defer func(n string) { Name = n }(Name)
	Name = "c4-walk"
	sc, err := farmcli.LookupScenario("evening")
	if err != nil {
		t.Fatal(err)
	}
	on := OnFarm(sc)
	n := 0
	for i, j := range on.Jobs {
		if j.Fail == nil {
			continue
		}
		n++
		if j.Fail.Node != "c4-walk-worker2" {
			t.Errorf("%s fails on %s, not c4-walk-worker2", j.Name, j.Fail.Node)
		}
		if sc.Jobs[i].Fail.Node != "worker2" {
			t.Errorf("the scenario's own %s now fails on %s", j.Name, sc.Jobs[i].Fail.Node)
		}
	}
	if n == 0 {
		t.Fatal("evening has no job that fails on a node")
	}
}

func TestProjectObjects(t *testing.T) {
	objs := ProjectObjects(ProjectSpec{Name: "glassworks", Title: "Glassworks (commercial)", LateCostPerHour: "200"})
	y := string(YAML(objs))
	for _, want := range []string{"kind: Namespace", "name: glassworks", "s3://quickstart/glassworks/", `burstBudgetPerDay: "20"`, `lateCostPerHour: "200"`} {
		if !strings.Contains(y, want) {
			t.Errorf("no %q in\n%s", want, y)
		}
	}
	if (ProjectSpec{Name: Project}).Store() != "s3://quickstart/c4/?endpoint=http://garage-s3.c4-store:3900" {
		t.Error("Saltmarsh moved in the store")
	}
}

// The farm's registry: every node's containerd reads per-registry hosts
// files, the manager pins from the registry without TLS, and a development
// farm's own images come from it.
func TestRegistry(t *testing.T) {
	if cfg := string(KindConfig(Options{})); !strings.Contains(cfg, `config_path = "/etc/containerd/certs.d"`) {
		t.Errorf("no containerd patch in\n%s", cfg)
	}
	args := strings.Join(ManagerArgs(Options{}), " ")
	if !strings.Contains(args, "--hud-plain-registries="+RegistryInFarm) || strings.Contains(args, "--hud-image-registry") {
		t.Errorf("a release farm: %s", args)
	}
	args = strings.Join(ManagerArgs(Options{LocalImages: true}), " ")
	if !strings.Contains(args, "--hud-image-registry="+RegistryInFarm) || !strings.Contains(args, "--hud-image-tag=dev") {
		t.Errorf("a development farm: %s", args)
	}
	for in, want := range map[string]string{"c4-job-hydra:dev": "c4-job-hydra:dev", "ghcr.io/avalanche-io/c4-runtime-oiio:v0.1.0": "c4-runtime-oiio:v0.1.0",
		"thumbs": "thumbs:latest", "registry.example/team/nuke:16@sha256:ab": "nuke:16"} {
		if got := localName(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
