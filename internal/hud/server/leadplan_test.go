package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

func livePlans(t *testing.T, s *Server) (*c4v1.WorkJob, []c4v1.WorkPlanRequest) {
	t.Helper()
	j := liveJob(t, s, "noor-lead")
	var reqs []c4v1.WorkPlanRequest
	if raw := j.Annotations[c4v1.AnnotationPlan]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &reqs); err != nil {
			t.Fatal(err)
		}
	}
	return j, reqs
}

func planAnswer(t *testing.T, raw []byte) api.PlanResponse {
	t.Helper()
	var out api.PlanResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

var firstPlan = api.PlanRequest{Key: "k1", Edits: []api.PlanEdit{
	{Add: &api.PlanUnit{Unit: json.RawMessage(`{"name": "parse", "work": 1800}`)}},
	{Add: &api.PlanUnit{Unit: json.RawMessage(`{"name": "docs", "after": ["parse"]}`)}},
	{Release: []string{"parse"}},
	{Order: []string{"parse"}}}}

// A lead plans its own job with one call: its edits are one plan document
// in the project's store (each unit it adds held), the call is signed
// (action plan) and listed for the queue, and the same key again answers
// the same without listing it twice. Once the queue has applied it the
// answer is applied, with the indexes of the units it added; a refusal it
// recorded reads as one; and a call older than status's recent list is
// found in the stored log.
func TestPlanLeadJob(t *testing.T) {
	s, st, roots := appendFarm(t, func(j *c4v1.WorkJob) { j.Annotations[c4v1.AnnotationLeadSession] = "session-1" })
	ctx := context.Background()
	code, msg, raw := post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", firstPlan, leadHeader("session-1"))
	out := planAnswer(t, raw)
	if code != http.StatusCreated || out.State != "pending" || len(out.Units) != 2 || out.Units[0].Name != "parse" || out.Units[0].Index != nil ||
		out.Record == "" || out.Document == "" {
		t.Fatalf("plan: %d %q %+v", code, msg, out)
	}
	j, reqs := livePlans(t, s)
	if len(reqs) != 1 || reqs[0].Key != "k1" || reqs[0].Document != out.Document || reqs[0].Record != out.Record ||
		!slices.Equal(reqs[0].Units, []string{"parse", "docs"}) || reqs[0].Actor != "agent/session-1 for dana" || reqs[0].Token == "" {
		t.Fatalf("requests %+v", reqs)
	}
	var doc class.Doc
	if err := class.Strict(readStored(t, st, out.Document), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Units) != 2 || !doc.Units[0].Held || !doc.Units[1].Held || doc.Units[0].Work != 1800 || !slices.Equal(doc.Release, []string{"parse"}) ||
		!slices.Equal(doc.Order, []string{"parse"}) || string(doc.Dependents) != "[]" {
		t.Fatalf("document %+v", doc)
	}
	cid, _ := c4.Parse(out.Record)
	data, err := c4view.ReadObject(st, cid)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		controlPredicate
		Change struct {
			Request map[string]any `json:"request"`
		} `json:"change"`
	}
	vr, err := provenance.VerifyBundle(data, roots)
	if err == nil {
		err = json.Unmarshal(vr.Predicate, &p)
	}
	if err != nil {
		t.Fatal(err)
	}
	if p.Action != "plan" || p.Target != "workjob:show-a/noor-lead" || p.Change.Request["key"] != "k1" || p.Change.Request["document"] != out.Document {
		t.Fatalf("record %+v %v", p.controlPredicate, p.Change.Request)
	}

	// The same key again: the same answer, nothing more listed.
	code, _, raw = post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", firstPlan, leadHeader("session-1"))
	if again := planAnswer(t, raw); code != http.StatusOK || again.State != "pending" || again.Record != out.Record || len(again.Units) != 2 {
		t.Fatalf("again: %d %+v", code, again)
	}
	if _, reqs := livePlans(t, s); len(reqs) != 1 {
		t.Fatalf("again listed %d requests", len(reqs))
	}

	// The queue applies it: the answer has the units' indexes, and a new
	// key is listed while the applied one leaves the list.
	j.Status.Plan = &c4v1.WorkPlan{Edits: 1, Token: reqs[0].Token, Recent: []c4v1.WorkPlanEdit{{Key: "k1", Document: out.Document, First: 1, Units: 2,
		Control: out.Record, At: metav1.Now()}}}
	j.Status.UnitStates = map[string]string{"running": "0", "held": "2", "queued": "1"}
	if err := s.Client.Update(ctx, j); err != nil {
		t.Fatal(err)
	}
	code, _, raw = post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", firstPlan, leadHeader("session-1"))
	again := planAnswer(t, raw)
	if code != http.StatusOK || again.State != "applied" || len(again.Units) != 2 || again.Units[1].Name != "docs" || again.Units[1].Index == nil || *again.Units[1].Index != 2 {
		t.Fatalf("applied: %d %+v", code, again)
	}
	second := api.PlanRequest{Key: "k2", Edits: []api.PlanEdit{{Withdraw: &api.PlanWithdraw{Unit: "docs", Why: "the parser documents itself"}}}}
	if code, msg, _ := post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", second, nil); code != http.StatusCreated {
		t.Fatalf("its person: %d %q", code, msg)
	}
	j, reqs = livePlans(t, s)
	if len(reqs) != 1 || reqs[0].Key != "k2" || reqs[0].Actor != "dana" || reqs[0].Units != nil {
		t.Fatalf("after a second key: %+v", reqs)
	}

	// A refusal the queue recorded reads as one; a call that has left the
	// recent list is read from the log.
	log, err := json.Marshal(map[string]any{"kind": "c4.avalanche.io/plan-log", "version": 1,
		"edits": []c4v1.WorkPlanEdit{{Key: "k0", First: 1, Units: 0, At: metav1.Now()}}})
	if err != nil {
		t.Fatal(err)
	}
	logID, err := st.PutBytes(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	j.Status.Plan.Edits, j.Status.Plan.Log, j.Status.Plan.Token = 3, logID.String(), reqs[0].Token
	j.Status.Plan.Recent = append(j.Status.Plan.Recent, c4v1.WorkPlanEdit{Key: "k2", First: 3, Error: "noor-lead/2 (docs) is running; withdraw it after it ends, or steer it"})
	if err := s.Client.Update(ctx, j); err != nil {
		t.Fatal(err)
	}
	code, _, raw = post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", second, nil)
	if r := planAnswer(t, raw); code != http.StatusOK || r.State != "refused" || !strings.Contains(r.Error, "is running; withdraw it after it ends, or steer it") {
		t.Fatalf("refused: %d %+v", code, r)
	}
	old := api.PlanRequest{Key: "k0", Edits: second.Edits}
	code, _, raw = post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", old, nil)
	if r := planAnswer(t, raw); code != http.StatusOK || r.State != "applied" {
		t.Fatalf("from the log: %d %+v", code, r)
	}
	if _, reqs := livePlans(t, s); len(reqs) != 1 {
		t.Fatalf("an old key listed again: %+v", reqs)
	}
}

// Who may and what, each refused in the farm's words with nothing listed:
// another session, a unit, a job that is not a lead's, a running unit
// withdrawn, an edit that is two, a call with no key, a closed job.
func TestPlanRefused(t *testing.T) {
	s := leadJobFarm(t, nil)
	withdrawRoot := api.PlanRequest{Key: "k1", Edits: []api.PlanEdit{{Withdraw: &api.PlanWithdraw{Unit: "plan", Why: "the lead does it"}}}}
	two := api.PlanRequest{Key: "k1", Edits: []api.PlanEdit{{Release: []string{"plan"}, Order: []string{"plan"}}}}
	for _, c := range []struct {
		name, as string
		req      api.PlanRequest
		headers  map[string]string
		code     int
		want     string
	}{
		{"another session", "wrangler", firstPlan, leadHeader("session-2"), http.StatusForbidden,
			"noor-lead is the job of lead session session-1, and the agent header (X-C4-Agent) names session session-2: only that lead, or dana with no agent header, plans it"},
		{"a unit", "unit", firstPlan, nil, http.StatusForbidden, "a unit cannot plan a job: its lead does"},
		{"a running unit", "wrangler", withdrawRoot, nil, http.StatusUnprocessableEntity, "noor-lead/0 (plan) is running; withdraw it after it ends, or steer it"},
		{"two edits in one", "wrangler", two, nil, http.StatusBadRequest, "edits[0]: give exactly one of add, split, merge, order, withdraw, release or edge (it gives 2)"},
		{"no key", "wrangler", api.PlanRequest{Edits: firstPlan.Edits}, nil, http.StatusBadRequest, "key: an idempotency key"},
	} {
		if code, msg, _ := post(t, s, "/api/v1/jobs/show-a/noor-lead/plan", c.as, c.req, c.headers); code != c.code || msg != c.want && !strings.HasPrefix(msg, c.want) {
			t.Errorf("%s: %d %q", c.name, code, msg)
		}
	}
	if _, reqs := livePlans(t, s); len(reqs) != 0 {
		t.Fatalf("a refused call was listed: %+v", reqs)
	}

	closed := leadJobFarm(t, func(j *c4v1.WorkJob) {
		j.Status.Lead = &c4v1.WorkLead{Session: "session-1", State: c4v1.LeadClosed,
			Closed: &c4v1.WorkClose{At: metav1.NewTime(time.Now()), By: "dana", Why: "done"}}
	})
	if code, msg, _ := post(t, closed, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", firstPlan, nil); code != http.StatusConflict ||
		msg != "noor-lead is closed (done): a closed lead's job takes no more edits; make a new job for them" {
		t.Errorf("closed: %d %q", code, msg)
	}
	plain, _, _ := appendFarm(t, nil)
	if code, msg, _ := post(t, plain, "/api/v1/jobs/show-a/noor-lead/plan", "wrangler", firstPlan, nil); code != http.StatusUnprocessableEntity ||
		msg != "noor-lead is not a lead's job: only a lead plans its own job (c4_plan); add units to it with POST /jobs/show-a/noor-lead/units" {
		t.Errorf("not a lead's job: %d %q", code, msg)
	}
}

// A lead plans workers as c4_agent starts them: each lowered as c4_agent
// lowers its workers, with its brief and the commit it starts from, and
// held, with the lead's estimate as its work.
func TestPlanAddsWorkers(t *testing.T) {
	s := submissionFarm(t)
	agentToolIn(t, s)
	base := strings.Repeat("0a1b", 10)
	first := api.LeadWorkersRequest{Project: "show-a", Root: "/Users/dev/textkit", Key: "call-1", Workers: []api.LeadWorker{
		{Name: "scan", Description: "Scan the parser", Prompt: "List the parser's modules.", Base: base}}}
	code, msg, raw := post(t, s, "/api/v1/lead/workers", "artist", first, leadHeader("session-1"))
	var made api.LeadWorkersResponse
	_ = json.Unmarshal(raw, &made)
	if code != http.StatusCreated {
		t.Fatalf("the lead's job: %d %q", code, msg)
	}
	plan := api.PlanRequest{Key: "plan-1", Root: "/Users/dev/textkit", Edits: []api.PlanEdit{
		{Add: &api.PlanUnit{Work: 1800, Worker: &api.LeadWorker{Name: "port", Description: "Port the parser", Prompt: "Port each module scan lists.", Base: base, After: []string{"scan"}}}}}}
	code, msg, raw = post(t, s, "/api/v1/jobs/"+made.Job+"/plan", "artist", plan, leadHeader("session-1"))
	out := planAnswer(t, raw)
	if code != http.StatusCreated || len(out.Units) != 1 || out.Units[0].Name != "port" {
		t.Fatalf("plan: %d %q %+v", code, msg, out)
	}
	st, _ := s.Stores(context.Background(), "show-a")
	var doc class.Doc
	if err := class.Strict(readStored(t, st, out.Document), &doc); err != nil {
		t.Fatal(err)
	}
	if u := doc.Units[0]; len(doc.Units) != 1 || !u.Held || u.Work != 1800 || u.Env["C4_BLOCK_PROMPT"] != "Port each module scan lists." ||
		u.Env["C4_BLOCK_BASE"] != base || !slices.Equal(u.After, []string{"scan"}) || u.Label != "Port the parser" {
		t.Fatalf("the worker: %+v", doc.Units)
	}
}

// A lead's plan shows where unit states do: held and withdrawn units in
// the snapshot's counts and sets, the job's plan (its calls, why each
// withdrawn unit went and what replaced it, the order), and in the job's
// unit list each withdrawn unit's replacements and each ordered unit's
// key; a unit blocked on a withdrawn one is blocked on what is left.
func TestPlanInSnapshotAndDetail(t *testing.T) {
	s := farm(t)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-2", "withdrawn": "3", "held": "4-5", "blocked": "6", "queued": "7-9"}
	rec.job.Status.Units = c4v1.UnitCounts{Total: 10, Succeeded: 3, Withdrawn: 1, Held: 2, Blocked: 1, Queued: 3}
	rec.job.Status.Plan = &c4v1.WorkPlan{Edits: 1, Order: "4:1", Recent: []c4v1.WorkPlanEdit{{Key: "k1", First: 4, Units: 2}},
		Withdrawn: map[string]c4v1.WorkWithdrawnUnit{"3": {Why: "split into lexer, grammar", Replacements: "4-5", Edit: "k1"}}}
	rec.plan.Units[3].Withdrawn = &class.Withdrawal{Why: "split into lexer, grammar", By: []int{4, 5}, Edit: "k1"}
	rec.plan.Units[4].OrderKey = 1
	rec.plan.Units[4].Name = "lexer"
	rec.plan.Units[6].After = []int{3, 4}
	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	i := slices.IndexFunc(snap.Jobs, func(j api.Job) bool { return j.Name == "sh040-light-v007" })
	if i < 0 {
		t.Fatal("job not in the snapshot")
	}
	j := snap.Jobs[i]
	if j.Units.Withdrawn != 1 || j.Units.Held != 2 || j.UnitStates.Withdrawn != "3" || j.UnitStates.Held != "4-5" {
		t.Fatalf("counts %+v states %+v", j.Units, j.UnitStates)
	}
	if p := j.Plan; p == nil || p.Edits != 1 || p.Order != "4:1" || len(p.Recent) != 1 || p.Withdrawn["3"].Replacements != "4-5" {
		t.Fatalf("plan %+v", j.Plan)
	}
	var detail struct {
		UnitList []struct {
			I         int    `json:"i"`
			State     string `json:"state"`
			BlockedOn string `json:"blockedOn"`
			OrderKey  int    `json:"orderKey"`
			Withdrawn *struct {
				Why          string `json:"why"`
				Replacements []int  `json:"replacements"`
			} `json:"withdrawn"`
		} `json:"unitList"`
	}
	get(t, s, "/api/v1/jobs/show-a/sh040-light-v007?as=wrangler", &detail)
	seen := 0
	for _, u := range detail.UnitList {
		switch u.I {
		case 3:
			seen++
			if u.State != "withdrawn" || u.Withdrawn == nil || !slices.Equal(u.Withdrawn.Replacements, []int{4, 5}) {
				t.Fatalf("unit 3: %+v", u)
			}
		case 4:
			seen++
			if u.State != "held" || u.OrderKey != 1 {
				t.Fatalf("unit 4: %+v", u)
			}
		case 6:
			seen++
			if u.State != "blocked" || u.BlockedOn != "lexer" {
				t.Fatalf("unit 6: %+v", u)
			}
		}
	}
	if seen != 3 {
		t.Fatalf("units %+v", detail.UnitList)
	}
}

// A lead's wait counts a withdrawn unit as ended (it never runs), and a
// unit a plan call adds as pending until the queue has applied the call.
func TestWaitOnPlannedUnits(t *testing.T) {
	s := farm(t)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.Status.UnitStates["withdrawn"] = "7"
	const job = "/api/v1/jobs/show-a/sh040-light-v007/wait?as=wrangler&within=0s"
	var a api.WaitAnswer
	if code := get(t, s, job+"&unit=7", &a); code != 200 || !a.Done || a.Units[0].State != "withdrawn" || a.Units[0].Settled != "ended" {
		t.Fatalf("a withdrawn unit: %d %+v", code, a)
	}
	rec.job.Annotations = map[string]string{c4v1.AnnotationPlan: `[{"token":"t2","key":"k1","document":"c4doc","units":["lexer"],"actor":"wrangler"}]`}
	a = api.WaitAnswer{}
	if code := get(t, s, job+"&unit=lexer", &a); code != 200 || a.Done || a.Units[0].State != "pending" {
		t.Fatalf("a planned unit: %d %+v", code, a)
	}
	rec.job.Status.Plan = &c4v1.WorkPlan{Edits: 1, Token: "t2", Recent: []c4v1.WorkPlanEdit{{Key: "k1", First: 10, Units: 1}}}
	if code := get(t, s, job+"&unit=lexer", nil); code != 404 {
		t.Fatalf("applied, yet not in the table: %d", code)
	}
}
