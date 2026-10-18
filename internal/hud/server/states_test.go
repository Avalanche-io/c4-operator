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
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

func TestRoleOf(t *testing.T) {
	for _, c := range []struct {
		groups []string
		want   string
	}{
		{nil, ""},
		{[]string{"system:authenticated", "c4:role:wrangler"}, "wrangler"},
		{[]string{"c4-role-pipeline-td"}, "pipeline-td"},
		{[]string{"c4:role:emperor", "c4:role:artist"}, "artist"},
	} {
		if got := RoleOf(c.groups); got != c.want {
			t.Errorf("RoleOf(%v) = %q, want %q", c.groups, got, c.want)
		}
	}
}

func TestShownRisk(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	dl := func(h int) *metav1.Time { return &metav1.Time{Time: now.Add(time.Duration(h) * time.Hour)} }
	for _, c := range []struct {
		name   string
		st     c4v1.WorkJobStatus
		queued string
		want   string
	}{
		{"no deadline", c4v1.WorkJobStatus{}, c4v1.RiskAt, c4v1.RiskNone},
		{"the queue's", c4v1.WorkJobStatus{Deadline: dl(2)}, c4v1.RiskWillMiss, c4v1.RiskWillMiss},
		{"passed since the last write", c4v1.WorkJobStatus{Deadline: dl(-1)}, c4v1.RiskAt, c4v1.RiskMissed},
		{"finished", c4v1.WorkJobStatus{Deadline: dl(-1), Finished: dl(-2)}, c4v1.RiskMissed, c4v1.RiskNone},
		{"not estimated yet", c4v1.WorkJobStatus{Deadline: dl(2)}, "", c4v1.RiskNone},
	} {
		if got := shownRisk(now, &c.st, c.queued); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// The done and delivered shares never go back between collections, except
// after finished units are requeued.
func TestSteadyFractions(t *testing.T) {
	s := &Server{}
	rec := &jobRec{job: c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{UID: types.UID("j1")}}}
	m := &model{jobs: []*jobRec{rec}}
	at := func(succeeded int32, done, delivered float64) (float64, float64) {
		rec.job.Status.Units.Succeeded = succeeded
		rec.work = fractions(api.Work{DoneSlotSeconds: done, RemainingSlotSeconds: 100 - done}, delivered, "Running")
		s.steady(m)
		return *rec.work.DoneFraction, *rec.work.DeliveredFraction
	}
	if d, v := at(5, 40, 30); d != 0.4 || v != 0.3 {
		t.Fatalf("first: %v %v", d, v)
	}
	// The estimate of what is left grew: the shares hold.
	rec.job.Status.Units.Succeeded = 6
	rec.work = fractions(api.Work{DoneSlotSeconds: 42, RemainingSlotSeconds: 120}, 33, "Running")
	s.steady(m)
	if d, v := *rec.work.DoneFraction, *rec.work.DeliveredFraction; d != 0.4 || v != 0.3 {
		t.Fatalf("after the estimate grew: %v %v", d, v)
	}
	// Finished units were requeued: the shares start again.
	if d, v := at(2, 10, 10); d != 0.1 || v != 0.1 {
		t.Fatalf("after a requeue of finished units: %v %v", d, v)
	}
	if w := fractions(api.Work{DoneSlotSeconds: 1, RemainingSlotSeconds: 9}, 1, "Succeeded"); *w.DoneFraction != 1 || *w.DeliveredFraction != 1 {
		t.Fatalf("a succeeded job is whole: %v", w)
	}
}

// A person takes a failed unit, and another accepts it; the HUD writes
// each as a request the queue carries out, and the job's summary shows
// the standing acknowledgements and why it needs a person.
func TestAckControl(t *testing.T) {
	s := farm(t)
	ctx := context.Background()
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.UnitStates = map[string]string{"failed": "9", "succeeded": "0-8"}
	since := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.Conditions = []metav1.Condition{{Type: c4v1.ConditionNeedsPerson, Status: metav1.ConditionTrue, Reason: c4v1.NeedsFailed,
		Message: "1 units failed after their retries", LastTransitionTime: since}}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy()).Build()
	post := func(body string) (int, string) {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/ack?as=wrangler", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if code, body := post(`{"units": "3"}`); code != http.StatusConflict {
		t.Fatalf("acknowledging a unit that has not failed: %d %s", code, body)
	}
	if code, body := post(`{"units": "9", "kind": "assign"}`); code != http.StatusBadRequest {
		t.Fatalf("assigning to nobody: %d %s", code, body)
	}
	if code, body := post(`{"units": [9]}`); code != http.StatusOK {
		t.Fatalf("acknowledge: %d %s", code, body)
	}
	if code, body := post(`{"units": "9", "kind": "accept", "note": "the splash reads fine"}`); code != http.StatusOK {
		t.Fatalf("accept: %d %s", code, body)
	}
	var live c4v1.WorkJob
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "sh040-light-v007"}, &live); err != nil {
		t.Fatal(err)
	}
	var reqs []c4v1.WorkAckRequest
	if err := json.Unmarshal([]byte(live.Annotations[c4v1.AnnotationAcks]), &reqs); err != nil || len(reqs) != 2 {
		t.Fatalf("requests: %v %s", err, live.Annotations[c4v1.AnnotationAcks])
	}
	if reqs[0].Kind != c4v1.AckAcknowledge || reqs[0].Actor != "dana" || reqs[1].Kind != c4v1.AckAccept || reqs[1].Note == "" ||
		reqs[0].Token == reqs[1].Token || reqs[1].At.IsZero() {
		t.Fatalf("requests: %+v", reqs)
	}

	// What the queue then records shows in the summary and the detail.
	rec.job.Status.Acks = []c4v1.WorkAck{{Kind: c4v1.AckAcknowledge, Units: "9", Actor: "dana", At: since}}
	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	for _, j := range snap.Jobs {
		if j.Name != "sh040-light-v007" {
			continue
		}
		if j.NeedsPerson == nil || j.NeedsPerson.Reason != "failed" || !j.NeedsPerson.Since.Equal(since.Time) || len(j.Acks) != 1 || j.Acks[0].Actor != "dana" {
			t.Fatalf("summary: %+v %+v", j.NeedsPerson, j.Acks)
		}
	}
	var detail struct {
		UnitList []struct {
			I     int      `json:"i"`
			State string   `json:"state"`
			Ack   *api.Ack `json:"ack"`
		} `json:"unitList"`
	}
	get(t, s, "/api/v1/jobs/show-a/sh040-light-v007?as=wrangler", &detail)
	found := false
	for _, u := range detail.UnitList {
		if u.I == 9 {
			found = true
			if u.State != "failed" || u.Ack == nil || u.Ack.Actor != "dana" {
				t.Fatalf("unit 9: %+v", u)
			}
		}
	}
	if !found {
		t.Fatal("unit 9 is not in the detail")
	}
}
