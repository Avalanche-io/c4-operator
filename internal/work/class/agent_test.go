package class

import (
	"strings"
	"testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

func agentJob(members string) *c4v1.WorkJob {
	return &c4v1.WorkJob{Spec: c4v1.WorkJobSpec{Class: "agent", Params: map[string]string{"members": members, "bundle": "team"}}}
}

func TestAgentPlan(t *testing.T) {
	d, err := Lookup("agent")
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.Plan(agentJob("lead, reviewer ,lane-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Units) != 3 || p.Units[1].Label != "reviewer" || p.Units[1].Slots != 1 {
		t.Fatalf("units %+v", p.Units)
	}
	env := map[string]string{}
	for _, e := range p.Units[2].Env {
		env[e.Name] = e.Value
	}
	if env["C4_AGENT_MEMBER"] != "lane-1" || env["C4_AGENT_BUNDLE"] != "team" {
		t.Fatalf("env %v", env)
	}
	if !d.Checkpoints() || !d.Preemptible() {
		t.Fatal("agent units must checkpoint and be preemptible")
	}
	r, ok := d.(Resumer)
	if !ok {
		t.Fatal("the agent class must resume from its runner's checkpoint")
	}
	if e := r.ResumeEnv("s#3:h"); len(e) != 1 || e[0].Name != "C4_AGENT_CHECKPOINT" || e[0].Value != "s#3:h" {
		t.Fatalf("resume env %v", e)
	}
	for _, bad := range []string{"", " , ", "lead,lead"} {
		if _, err := d.Plan(agentJob(bad)); err == nil || !strings.Contains(err.Error(), "members") {
			t.Fatalf("members %q: err %v", bad, err)
		}
	}
}
