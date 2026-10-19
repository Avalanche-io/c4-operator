package queue

// Rework history. A review's rework rate and what a rework costs are
// learned per project, class and unit key, across jobs, and saved with the
// project's calibrations, so the first job of a kind after others starts
// from what they did (expectedRework, verdict.go):
//
//   - the rate: of the units a review key judged, how many it sent back;
//   - the cost: a rerun's run time over the unit's first pass, per key (a
//     rework resumes from the unit's last output, as a yield does, so it
//     is usually much less than a pass).
//
// Before any history a review marked in its document (params.review) has
// reworkDefault, unless its reworkRate param (else the job's) says
// otherwise, and a rework costs reworkFracDefault of a pass.

const (
	reworkDefault     = 0.3
	reworkFracDefault = 0.5
	reworkFracShrink  = 2.0
)

// reworkStat is one review or reworked key's history in a project.
type reworkStat struct {
	// Reworked and Judged count a review key's verdicts: units sent back,
	// and units judged.
	Reworked float64 `json:"reworked,omitempty"`
	Judged   float64 `json:"judged,omitempty"`
	// Ratio sums, for a unit key, each rerun's run time over its unit's
	// first pass; Reruns counts them.
	Ratio  float64 `json:"ratio,omitempty"`
	Reruns float64 `json:"reruns,omitempty"`
}

func reworkKey(class, key string) string { return class + "/" + key }

func (q *Queue) reworkOf(project, class, key string) reworkStat {
	if s := q.rework[project][reworkKey(class, key)]; s != nil {
		return *s
	}
	return reworkStat{}
}

func (q *Queue) reworkEntry(project, class, key string) *reworkStat {
	if q.rework == nil {
		q.rework = map[string]map[string]*reworkStat{}
	}
	if q.rework[project] == nil {
		q.rework[project] = map[string]*reworkStat{}
	}
	k := reworkKey(class, key)
	s := q.rework[project][k]
	if s == nil {
		s = &reworkStat{}
		q.rework[project][k] = s
	}
	return s
}

// noteVerdict counts a review key's verdict: reworked of judged units.
func (q *Queue) noteVerdict(project, class, key string, reworked, judged int) {
	if judged <= 0 {
		return
	}
	s := q.reworkEntry(project, class, key)
	s.Reworked += float64(reworked)
	s.Judged += float64(judged)
	q.calp.touch(project)
}

// noteRerun keeps a rerun's run time as a share of its unit's first pass.
func (q *Queue) noteRerun(project, class, key string, ratio float64) {
	if ratio <= 0 {
		return
	}
	s := q.reworkEntry(project, class, key)
	s.Ratio += ratio
	s.Reruns++
	q.calp.touch(project)
}

// reworkFrac is what a rerun of key costs as a share of a first pass:
// its history shrunk toward reworkFracDefault, between a tenth and once
// and a half.
func (q *Queue) reworkFrac(project, class, key string) float64 {
	s := q.reworkOf(project, class, key)
	f := (s.Ratio + reworkFracShrink*reworkFracDefault) / (s.Reruns + reworkFracShrink)
	return min(max(f, 0.1), 1.5)
}

// restoreRework adds a project's saved history to what was learned since
// the start.
func (q *Queue) restoreRework(project string, saved map[string]reworkStat) {
	for k, s := range saved {
		if q.rework == nil {
			q.rework = map[string]map[string]*reworkStat{}
		}
		if q.rework[project] == nil {
			q.rework[project] = map[string]*reworkStat{}
		}
		cur := q.rework[project][k]
		if cur == nil {
			cur = &reworkStat{}
			q.rework[project][k] = cur
		}
		cur.Reworked += s.Reworked
		cur.Judged += s.Judged
		cur.Ratio += s.Ratio
		cur.Reruns += s.Reruns
	}
}

// savedRework is a project's history as saved.
func (q *Queue) savedRework(project string) map[string]reworkStat {
	if len(q.rework[project]) == 0 {
		return nil
	}
	out := map[string]reworkStat{}
	for k, s := range q.rework[project] {
		out[k] = *s
	}
	return out
}
