package localfarm

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// --tracker gives c4-sync its HUD sign-in and the HUD the sandbox, and
// ManagerPatch replaces an earlier --hud-sandbox rather than adding one.
func TestTrackerManagerArgs(t *testing.T) {
	plain := strings.Join(ManagerArgs(Options{}), " ")
	if strings.Contains(plain, "--hud-sandbox") || strings.Contains(plain, c4v1.SyncDevViewer) {
		t.Fatalf("a farm without a tracker: %s", plain)
	}
	args := ManagerArgs(Options{Tracker: true})
	if !slices.Contains(args, "--hud-sandbox=http://c4-sandbox-github.c4-system.svc:8080") ||
		!slices.Contains(args, "--hud-dev-viewers="+DevViewers()+";c4-sync@wrangler=saltmarsh") {
		t.Fatalf("a farm with one: %q", args)
	}
	var p struct {
		Spec struct {
			Template struct {
				Spec struct{ Containers []struct{ Args []string } }
			}
		}
	}
	if err := json.Unmarshal(ManagerPatch(Options{Tracker: true}, []string{"manager", "--hud-sandbox=http://old"}), &p); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(p.Spec.Template.Spec.Containers[0].Args, " ")
	if strings.Count(got, "--hud-sandbox=") != 1 || strings.Contains(got, "http://old") {
		t.Fatalf("patched args %s", got)
	}
}

// The Tracker is one the operator's types take as written, and its job is
// one the render class plans: two stand-in units, then the supervisor's
// review, which takes a rework for the next version.
func TestTrackerObjects(t *testing.T) {
	objs, err := TrackerObjects(Options{})
	if err != nil {
		t.Fatal(err)
	}
	var tr c4v1.Tracker
	kinds := map[string]bool{}
	for _, o := range objs {
		kinds[o["kind"].(string)] = true
		if o["kind"] != "Tracker" {
			continue
		}
		b, _ := json.Marshal(o)
		if err := yaml.UnmarshalStrict(b, &tr); err != nil {
			t.Fatalf("not a Tracker: %v", err)
		}
	}
	for _, k := range []string{"Secret", "Deployment", "Service", "Role", "RoleBinding", "Tracker"} {
		if !kinds[k] {
			t.Errorf("no %s", k)
		}
	}
	if tr.Namespace != Project || tr.Spec.Kind != "github" || tr.Spec.Endpoint != sandboxURL || len(tr.Spec.People) != len(Viewers) {
		t.Fatalf("tracker %+v", tr.Spec)
	}
	in := tr.Spec.Intake[0]
	if in.For == nil || !in.For.Assignee || in.Rework != "nextVersion" || !slices.Equal(in.Match.Labels, []string{"c4"}) {
		t.Fatalf("intake %+v", in)
	}
	d, err := class.Lookup(in.Job.Class)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := d.Plan(&c4v1.WorkJob{Spec: *in.Job})
	if err != nil || len(plan.Units) != 2 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if r := in.Job.Review; r == nil || r.Server != "person:supervisor" || !slices.Contains(r.Choices, "rework") {
		t.Fatalf("review %+v", in.Job.Review)
	}
	if !strings.Contains(string(YAML(objs)), "C4_SANDBOX_TOKEN") {
		t.Fatal("the sandbox does not read its token from the Secret")
	}
}

func TestSyncImageIsReplaced(t *testing.T) {
	m := []byte("kind: Deployment\nspec:\n  containers:\n  - name: sync\n    image: ghcr.io/avalanche-io/c4-sync:v0.2.0\n")
	if s := syncImage.FindSubmatch(m); s == nil || string(s[2]) != "ghcr.io/avalanche-io/c4-sync:v0.2.0" {
		t.Fatalf("found %q", s)
	}
	got := string(syncImage.ReplaceAll(m, []byte("${1}c4-local-registry:5000/c4-sync:dev${3}")))
	if !strings.Contains(got, "image: c4-local-registry:5000/c4-sync:dev\n") {
		t.Fatalf("replaced:\n%s", got)
	}
}
