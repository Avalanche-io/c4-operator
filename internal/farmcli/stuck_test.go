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

// A job held for its failed units says what they said as they failed. One
// that names no tool adds that it has no network only when that may be
// why (a unit failed reaching a host, or none said why), never while it
// runs or when nothing failed.
func TestHeldFailed(t *testing.T) {
	say := "this job names no tool, so it has no network: name a tool that declares one"
	failed := &api.NeedsPerson{Reason: "failed", Message: "2 units failed"}
	bare := []api.Cause{{Kind: "exit", Title: "exit 2", Count: 2}}
	empty := []api.Cause{{Kind: "exit", Title: "exit 2: no pytest configuration, tests/ folder or go.mod", Count: 6},
		{Kind: "node", Title: "node worker2 failed 3 units", Count: 3},
		{Kind: "exit", Title: "exit 137 OOMKilled", Count: 1}}
	fetch := []api.Cause{{Kind: "exit", Title: "exit 1: dial tcp: lookup proxy.golang.org: no such host", Count: 2}}
	for _, c := range []struct {
		j    *api.Job
		want string
	}{
		{&api.Job{NoNetwork: say, NeedsPerson: failed}, "2 units failed; " + say},
		{&api.Job{NoNetwork: say, NeedsPerson: failed, Causes: bare}, "2 units failed; why: exit 2; " + say},
		{&api.Job{NoNetwork: say, NeedsPerson: failed, Causes: empty},
			"2 units failed; why: exit 2: no pytest configuration, tests/ folder or go.mod, or exit 137 OOMKilled"},
		{&api.Job{NoNetwork: say, NeedsPerson: failed, Causes: fetch},
			"2 units failed; why: exit 1: dial tcp: lookup proxy.golang.org: no such host; " + say},
		{&api.Job{NeedsPerson: failed, Causes: fetch}, "2 units failed; why: exit 1: dial tcp: lookup proxy.golang.org: no such host"},
		{&api.Job{NeedsPerson: failed}, "2 units failed"},
		{&api.Job{NoNetwork: say}, ""},
		{&api.Job{NoNetwork: say, NeedsPerson: failed, Units: api.Counts{Running: 1}}, ""},
		{&api.Job{NoNetwork: say, NeedsPerson: &api.NeedsPerson{Reason: "approval"}}, ""},
	} {
		if got := heldFailed(c.j); got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c.j.Causes, got, c.want)
		}
	}
}
