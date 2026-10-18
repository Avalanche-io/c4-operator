package queue

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/forecast"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Two projects run a job each to success; their calibrations are saved
// each in its own store, and a fresh queue on the same cluster restores
// them with the same counts and quantiles.
func TestCalibrationsSurviveRestart(t *testing.T) {
	ns, q, now := farm(t, "cpu", 2, 2)
	ctx := context.Background()
	nsB := ns + "-b"
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsB}}); err != nil {
		t.Fatal(err)
	}
	spec := c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: "file:///tmp/none"}}}
	prA := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "cal-a-" + ns}, Spec: c4v1.ProjectSpec{Namespaces: []string{ns}, Store: spec}}
	prB := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "cal-b-" + ns}, Spec: c4v1.ProjectSpec{Namespaces: []string{nsB}, Store: spec}}
	for _, p := range []*c4v1.Project{prA, prB} {
		if err := k8s.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { k8s.Delete(ctx, prA); k8s.Delete(ctx, prB) })
	A, B := prA.Name, prB.Name
	stores := map[string]*storeurl.Sharded{}
	for _, p := range []string{A, B} {
		st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
		if err != nil {
			t.Fatal(err)
		}
		stores[p] = st
	}
	open := func(_ context.Context, p string) (*storeurl.Sharded, error) { return stores[p], nil }
	q.Namespace, q.Stores = ns, open

	jobs := map[string]string{ns: "sh020", nsB: "sh030"}
	for n, name := range jobs {
		j := job(n, name, "render", "cpu-"+ns, map[string]string{"frames": "1001-1008", "estimateSeconds": "60"}, now.Add(time.Hour))
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	done := func() bool {
		for n, name := range jobs {
			if get(t, n, name).Status.Phase != "Succeeded" {
				return false
			}
		}
		return true
	}
	for i := 0; i < 30 && !done(); i++ {
		tick(t, q)
		*now = now.Add(60 * time.Second)
		for n, name := range jobs {
			for _, p := range unitPods(t, n, name) {
				if p.Status.Phase == "" || p.Status.Phase == corev1.PodPending {
					finish(t, &p, corev1.PodSucceeded, *now, 60*time.Second, nil)
				}
			}
		}
	}
	tick(t, q)
	if !done() {
		t.Fatal("jobs did not finish")
	}
	*now = now.Add(calSaveEvery)
	tick(t, q)
	if len(q.calp.dirty) != 0 {
		t.Fatalf("not saved: %v", q.calp.dirty)
	}

	// Each project's object is in its own store only.
	var cm corev1.ConfigMap
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: calConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	for p, other := range map[string]string{A: B, B: A} {
		id, err := c4.Parse(cm.Data[p])
		if err != nil {
			t.Fatalf("pointer of %s: %v", p, err)
		}
		if _, err := stores[other].Head(ctx, id); !errors.Is(err, storeurl.ErrNotFound) {
			t.Fatalf("%s's calibrations are in %s's store (%v)", p, other, err)
		}
		data, err := c4view.ReadObject(stores[p], id)
		if err != nil {
			t.Fatal(err)
		}
		var rec calRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Project != p || rec.Classes["render"].Work == nil || rec.Classes["render"].Finish == nil {
			t.Fatalf("%s's object: project %s, classes %v", p, rec.Project, rec.Classes)
		}
	}

	fresh := func(open StoreFunc) *Queue {
		return &Queue{Client: k8s, Policy: sched.Decide, Cfg: sched.DefaultConfig(), Log: zap.New(zap.WriteTo(os.Stderr)),
			Now: func() time.Time { return *now }, CaptureWait: time.Minute, Namespace: ns, Stores: open}
	}
	q2 := fresh(open)
	tick(t, q2)
	for _, p := range []string{A, B} {
		sameCalibrations(t, p, q, q2)
	}

	// A store that cannot be reached holds the project back: it is not
	// saved over, and restored once the store answers.
	down := true
	q3 := fresh(func(ctx context.Context, p string) (*storeurl.Sharded, error) {
		if p == A && down {
			return nil, errors.New("unreachable")
		}
		return open(ctx, p)
	})
	tick(t, q3)
	if _, ok := q3.calp.pending[A]; !ok || len(q3.cal.own[[2]string{A, "render"}].Snapshot().Strata) != 0 {
		t.Fatal("a project whose store was unreachable was not held for a retry")
	}
	q3.calp.touch(A)
	*now = now.Add(time.Second)
	tick(t, q3)
	var after corev1.ConfigMap
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: calConfigMap}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Data[A] != cm.Data[A] {
		t.Fatal("a project was saved before its history was read back")
	}
	down = false
	*now = now.Add(calSaveEvery)
	tick(t, q3)
	if len(q3.calp.pending) != 0 {
		t.Fatalf("still pending: %v", q3.calp.pending)
	}
	sameCalibrations(t, A, q, q3)

	// A pointer to an object its store does not hold starts the project
	// empty and does not stop the queue.
	patch := client.MergeFrom(cm.DeepCopy())
	cm.Data[B] = cm.Data[A]
	if err := k8s.Patch(ctx, &cm, patch); err != nil {
		t.Fatal(err)
	}
	q4 := fresh(open)
	tick(t, q4)
	if len(q4.cal.own[[2]string{B, "render"}].Snapshot().Strata) != 0 || len(q4.calp.pending) != 0 {
		t.Fatal("a missing object was restored or held")
	}
	sameCalibrations(t, A, q, q4)
}

// sameCalibrations checks that b holds a's calibrations of project p:
// the same scores, counts and quantiles.
func sameCalibrations(t *testing.T, p string, a, b *Queue) {
	t.Helper()
	k := [2]string{p, "render"}
	wa, wb := a.cal.own[k], b.cal.own[k]
	fa, fb := a.fcal.own[k], b.fcal.own[k]
	if wa == nil || wb == nil || fa == nil || fb == nil {
		t.Fatalf("%s: calibrations missing", p)
	}
	sa := wa.Snapshot()
	if len(sa.Strata) == 0 || !reflect.DeepEqual(sa, wb.Snapshot()) {
		t.Fatalf("%s: remaining-work scores differ after the restart", p)
	}
	ma, mb := wa.Min, wb.Min
	wa.Min, wb.Min = 1, 1
	defer func() { wa.Min, wb.Min = ma, mb }()
	for _, s := range sa.Strata {
		st := s.Stratum()
		if wa.Count(st) != wb.Count(st) {
			t.Fatalf("%s %v: count %d, restored %d", p, st, wa.Count(st), wb.Count(st))
		}
		for _, x := range []float64{0.1, 0.5, 0.9, 0.95} {
			qa, _ := wa.Quantile(st, x)
			qb, _ := wb.Quantile(st, x)
			if qa != qb {
				t.Fatalf("%s %v q%.2f: %v, restored %v", p, st, x, qa, qb)
			}
		}
	}
	if !reflect.DeepEqual(fa.Snapshot(), fb.Snapshot()) {
		t.Fatalf("%s: finish scores differ after the restart", p)
	}
	any := forecast.FinishKey{Class: "render", Stage: forecast.AnyStage}
	if fa.Count(any) == 0 || fa.Count(any) != fb.Count(any) {
		t.Fatalf("%s: finish count %d, restored %d", p, fa.Count(any), fb.Count(any))
	}
	fma, fmb := fa.Min, fb.Min
	fa.Min, fb.Min = 1, 1
	defer func() { fa.Min, fb.Min = fma, fmb }()
	t0 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	band := forecast.Band{P50: t0.Add(time.Hour), P80: t0.Add(2 * time.Hour), P90: t0.Add(3 * time.Hour), P95: t0.Add(4 * time.Hour)}
	if fa.Apply(any, t0, band) != fb.Apply(any, t0, band) {
		t.Fatalf("%s: restored finish calibration moves bands differently", p)
	}
}

// The farm pools are rebuilt from the projects that share now: one that
// stopped sharing drops out, and none crowds out the others.
func TestFarmRebuiltFromSharingProjects(t *testing.T) {
	st := estimate.Stratum{Sampled: 1, Measured: -1, Open: -1}
	rec := func(p string, x float64, n int) *calRecord {
		v := make([]float64, n)
		for i := range v {
			v[i] = x
		}
		return &calRecord{Kind: calKind, Project: p, Classes: map[string]classCalibration{"render": {
			Work:   &estimate.CalibrationSnapshot{Strata: []estimate.StratumScores{{Sampled: st.Sampled, Measured: st.Measured, Open: st.Open, Scores: v}}},
			Finish: &forecast.FinishCalibrationSnapshot{Keys: []forecast.FinishScores{{Class: "render", Stage: forecast.AnyStage, Scores: v}}},
		}}}
	}
	q := &Queue{shareHist: map[string]bool{"a": true, "b": true, "c": true}}
	for _, r := range []*calRecord{rec("a", 1, 400), rec("b", 2, 400), rec("c", 3, 400), rec("d", 1000, 400)} {
		q.restoreProject(r)
	}
	q.rebuildFarm()
	if !reflect.DeepEqual(q.cal.farmBy["render"], map[string]bool{"a": true, "b": true, "c": true}) ||
		!reflect.DeepEqual(q.fcal.farmBy["render"], q.cal.farmBy["render"]) {
		t.Fatalf("contributors %v %v", q.cal.farmBy, q.fcal.farmBy)
	}
	farm := q.cal.forJob("a", "render", true).Fallback
	if farm == nil || farm.Count(st) != calCapacity {
		t.Fatal("farm pool not rebuilt to its capacity")
	}
	n := map[float64]int{}
	for _, x := range farm.Snapshot().Strata[0].Scores {
		n[x]++
	}
	if n[1000] != 0 || n[1] < 166 || n[2] < 166 || n[3] < 166 {
		t.Fatalf("farm pool by project: %v", n)
	}
	if q.cal.forJob("d", "render", false).Fallback != nil || q.cal.own[[2]string{"d", "render"}].Count(st) != 400 {
		t.Fatal("a project that does not share lost its own scores or reads the farm")
	}
	q.shareHist["c"] = false
	q.rebuildFarm()
	if q.cal.forJob("a", "render", true).Fallback != nil {
		t.Fatal("farm pool consulted with two contributing projects")
	}
	if len(q.fcal.farmBy["render"]) != 2 {
		t.Fatal("finish farm pool consulted with two contributing projects")
	}
}
