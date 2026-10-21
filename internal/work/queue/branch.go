package queue

import (
	"encoding/json"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A block on a host works on its own git branch of the developer's
// repository, and its commits are its output. The host runner records the
// branch on the pod at the end of each attempt (AnnotationOutputBranch);
// the queue keeps the latest attempt's in the job's status, beside the
// units' outputs, so the HUD and c4 farm get read it once the pod is gone.

// noteBranch keeps an ended pod's branch record, unless an earlier attempt
// wrote it than the one already kept.
func (m *jobMem) noteBranch(idx int, p *corev1.Pod) {
	raw := p.Annotations[c4v1.AnnotationOutputBranch]
	if raw == "" {
		return
	}
	var b c4v1.WorkUnitBranch
	if err := json.Unmarshal([]byte(raw), &b); err != nil || b.Branch == "" {
		return
	}
	b.Attempt = int32(attemptOf(p))
	if cur, ok := m.branches[idx]; ok && cur.Attempt > b.Attempt {
		return
	}
	if m.branches == nil {
		m.branches = map[int]c4v1.WorkUnitBranch{}
	}
	m.branches[idx] = b
}

func (m *jobMem) loadBranches(st *c4v1.WorkJobStatus) {
	for k, b := range st.UnitBranches {
		if i, err := strconv.Atoi(k); err == nil {
			if m.branches == nil {
				m.branches = map[int]c4v1.WorkUnitBranch{}
			}
			m.branches[i] = b
		}
	}
}

func (m *jobMem) branchStatus() map[string]c4v1.WorkUnitBranch {
	if len(m.branches) == 0 {
		return nil
	}
	out := make(map[string]c4v1.WorkUnitBranch, len(m.branches))
	for i, b := range m.branches {
		out[strconv.Itoa(i)] = b
	}
	return out
}
