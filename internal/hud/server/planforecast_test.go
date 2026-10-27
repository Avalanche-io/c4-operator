package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/work/estimate"
	"github.com/Avalanche-io/c4-operator/work/forecast"
)

// forecastCall is one call of a stub PlanForecast.
type forecastCall struct {
	earlier, edits [][]byte
}

// hostMachines puts a host pool in the model: dana's machine, capped
// for power, noor's, and one that is not ready.
func hostMachines(s *Server) {
	s.m.pools = append(s.m.pools, c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "hosts"},
		Spec: c4v1.PoolSpec{NodeSelector: map[string]string{hostLabel: "true"}, SlotsPerNode: 4}})
	node := func(name, owners string, ready corev1.ConditionStatus, labels map[string]string, notes map[string]string) corev1.Node {
		l := map[string]string{hostLabel: "true"}
		for k, v := range labels {
			l[k] = v
		}
		a := map[string]string{c4v1.AnnotationNodeOwners: owners}
		for k, v := range notes {
			a[k] = v
		}
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l, Annotations: a},
			Spec:   corev1.NodeSpec{Taints: []corev1.Taint{{Key: hostTaint, Effect: corev1.TaintEffectNoSchedule}}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}}}
	}
	s.m.nodes = append(s.m.nodes,
		node("dana-mac", `["dana"]`, corev1.ConditionTrue, map[string]string{c4v1.LabelSlotLimit: "2"},
			map[string]string{c4v1.AnnotationCapacityReason: "power: 38 of 40 W, battery 48% on a 39 W adapter"}),
		node("noor-mac", `["noor"]`, corev1.ConditionTrue, nil, nil),
		node("dana-old", `["dana"]`, corev1.ConditionFalse, nil, nil))
}

// Every plan call answers with the forecast, without waiting for the
// queue: the job with the calls before it and then its own, beside the
// job without its own, and what it changes in plain words, local time
// with UTC beside it. It names the critical path, the machines the job
// runs on with their power, and each unmeasured unit with where its
// estimate comes from. When the queue cannot answer at once the answer
// says the forecast follows on the next tick; an applied call is
// forecast from the job as it is; the job's detail carries the latest.
func TestPlanForecast(t *testing.T) {
	s, _, _ := appendFarm(t, func(j *c4v1.WorkJob) { j.Annotations[c4v1.AnnotationLeadSession] = "session-1" })
	hostMachines(s)
	var err error
	if s.Loc, err = time.LoadLocation("America/Los_Angeles"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC) // 13:00 PDT
	dl := at.Add(5 * time.Hour)
	fc := func(ready time.Duration, path ...queue.UnitEstimate) queue.PlanForecast {
		return queue.PlanForecast{AsOf: at, Band: forecast.Band{P50: at.Add(ready), P80: at.Add(ready + 20*time.Minute),
			P90: at.Add(ready + 45*time.Minute), P95: at.Add(ready + time.Hour)}, Deadline: dl, Feasible: true,
			Path: path, Workers: 2, Slots: 4, Pools: []string{"hosts"}, Agent: true}
	}
	var calls []forecastCall
	busy := false
	s.PlanForecast = func(_ context.Context, j *c4v1.WorkJob, earlier, edits [][]byte) (queue.PlanForecast, queue.PlanForecast, error) {
		calls = append(calls, forecastCall{earlier, edits})
		if busy {
			return queue.PlanForecast{}, queue.PlanForecast{}, queue.ErrBusy
		}
		before := fc(2*time.Hour, queue.UnitEstimate{Index: 0, Name: "plan", Seconds: 600})
		after := fc(80*time.Minute, queue.UnitEstimate{Index: 0, Name: "plan", Seconds: 600}, queue.UnitEstimate{Index: 1, New: true, Name: "parse", Seconds: 1800})
		after.Unmeasured = []queue.UnitEstimate{{Index: 1, New: true, Name: "parse", Seconds: 1800, From: queue.FromAgentEnv},
			{Index: 2, New: true, Name: "docs", Seconds: 600, From: queue.FromClassDefault}}
		return before, after, nil
	}

	code, msg, raw := post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", firstPlan, leadHeader("session-1"))
	out := planAnswer(t, raw)
	f := out.Forecast
	if code != http.StatusCreated || f == nil || f.Before == nil || len(calls) != 1 || len(calls[0].earlier) != 0 || len(calls[0].edits) != 1 {
		t.Fatalf("plan: %d %q %+v, calls %d", code, msg, out, len(calls))
	}
	if !strings.Contains(string(calls[0].edits[0]), `"parse"`) {
		t.Fatalf("forecast with %s", calls[0].edits[0])
	}
	want := "adding parse and docs, releasing parse and the new order brings ready to Mon 14:20 PDT (21:20 UTC), 40 min earlier " +
		"(p90 Mon 15:05 PDT (22:05 UTC)), 2 h 55 min before its deadline Mon 18:00 PDT (Tue 01:00 UTC)"
	if f.Change != want {
		t.Fatalf("change\n%q\nwant\n%q", f.Change, want)
	}
	a := f.After
	if a.Ready50 == nil || !a.Ready50.Equal(at.Add(80*time.Minute)) || a.SlackSeconds != int64((2*time.Hour+55*time.Minute).Seconds()) ||
		len(a.CriticalPath) != 2 || a.CriticalPath[0].Index == nil || *a.CriticalPath[0].Index != 0 || a.CriticalPath[1].Name != "parse" ||
		a.CriticalPath[1].Index != nil || len(a.Unmeasured) != 2 || a.Unmeasured[0].From != "agent-env" || a.Unmeasured[1].From != "class-default" {
		t.Fatalf("after %+v", a)
	}
	if c := a.Capacity; c.Workers != 2 || c.Slots != 4 || len(c.Machines) != 1 || c.Machines[0].Name != "dana-mac" || c.Machines[0].Slots != 2 ||
		c.Machines[0].Power != "power: 38 of 40 W, battery 48% on a 39 W adapter" {
		t.Fatalf("capacity %+v", c)
	}

	// A second call while the first is pending: forecast with the first,
	// then with it. Then the queue is busy: the forecast follows.
	second := api.PlanRequest{Key: "k2", Edits: []api.PlanEdit{{Withdraw: &api.PlanWithdraw{Unit: "docs", Why: "the parser documents itself"}}}}
	code, _, raw = post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", second, leadHeader("session-1"))
	if out := planAnswer(t, raw); code != http.StatusCreated || out.Forecast == nil || len(calls) != 2 || len(calls[1].earlier) != 1 ||
		!strings.Contains(string(calls[1].earlier[0]), `"parse"`) || !strings.HasPrefix(out.Forecast.Change, "withdrawing docs brings ready to") {
		t.Fatalf("second: %d %+v, calls %d", code, out, len(calls))
	}
	busy = true
	code, _, raw = post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", firstPlan, leadHeader("session-1"))
	if out := planAnswer(t, raw); code != http.StatusOK || out.Forecast != nil || out.Message != "noor-lead: plan to be applied; "+api.ForecastLater {
		t.Fatalf("busy: %d %+v", code, out)
	}
	if len(calls[2].earlier) != 0 || len(calls[2].edits) != 1 {
		t.Fatalf("again: forecast with %d calls before it and %d", len(calls[2].earlier), len(calls[2].edits))
	}

	// Applied: forecast from the job as it is, nothing to compare.
	busy = false
	j, reqs := livePlans(t, s)
	j.Status.Plan = &c4v1.WorkPlan{Edits: 1, Token: reqs[0].Token, Recent: []c4v1.WorkPlanEdit{{Key: "k1", Document: reqs[0].Document, First: 1, Units: 2,
		Control: reqs[0].Record, At: metav1.Now()}}}
	if err := s.Client.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	code, _, raw = post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", firstPlan, leadHeader("session-1"))
	out = planAnswer(t, raw)
	if code != http.StatusOK || out.State != "applied" || out.Forecast == nil || out.Forecast.Before != nil ||
		len(calls[3].earlier)+len(calls[3].edits) != 0 || !strings.HasPrefix(out.Forecast.Change, "adding parse and docs, releasing parse and the new order, applied: ready Mon 14:20 PDT") {
		t.Fatalf("applied: %d %+v", code, out)
	}

	// The job's detail carries the latest: the job with the calls pending.
	s.m.byKey["show-a/noor-lead"].job = *j
	var detail struct {
		Plan *api.LeadPlan `json:"plan"`
	}
	get(t, s, "/api/v1/jobs/show-a/noor-lead?as=wrangler", &detail)
	if detail.Plan == nil || detail.Plan.Forecast == nil || detail.Plan.Forecast.Before != nil || len(calls) != 5 ||
		len(calls[4].earlier) != 1 || !strings.Contains(string(calls[4].earlier[0]), `"withdraw"`) || calls[4].edits != nil {
		t.Fatalf("detail %+v, calls %d", detail.Plan, len(calls))
	}
}

// A guess says so in place of a slack, in the words for workers; spans
// read as a person says them.
func TestPlanForecastWords(t *testing.T) {
	s := farm(t)
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	g := queue.PlanForecast{Band: forecast.Band{P50: at, P90: at.Add(time.Hour)}, Deadline: at.Add(2 * time.Hour), Agent: true,
		Estimate: estimate.Summary{W: 600, NoHistory: true}}
	e := s.planEstimate(nil, &c4v1.WorkJob{Spec: c4v1.WorkJobSpec{Class: "graph"}}, g, "dana")
	if !e.NoHistory || e.Message != forecast.NoWorkerHistory || e.SlackSeconds != 0 {
		t.Fatalf("a guess: %+v", e)
	}
	if c := planChange("adding parse", nil, e, time.UTC); c != "adding parse, applied: ready Mon 20:00 UTC (p90 Mon 21:00 UTC); "+forecast.NoWorkerHistory {
		t.Fatalf("change %q", c)
	}
	for d, want := range map[time.Duration]string{20 * time.Second: "under a minute", 40 * time.Minute: "40 min",
		2*time.Hour + 5*time.Minute: "2 h 5 min", 3 * time.Hour: "3 h", 80 * time.Hour: "3 days"} {
		if got := span(d); got != want {
			t.Errorf("span(%s) = %q, want %q", d, got, want)
		}
	}
}
