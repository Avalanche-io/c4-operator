package server

import (
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A unit's branch in the job's status shows in the snapshot's job and on
// its unit in the job's detail, with no pod to read.
func TestUnitBranches(t *testing.T) {
	s := farm(t)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.UnitBranches = map[string]c4v1.WorkUnitBranch{
		"3": {Repo: "/Users/sam/src/textkit", Branch: "c4/sh040-light-v007/3", Base: "b0", Head: "h2", Commits: 3, Attempt: 2}}

	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	var job *api.Job
	for i := range snap.Jobs {
		if snap.Jobs[i].Name == "sh040-light-v007" {
			job = &snap.Jobs[i]
		}
	}
	if job == nil || len(job.Branches) != 1 {
		t.Fatalf("snapshot job: %+v", job)
	}
	if b := job.Branches[0]; b.Unit != 3 || b.Branch != "c4/sh040-light-v007/3" || b.Head != "h2" || b.Commits != 3 || b.Repo != "/Users/sam/src/textkit" {
		t.Fatalf("snapshot branch: %+v", b)
	}

	var d struct {
		UnitList []struct {
			I      int `json:"i"`
			Branch *struct {
				Branch  string `json:"branch"`
				Head    string `json:"head"`
				Commits int    `json:"commits"`
			} `json:"branch"`
		} `json:"unitList"`
	}
	if code := get(t, s, "/api/v1/jobs/show-a/sh040-light-v007?as=wrangler", &d); code != 200 {
		t.Fatal(code)
	}
	for _, u := range d.UnitList {
		switch {
		case u.I == 3 && (u.Branch == nil || u.Branch.Branch != "c4/sh040-light-v007/3" || u.Branch.Commits != 3):
			t.Fatalf("unit 3's branch: %+v", u.Branch)
		case u.I != 3 && u.Branch != nil:
			t.Fatalf("unit %d has a branch: %+v", u.I, u.Branch)
		}
	}
}
