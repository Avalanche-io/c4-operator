package server

import (
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A job's loops reach the snapshot with their generators and what the
// queue expects of the rest.
func TestRoundsInTheSnapshot(t *testing.T) {
	s := farm(t)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.Rounds = []c4v1.WorkRounds{{Unit: 0, Name: "plan", Round: 2, Max: 20, Until: "judge", Generators: []int32{0, 3, 6},
		ExpectMore: "4.2", ExpectSeconds: 5400}}
	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	for _, j := range snap.Jobs {
		if j.Name != "sh040-light-v007" {
			continue
		}
		if len(j.Rounds) != 1 {
			t.Fatalf("rounds %+v", j.Rounds)
		}
		r := j.Rounds[0]
		if r.Name != "plan" || r.Round != 2 || r.Max != 20 || r.Until != "judge" || len(r.Generators) != 3 || r.Generators[2] != 6 ||
			r.ExpectMore != 4.2 || r.ExpectSeconds != 5400 || r.StoppedBy != "" {
			t.Fatalf("rounds %+v", r)
		}
		return
	}
	t.Fatal("job not in the snapshot")
}
