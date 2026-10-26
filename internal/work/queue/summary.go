package queue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/Avalanche-io/c4"
	corev1 "k8s.io/api/core/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// A block ends its task with a summary of it (.c4/done), which its runner
// stores and names on the pod (AnnotationOutputSummary). The queue keeps
// the latest finished attempt's in the job's status, so a lead's c4 farm
// wait, c4_get_unit and the HUD find the worker's result once the pod is
// gone. It is the unit's own words, not something the farm checked.
//
// A WorkJob is one object, and a plan or a lead's job can have hundreds of
// units, so status keeps only an excerpt (its first line, cut short) and
// the stored document's ID; the HUD reads the whole text from the store.

// maxExcerpt bounds a summary's excerpt in the job's status, in bytes.
const maxExcerpt = 200

// A job's status keeps excerpts for at most excerptBudget bytes in all,
// and summaries for at most maxSummaries units, the newest units (the
// highest indices: a lead's latest workers) first. Past the budget a unit
// keeps its document's ID, which readers fetch; past the count, none.
const (
	excerptBudget = 64 << 10
	maxSummaries  = 2000
)

// maxSummaryDoc bounds what the queue reads of a stored summary; the
// runners store at most 4 KiB.
const maxSummaryDoc = 64 << 10

// noteSummary keeps a succeeded pod's summary, unless a later attempt's
// is already kept. It reports false only when the store could not be
// reached, so the pod is kept and read next tick; a summary that cannot be
// read for good (a bad ID, a document the store lacks, one that does not
// match its ID) is kept with why, and the unit goes on.
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
	s := c4v1.WorkUnitSummary{Doc: id, Attempt: attempt}
	b, err := q.readSummary(ctx, v.project, id)
	var gone *summaryGone
	switch {
	case errors.As(err, &gone):
		s.Error = gone.why
	case err != nil:
		q.Log.Error(err, "summary", "job", v.key, "pod", p.Name)
		return false
	default:
		s.Excerpt = excerpt(b, maxExcerpt)
	}
	if m.summaries == nil {
		m.summaries = map[int]c4v1.WorkUnitSummary{}
	}
	m.summaries[idx] = s
	return true
}

// summaryGone is a summary that cannot be read, now or later.
type summaryGone struct{ why string }

func (g *summaryGone) Error() string { return g.why }

// readSummary reads a stored summary, past the job's document cache: it is
// read once, and a job may have many.
func (q *Queue) readSummary(ctx context.Context, project, id string) ([]byte, error) {
	cid, err := c4.Parse(id)
	if err != nil {
		return nil, &summaryGone{fmt.Sprintf("its summary's ID %q is not a C4 ID", id)}
	}
	if q.Stores == nil {
		return nil, &summaryGone{"the farm has no store to read its summary from"}
	}
	st, err := q.Stores(ctx, project)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, &summaryGone{fmt.Sprintf("project %s has no store to read its summary from", project)}
	}
	r, err := st.Open(cid)
	if errors.Is(err, storeurl.ErrNotFound) {
		return nil, &summaryGone{"the store does not have its summary " + id}
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, maxSummaryDoc+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSummaryDoc || c4.Identify(bytes.NewReader(b)) != cid {
		return nil, &summaryGone{"its stored summary does not match its ID " + id}
	}
	return b, nil
}

// excerpt is the first line of b, at most n bytes, cut at a character.
func excerpt(b []byte, n int) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	b = bytes.TrimSpace(b)
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
	idx := make([]int, 0, len(m.summaries))
	for i := range m.summaries {
		idx = append(idx, i)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(idx)))
	if len(idx) > maxSummaries {
		idx = idx[:maxSummaries]
	}
	out := make(map[string]c4v1.WorkUnitSummary, len(idx))
	left := excerptBudget
	for _, i := range idx {
		s := m.summaries[i]
		if len(s.Excerpt) > left {
			s.Excerpt = "" // its document has it
		}
		left -= len(s.Excerpt)
		out[strconv.Itoa(i)] = s
	}
	return out
}
