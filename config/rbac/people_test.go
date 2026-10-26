package rbac

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The roles for people give each of them what the HUD checks for, in one
// project, and nothing beyond it: an artist sees and submits, a wrangler
// also removes jobs and approves burst, a project admin also manages the
// project's tools and grants the roles; nobody disables a node.
func TestPeopleRoles(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob("../../bin/k8s/k8s/*")
		if len(matches) == 0 {
			t.Skip("envtest binaries not found; run `make envtest`")
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		t.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Stop() })
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	data, err := os.ReadFile("people.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
	roles := map[string]*rbacv1.ClusterRole{}
	for {
		var cr rbacv1.ClusterRole
		if err := dec.Decode(&cr); err != nil {
			break
		}
		if cr.Name == "" {
			continue
		}
		if err := c.Create(ctx, &cr); err != nil {
			t.Fatal(err)
		}
		roles[cr.Name] = &cr
	}
	// The read and submit rules join Kubernetes' own roles (the
	// aggregation controller, not in envtest, does the joining).
	if l := roles["c4-view"].Labels; l["rbac.authorization.k8s.io/aggregate-to-view"] != "true" || l["rbac.authorization.k8s.io/aggregate-to-edit"] != "true" {
		t.Fatalf("c4-view does not join view and edit: %v", l)
	}
	if l := roles["c4-submit"].Labels; l["rbac.authorization.k8s.io/aggregate-to-edit"] != "true" {
		t.Fatalf("c4-submit does not join edit: %v", l)
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "show-a"}}); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"maya": "c4-artist", "wren": "c4-wrangler", "adele": "c4-project-admin"} {
		rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: user},
			RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role},
			Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: user}}}
		if err := c.Create(ctx, rb); err != nil {
			t.Fatal(err)
		}
	}
	may := func(user, ns, verb, group, resource string) bool {
		t.Helper()
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{User: user,
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns, Verb: verb, Group: group, Resource: resource}}}
		if err := c.Create(ctx, sar); err != nil {
			t.Fatal(err)
		}
		return sar.Status.Allowed
	}
	const c4 = "c4.avalanche.io"
	// The authorizer reads roles and bindings from a cache that lags the
	// writes above: wait until each person is allowed what all three roles
	// give, so a denial below is the role's, not the cache's.
	for _, user := range []string{"maya", "wren", "adele"} {
		deadline := time.Now().Add(15 * time.Second)
		for !may(user, "show-a", "list", c4, "workjobs") {
			if time.Now().After(deadline) {
				t.Fatalf("%s's binding was never seen", user)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	for _, x := range []struct {
		verb, group, resource   string
		artist, wrangler, admin bool
	}{
		{"list", c4, "workjobs", true, true, true}, // sees the project
		{"create", c4, "workjobs", true, true, true},
		{"patch", c4, "workjobs", true, true, true}, // hold, boost, steer, requeue, decide
		{"create", c4, "materializations", true, true, true},
		{"create", c4, "agentbundles", true, true, true},
		{"delete", c4, "workjobs", false, true, true},
		{"patch", c4, "capacityrequests", false, true, true}, // approve burst
		{"delete", c4, "submitgrants", false, true, true},
		{"create", c4, "tools", false, false, true},
		{"create", "rbac.authorization.k8s.io", "rolebindings", false, false, true},
		{"create", "", "pods", false, false, false},
		{"get", "", "secrets", false, false, false},
	} {
		for user, want := range map[string]bool{"maya": x.artist, "wren": x.wrangler, "adele": x.admin} {
			if got := may(user, "show-a", x.verb, x.group, x.resource); got != want {
				t.Errorf("%s %s %s: %v, want %v", user, x.verb, x.resource, got, want)
			}
		}
	}
	// One project only, and no node anywhere.
	for _, user := range []string{"maya", "wren", "adele"} {
		if may(user, "show-b", "list", c4, "workjobs") {
			t.Errorf("%s sees a project they were not given", user)
		}
		if may(user, "", "patch", "", "nodes") {
			t.Errorf("%s may disable a node", user)
		}
	}
}
