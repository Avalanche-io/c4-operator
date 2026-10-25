package server

import (
	"net/http"
	"testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A job just made is read at once: its detail, a unit, its custody and
// its book answer from the API server before the model has collected
// it, marked fresh; a job that does not exist is still 404.
func TestFreshJobIsReadAtOnce(t *testing.T) {
	s, _ := commitFarm(t)
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	stale(s)
	s.DevViewers["outsider"] = &api.Viewer{User: "sam", Projects: []string{"show-b"}}
	code, out := submit(t, s, api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree)})
	if code != http.StatusCreated || len(out.Jobs) != 2 {
		t.Fatalf("submit: %d %+v", code, out)
	}
	name := out.Jobs[0].Name
	if s.current().byKey["show-a/"+name] != nil {
		t.Fatal("the model collected the job: the test reads nothing fresh")
	}
	var detail map[string]any
	if code := get(t, s, "/api/v1/jobs/show-a/"+name+"?as=artist", &detail); code != http.StatusOK {
		t.Fatalf("detail of a job just made: %d %v", code, detail)
	}
	if detail["fresh"] != true || detail["name"] != name {
		t.Fatalf("detail: fresh %v, name %v", detail["fresh"], detail["name"])
	}
	for _, path := range []string{"/units/0", "/deadline-options"} {
		var v map[string]any
		if code := get(t, s, "/api/v1/jobs/show-a/"+name+path+"?as=artist", &v); code == http.StatusNotFound {
			t.Fatalf("%s of a job just made: 404 %v", path, v)
		}
	}
	var book api.Book
	if code := get(t, s, "/api/v1/book?as=artist&job=show-a/"+name, &book); code != http.StatusOK {
		t.Fatalf("book of a job just made: %d", code)
	}
	// a burst of reads plans the job once
	first := s.fresh.recs["show-a/"+name]
	get(t, s, "/api/v1/jobs/show-a/"+name+"?as=artist", &detail)
	if s.fresh.recs["show-a/"+name] != first {
		t.Fatal("a second read of the same version planned the job again")
	}
	var miss map[string]any
	if code := get(t, s, "/api/v1/jobs/show-a/sm0999-proxies-v001?as=artist", &miss); code != http.StatusNotFound {
		t.Fatalf("a job that does not exist: %d", code)
	}
	// another project's viewer does not learn the name
	if code := get(t, s, "/api/v1/jobs/show-a/"+name+"?as=outsider", &miss); code != http.StatusNotFound {
		t.Fatalf("a viewer outside the project, a fresh job: %d", code)
	}
}
