package queue

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// A job's tool named by a view ID from before the c4 pin moved: the
// queue's check of what the tool declares reads its tree through the
// table the project's status names. With no table it cannot read it.
func TestAToolsOldIDIsReadThroughTheTable(t *testing.T) {
	ctx := context.Background()
	st, dir := foldStore(t)
	fixture := filepath.Join("..", "..", "view", "testdata", "pin19c9")
	files, err := os.ReadDir(filepath.Join(fixture, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(fixture, "objects", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutBytes(ctx, data); err != nil {
			t.Fatal(err)
		}
	}
	index, err := os.ReadFile(filepath.Join(fixture, "index.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var old c4.ID
	for _, l := range strings.Split(string(index), "\n") {
		if w := strings.Fields(l); len(w) == 3 && w[0] == "view" && w[1] == "tool" {
			if old, err = c4.Parse(w[2]); err != nil {
				t.Fatal(err)
			}
		}
	}
	pr := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "pinned-tools"}, Spec: c4v1.ProjectSpec{Namespaces: []string{"pinned-tools"},
		Store: c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: dir}}}}}
	if err := k8s.Create(ctx, pr); err != nil {
		t.Fatal(err)
	}
	q := &Queue{Client: k8s, Stores: func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }}
	if _, err := q.declaredNetworks(ctx, "pinned-tools", old.String()); err == nil {
		t.Fatal("an old tool ID with no table was read, so this test shows nothing")
	}

	// What migrate-pin does: the new records, then the table the status
	// names.
	v, err := c4view.Reidentify(ctx, old, c4view.StoreRecords(st), 4)
	if err != nil || v.ID == old {
		t.Fatalf("the fixture's tool re-identifies as %v (%v)", v, err)
	}
	recs, err := v.Records()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		st.PutBytes(ctx, r.Data)
	}
	table, err := st.PutBytes(ctx, c4view.Renames{old: v.ID}.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	pr.Status.Renames = table.String()
	if err := k8s.Status().Update(ctx, pr); err != nil {
		t.Fatal(err)
	}
	// The client here reads through a cache.
	var nets []string
	for i := 0; ; i++ {
		if nets, err = q.declaredNetworks(ctx, "pinned-tools", old.String()); err == nil || i == 100 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("an old tool ID through the table: %v", err)
	}
	if len(nets) != 0 {
		t.Fatalf("the fixture's tool declares %v", nets)
	}
}
