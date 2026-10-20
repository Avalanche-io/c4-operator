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

// A unit that stopped for information is an incident of its own, with the
// question as its detail and supplying it as its last step; supplying is
// a signed request the queue carries out. Only a unit that stopped for
// information takes one.
func TestSupplyInformation(t *testing.T) {
	s := farm(t)
	ctx := context.Background()
	rec := s.m.byKey["show-a/sh040-light-v007"]
	at := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-6,8-9", "failed": "7"}
	rec.job.Status.Units = c4v1.UnitCounts{Total: 10, Succeeded: 9, Failed: 1}
	rec.job.Status.Needs = map[string]c4v1.WorkNeeds{"7": {Text: "the delivery frame rate", Why: "the brief gives 24 and 25", At: at}}
	rec.job.Status.Conditions = []metav1.Condition{{Type: c4v1.ConditionNeedsPerson, Status: metav1.ConditionTrue, Reason: c4v1.NeedsInformation,
		Message: "needs information: the delivery frame rate", LastTransitionTime: at}}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy()).Build()

	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	var found bool
	for _, it := range snap.Incidents {
		if !strings.HasPrefix(it.ID, "needsinformation:") {
			continue
		}
		found = true
		last := it.Checklist[len(it.Checklist)-1]
		if !it.NeedsYou || !strings.Contains(it.Title, "needs information") || it.Detail != "the delivery frame rate (the brief gives 24 and 25)" ||
			last.Action != "supply" || last.Units != "7" {
			t.Fatalf("incident %+v", it)
		}
	}
	if !found {
		t.Fatalf("no incident: %+v", snap.Incidents)
	}

	post := func(body string) (int, string) {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/supply?as=wrangler", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	for body, want := range map[string]int{
		`{"unit": 7, "text": ""}`:    http.StatusBadRequest,
		`{"unit": 3, "text": "24"}`:  http.StatusConflict, // it did not stop for information
		`{"unit": 99, "text": "24"}`: http.StatusConflict,
	} {
		if code, out := post(body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, out, want)
		}
	}
	if code, out := post(`{"unit": 7, "text": "24 fps"}`); code != http.StatusOK {
		t.Fatalf("supply: %d %s", code, out)
	}
	var live c4v1.WorkJob
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "sh040-light-v007"}, &live); err != nil {
		t.Fatal(err)
	}
	var reqs []c4v1.WorkSupplyRequest
	if err := json.Unmarshal([]byte(live.Annotations[c4v1.AnnotationSupply]), &reqs); err != nil || len(reqs) != 1 || reqs[0].Text != "24 fps" || reqs[0].Unit != "7" || reqs[0].Actor != "dana" {
		t.Fatalf("requests %v %s", err, live.Annotations[c4v1.AnnotationSupply])
	}
}
