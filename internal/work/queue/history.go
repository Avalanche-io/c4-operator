package queue

import (
	"time"

	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/work/estimate"
)

// Shot history (proposal eta-deadlines section 1.3). The best prior for a
// render is the previous version of the same shot: in the stage 1
// evidence its per-frame durations put the estimate at submission within
// 12% of the truth, against 65% for farm-wide history. The queue keeps,
// per project, shot, class and unit key (render pass), the durations of
// the most recently finished job of that shot by axis value (frame), and
// gives them to the next job of the shot as a profile
// (estimate.SetProfile). The profiles come from WorkJobs the queue
// already sees, so they survive a restart without storage of their own;
// a deleted job's profile lasts until the queue restarts. History never
// crosses projects.

// maxProfiles bounds the profiles kept; the oldest go first.
const maxProfiles = 10000

type profileKey struct{ project, shot, class, key string }

type shotProfile struct {
	finished time.Time
	uid      types.UID
	byAxis   map[int]float64 // seconds by axis value (frame)
}

// noteProfiles records the profiles of a job that has succeeded, if it
// is the latest of its shot.
func (q *Queue) noteProfiles(v *view) {
	m := v.mem
	shot := v.job.Labels[c4v1.LabelShot]
	if m.profiled || shot == "" || m.plan == nil || !isFinished(v) || len(m.succeeded) < len(m.plan.Units) {
		return
	}
	m.profiled = true
	finished := m.finished
	if finished.IsZero() && v.job.Status.Finished != nil {
		finished = v.job.Status.Finished.Time
	}
	by := map[string]map[int]float64{}
	for idx, ms := range m.millis {
		if idx < 0 || idx >= len(m.plan.Units) || ms <= 0 {
			continue
		}
		u := m.plan.Units[idx]
		if by[u.Key] == nil {
			by[u.Key] = map[int]float64{}
		}
		by[u.Key][m.plan.Axis.Start+u.Pos] = float64(ms) / 1000
	}
	if q.profiles == nil {
		q.profiles = map[profileKey]*shotProfile{}
	}
	for key, byAxis := range by {
		k := profileKey{v.project, shot, v.job.Spec.Class, key}
		if p := q.profiles[k]; p != nil && (p.uid == v.job.UID || p.finished.After(finished)) {
			continue
		}
		q.profiles[k] = &shotProfile{finished: finished, uid: v.job.UID, byAxis: byAxis}
	}
	for len(q.profiles) > maxProfiles {
		var oldest profileKey
		var at time.Time
		first := true
		for k, p := range q.profiles {
			if first || p.finished.Before(at) {
				oldest, at, first = k, p.finished, false
			}
		}
		delete(q.profiles, oldest)
	}
}

// useProfiles gives a job's estimator the profiles of the previous job of
// its shot.
func (q *Queue) useProfiles(v *view, e *estimate.Estimator) {
	shot := v.job.Labels[c4v1.LabelShot]
	if shot == "" || len(q.profiles) == 0 {
		return
	}
	plan := v.mem.plan
	byKey := map[string]map[int]float64{}
	for _, u := range plan.Units {
		p := q.profiles[profileKey{v.project, shot, v.job.Spec.Class, u.Key}]
		if p == nil || p.uid == v.job.UID {
			continue
		}
		d, ok := p.byAxis[plan.Axis.Start+u.Pos]
		if !ok {
			continue
		}
		if byKey[u.Key] == nil {
			byKey[u.Key] = map[int]float64{}
		}
		byKey[u.Key][u.Pos] = d
	}
	for key, byPos := range byKey {
		e.SetProfile(key, byPos)
	}
}
