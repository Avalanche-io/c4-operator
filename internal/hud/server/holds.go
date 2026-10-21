package server

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/schema"
)

// heldBy is why j's units are held back, from the queue's conditions:
// its grant (Grant), its tool (Tools), and each job one of its units
// submitted that is held itself, so the parent says what it waits for.
func heldBy(m *model, j *c4v1.WorkJob) []api.Hold {
	var out []api.Hold
	for _, kind := range []string{c4v1.ConditionGrant, "Tools"} {
		if c := meta.FindStatusCondition(j.Status.Conditions, kind); c != nil && c.Status == metav1.ConditionFalse {
			out = append(out, api.Hold{Kind: strings.ToLower(kind), Message: c.Message, Since: c.LastTransitionTime.Time.UTC()})
		}
	}
	if m == nil {
		return out
	}
	// a unit's job names it as <ns>/<job>@<uid>/<unit>
	prefix := strings.TrimSuffix(schema.UnitRef(j.Namespace, j.Name, string(j.UID), ""), "/") + "/"
	for _, r := range m.jobs {
		by := r.job.Annotations[c4v1.AnnotationSubmittedByUnit]
		if by == "" || !strings.HasPrefix(by, prefix) {
			continue
		}
		c := meta.FindStatusCondition(r.job.Status.Conditions, c4v1.ConditionGrant)
		if c == nil || c.Status != metav1.ConditionFalse {
			continue
		}
		out = append(out, api.Hold{Kind: "child", Child: r.job.Name, Since: c.LastTransitionTime.Time.UTC(),
			Message: fmt.Sprintf("units wait for child job %s: %s", r.job.Name, c.Message)})
	}
	return out
}
