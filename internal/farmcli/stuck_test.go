package farmcli

import (
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A job that cannot run reads as stuck, with its reason, never as ready
// at about now with no slack.
func TestStuckJobsReadAsStuck(t *testing.T) {
	refused := &api.Job{Namespace: "saltmarsh", Name: "agents", Phase: "Queued",
		NeedsPerson: &api.NeedsPerson{Reason: "permission", Message: "Pod Security (restricted) in saltmarsh refuses the job's units"}}
	refused.Estimate.EtaP50 = time.Now()
	if got := stuck(refused); !strings.Contains(got, "Pod Security") {
		t.Fatalf("a refused job is not stuck: %q", got)
	}
	if line := titleBlock(Clock{}, refused); !strings.Contains(line, "stuck: Pod Security") || strings.Contains(line, "ready ~") {
		t.Fatalf("watch shows a refused job as ready: %s", line)
	}
	for _, j := range []*api.Job{
		{NeedsPerson: &api.NeedsPerson{Reason: "failed", Message: "2 units failed"}},
		{NeedsPerson: &api.NeedsPerson{Reason: "permission"}, Units: api.Counts{Running: 1}},
		{},
	} {
		if got := stuck(j); got != "" {
			t.Errorf("%+v reads as stuck: %q", j.NeedsPerson, got)
		}
	}
}

// A job that names no tool says it has no network only when it is held
// for its failed units with nothing running, never while it runs or when
// nothing failed.
func TestHeldWithoutNetwork(t *testing.T) {
	say := "this job names no tool, so it has no network: name a tool that declares one"
	held := &api.Job{NoNetwork: say, NeedsPerson: &api.NeedsPerson{Reason: "failed", Message: "2 units failed"}}
	if got := heldWithoutNetwork(held); got != "2 units failed; "+say {
		t.Fatalf("held: %q", got)
	}
	for _, j := range []*api.Job{
		{NoNetwork: say},
		{NoNetwork: say, NeedsPerson: &api.NeedsPerson{Reason: "failed"}, Units: api.Counts{Running: 1}},
		{NoNetwork: say, NeedsPerson: &api.NeedsPerson{Reason: "approval"}},
		{NeedsPerson: &api.NeedsPerson{Reason: "failed", Message: "2 units failed"}},
	} {
		if got := heldWithoutNetwork(j); got != "" {
			t.Errorf("%+v: %q", j, got)
		}
	}
}
