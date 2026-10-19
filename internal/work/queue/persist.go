package queue

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/process"
)

// Calibrations survive a restart (calibrate.go). Every calSaveEvery, when
// a project has learned scores since its last save, the queue writes that
// project's own calibrations, remaining work and finish for every class,
// as one JSON object into the project's own store. A project's scores
// never go to another project's store. The ConfigMap calConfigMap in the
// operator's namespace maps each project to its latest object's C4 ID:
// the store is content-addressed, so it needs one small mutable pointer
// beside it, and the queue's own namespace is the one place the manager
// writes that only the operator reads.
//
// On the first tick, before any estimate, the queue reads the pointers and
// restores each project's calibrations. A missing or unreadable object is
// logged and the project starts empty; a store that cannot be reached is
// tried again every calSaveEvery, and the project is not saved until then,
// so a passing outage does not overwrite its history with less. The
// farm-wide pools are not stored: they are rebuilt from the restored
// calibrations of the projects that share now, so a project that has
// stopped sharing drops out of them.

const (
	// calSaveEvery spaces saves and retries.
	calSaveEvery = 5 * time.Minute
	// calConfigMap holds each project's pointer, keyed by project name.
	calConfigMap = "c4-work-calibrations"
	calKind      = "c4.avalanche.io/work-calibrations"
)

// calRecord is a project's stored calibrations.
type calRecord struct {
	Kind    string `json:"kind"`
	Version int    `json:"version"`
	Project string `json:"project"`
	// Saved is when the object was written.
	Saved   time.Time                   `json:"saved"`
	Classes map[string]classCalibration `json:"classes"`
	// Derivations is the project's record of derivations (derive.go), so
	// the verification it has earned survives a restart. Experimental.
	Derivations *process.BookSnapshot `json:"derivations,omitempty"`
	// Durations are the project's recent unit run times by class, in
	// seconds (classprior.go).
	Durations map[string][]float64 `json:"durations,omitempty"`
	// Rework is the project's review and rerun history by class and key
	// (rework.go).
	Rework map[string]reworkStat `json:"rework,omitempty"`
}

// classCalibration is one class's calibrations: of the remaining work
// (estimate.Calibration) and of the finish (forecast.FinishCalibration).
type classCalibration struct {
	Work   *estimate.CalibrationSnapshot       `json:"work,omitempty"`
	Finish *forecast.FinishCalibrationSnapshot `json:"finish,omitempty"`
}

// calPersist is the queue's persistence state.
type calPersist struct {
	read    bool              // the pointers have been read
	pending map[string]string // project: object not read yet
	dirty   map[string]bool   // projects with scores since their last save
	last    time.Time         // last save round
}

// touch notes that a project learned a score.
func (p *calPersist) touch(project string) {
	if p.dirty == nil {
		p.dirty = map[string]bool{}
	}
	p.dirty[project] = true
}

// persists reports whether calibrations are saved: it takes the
// operator's namespace and the project stores.
func (q *Queue) persists() bool { return q.Namespace != "" && q.Stores != nil }

// loadCalibrations reads the pointers once and restores every project's
// calibrations. If the ConfigMap cannot be read, it tries again next tick.
func (q *Queue) loadCalibrations(ctx context.Context) {
	p := &q.calp
	if p.read || !q.persists() {
		return
	}
	var cm corev1.ConfigMap
	err := q.Client.Get(ctx, client.ObjectKey{Namespace: q.Namespace, Name: calConfigMap}, &cm)
	if err != nil && !apierrors.IsNotFound(err) {
		q.Log.Error(err, "calibrations: reading pointers, will try again")
		return
	}
	p.read = true
	p.pending = maps.Clone(cm.Data)
	q.loadPending(ctx)
}

// errRetry marks a load that may succeed later.
var errRetry = errors.New("store not reachable")

// loadPending restores the projects whose object has not been read, and
// rebuilds the farm pools when any was.
func (q *Queue) loadPending(ctx context.Context) {
	p := &q.calp
	restored := false
	for _, project := range slices.Sorted(maps.Keys(p.pending)) {
		id := p.pending[project]
		rec, err := q.readCalibrations(ctx, project, id)
		if errors.Is(err, errRetry) {
			q.Log.Info("calibrations: store not reachable, will try again", "project", project, "error", err.Error())
			continue
		}
		delete(p.pending, project)
		if err != nil {
			q.Log.Error(err, "calibrations: starting empty", "project", project, "object", id)
			continue
		}
		q.restoreProject(rec)
		restored = true
	}
	if restored {
		q.rebuildFarm()
	}
}

// readCalibrations reads a project's object from the project's store.
func (q *Queue) readCalibrations(ctx context.Context, project, id string) (*calRecord, error) {
	cid, err := c4.Parse(id)
	if err != nil {
		return nil, err
	}
	st, err := q.Stores(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errRetry, err)
	}
	if st == nil {
		return nil, fmt.Errorf("%w: project has no store", errRetry)
	}
	data, err := c4view.ReadObject(st, cid)
	if err != nil {
		if errors.Is(err, storeurl.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", errRetry, err)
	}
	var rec calRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if rec.Kind != calKind || rec.Project != project {
		return nil, fmt.Errorf("object is %q of project %q", rec.Kind, rec.Project)
	}
	return &rec, nil
}

// restoreProject puts a project's stored scores behind any it has learned
// since the start.
func (q *Queue) restoreProject(rec *calRecord) {
	if q.cal.own == nil {
		q.cal.own = map[[2]string]*estimate.Calibration{}
	}
	q.fcal.init()
	if rec.Derivations != nil && q.book(rec.Project).Len() == 0 {
		q.setBook(rec.Project, process.RestoreBook(*rec.Derivations))
	}
	q.restoreRework(rec.Project, rec.Rework)
	for class, ds := range rec.Durations {
		// The stored window first, then what ran since the start.
		if q.durations == nil {
			q.durations = map[string]classDurations{}
		}
		if q.durations[rec.Project] == nil {
			q.durations[rec.Project] = classDurations{}
		}
		w := append(append([]float64(nil), ds...), q.durations[rec.Project][class]...)
		if len(w) > classDurKept {
			w = w[len(w)-classDurKept:]
		}
		q.durations[rec.Project][class] = w
	}
	for class, cc := range rec.Classes {
		k := [2]string{rec.Project, class}
		if cc.Work != nil {
			s := *cc.Work
			s.Strata = append(s.Strata, q.cal.own[k].Snapshot().Strata...)
			q.cal.own[k] = estimate.RestoreCalibration(s, calCapacity)
		}
		if cc.Finish != nil {
			s := *cc.Finish
			s.Keys = append(s.Keys, q.fcal.own[k].Snapshot().Keys...)
			q.fcal.own[k] = forecast.RestoreFinishCalibration(s, calCapacity)
		}
	}
}

// rebuildFarm makes the farm pools anew from the own calibrations of the
// projects that share now, taking each stratum's newest scores from every
// project in turn.
func (q *Queue) rebuildFarm() {
	work := map[string]map[estimate.Stratum][][]float64{}
	workBy := map[string]map[string]bool{}
	for _, k := range slices.SortedFunc(maps.Keys(q.cal.own), compareKeys) {
		if !q.shareHist[k[0]] {
			continue
		}
		for _, s := range q.cal.own[k].Snapshot().Strata {
			addGroup(work, workBy, k, s.Stratum(), s.Scores)
		}
	}
	fin := map[string]map[forecast.FinishKey][][]float64{}
	finBy := map[string]map[string]bool{}
	for _, k := range slices.SortedFunc(maps.Keys(q.fcal.own), compareKeys) {
		if !q.shareHist[k[0]] {
			continue
		}
		for _, s := range q.fcal.own[k].Snapshot().Keys {
			addGroup(fin, finBy, k, s.Key(), s.Scores)
		}
	}
	q.cal.farm, q.cal.farmBy = map[string]*estimate.Calibration{}, workBy
	for class, g := range work {
		var s estimate.CalibrationSnapshot
		for st, v := range newest(g, calCapacity) {
			s.Strata = append(s.Strata, estimate.StratumScores{Sampled: st.Sampled, Measured: st.Measured, Open: st.Open, Scores: v})
		}
		q.cal.farm[class] = estimate.RestoreCalibration(s, calCapacity)
	}
	q.fcal.farm, q.fcal.farmBy = map[string]*forecast.FinishCalibration{}, finBy
	for class, g := range fin {
		var s forecast.FinishCalibrationSnapshot
		for k, v := range newest(g, calCapacity) {
			s.Keys = append(s.Keys, forecast.FinishScores{Class: k.Class, Stage: k.Stage, Scores: v})
		}
		q.fcal.farm[class] = forecast.RestoreFinishCalibration(s, calCapacity)
	}
}

func compareKeys(a, b [2]string) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) }

// addGroup files one project's scores for a key under its class.
func addGroup[K comparable](g map[string]map[K][][]float64, by map[string]map[string]bool, pc [2]string, k K, v []float64) {
	if len(v) == 0 {
		return
	}
	if g[pc[1]] == nil {
		g[pc[1]] = map[K][][]float64{}
		by[pc[1]] = map[string]bool{}
	}
	g[pc[1]][k] = append(g[pc[1]][k], v)
	by[pc[1]][pc[0]] = true
}

// newest merges each key's lists (oldest first) into at most capacity
// scores, oldest first, taking the newest of every list in turn so that
// no project crowds out the others.
func newest[K comparable](g map[K][][]float64, capacity int) map[K][]float64 {
	out := make(map[K][]float64, len(g))
	for k, lists := range g {
		var rev []float64
		for i := 1; capacity <= 0 || len(rev) < capacity; i++ {
			took := false
			for _, l := range lists {
				if i <= len(l) && (capacity <= 0 || len(rev) < capacity) {
					rev = append(rev, l[len(l)-i])
					took = true
				}
			}
			if !took {
				break
			}
		}
		slices.Reverse(rev)
		out[k] = rev
	}
	return out
}

// saveCalibrations, every calSaveEvery, retries the loads that could not
// reach their store and saves the projects that learned scores.
func (q *Queue) saveCalibrations(ctx context.Context, now time.Time) {
	p := &q.calp
	if !q.persists() || !p.read || (!p.last.IsZero() && now.Sub(p.last) < calSaveEvery) {
		return
	}
	if len(p.pending) == 0 && len(p.dirty) == 0 {
		return
	}
	p.last = now
	if len(p.pending) > 0 {
		q.loadPending(ctx)
	}
	ptrs := map[string]string{}
	for _, project := range slices.Sorted(maps.Keys(p.dirty)) {
		if _, ok := p.pending[project]; ok {
			continue // its stored history has not been read back yet
		}
		if len(validation.IsConfigMapKey(project)) > 0 {
			delete(p.dirty, project) // a namespace without a Project has no store
			continue
		}
		id, err := q.writeCalibrations(ctx, project, now)
		if err != nil {
			q.Log.Error(err, "calibrations: saving", "project", project)
			continue
		}
		if id == "" {
			delete(p.dirty, project)
			continue
		}
		ptrs[project] = id
	}
	if len(ptrs) == 0 {
		return
	}
	if err := q.setPointers(ctx, ptrs); err != nil {
		q.Log.Error(err, "calibrations: writing pointers")
		return
	}
	for project := range ptrs {
		delete(p.dirty, project)
	}
}

// writeCalibrations stores a project's own calibrations in its store and
// returns the object's ID, or "" when the project has no store.
func (q *Queue) writeCalibrations(ctx context.Context, project string, now time.Time) (string, error) {
	st, err := q.Stores(ctx, project)
	if err != nil || st == nil {
		return "", err
	}
	b, err := json.Marshal(q.calRecordOf(project, now))
	if err != nil {
		return "", err
	}
	id, err := st.PutBytes(ctx, b)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// calRecordOf collects a project's own calibrations, every class.
func (q *Queue) calRecordOf(project string, now time.Time) calRecord {
	rec := calRecord{Kind: calKind, Version: 1, Project: project, Saved: now.UTC(), Classes: map[string]classCalibration{}}
	for k, c := range q.cal.own {
		if k[0] != project {
			continue
		}
		if s := c.Snapshot(); len(s.Strata) > 0 {
			cc := rec.Classes[k[1]]
			cc.Work = &s
			rec.Classes[k[1]] = cc
		}
	}
	for k, c := range q.fcal.own {
		if k[0] != project {
			continue
		}
		if s := c.Snapshot(); len(s.Keys) > 0 {
			cc := rec.Classes[k[1]]
			cc.Finish = &s
			rec.Classes[k[1]] = cc
		}
	}
	if b := q.bookOf(project); b != nil && b.Len() > 0 {
		s := b.Snapshot()
		rec.Derivations = &s
	}
	rec.Rework = q.savedRework(project)
	if d := q.durations[project]; len(d) > 0 {
		rec.Durations = map[string][]float64{}
		for class, w := range d {
			rec.Durations[class] = append([]float64(nil), w...)
		}
	}
	return rec
}

// setPointers points each project at its latest object.
func (q *Queue) setPointers(ctx context.Context, ptrs map[string]string) error {
	patch, err := json.Marshal(map[string]any{"data": ptrs})
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: q.Namespace, Name: calConfigMap}}
	err = q.Client.Patch(ctx, cm, client.RawPatch(types.MergePatchType, patch))
	if !apierrors.IsNotFound(err) {
		return err
	}
	cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: q.Namespace, Name: calConfigMap,
		Labels: map[string]string{"app.kubernetes.io/name": "c4-operator", "app.kubernetes.io/component": "queue"}},
		Data: ptrs}
	return q.Client.Create(ctx, cm)
}
