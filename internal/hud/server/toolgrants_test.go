package server

import (
	"context"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// personFirst is a client that, before the HUD's first patch of a
// project, has a person add a grant to it, as an edit between the HUD's
// read and its write would.
type personFirst struct {
	client.Client
	project, tree string
	done          bool
}

func (c *personFirst) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if p, ok := obj.(*c4v1.Project); ok && p.Name == c.project && !c.done {
		c.done = true
		var cur c4v1.Project
		if err := c.Client.Get(ctx, types.NamespacedName{Name: c.project}, &cur); err != nil {
			return err
		}
		cur.Spec.Grants = append(cur.Spec.Grants, c4v1.Grant{Tree: c.tree})
		if err := c.Client.Update(ctx, &cur); err != nil {
			return err
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// A grant a person adds while the HUD grants a farm tool is kept: the
// HUD's write of the list is guarded by the project's resourceVersion and
// made again over what it reads anew. Before, the list was written whole
// over the person's grant.
func TestAToolGrantKeepsAGrantMadeMeanwhile(t *testing.T) {
	s := toolFarm(t)
	ctx := context.Background()
	stores := map[string]*storeurl.Sharded{}
	for _, p := range []string{"show-a", "show-b"} {
		stores[p], _ = s.Stores(ctx, p)
	}
	stC, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	stores["show-c"] = stC
	s.Stores = func(_ context.Context, project string) (*storeurl.Sharded, error) { return stores[project], nil }
	_, v1 := publish(t, s, "admin", "show-a", api.ToolPublishRequest{View: putTree(t, s, "show-a", notesTool("echo hi > out.txt")).String(), Farm: true})
	finishTest(t, s, v1.Test.Job, "Succeeded", map[string]string{"out.txt": "hi"})

	// show-c, made later, reading only granted trees; a person grants it
	// a tree while the HUD grants it the tool.
	c := c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-c"}, Spec: c4v1.ProjectSpec{Namespaces: []string{"show-c"}, Scope: c4v1.ScopeGrants}}
	if err := s.Client.Create(ctx, &c); err != nil {
		t.Fatal(err)
	}
	person := putTree(t, s, "show-a", notesTool("echo theirs")).String()
	s.Client = &personFirst{Client: s.Client, project: "show-c", tree: person}
	s.m.projects = append(s.m.projects, c)
	s.reconcileFarmTools(ctx, s.current())

	var got c4v1.Project
	if err := s.Client.Get(ctx, types.NamespacedName{Name: "show-c"}, &got); err != nil {
		t.Fatal(err)
	}
	var trees []string
	for _, g := range got.Spec.Grants {
		trees = append(trees, g.Tree)
	}
	if !slices.Contains(trees, person) {
		t.Errorf("the person's grant was written over: %v", trees)
	}
	if !slices.Contains(trees, v1.View) {
		t.Errorf("the tool was not granted: %v", trees)
	}
}
