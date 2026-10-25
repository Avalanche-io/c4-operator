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

// --tracker gives the HUD the sandbox, and ManagerPatch replaces an
// earlier --hud-sandbox rather than adding one. c4-sync is no development
// viewer: it signs in with its own projected token, as on any farm, since
// a pod never holds the installer's.
func TestTrackerManagerArgs(t *testing.T) {
	plain := strings.Join(ManagerArgs(Options{}), " ")
	if strings.Contains(plain, "--hud-sandbox") || strings.Contains(plain, c4v1.SyncDevViewer) {
		t.Fatalf("a farm without a tracker: %s", plain)
	}
	args := ManagerArgs(Options{Tracker: true})
	if !slices.Contains(args, "--hud-sandbox=http://c4-sandbox-github.c4-system.svc:8080") ||
		!slices.Contains(args, "--hud-dev-viewers="+DevViewers("")) || strings.Contains(strings.Join(args, " "), c4v1.SyncDevViewer) {
		t.Fatalf("a farm with one: %q", args)
	}
	sync := strings.Join(syncArgs(Options{}), " ")
	if !strings.Contains(sync, "--hud-token-file=/var/run/c4-sync/token") || strings.Contains(sync, "--hud-dev-viewer") {
		t.Fatalf("c4-sync's arguments: %s", sync)
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

// On a farm with an owner, the sandbox acts as the owner first, maps the
// owner's login, and the owner reviews: the one person serves every wait.
func TestSandboxOwner(t *testing.T) {
	objs, err := TrackerObjects(Options{Owner: "noor"})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		switch o["kind"] {
		case "Deployment":
			args := strings.Join(o["spec"].(obj)["template"].(obj)["spec"].(obj)["containers"].([]obj)[0]["args"].([]string), " ")
			if !strings.Contains(args, "--people=noor=noor,artist=artist,") {
				t.Fatalf("args %s", args)
			}
		case "Tracker":
			b, _ := json.Marshal(o)
			var tr c4v1.Tracker
			if err := json.Unmarshal(b, &tr); err != nil {
				t.Fatal(err)
			}
			if tr.Spec.People["noor"] != "noor" || len(tr.Spec.People) != len(Viewers)+1 || tr.Spec.Intake[0].Job.Review.Server != "person:noor" {
				t.Fatalf("tracker %+v review %+v", tr.Spec.People, tr.Spec.Intake[0].Job.Review)
			}
		}
	}
	_, people := sandboxPeople("artist")
	if len(people) != len(Viewers) {
		t.Fatalf("an owner named as a role is listed twice: %v", people)
	}
}

// The stand-in GitHub reads the HUD as itself: its own service account, a
// projected token for the HUD alone, and a role that only reads the
// project's jobs, so it can change nothing and is never a person.
func TestSandboxReadsAsItself(t *testing.T) {
	objs, err := TrackerObjects(Options{})
	if err != nil {
		t.Fatal(err)
	}
	var sa, role, binding bool
	for _, o := range objs {
		m := o["metadata"].(obj)
		switch o["kind"] {
		case "ServiceAccount":
			sa = m["name"] == sandboxName
		case "Role":
			if m["name"] != sandboxName {
				continue
			}
			role = true
			for _, r := range o["rules"].([]obj) {
				for _, v := range r["verbs"].([]string) {
					if v != "get" && v != "list" {
						t.Errorf("the sandbox may %s", v)
					}
				}
			}
		case "RoleBinding":
			binding = binding || m["name"] == sandboxName
		case "Deployment":
			spec := o["spec"].(obj)["template"].(obj)["spec"].(obj)
			args := strings.Join(spec["containers"].([]obj)[0]["args"].([]string), " ")
			b, _ := json.Marshal(spec["volumes"])
			if spec["serviceAccountName"] != sandboxName || !strings.Contains(args, "--hud-token-file=/var/run/c4-sandbox/token") ||
				strings.Contains(args, "--hud-as") || !strings.Contains(string(b), c4v1.HUDAPIAudience) {
				t.Fatalf("deployment %v args %s", spec["serviceAccountName"], args)
			}
		}
	}
	if !sa || !role || !binding {
		t.Fatalf("service account %v, role %v, binding %v", sa, role, binding)
	}
}
