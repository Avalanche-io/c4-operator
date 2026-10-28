package farmcli

import (
	"context"
	"fmt"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// JobLag bounds the wait for a job the farm has just accepted to show in
// the HUD's snapshot, which it collects about once a second: a script
// that submits and then lists, opens, watches or moves the job finds it.
var JobLag = 2 * time.Second

// jobPoll is how often the snapshot is read meanwhile.
var jobPoll = 250 * time.Millisecond

// awaitJobs waits, up to JobLag, for jobs the farm just accepted
// (namespace/name) to show in its snapshot. It never fails: a snapshot
// that cannot be read is the next command's to say.
func awaitJobs(ctx context.Context, c *Client, refs ...string) {
	if len(refs) == 0 {
		return
	}
	end := time.Now().Add(JobLag)
	for {
		s, err := Snapshot(ctx, c)
		if err != nil || hasJobs(s, refs) || time.Now().After(end) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(jobPoll):
		}
	}
}

func hasJobs(s *api.Snapshot, refs []string) bool {
	seen := map[string]bool{}
	for _, j := range s.Jobs {
		seen[j.Namespace+"/"+j.Name] = true
	}
	for _, r := range refs {
		if !seen[r] {
			return false
		}
	}
	return true
}

// FarmLine is a farm's count as `c4 farm status` says it, in up's words:
// its nodes (a burst node not rented yet is none of them), its slots in
// use, the units waiting, and the slots it rents only when a deadline
// needs them.
func FarmLine(s *api.Snapshot) string {
	burst, more := map[string]bool{}, 0
	for _, p := range s.Pools {
		if p.Burst != nil {
			burst[p.Name] = true
			more += p.Burst.MaxSlots
		}
	}
	nodes, rented := 0, 0
	for _, n := range s.Nodes {
		if n.State == "unprovisioned" {
			continue
		}
		nodes++
		if burst[n.Pool] {
			rented++
		}
	}
	line := fmt.Sprintf("%s · %d of %d slots in use · %s waiting", plural(nodes, "node", "nodes"), s.Farm.SlotsUsed, s.Farm.SlotsTotal,
		plural(s.Farm.QueuedUnits, "unit", "units"))
	switch {
	case rented > 0:
		line += fmt.Sprintf(" · %d of them rented for a deadline", rented)
	case more > 0:
		line += fmt.Sprintf(" · %d more slots it rents only when a deadline needs them", more)
	}
	return line
}
