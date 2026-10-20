package server

import (
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A superseded unit is counted and drawn as superseded, with its score;
// the review that chose shows its selection rule.
func TestSupersededInTheHUD(t *testing.T) {
	s := farm(t)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-6,8-9", "superseded": "7"}
	rec.job.Status.Units = c4v1.UnitCounts{Total: 10, Succeeded: 9, Superseded: 1}
	rec.job.Status.Reviews = map[string]c4v1.WorkReview{"7": {Do: "supersede", Why: "not selected: top 1", By: 9, Score: "0.4", Scored: 1}}
	rec.job.Status.Verdicts = []c4v1.WorkVerdict{{Unit: 9, Verdict: "pass", Passed: 1, Superseded: 1, Select: "top 1"}}
	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	found := false
	for _, j := range snap.Jobs {
		if j.Name == "sh040-light-v007" {
			found = true
			if j.Units.Superseded != 1 || j.UnitStates.Superseded != "7" {
				t.Fatalf("counts %+v states %+v", j.Units, j.UnitStates)
			}
		}
	}
	if !found {
		t.Fatal("job not in the snapshot")
	}
	var detail struct {
		UnitList []struct {
			I      int    `json:"i"`
			State  string `json:"state"`
			Select string `json:"select"`
			Review *struct {
				Do    string `json:"do"`
				Score string `json:"score"`
			} `json:"review"`
		} `json:"unitList"`
	}
	get(t, s, "/api/v1/jobs/show-a/sh040-light-v007?as=wrangler", &detail)
	for _, u := range detail.UnitList {
		switch u.I {
		case 7:
			if u.State != "superseded" || u.Review == nil || u.Review.Do != "supersede" || u.Review.Score != "0.4" {
				t.Fatalf("unit 7: %+v", u)
			}
		case 9:
			if u.Select != "top 1" {
				t.Fatalf("unit 9: %+v", u)
			}
		}
	}
}
