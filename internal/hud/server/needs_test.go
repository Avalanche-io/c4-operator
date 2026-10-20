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
// question as its detail and supplying it as its step; supplying is
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
		supply := it.Checklist[1]
		if !it.NeedsYou || !strings.Contains(it.Title, "needs information") || it.Detail != "the delivery frame rate (the brief gives 24 and 25)" ||
			supply.Action != "supply" || supply.Units != "7" || it.Severity != "warning" {
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

// A question shows its grade: pressing, with why and by when, makes the
// incident urgent; the lead may withdraw it, a signed request the queue
// carries out, with why. A question supplied or withdrawn takes none.
func TestWithdrawNeeds(t *testing.T) {
	s := farm(t)
	ctx := context.Background()
	rec := s.m.byKey["show-a/sh040-light-v007"]
	at := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	by := metav1.NewTime(at.Add(5 * time.Hour))
	pressed := metav1.NewTime(at.Add(2 * time.Hour))
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-5,8-9", "failed": "6-7"}
	rec.job.Status.Units = c4v1.UnitCounts{Total: 10, Succeeded: 8, Failed: 2}
	rec.job.Status.Needs = map[string]c4v1.WorkNeeds{
		"7": {Text: "the delivery frame rate", At: at, State: "pressing", AnswerBy: &by, PressingAt: &pressed, PressingWhy: "unanswered for 2h",
			About: []string{"unit:grade"}, Rechecks: &c4v1.WorkRechecks{Done: 5, Max: 5, LastWhy: "grade finished"}},
		"6": {Text: "the codec", At: at, State: "withdrawn", Withdrawn: &c4v1.WorkWithdrawal{By: "lead", Why: "moot", At: pressed}},
	}
	rec.job.Status.Conditions = []metav1.Condition{{Type: c4v1.ConditionNeedsPerson, Status: metav1.ConditionTrue, Reason: c4v1.NeedsInformation,
		Message: "needs information: the delivery frame rate", LastTransitionTime: at}}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy()).Build()

	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	var job *api.Job
	for i := range snap.Jobs {
		if snap.Jobs[i].Name == "sh040-light-v007" {
			job = &snap.Jobs[i]
		}
	}
	if job == nil || len(job.Needs) != 2 {
		t.Fatalf("job %+v", job)
	}
	n := job.Needs[1]
	if n.Unit != 7 || n.State != "pressing" || !n.RaisedAt.Equal(at.Time) || n.AnswerBy == nil || !n.AnswerBy.Equal(by.Time) || n.PressingWhy != "unanswered for 2h" ||
		n.Rechecks == nil || !n.Rechecks.UsedUp || n.Rechecks.LastWhy != "grade finished" || len(n.About) != 1 {
		t.Fatalf("needs %+v", n)
	}
	if w := job.Needs[0].Withdrawn; w == nil || w.By != "lead" || job.Needs[0].State != "withdrawn" {
		t.Fatalf("withdrawn %+v", job.Needs[0])
	}
	for _, it := range snap.Incidents {
		if strings.HasPrefix(it.ID, "needsinformation:") && (it.Severity != "urgent" || it.Count != 1 || !strings.Contains(it.Detail, "pressing: unanswered for 2h") ||
			it.Checklist[2].Action != "withdraw-needs" || it.Checklist[2].Units != "7") {
			t.Fatalf("incident %+v", it)
		}
	}

	post := func(unit, body string) (int, string) {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/needs/"+unit+"/withdraw?as=wrangler", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	// Another unit of the job may not withdraw its sibling's question,
	// though its tool lets it change the job.
	s.DevViewers["unit"] = &api.Viewer{User: "unit:show-a/sh040-light-v007/3", Projects: []string{"show-a"},
		Unit: &api.UnitViewer{Namespace: "show-a", Job: "sh040-light-v007", Unit: "3", Grant: "job"}}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/needs/7/withdraw?as=unit", strings.NewReader(`{"why": "moot"}`)))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "only a person may withdraw") {
		t.Errorf("a unit withdraws a sibling's question: %d %s", w.Code, w.Body.String())
	}
	for _, c := range []struct {
		unit, body string
		want       int
	}{
		{"7", `{"why": ""}`, http.StatusBadRequest},
		{"6", `{"why": "moot"}`, http.StatusConflict}, // withdrawn already
		{"3", `{"why": "moot"}`, http.StatusConflict}, // it asked nothing
		{"99", `{"why": "moot"}`, http.StatusConflict},
	} {
		if code, out := post(c.unit, c.body); code != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.unit, c.body, code, out, c.want)
		}
	}
	if code, out := post("7", `{"why": "the client chose 24 in the kickoff"}`); code != http.StatusOK {
		t.Fatalf("withdraw: %d %s", code, out)
	}
	var live c4v1.WorkJob
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "sh040-light-v007"}, &live); err != nil {
		t.Fatal(err)
	}
	var reqs []c4v1.WorkWithdrawRequest
	if err := json.Unmarshal([]byte(live.Annotations[c4v1.AnnotationWithdrawNeeds]), &reqs); err != nil || len(reqs) != 1 ||
		reqs[0].Why != "the client chose 24 in the kickoff" || reqs[0].Unit != "7" || reqs[0].Actor != "dana" {
		t.Fatalf("requests %v %s", err, live.Annotations[c4v1.AnnotationWithdrawNeeds])
	}
}
