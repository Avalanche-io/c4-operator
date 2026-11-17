package project

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

func projectWith(name string, access c4v1.StoreAccess, urls ...string) *c4v1.Project {
	p := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}}
	p.Spec.Store.Access = access
	for i, u := range urls {
		p.Spec.Store.Shards = append(p.Spec.Store.Shards, c4v1.StoreShard{Name: "s" + string(rune('0'+i)), URL: u})
	}
	return p
}

// A registry that refuses local shards (a farm's node daemons and regional
// tier) refuses a project with one, Direct or Tiered, before it opens
// anything; the same project opens in a registry that does not (the manager
// and the tests, one process); a project on network shards opens in both.
func TestARegistryThatRefusesLocalShards(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, access := range []c4v1.StoreAccess{c4v1.AccessDirect, c4v1.AccessTiered, ""} {
		for _, path := range []string{dir, "file://" + dir} {
			p := projectWith("frames", access, "c4d://store:7000", path)
			_, err := (&Registry{RefuseLocal: true}).Store(ctx, p)
			if err == nil || !strings.Contains(err.Error(), "project frames: shard s1 is the local path") || strings.Contains(err.Error(), "Tiered") {
				t.Errorf("%q access, shard %q, refusing: %v", access, path, err)
			}
			if _, err := (&Registry{}).Store(ctx, p); err != nil {
				t.Errorf("%q access, shard %q, not refusing: %v", access, path, err)
			}
		}
		p := projectWith("net", access, "c4d://store:7000", "https://store.example")
		if _, err := (&Registry{RefuseLocal: true}).Store(ctx, p); err != nil {
			t.Errorf("%q access, network shards, refusing local: %v", access, err)
		}
	}
}

func TestLocalShardFindsTheFirstPath(t *testing.T) {
	if sh, ok := LocalShard(projectWith("p", "", "s3://b/p/", "/var/a", "file:///var/b")); !ok || sh.Name != "s1" {
		t.Errorf("LocalShard: %+v, %v", sh, ok)
	}
	if sh, ok := LocalShard(projectWith("p", "", "s3://b/p/", "https://store")); ok {
		t.Errorf("LocalShard of network shards: %+v", sh)
	}
}
