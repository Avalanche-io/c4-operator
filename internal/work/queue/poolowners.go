package queue

import (
	"fmt"
	"slices"
	"strings"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// servingPools keeps those of names whose pool runs j's owner's work
// (Pool spec.owners; a pool with none runs anyone's). When none does, why
// says whose work they run, for the job's Schedulable condition.
func (q *Queue) servingPools(j *c4v1.WorkJob, names []string) ([]string, string) {
	q.derivMu.Lock()
	defer q.derivMu.Unlock()
	owner := j.Annotations[c4v1.AnnotationOwner]
	var kept, refused []string
	for _, n := range names {
		o := q.poolOwners[n]
		if len(o) == 0 || owner != "" && slices.Contains(o, owner) {
			kept = append(kept, n)
			continue
		}
		refused = append(refused, fmt.Sprintf("pool %s runs only the jobs of %s", n, strings.Join(o, ", ")))
	}
	if len(kept) > 0 || len(refused) == 0 {
		return kept, ""
	}
	whose := "this job has no owner"
	if owner != "" {
		whose = "this job is " + owner + "'s"
	}
	return nil, strings.Join(refused, "; ") + "; " + whose
}
