package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/project"
)

// In a farm (every farm command's registry refuses local shards), a Project
// with a shard on a local path reports the registry's refusal in its Ready
// condition, once, where a person looks rather than in each pod's events.
// With a registry that does not refuse (the tests' own, one process), nothing
// is reported, and a Project on network shards is never reported.
func TestALocalShardIsAProblemInAFarm(t *testing.T) {
	ctx := context.Background()
	dir := "file://" + t.TempDir()
	farm := &project.Registry{Cache: k8s, Namespace: "c4-system", RefuseLocal: true}
	alone := &project.Registry{Cache: k8s, Namespace: "c4-system"}
	p := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "local-shard"}, Spec: c4v1.ProjectSpec{
		Namespaces: []string{"local-shard"},
		Store:      c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: "c4d://store:7000"}, {Name: "s1", URL: dir}}},
	}}
	count := func(problems []string) int {
		n := 0
		for _, pr := range problems {
			if strings.Contains(pr, "shard s1 is the local path "+dir+", which a farm's node daemons and regional tier refuse") {
				n++
			}
		}
		return n
	}
	if got := (&ProjectReconciler{Client: k8s, Projects: farm}).validate(ctx, p); count(got) != 1 {
		t.Errorf("in a farm, the refusal once: %v", got)
	}
	if got := (&ProjectReconciler{Client: k8s, Projects: alone}).validate(ctx, p); count(got) != 0 {
		t.Errorf("run alone: %v", got)
	}
	p.Spec.Store.Shards = p.Spec.Store.Shards[:1]
	if got := (&ProjectReconciler{Client: k8s, Projects: farm}).validate(ctx, p); count(got) != 0 {
		t.Errorf("network shards, in a farm: %v", got)
	}
}
