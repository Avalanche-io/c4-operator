package project

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// Current renames through the Project's table; a Project with no table, or
// none at all, renames nothing; one that cannot be read says so, and the
// ID comes back as it was given, never silently as if resolved.
func TestCurrentSaysWhyItCouldNotResolve(t *testing.T) {
	ctx := context.Background()
	ram := store.NewRAM()
	dir := filepath.Join("..", "view", "testdata", "pin19c9")
	files, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, "objects", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		ram.Put(bytes.NewReader(data))
	}
	index, _ := os.ReadFile(filepath.Join(dir, "index.txt"))
	var old c4.ID
	for _, l := range strings.Split(string(index), "\n") {
		if w := strings.Fields(l); len(w) == 3 && w[0] == "view" && w[1] == "null-depth2" {
			old, _ = c4.Parse(w[2])
		}
	}
	v, err := view.Reidentify(ctx, old, view.StoreRecords(ram), 4)
	if err != nil || v.ID == old {
		t.Fatalf("the fixture's view re-identifies as %v (%v)", v, err)
	}
	recs, _ := v.Records()
	for _, r := range recs {
		if !ram.Has(r.ID) {
			ram.Put(bytes.NewReader(r.Data))
		}
	}
	table, err := ram.Put(bytes.NewReader(view.Renames{old: v.ID}.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	c4v1.AddToScheme(s)
	withTable := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "renamed"}, Status: c4v1.ProjectStatus{Renames: table.String()}}
	without := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "plain"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(withTable, without).Build()

	if got, err := Current(ctx, c, ram, "renamed", old); err != nil || got != v.ID {
		t.Fatalf("through the table: %s, %v", got, err)
	}
	for _, name := range []string{"plain", "absent"} {
		if got, err := Current(ctx, c, ram, name, old); err != nil || got != old {
			t.Fatalf("project %s: %s, %v", name, got, err)
		}
	}
	down := fake.NewClientBuilder().WithScheme(s).WithObjects(withTable).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("injected: the API server did not answer")
		}}).Build()
	if got, err := Current(ctx, down, ram, "renamed", old); err == nil || got != old || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("an API server that does not answer: %s, %v", got, err)
	}
	// A table the store lacks (and this process has not read: tables are
	// cached by C4 ID, per process).
	other := view.Renames{old: v.ID, c4.Identify(strings.NewReader("x")): c4.Identify(strings.NewReader("y"))}
	gone := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "gone"},
		Status: c4v1.ProjectStatus{Renames: c4.Identify(bytes.NewReader(other.Bytes())).String()}}
	if err := c.Create(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if got, err := Current(ctx, c, ram, "gone", old); err == nil || got != old {
		t.Fatalf("a table the store lacks: %s, %v", got, err)
	}
}
