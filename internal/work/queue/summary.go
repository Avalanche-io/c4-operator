package queue

import (
	"context"
	"strconv"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A block ends its task with a summary of it (.c4/done), which its runner
// stores and names on the pod (AnnotationOutputSummary). The queue keeps
// the latest finished attempt's in the job's status, so a lead's c4 farm
// wait, c4_get_unit and the HUD read the worker's result once the pod is
// gone. It is the unit's own words, not something the farm checked.

// maxSummary bounds a summary in the job's status, in bytes: it is a
// model's text, and a job may have many units.
const maxSummary = 4 << 10

// noteSummary keeps a succeeded pod's summary, unless a later attempt's
// is already kept. It reports false when the summary could not be read
// yet (the store is unreachable), so the pod is kept and read next tick.
func (q *Queue) noteSummary(ctx context.Context, v *view, idx int, p *corev1.Pod) bool {
	id := p.Annotations[c4v1.AnnotationOutputSummary]
	if id == "" {
		return true
	}
	m := v.mem
	attempt := int32(attemptOf(p))
	if cur, ok := m.summaries[idx]; ok && (cur.Attempt > attempt || cur.Doc == id) {
		return true
	}
	b, err := q.expansionDoc(ctx, v.project, m, id)
	if err != nil {
		q.Log.Error(err, "summary", "job", v.key, "pod", p.Name)
		return false
	}
	if m.summaries == nil {
		m.summaries = map[int]c4v1.WorkUnitSummary{}
	}
	m.summaries[idx] = c4v1.WorkUnitSummary{Text: cutText(b, maxSummary), Doc: id, Attempt: attempt}
	return true
}

// cutText is b as text of at most n bytes, cut at a character boundary.
func cutText(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
		for len(b) > 0 && !utf8.Valid(b) {
			b = b[:len(b)-1]
		}
	}
	if !utf8.Valid(b) {
		return ""
	}
	return string(b)
}

func (m *jobMem) loadSummaries(st *c4v1.WorkJobStatus) {
	for k, s := range st.UnitSummaries {
		if i, err := strconv.Atoi(k); err == nil {
			if m.summaries == nil {
				m.summaries = map[int]c4v1.WorkUnitSummary{}
			}
			m.summaries[i] = s
		}
	}
}

func (m *jobMem) summaryStatus() map[string]c4v1.WorkUnitSummary {
	if len(m.summaries) == 0 {
		return nil
	}
	out := make(map[string]c4v1.WorkUnitSummary, len(m.summaries))
	for i, s := range m.summaries {
		out[strconv.Itoa(i)] = s
	}
	return out
}
