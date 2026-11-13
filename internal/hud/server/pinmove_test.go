package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// A view's ID from before the c4 pin moved, given to the HUD from outside
// (`c4 farm get` of an old job's output, a tracker's link), lists the view
// as it is now: the reader resolves it through the table the Project's
// status names. Without the table it is not found, since its records
// identify as another view now.
func TestAnOldViewIDIsListedThroughTheProjectsTable(t *testing.T) {
	ctx := context.Background()
	s := submitFarm(t)
	st, err := s.Stores(ctx, "show-a")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("..", "..", "view", "testdata", "pin19c9")
	files, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, "objects", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutBytes(ctx, data); err != nil {
			t.Fatal(err)
		}
	}
	// null-depth2: s/r/f.0001.exr and s/r/f.0002.exr, null-sized.
	old, err := c4.Parse("c43kXDKcYWTTcbR2rDXNdssgRHMnTWX3PT3iMDoDJ6ArP57WTh8kvC4hS4gCoXK138NUEwsruPMWP47PFuH1vemdqe")
	if err != nil {
		t.Fatal(err)
	}
	index, _ := os.ReadFile(filepath.Join(dir, "index.txt"))
	if !strings.Contains(string(index), "view null-depth2 "+old.String()) {
		t.Fatal("the fixture's null-depth2 is not the view this test names")
	}
	p := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-a"}}
	if err := s.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/views/show-a/" + old.String() + "/files?as=artist"
	if code := call(t, s, http.MethodGet, path, nil, nil); code != http.StatusNotFound {
		t.Fatalf("an old ID with no table: %d, so this test shows nothing", code)
	}

	v, err := c4view.Reidentify(ctx, old, c4view.StoreRecords(st), 4)
	if err != nil {
		t.Fatal(err)
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
	p.Status.Renames = table.String()
	if err := s.Client.Update(ctx, p); err != nil { // this fake client has no status subresource
		t.Fatal(err)
	}
	var ls api.ViewListing
	if code := call(t, s, http.MethodGet, path, nil, &ls); code != http.StatusOK || len(ls.Files) != 2 || ls.View != v.ID.String() {
		t.Fatalf("an old ID through the table: %d %+v", code, ls)
	}
}
