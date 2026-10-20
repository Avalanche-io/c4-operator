package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

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

// A forecast whose work is no longer expected is withdrawn: deleted,
// once; a real job is not.
func TestWithdrawForecast(t *testing.T) {
	s := submitFarm(t)
	ctx := context.Background()
	m := s.current()
	for _, j := range []*c4v1.WorkJob{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "sh060-light-forecast", Annotations: map[string]string{c4v1.AnnotationOwner: "maya"}},
			Spec: c4v1.WorkJobSpec{Class: "render", Forecast: &c4v1.WorkForecast{SlotHours: "30"}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "sh060-light-v001"}, Spec: c4v1.WorkJobSpec{Class: "render"}},
	} {
		if err := s.Client.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		rec := &jobRec{job: *j, key: j.Namespace + "/" + j.Name, project: "show-a"}
		m.jobs = append(m.jobs, rec)
		m.byKey[rec.key] = rec
	}
	var out struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	path := "/api/v1/jobs/show-a/sh060-light-forecast/withdraw?as=artist"
	body := map[string]string{"reason": "Task/4411 no longer matches any of the tracker's intake rules"}
	if code := call(t, s, http.MethodPost, "/api/v1/jobs/show-a/sh060-light-forecast/withdraw?as=wrangler", body, &out); code != http.StatusForbidden {
		t.Fatalf("someone else's forecast: %d %+v", code, out)
	}
	if code := call(t, s, http.MethodPost, path, body, &out); code != http.StatusOK ||
		out.Message != "sh060-light-forecast: forecast withdrawn, Task/4411 no longer matches any of the tracker's intake rules" {
		t.Fatalf("withdraw: %d %+v", code, out)
	}
	var live c4v1.WorkJob
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "sh060-light-forecast"}, &live); !apierrors.IsNotFound(err) {
		t.Fatalf("the forecast is still there: %v", err)
	}
	if code := call(t, s, http.MethodPost, path, body, &out); code != http.StatusNotFound {
		t.Fatalf("again: %d %+v", code, out)
	}
	if code := call(t, s, http.MethodPost, "/api/v1/jobs/show-a/sh060-light-v001/withdraw?as=artist", body, &out); code != http.StatusConflict {
		t.Fatalf("a real job: %d %+v", code, out)
	}
}

// Only one who could have made a forecast replaces or withdraws it: its
// owner, the sync for the tracker it came from, or the sync under a grant
// from its owner. Each replacement is signed and booked with its record.
func TestRetireOnlyByWhoCouldMakeIt(t *testing.T) {
	s := grantFarm(t)
	kube := kfake.NewSimpleClientset()
	n := 0
	kube.PrependReactor("create", "events", func(a ktesting.Action) (bool, runtime.Object, error) {
		if ev := a.(ktesting.CreateAction).GetObject().(*corev1.Event); ev.Name == "" {
			n++
			ev.Name = fmt.Sprintf("%s%d", ev.GenerateName, n)
		}
		return false, nil, nil
	})
	s.Kube = kube
	s.DryRun = func(context.Context, *c4v1.WorkJob, string) (api.SubmitEstimate, error) {
		return api.SubmitEstimate{}, nil
	}
	fc, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Pools: []string{"cpu"}, Params: map[string]string{"frames": "1-100"},
		Forecast: &c4v1.WorkForecast{SlotHours: "40"}})
	real, _ := json.Marshal(c4v1.WorkJobSpec{Class: "render", Pools: []string{"cpu"}, Params: map[string]string{"frames": "1-100"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}})
	submit := func(as string, req api.SubmitRequest) int {
		t.Helper()
		var out map[string]any
		return call(t, s, http.MethodPost, "/api/v1/jobs?as="+as, req, &out)
	}
	// maya's own forecast, and one c4-sync made from tracker github.
	if code := submit("artist", api.SubmitRequest{Project: "show-a", Name: "sh100-forecast", Spec: fc}); code != http.StatusCreated {
		t.Fatalf("maya's forecast: %d", code)
	}
	if code := submit(c4v1.SyncDevViewer, api.SubmitRequest{Project: "show-a", Name: "sh110-forecast", Spec: fc, TrackerRef: "github/Issue/110"}); code != http.StatusCreated {
		t.Fatalf("the tracker's forecast: %d", code)
	}
	for _, c := range []struct {
		as   string
		req  api.SubmitRequest
		want int
	}{
		{"lee", api.SubmitRequest{Project: "show-a", Name: "sh100", Spec: real, Replaces: "sh100-forecast"}, http.StatusForbidden},
		{c4v1.SyncDevViewer, api.SubmitRequest{Project: "show-a", Name: "sh100", Spec: real, Replaces: "sh100-forecast"}, http.StatusForbidden},
		{"lee", api.SubmitRequest{Project: "show-a", Name: "sh110", Spec: real, Replaces: "sh110-forecast"}, http.StatusForbidden},
	} {
		if code := submit(c.as, c.req); code != c.want {
			t.Errorf("%s replacing %s: %d, want %d", c.as, c.req.Replaces, code, c.want)
		}
	}
	var live c4v1.WorkJob
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: "sh100-forecast"}, &live); err != nil {
		t.Fatal(err)
	}
	rec := &jobRec{job: live, key: "show-a/sh100-forecast", project: "show-a"}
	s.current().jobs = append(s.current().jobs, rec)
	s.current().byKey[rec.key] = rec
	if code := call(t, s, http.MethodPost, "/api/v1/jobs/show-a/sh100-forecast/withdraw?as="+c4v1.SyncDevViewer, map[string]string{}, nil); code != http.StatusForbidden {
		t.Errorf("the sync withdrawing maya's forecast with no grant from her: %d", code)
	}
	// Under maya's grant, the sync replaces her forecast with work for her;
	// the sync replaces its own tracker's.
	var g api.GrantResponse
	if code := call(t, s, http.MethodPost, "/api/v1/grants?as=artist", api.GrantRequest{Project: "show-a", Tracker: "github"}, &g); code != http.StatusOK {
		t.Fatalf("grant: %d", code)
	}
	if code := submit(c4v1.SyncDevViewer, api.SubmitRequest{Project: "show-a", Name: "sh100", Spec: real, Replaces: "sh100-forecast",
		For: "maya", TrackerRef: "github/Issue/100"}); code != http.StatusCreated {
		t.Fatalf("the sync under maya's grant: %d", code)
	}
	if code := submit(c4v1.SyncDevViewer, api.SubmitRequest{Project: "show-a", Name: "sh110", Spec: real, Replaces: "sh110-forecast",
		TrackerRef: "github/Issue/110"}); code != http.StatusCreated {
		t.Fatalf("the sync for its own tracker: %d", code)
	}
	for _, name := range []string{"sh100-forecast", "sh110-forecast"} {
		var j c4v1.WorkJob
		if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: name}, &j); !apierrors.IsNotFound(err) {
			t.Errorf("%s is still there: %v", name, err)
		}
	}
	var book api.Book
	if code := call(t, s, http.MethodGet, "/api/v1/book?as=artist", nil, &book); code != http.StatusOK {
		t.Fatalf("book: %d", code)
	}
	replaced := 0
	for _, e := range book.Entries {
		if e.Kind == "replaced" && e.Record != "" {
			replaced++
		}
	}
	if replaced != 2 {
		t.Fatalf("two replacements booked with their records: %d in %+v", replaced, book.Entries)
	}
}
