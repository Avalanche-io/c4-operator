package queue

import (
	"context"
	"slices"
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Milestones (DESIGN 10.7). A job with no deadline of its own takes the
// earliest due of the Milestones in its namespace that select it: its
// deadline source is "milestone", and the farm does not suggest one over
// it (whoever owns the Milestone owns the date). Each tick the queue
// writes, on every Milestone, the jobs it covers, how many are done, and
// the worst risk among the rest (a failed job counts as willMiss), from
// what the jobs' status last said.

// milestoneJobsKept caps the names a Milestone's status lists.
const milestoneJobsKept = 200

// listMilestones returns the Milestones by namespace; none when the
// cluster has no Milestone kind yet.
func (q *Queue) listMilestones(ctx context.Context) map[string][]*c4v1.Milestone {
	var l c4v1.MilestoneList
	if err := q.Client.List(ctx, &l); err != nil {
		if !meta.IsNoMatchError(err) {
			q.Log.Error(err, "milestones")
		}
		return nil
	}
	out := map[string][]*c4v1.Milestone{}
	for i := range l.Items {
		m := &l.Items[i]
		out[m.Namespace] = append(out[m.Namespace], m)
	}
	return out
}

// selects reports whether Milestone m covers job j; an empty selector
// covers none.
func selects(m *c4v1.Milestone, j *c4v1.WorkJob) bool {
	if len(m.Spec.Selector.MatchLabels) == 0 && len(m.Spec.Selector.MatchExpressions) == 0 {
		return false
	}
	sel, err := metav1.LabelSelectorAsSelector(&m.Spec.Selector)
	return err == nil && sel.Matches(labels.Set(j.Labels))
}

// milestoneFor is the Milestone a job's deadline comes from: the earliest
// due of those that select it (by name on a tie), nil for a job with a
// deadline of its own or none selecting it.
func milestoneFor(ms []*c4v1.Milestone, j *c4v1.WorkJob) *c4v1.Milestone {
	if j.Spec.Deadline != nil {
		return nil
	}
	var best *c4v1.Milestone
	for _, m := range ms {
		if !selects(m, j) {
			continue
		}
		if best == nil || m.Spec.Due.Before(&best.Spec.Due) || m.Spec.Due.Equal(&best.Spec.Due) && m.Name < best.Name {
			best = m
		}
	}
	return best
}

// riskRank orders WorkEstimate.Risk, worst last.
var riskRank = map[string]int{"": 0, "none": 0, "atRisk": 1, "willMiss": 2, "missed": 3}

// milestoneStatus writes each Milestone's covered jobs and their worst
// risk.
func (q *Queue) milestoneStatus(ctx context.Context, ms map[string][]*c4v1.Milestone, jobs []c4v1.WorkJob) {
	for _, list := range ms {
		for _, m := range list {
			st := c4v1.MilestoneStatus{ObservedGeneration: m.Generation, Risk: "none"}
			var names []string
			for i := range jobs {
				j := &jobs[i]
				if j.Namespace != m.Namespace || !selects(m, j) {
					continue
				}
				names = append(names, j.Name)
				switch j.Status.Phase {
				case "Succeeded":
					st.Done++
					continue
				case "Failed":
					// It will not finish until a person acts.
					if riskRank[st.Risk] < riskRank["willMiss"] {
						st.Risk = "willMiss"
					}
					continue
				}
				if e := j.Status.Estimate; e != nil && riskRank[e.Risk] > riskRank[st.Risk] {
					st.Risk = e.Risk
				}
			}
			sort.Strings(names)
			st.Count = int32(len(names))
			st.Jobs = names[:min(len(names), milestoneJobsKept)]
			if st.ObservedGeneration == m.Status.ObservedGeneration && st.Count == m.Status.Count && st.Done == m.Status.Done &&
				st.Risk == m.Status.Risk && slices.Equal(st.Jobs, m.Status.Jobs) {
				continue
			}
			m.Status = st
			if err := q.Client.Status().Update(ctx, m); err != nil {
				q.Log.Error(err, "milestone status", "milestone", m.Namespace+"/"+m.Name)
			}
		}
	}
}
