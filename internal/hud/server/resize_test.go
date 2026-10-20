package server

import (
	"context"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A forecast is resized as its bid changes: the fields sent replace its
// own, the rest stay; the same change again writes nothing; a real job is
// not resized, and a size that is not a number is refused before it is
// signed.
func TestResizeForecast(t *testing.T) {
	s := submitFarm(t)
	ctx := context.Background()
	m := s.current()
	for _, j := range []*c4v1.WorkJob{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "sh050-light-forecast"},
			Spec: c4v1.WorkJobSpec{Class: "render", Forecast: &c4v1.WorkForecast{SlotHours: "30", UnitHours: "1", Exists: "0.8"}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "sh050-light-v001"}, Spec: c4v1.WorkJobSpec{Class: "render"}},
	} {
		if err := s.Client.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		rec := &jobRec{job: *j, key: j.Namespace + "/" + j.Name, project: "show-a"}
		m.jobs = append(m.jobs, rec)
		m.byKey[rec.key] = rec
	}
	str := func(s string) *string { return &s }
	var out struct {
		OK      bool    `json:"ok"`
		Message string  `json:"message"`
		Job     api.Job `json:"job"`
	}
	path := "/api/v1/jobs/show-a/sh050-light-forecast/forecast?as=artist"
	if code := call(t, s, http.MethodPost, path, api.ForecastChange{SlotHours: str("45")}, &out); code != http.StatusOK {
		t.Fatalf("resize: %d %+v", code, out)
	}
	var live c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "sh050-light-forecast"}, &live); err != nil {
		t.Fatal(err)
	}
	if f := live.Spec.Forecast; f.SlotHours != "45" || f.UnitHours != "1" || f.Exists != "0.8" {
		t.Fatalf("slot-hours replaced, the rest kept: %+v", f)
	}
	if out.Job.Forecast == nil || out.Job.Forecast.SlotHours != "45" || out.Message != "sh050-light-forecast: forecast resized, slotHours 45" {
		t.Fatalf("answer: %+v", out)
	}
	rv := live.ResourceVersion
	if code := call(t, s, http.MethodPost, path, api.ForecastChange{SlotHours: str("45"), UnitHours: str("1")}, &out); code != http.StatusOK ||
		out.Message != "sh050-light-forecast: forecast unchanged" {
		t.Fatalf("the same again: %d %+v", code, out)
	}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "sh050-light-forecast"}, &live); err != nil || live.ResourceVersion != rv {
		t.Fatalf("an unchanged forecast was written: %v %s", err, live.ResourceVersion)
	}
	if code := call(t, s, http.MethodPost, path, api.ForecastChange{UnitHours: str("")}, &out); code != http.StatusOK {
		t.Fatalf("clear: %d", code)
	}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "sh050-light-forecast"}, &live); err != nil || live.Spec.Forecast.UnitHours != "" {
		t.Fatalf("unit hours cleared: %+v", live.Spec.Forecast)
	}
	var e struct{ Error string }
	if code := call(t, s, http.MethodPost, path, api.ForecastChange{SlotHours: str("4O")}, &e); code != http.StatusBadRequest {
		t.Fatalf("4O: %d %s", code, e.Error)
	}
	if code := call(t, s, http.MethodPost, path, api.ForecastChange{Exists: str("1.5")}, &e); code != http.StatusBadRequest {
		t.Fatalf("exists 1.5: %d %s", code, e.Error)
	}
	if code := call(t, s, http.MethodPost, "/api/v1/jobs/show-a/sh050-light-v001/forecast?as=artist", api.ForecastChange{SlotHours: str("45")}, &e); code != http.StatusConflict {
		t.Fatalf("a real job: %d %s", code, e.Error)
	}
}
