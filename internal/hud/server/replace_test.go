package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A forecast whose delete fails when the real job replaces it is deleted
// later, with backoff, until it is gone: the real job names it. A forecast
// made after the real job (the entity was a forecast again) is not.
func TestReplacedForecastRetried(t *testing.T) {
	s := submitFarm(t)
	s.DryRun = func(context.Context, *c4v1.WorkJob, string) (api.SubmitEstimate, error) {
		return api.SubmitEstimate{}, nil
	}
	failing, deletes := true, 0
	s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deletes++
			if failing {
				return errors.New("the API server is away")
			}
			return c.Delete(ctx, obj, opts...)
		}})
	ctx := context.Background()
	fc, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-100"},
		Forecast: &c4v1.WorkForecast{SlotHours: "40"}})
	real, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-100"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	var out api.SubmitResponse
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", api.SubmitRequest{Project: "show-a", Name: "sh090-forecast", Spec: fc}, &out); code != http.StatusCreated {
		t.Fatalf("forecast: %d %+v", code, out)
	}
	if code := call(t, s, http.MethodPost, "/api/v1/jobs?as=artist", api.SubmitRequest{Project: "show-a", Name: "sh090", Spec: real, Replaces: "sh090-forecast"}, &out); code != http.StatusCreated {
		t.Fatalf("the real job, though the forecast's delete failed: %d %+v", code, out)
	}
	get := func(name string) (*c4v1.WorkJob, error) {
		var j c4v1.WorkJob
		err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: name}, &j)
		return &j, err
	}
	r, err := get("sh090")
	if err != nil || r.Annotations[c4v1.AnnotationReplaces] != "sh090-forecast" {
		t.Fatalf("the real job names what it replaced: %v %v", err, r.Annotations)
	}
	f, err := get("sh090-forecast")
	if err != nil {
		t.Fatalf("the forecast, its delete failed: %v", err)
	}
	// The model the collect loop reads: the forecast made before the real
	// job.
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	f.CreationTimestamp, r.CreationTimestamp = metav1.NewTime(t0), metav1.NewTime(t0.Add(time.Hour))
	m := s.current()
	for _, j := range []*c4v1.WorkJob{f, r} {
		rec := &jobRec{job: *j, key: "show-a/" + j.Name, project: "show-a"}
		m.jobs = append(m.jobs, rec)
		m.byKey[rec.key] = rec
	}

	now := t0.Add(2 * time.Hour)
	deletes = 0
	s.sweepReplaced(ctx, now)
	s.sweepReplaced(ctx, now.Add(time.Second))
	if deletes != 1 || s.retire["show-a/sh090-forecast"].wait != retireFirst {
		t.Fatalf("one try, then a wait: %d deletes, %+v", deletes, s.retire)
	}
	s.sweepReplaced(ctx, now.Add(retireFirst))
	if deletes != 2 || s.retire["show-a/sh090-forecast"].wait != 2*retireFirst {
		t.Fatalf("the wait doubles: %d deletes, %+v", deletes, s.retire)
	}
	failing = false
	s.sweepReplaced(ctx, now.Add(3*retireFirst))
	if _, err := get("sh090-forecast"); !apierrors.IsNotFound(err) || len(s.retire) != 0 {
		t.Fatalf("the forecast is gone, and nothing waits: %v %+v", err, s.retire)
	}

	// The entity is a forecast again, made after the real job: it stays.
	again := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "sh090-forecast"},
		Spec: c4v1.WorkJobSpec{Class: "render", Forecast: &c4v1.WorkForecast{SlotHours: "40"}}}
	if err := s.Client.Create(ctx, again); err != nil {
		t.Fatal(err)
	}
	again.CreationTimestamp = metav1.NewTime(t0.Add(3 * time.Hour))
	m.byKey["show-a/sh090-forecast"].job = *again
	deletes = 0
	s.sweepReplaced(ctx, now.Add(time.Hour))
	if _, err := get("sh090-forecast"); err != nil || deletes != 0 {
		t.Fatalf("a later forecast was deleted: %v, %d deletes", err, deletes)
	}
}
