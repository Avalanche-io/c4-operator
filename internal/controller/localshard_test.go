package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/project"
)

// In a farm (the manager command sets RefuseLocal), a Project with a shard
// on a local path reports it in its Ready condition, since the farm's node
// daemons and regional tier refuse it and a person looks on the Project, not
// in each pod's events. Run alone (the tests), the manager reports nothing,
// and a Project on network shards is never reported.
func TestALocalShardIsAProblemInAFarm(t *testing.T) {
	ctx := context.Background()
	dir := "file://" + t.TempDir()
	// The manager's registry, which opens a path (RefuseLocal off).
	reg := &project.Registry{Cache: k8s, Namespace: "c4-system"}
	p := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "local-shard"}, Spec: c4v1.ProjectSpec{
		Namespaces: []string{"local-shard"},
		Store:      c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: "c4d://store:7000"}, {Name: "s1", URL: dir}}},
	}}
	has := func(problems []string) bool {
		for _, pr := range problems {
			if strings.Contains(pr, "shard s1 is the local path "+dir+", which a farm's node daemons and regional tier refuse") {
				return true
			}
		}
		return false
	}
	if got := (&ProjectReconciler{Client: k8s, Projects: reg, RefuseLocal: true}).validate(ctx, p); !has(got) {
		t.Errorf("in a farm: %v", got)
	}
	if got := (&ProjectReconciler{Client: k8s, Projects: reg}).validate(ctx, p); has(got) {
		t.Errorf("run alone: %v", got)
	}
	p.Spec.Store.Shards = p.Spec.Store.Shards[:1]
	if got := (&ProjectReconciler{Client: k8s, Projects: reg, RefuseLocal: true}).validate(ctx, p); has(got) {
		t.Errorf("network shards, in a farm: %v", got)
	}
}
