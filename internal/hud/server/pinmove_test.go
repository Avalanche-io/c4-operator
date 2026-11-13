package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

// oldPin puts the view package's old-pin fixture into project's store and
// returns its views by name.
func oldPin(t *testing.T, s *Server, project string) map[string]c4.ID {
	t.Helper()
	ctx := context.Background()
	st, err := s.Stores(ctx, project)
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
	index, err := os.ReadFile(filepath.Join(dir, "index.txt"))
	if err != nil {
		t.Fatal(err)
	}
	views := map[string]c4.ID{}
	for _, l := range strings.Split(string(index), "\n") {
		w := strings.Fields(l)
		if len(w) == 3 && w[0] == "view" {
			id, err := c4.Parse(w[2])
			if err != nil {
				t.Fatal(err)
			}
			views[w[1]] = id
		}
	}
	return views
}

// migrate does what migrate-pin does for these views in project's store:
// the new records, then the table the Project's status names. It returns
// each view's new ID.
func migrate(t *testing.T, s *Server, project string, olds ...c4.ID) map[c4.ID]c4.ID {
	t.Helper()
	ctx := context.Background()
	st, err := s.Stores(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	table := c4view.Renames{}
	for _, old := range olds {
		v, err := c4view.Reidentify(ctx, old, c4view.StoreRecords(st), 4)
		if err != nil {
			t.Fatal(err)
		}
		if v.ID == old {
			t.Fatalf("%s is not renamed, so this test shows nothing", old)
		}
		recs, err := v.Records()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			st.PutBytes(ctx, r.Data)
		}
		table[old] = v.ID
	}
	tid, err := st.PutBytes(ctx, table.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var p c4v1.Project
	if err := s.Client.Get(ctx, types.NamespacedName{Name: project}, &p); err != nil {
		t.Fatal(err)
	}
	p.Status.Renames = tid.String()
	if err := s.Client.Update(ctx, &p); err != nil { // these fake clients have no Project status subresource
		t.Fatal(err)
	}
	return table
}

// The fixture's tool tree is the tool these tests publish.
func checkToolFixture(t *testing.T, s *Server, project string, tool c4.ID) {
	t.Helper()
	ctx := context.Background()
	st, _ := s.Stores(ctx, project)
	v, err := c4view.Reidentify(ctx, tool, c4view.StoreRecords(st), 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Files {
		if f.Path == "tool.yaml" && f.ID == c4.Identify(strings.NewReader(notesTool("echo hi > out.txt")["tool.yaml"])) {
			return
		}
	}
	t.Fatal("the fixture's tool.yaml is not notesTool's")
}

// A tool published by a view ID from before the pin moved is read through
// the table: refused while the store has no table, published once it has.
func TestAToolPublishedByAnOldIDIsReadThroughTheTable(t *testing.T) {
	s := toolFarm(t)
	views := oldPin(t, s, "show-a")
	checkToolFixture(t, s, "show-a", views["tool"])
	req := api.ToolPublishRequest{View: views["tool"].String(), DryRun: true}
	if code, out := publish(t, s, "td", "show-a", req); code != http.StatusNotFound {
		t.Fatalf("an old ID with no table: %d %+v, so this test shows nothing", code, out)
	}
	migrate(t, s, "show-a", views["tool"])
	if code, out := publish(t, s, "td", "show-a", req); code != http.StatusOK || !out.OK || out.Tool != "notes" {
		t.Fatalf("an old ID through the table: %d %+v", code, out)
	}
}

// A published version's tree named by an old ID is read through the table.
func TestAPublishedTreeByAnOldIDIsReadThroughTheTable(t *testing.T) {
	ctx := context.Background()
	s := toolFarm(t)
	views := oldPin(t, s, "show-a")
	if _, err := s.readPublished(ctx, "show-a", views["tool"].String()); err == nil {
		t.Fatal("an old ID with no table was read, so this test shows nothing")
	}
	migrate(t, s, "show-a", views["tool"])
	if tree, err := s.readPublished(ctx, "show-a", views["tool"].String()); err != nil || tree == nil {
		t.Fatalf("an old ID through the table: %v", err)
	}
}

// A test job's output named by an old ID is read through the table, and
// the test passes on what it wrote. (The tool's own tree is named by its
// new ID here, so only the output is read through the table.)
func TestATestJobsOldOutputIsReadThroughTheTable(t *testing.T) {
	ctx := context.Background()
	s := toolFarm(t)
	views := oldPin(t, s, "show-a")
	renamed := migrate(t, s, "show-a", views["tool"])
	job := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "notes-test"}}
	if err := s.Client.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	job.Status.Phase, job.Status.Finished = "Succeeded", &now
	job.Status.Output = &c4v1.WorkOutput{View: views["toolout"].String()}
	if err := s.Client.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	tool := c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "notes"}}
	ver := c4v1.ToolVersion{Version: "v1", View: renamed[views["tool"]].String(), Project: "show-a",
		Test: c4v1.ToolTest{State: c4v1.ToolTestRunning, Job: "show-a/notes-test"}}
	if test, done := s.testOutcome(ctx, s.current(), tool, ver); done {
		t.Fatalf("an old output with no table settled the test (%+v), so this test shows nothing", test)
	}
	migrate(t, s, "show-a", views["toolout"])
	if test, done := s.testOutcome(ctx, s.current(), tool, ver); !done || test.State != c4v1.ToolTestOK {
		t.Fatalf("an old output through the table: %+v, settled %v", test, done)
	}
}

// The diff of a view's old ID against its new one is empty: they are the
// same view. (ChangedFiles reads records without recomputing an ID, so it
// gives the same answer without the table; this pins the answer, and
// DESIGN says why the table is not what makes it.)
func TestAnOldIDDiffsEmptyAgainstItsNewOne(t *testing.T) {
	s := toolFarm(t)
	views := oldPin(t, s, "show-a")
	renamed := migrate(t, s, "show-a", views["null-depth2"])
	var out struct {
		Changes []map[string]any `json:"changes"`
	}
	path := "/api/v1/views/show-a/diff?from=" + views["null-depth2"].String() + "&to=" + renamed[views["null-depth2"]].String() + "&as=artist"
	if code := call(t, s, http.MethodGet, path, nil, &out); code != http.StatusOK || len(out.Changes) != 0 {
		t.Fatalf("old against new: %d, %d changes %v", code, len(out.Changes), out.Changes)
	}
}
