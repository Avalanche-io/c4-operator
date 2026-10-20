package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A person steers a unit while it runs: the HUD signs the message and
// writes it as a request the queue carries out. A message without text,
// to no unit, or to a unit that has ended is refused. The job's summary
// lists the messages, delivered and read.
func TestSteerControl(t *testing.T) {
	s := farm(t)
	ctx := context.Background()
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-6", "running": "7-8", "queued": "9"}
	at := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.Steers = []c4v1.WorkSteer{{Unit: 7, Seq: 1, Actor: "dana", At: at, Text: "use the v3 plates", Delivered: &at, Seen: true}}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy()).Build()
	post := func(body string) (int, string) {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/steer?as=wrangler", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	for body, want := range map[string]int{
		`{"unit": 8, "text": ""}`:                                  http.StatusBadRequest,
		`{"unit": 8, "text": "` + strings.Repeat("x", 9000) + `"}`: http.StatusBadRequest,
		`{"unit": "nothing", "text": "hi"}`:                        http.StatusNotFound,
		`{"unit": 3, "text": "hi"}`:                                http.StatusConflict, // it has ended
	} {
		if code, out := post(body); code != want {
			t.Errorf("%.60s: %d %s, want %d", body, code, out, want)
		}
	}
	if code, out := post(`{"unit": 8, "text": "skip the lighting pass"}`); code != http.StatusOK {
		t.Fatalf("steer: %d %s", code, out)
	}
	if code, out := post(`{"unit": 9, "text": "start from frame 1040"}`); code != http.StatusOK {
		t.Fatalf("steer a queued unit: %d %s", code, out)
	}
	var live c4v1.WorkJob
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "sh040-light-v007"}, &live); err != nil {
		t.Fatal(err)
	}
	var reqs []c4v1.WorkSteerRequest
	if err := json.Unmarshal([]byte(live.Annotations[c4v1.AnnotationSteer]), &reqs); err != nil || len(reqs) != 2 {
		t.Fatalf("requests: %v %s", err, live.Annotations[c4v1.AnnotationSteer])
	}
	if reqs[0].Text != "skip the lighting pass" || reqs[0].Actor != "dana" || reqs[1].Unit != "9" {
		t.Fatalf("requests %+v", reqs)
	}

	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	for _, j := range snap.Jobs {
		if j.Name != "sh040-light-v007" {
			continue
		}
		if len(j.Steers) != 1 || j.Steers[0].Text != "use the v3 plates" || !j.Steers[0].Seen || j.Steers[0].Delivered == nil {
			t.Fatalf("steers %+v", j.Steers)
		}
	}
}
