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
		// The studio's own group names, ahead of the convention.
		{[]string{"c4:role:artist", "vfx-leads"}, "supervisor"},
		{[]string{"render-wranglers"}, "wrangler"},
		{[]string{"bad-map"}, ""},
	} {
		byGroup := map[string]string{"vfx-leads": "supervisor", "render-wranglers": "wrangler", "bad-map": "emperor"}
		if got := RoleOf(c.groups, byGroup); got != c.want {
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

// A person decides a wait: the HUD signs the decision and writes it as a
// request the queue carries out; the job's summary shows its waits, a
// blocking one as needing the person. A decision on nothing open, an
// unknown choice, and the wrong kind for an escalated unit are refused.
func TestDecideControl(t *testing.T) {
	s := farm(t)
	ctx := context.Background()
	rec := s.m.byKey["show-a/sh040-light-v007"]
	since := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-7", "waits": "8", "blocked": "9"}
	rec.job.Status.Waits = []c4v1.WorkWait{
		{Unit: 8, Name: "ruling", Server: "person:owner", Ask: "split the chapter?", Choices: []string{"split", "keep"}, Since: &since, ExpectSeconds: 46080, Blocking: true},
		{Unit: 7, Name: "f7", Server: "person:lead", Escalated: true, Since: &since},
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy()).Build()
	post := func(body string) (int, string) {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/decide?as=wrangler", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	for body, want := range map[string]int{
		`{"unit": "nothing", "do": "approve"}`: http.StatusConflict,
		`{"unit": "ruling", "do": "approve"}`:  http.StatusBadRequest, // not one of its choices
		`{"unit": "f7", "do": "approve"}`:      http.StatusBadRequest, // escalated: accept, rework or fail
		`{"unit": "ruling"}`:                   http.StatusBadRequest,
	} {
		if code, out := post(body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, out, want)
		}
	}
	if code, out := post(`{"unit": "ruling", "do": "split", "note": "two lanes"}`); code != http.StatusOK {
		t.Fatalf("decide: %d %s", code, out)
	}
	if code, out := post(`{"unit": 7, "do": "rework"}`); code != http.StatusOK {
		t.Fatalf("decide escalated: %d %s", code, out)
	}
	var live c4v1.WorkJob
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "sh040-light-v007"}, &live); err != nil {
		t.Fatal(err)
	}
	var reqs []c4v1.WorkDecideRequest
	if err := json.Unmarshal([]byte(live.Annotations[c4v1.AnnotationDecide]), &reqs); err != nil || len(reqs) != 2 {
		t.Fatalf("requests: %v %s", err, live.Annotations[c4v1.AnnotationDecide])
	}
	if reqs[0].Unit != "ruling" || reqs[0].Do != "split" || reqs[0].Actor != "dana" || reqs[0].Note != "two lanes" || reqs[1].Unit != "f7" || reqs[1].Do != "rework" {
		t.Fatalf("requests: %+v", reqs)
	}

	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	for _, j := range snap.Jobs {
		if j.Name != "sh040-light-v007" {
			continue
		}
		if len(j.Waits) != 2 || !j.Waits[0].NeedsYou || j.Waits[0].WaitsFor != "person:owner" || !j.Waits[1].NeedsYou || !j.Waits[1].Escalated || j.UnitStates.Waits != "8" {
			t.Fatalf("summary waits %+v, states %+v", j.Waits, j.UnitStates)
		}
	}
	var detail struct {
		UnitList []struct {
			I     int    `json:"i"`
			State string `json:"state"`
		} `json:"unitList"`
	}
	get(t, s, "/api/v1/jobs/show-a/sh040-light-v007?as=wrangler", &detail)
	for _, u := range detail.UnitList {
		if u.I == 8 && u.State != "waits" {
			t.Fatalf("unit 8 is %s, want waits", u.State)
		}
	}
}

// Job summaries carry planned (also unevaluated) and flagged counts and
// sets, so they survive when unitList is omitted; a blocked unit names
// the first unit it waits on; interruptions come with their count.
func TestPlannedFlaggedBlockedOn(t *testing.T) {
	s := farm(t)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-2,4,6-7", "planned": "3", "flagged": "5", "running": "8", "blocked": "9"}
	rec.job.Status.Units = c4v1.UnitCounts{Total: 10, Succeeded: 6, Planned: 1, Flagged: 1, Running: 1, Blocked: 1}
	rec.job.Status.Interruptions = []c4v1.WorkInterruption{{Unit: 2, Reason: "EvictionByEvictionAPI"}}
	rec.job.Status.Interrupted = 3
	rec.plan.Units[8].Name = "deep"
	rec.plan.Units[9].After = []int{0, 8}
	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	var found bool
	for _, j := range snap.Jobs {
		if j.Name != "sh040-light-v007" {
			continue
		}
		found = true
		if j.Units.Planned != 1 || j.Units.Unevaluated != 1 || j.Units.Flagged != 1 || j.UnitStates.Planned != "3" || j.UnitStates.Unevaluated != "3" || j.UnitStates.Flagged != "5" {
			t.Fatalf("counts %+v states %+v", j.Units, j.UnitStates)
		}
	}
	if !found {
		t.Fatal("job not in the snapshot")
	}
	var detail struct {
		Interrupted int `json:"interrupted"`
		UnitList    []struct {
			I         int    `json:"i"`
			State     string `json:"state"`
			BlockedOn string `json:"blockedOn"`
		} `json:"unitList"`
	}
	get(t, s, "/api/v1/jobs/show-a/sh040-light-v007?as=wrangler", &detail)
	if detail.Interrupted != 3 {
		t.Fatalf("interrupted %d", detail.Interrupted)
	}
	for _, u := range detail.UnitList {
		switch u.I {
		case 9:
			if u.State != "blocked" || u.BlockedOn != "deep" {
				t.Fatalf("unit 9: %+v", u)
			}
		case 3, 5:
			if want := map[int]string{3: "planned", 5: "flagged"}[u.I]; u.State != want {
				t.Fatalf("unit %d is %s, want %s", u.I, u.State, want)
			}
		}
	}
}

// The snapshot lists each gate with its holder and its line, first come
// first served, across jobs; a waiting unit says what it waits for.
func TestSnapshotGates(t *testing.T) {
	s := farm(t)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	t0 := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	t1 := metav1.NewTime(t0.Add(time.Minute))
	at := metav1.NewTime(t0.Add(20 * time.Minute))
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-6", "running": "7", "locked": "8-9"}
	rec.job.Status.Gates = []c4v1.WorkGateEntry{
		{Gate: "merge", Unit: 7, Name: "m7", Position: 0},
		{Gate: "merge", Unit: 9, Name: "m9", Position: 2, Since: &t1, Holder: "show-a/sh040-light-v007#m7", ExpectStart: &at},
		{Gate: "merge", Unit: 8, Name: "m8", Position: 1, Since: &t0, Holder: "show-a/sh040-light-v007#m7", ExpectStart: &t1},
	}
	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	if len(snap.Gates) != 1 || snap.Gates[0].Name != "merge" || snap.Gates[0].Holder == nil || snap.Gates[0].Holder.Name != "m7" ||
		len(snap.Gates[0].Waiting) != 2 || snap.Gates[0].Waiting[0].Name != "m8" {
		t.Fatalf("gates %+v", snap.Gates)
	}
	var detail struct {
		UnitList []struct {
			I        int            `json:"i"`
			State    string         `json:"state"`
			LockedBy map[string]any `json:"lockedBy"`
		} `json:"unitList"`
	}
	get(t, s, "/api/v1/jobs/show-a/sh040-light-v007?as=wrangler", &detail)
	for _, u := range detail.UnitList {
		if u.I == 9 && (u.State != "locked" || u.LockedBy["gate"] != "merge" || u.LockedBy["position"] != float64(2)) {
			t.Fatalf("unit 9: %+v", u)
		}
	}
}
