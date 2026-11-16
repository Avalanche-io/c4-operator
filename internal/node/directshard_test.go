package node

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A Direct project's store is written by every node daemon, so a shard on
// a local path would have as many writers as the farm has nodes, and c4's
// TreeStore is safe for one process only: a node refuses such a project,
// naming the shard, before it opens anything.
func TestADirectProjectOnALocalPathIsRefused(t *testing.T) {
	for _, url := range []string{"file:///mnt/shared/store", "/mnt/shared/store", " /mnt/shared/store"} {
		p := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "frames"}, Spec: c4v1.ProjectSpec{Store: c4v1.ProjectStore{
			Access: c4v1.AccessDirect,
			Shards: []c4v1.StoreShard{{Name: "s0", URL: "s3://bucket/p/"}, {Name: "s1", URL: url}},
		}}}
		// A bare Daemon: the refusal comes before the node opens a cache, a
		// layer store or the store, so it reaches none of them.
		_, err := (&Daemon{}).projectCtx(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), "project frames is Direct, and its shard s1 is the local path") || !strings.Contains(err.Error(), "make the project Tiered") {
			t.Errorf("a Direct project with the shard %q: %v", url, err)
		}
	}
}
