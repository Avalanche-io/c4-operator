package class

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/estimate"
)

func init() { register(agent{}) }

// agent: one unit per member of an agent bundle (docs/agents/c4-agent.md,
// "Bundles on the farm"). A member is a Claude Code session that a runner
// resumes by its session ID, so its units checkpoint at every turn and
// yield at a turn boundary, and the next attempt resumes the same session
// from the checkpoint its runner reports.
//
//	params: members "lead,reviewer,lane-1" (required), bundle (the
//	        AgentBundle's name), turnaround "168h", estimateSeconds
type agent struct{}

func (agent) Name() string      { return "agent" }
func (agent) Pools() []string   { return []string{"agent"} }
func (agent) Preemptible() bool { return true }
func (agent) Checkpoints() bool { return true }
func (agent) Lens() string      { return "agent" }

// A member's turns are a model's work.
func (agent) UnitDoes() string { return DoesModel }

// A member stops at the end of its turn, which the Stop hook checkpoints.
func (agent) CheckpointTime(job *c4v1.WorkJob) time.Duration { return 3 * time.Minute }

// Members returns the member names an agent job's params list.
func Members(job *c4v1.WorkJob) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, m := range strings.Split(param(job, "members", ""), ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if seen[m] {
			return nil, fmt.Errorf("param members: %q listed twice", m)
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("param members: name at least one member")
	}
	return out, nil
}

func (agent) Plan(job *c4v1.WorkJob) (*Plan, error) {
	members, err := Members(job)
	if err != nil {
		return nil, err
	}
	prior, err := priorParam(job)
	if err != nil {
		return nil, err
	}
	bundle := param(job, "bundle", job.Name)
	cfg := estimate.DefaultConfig()
	cfg.NoLocality = true
	p := &Plan{OrderName: "members", Estimate: cfg, Prior: prior, Axis: Axis{Kind: "member"}}
	for i, m := range members {
		p.Units = append(p.Units, Unit{Index: i, Label: m, Key: "agent/" + m, Pos: i, Slots: 1,
			Env: []corev1.EnvVar{env("C4_AGENT_MEMBER", m), env("C4_AGENT_BUNDLE", bundle)}})
		p.Order = append(p.Order, i)
	}
	return p, nil
}

func (agent) Objective(job *c4v1.WorkJob, ready time.Time) Objective {
	if job.Spec.Deadline != nil {
		return Objective{Type: "deadline", Deadline: job.Spec.Deadline.Time}
	}
	t, err := durationParam(job, "turnaround", 7*24*time.Hour)
	if err != nil {
		t = 7 * 24 * time.Hour
	}
	return Objective{Type: "deadline", Deadline: job.CreationTimestamp.Add(t)}
}

// ResumeEnv gives the next attempt the checkpoint its runner reported, so
// the runner can check it resumes the same session from the same turn.
func (agent) ResumeEnv(point string) []corev1.EnvVar {
	return []corev1.EnvVar{env("C4_AGENT_CHECKPOINT", point)}
}

// CheckpointPoint formats a member's resume point for
// AnnotationOutputCheckpoint.
func CheckpointPoint(session string, seq int, head string) string {
	return session + "#" + strconv.Itoa(seq) + ":" + head
}
