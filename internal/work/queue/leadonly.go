package queue

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
)

// A worker may not start workers (agents.submit, lead by default). The HUD
// refuses a unit's agent work at submit, where the job's spec shows it;
// what it cannot see is a unit's job whose generator writes agent units
// only while it runs. The queue refuses those units when it reads the
// expansion: nothing is added, the generator fails (or the review whose
// verdict carried it), the signed expand decision carries the refusal,
// and the job's AgentWork condition says why in the HUD's words, until a
// person acts.

// leadOnlyError is an expansion whose agent units the queue refused.
type leadOnlyError struct {
	units []string
	use   policy.Use
}

func (leadOnlyError) Error() string { return c4v1.LeadOnlyMessage }

// leadExpands refuses expansion ex if the job is a unit's, its project's
// agents.submit is lead, and ex adds agent units.
func (q *Queue) leadExpands(v *view, ex *class.Expansion) error {
	if v.job.Annotations[c4v1.AnnotationSubmittedByUnit] == "" {
		return nil // a person's job: a lead's own session signs in as one
	}
	var agents []string
	for _, u := range ex.Units {
		if agentUnit(v.job, u) {
			agents = append(agents, u.Name)
		}
	}
	if len(agents) == 0 {
		return nil
	}
	who, from := policy.Value(v.job, q.projectOf(v), -1, policy.AgentsSubmit)
	if who != policy.SubmitLead {
		return nil
	}
	return leadOnlyError{units: agents, use: policy.Use{Key: policy.AgentsSubmit, Value: who, From: from}}
}

// agentUnit says whether unit u runs an agent: a bundle member
// (C4_AGENT_MEMBER) or a block (C4_AGENT_BLOCK), set in its own
// environment or its job's template. Any value counts, as at submit, so
// a malformed one is never a way past the rule.
func agentUnit(j *c4v1.WorkJob, u class.Unit) bool {
	for _, e := range u.Env {
		if (e.Name == "C4_AGENT_MEMBER" || e.Name == "C4_AGENT_BLOCK") && e.Value != "" {
			return true
		}
	}
	for _, c := range j.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "C4_AGENT_BLOCK" && e.Value != "" {
				return true
			}
		}
	}
	return false
}

// leadOnlyRefused is the unit whose expansion, or whose verdict's, the
// queue refused as a unit's agent work, or -1.
func leadOnlyRefused(m *jobMem) int {
	for _, x := range m.expansions {
		if x.Error == c4v1.LeadOnlyMessage {
			return x.Unit
		}
	}
	for _, r := range m.verdicts {
		if strings.Contains(r.Error, c4v1.LeadOnlyMessage) {
			return int(r.Unit)
		}
	}
	return -1
}

// agentWorkCondition sets the job's AgentWork condition while an
// expansion of it was refused as a unit's agent work.
func agentWorkCondition(st *c4v1.WorkJobStatus, v *view) {
	gen := leadOnlyRefused(v.mem)
	if gen < 0 {
		meta.RemoveStatusCondition(&st.Conditions, c4v1.ConditionAgentWork)
		return
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: c4v1.ConditionAgentWork, Status: metav1.ConditionFalse,
		Reason: "LeadOnly", ObservedGeneration: v.job.Generation,
		Message: fmt.Sprintf("%s (unit %s would add agent units, and this job was submitted by a unit): %s",
			c4v1.LeadOnlyMessage, unitName(v.mem.plan, gen), c4v1.LeadOnlyFix)})
}
