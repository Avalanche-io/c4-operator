package queue

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// planTo lists plan requests on a job, as the HUD writes them.
func planTo(t *testing.T, ns, name string, reqs ...c4v1.WorkPlanRequest) {
	t.Helper()
	j := get(t, ns, name)
	var list []c4v1.WorkPlanRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationPlan]), &list)
	for i, r := range reqs {
		if r.Token == "" {
			r.Token = strconv.FormatInt(time.Now().UnixNano()+int64(i), 36)
		}
		if r.Actor == "" {
			r.Actor = "agent/session-1 for noor"
		}
		list = append(list, r)
	}
	b, _ := json.Marshal(list)
	j.Annotations[c4v1.AnnotationPlan] = string(b)
	if err := k8s.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

// A lead's planned units are held: no pod, no work to ration, until it
// releases them. Released together, they run in its order: the lower key
// first, then the unit with the longer chain after it.
func TestPlanHeldReleasedInOrder(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	leadJob(t, ns, "comp", q, *now)
	add := putDoc(t, st, `{"version": 1, "units": [
		{"name": "a", "held": true, "work": 60}, {"name": "b", "held": true, "work": 60},
		{"name": "c", "held": true, "work": 60}, {"name": "d", "held": true, "work": 60},
		{"name": "e", "held": true, "work": 60}, {"name": "f", "held": true, "work": 600, "after": ["c"]}]}`)
	planTo(t, ns, "comp", c4v1.WorkPlanRequest{Key: "k1", Document: add, Units: []string{"a", "b", "c", "d", "e", "f"}})
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "comp")
	p := got.Status.Plan
	if p == nil || p.Edits != 1 || len(p.Recent) != 1 || p.Recent[0].First != 1 || p.Recent[0].Units != 6 || p.Recent[0].Error != "" || p.Log == "" {
		t.Fatalf("plan %+v", p)
	}
	if h := states(t, got, "held"); !slices.Equal(h, []int{1, 2, 3, 4, 5, 6}) || got.Status.Units.Held != 6 {
		t.Fatalf("held %v, units %+v", h, got.Status.Units)
	}
	if units := podUnits(unitPods(t, ns, "comp")); !slices.Equal(units, []int{0}) {
		t.Fatalf("pods of %v: held units ran", units)
	}
	if got.Status.Lead.State != c4v1.LeadWorking {
		t.Fatalf("lead %+v", got.Status.Lead)
	}

	// Released, with e and d first: three slots are free beside the root,
	// and the third goes to c, which f waits on.
	rel := putDoc(t, st, `{"version": 1, "release": ["a", "b", "c", "d", "e", "f"], "order": ["e", "d"]}`)
	planTo(t, ns, "comp", c4v1.WorkPlanRequest{Key: "k2", Document: rel})
	tick(t, q)
	tick(t, q)
	got = get(t, ns, "comp")
	if units := podUnits(unitPods(t, ns, "comp")); !slices.Equal(units, []int{0, 3, 4, 5}) {
		t.Fatalf("pods of %v, want the root, c, d and e", units)
	}
	if p := got.Status.Plan; p.Edits != 2 || p.Order != "4:2,5:1" || len(states(t, got, "held")) != 0 {
		t.Fatalf("plan %+v, states %v", p, got.Status.UnitStates)
	}
}

// A split appends the replacements and withdraws the unit it replaces,
// naming them; what followed it follows them, and no index moves. A
// running unit is refused in the farm's words, and the call makes none of
// its edits. A withdrawn unit counts as done and never runs. Each call is
// signed (kind plan); a restart replays every call among the job's
// appends and expansions, from the stored log, and applies no key twice.
func TestPlanSplitWithdrawReplay(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if q.Signer, err = provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("queue"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	leadJob(t, ns, "comp", q, *now)
	root := podOfUnit(t, ns, "comp", 0)
	first := putDoc(t, st, `{"version": 1, "units": [
		{"name": "parse", "held": true, "work": 60}, {"name": "docs", "held": true, "after": ["parse"]},
		{"name": "report", "held": true, "after": ["docs"]}], "release": ["parse"]}`)
	planTo(t, ns, "comp", c4v1.WorkPlanRequest{Key: "k1", Document: first, Units: []string{"parse", "docs", "report"}})
	tick(t, q)
	tick(t, q)
	parse := podOfUnit(t, ns, "comp", 1)

	running := putDoc(t, st, `{"version": 1, "withdraw": [{"unit": "parse", "why": "done by hand"}, {"unit": "report", "why": "not needed"}]}`)
	split := putDoc(t, st, `{"version": 1, "units": [{"name": "docs-api"}, {"name": "docs-guide"}],
		"split": [{"unit": "docs", "into": ["docs-api", "docs-guide"], "why": "two readers"}],
		"withdraw": [{"unit": "report", "why": "the review covers it"}]}`)
	planTo(t, ns, "comp", c4v1.WorkPlanRequest{Key: "k2", Document: running}, c4v1.WorkPlanRequest{Key: "k3", Document: split, Units: []string{"docs-api", "docs-guide"}})
	tick(t, q)
	got := get(t, ns, "comp")
	p := got.Status.Plan
	if p == nil || p.Edits != 3 || p.Recent[1].Units != 0 || p.Recent[1].Error != "comp/1 (parse) is running; withdraw it after it ends, or steer it" {
		t.Fatalf("a running unit withdrawn: %+v", p)
	}
	if w := states(t, got, "withdrawn"); !slices.Equal(w, []int{2, 3}) || got.Status.Units.Withdrawn != 2 || got.Status.Units.Total != 6 {
		t.Fatalf("withdrawn %v (not report, whose first withdrawal was refused with parse's), units %+v", w, got.Status.Units)
	}
	if w := p.Withdrawn; len(w) != 2 || w["2"] != (c4v1.WorkWithdrawnUnit{Why: "two readers", Replacements: "4-5", Edit: "k3"}) ||
		w["3"] != (c4v1.WorkWithdrawnUnit{Why: "the review covers it", Edit: "k3"}) {
		t.Fatalf("withdrawn %+v", w)
	}
	plan, _ := q.PlanOf(ns + "/comp")
	if !slices.Equal(plan.Units[4].After, []int{1}) || !plan.Units[4].Held || !slices.Equal(plan.Units[3].After, []int{2, 4, 5}) {
		t.Fatalf("docs-api %+v, report after %v", plan.Units[4], plan.Units[3].After)
	}
	// The signed decision is kind plan, names the call and what it withdrew.
	cid, err := c4.Parse(p.Recent[2].Record)
	if err != nil {
		t.Fatal(err)
	}
	data, err := c4view.ReadObject(st, cid)
	if err != nil {
		t.Fatal(err)
	}
	var b provenance.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.DecodeString(b.DSSEEnvelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"decision":"plan"`, `"key":"k3"`, `"withdrawn":"2-3"`, `"first":4`, split} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("record lacks %s: %s", want, payload)
		}
	}

	// Its lead adds a worker, plans again, and the root expands: each
	// after the other in the table.
	w := putDoc(t, st, `{"version": 1, "dependents": [], "units": [{"name": "worker-1"}]}`)
	appendTo(t, ns, "comp", c4v1.WorkAppendRequest{Key: "a1", Document: w, Units: []string{"worker-1"}, Actor: "noor"})
	tick(t, q)
	late := putDoc(t, st, `{"version": 1, "units": [{"name": "late", "held": true, "after": ["worker-1"]}, {"name": "tail", "after": ["report"]}],
		"release": ["docs-api", "tail"]}`)
	planTo(t, ns, "comp", c4v1.WorkPlanRequest{Key: "k4", Document: late, Units: []string{"late", "tail"}})
	tick(t, q)
	lane := putDoc(t, st, `{"version": 1, "units": [{"name": "lane", "work": 60}]}`)
	*now = now.Add(time.Minute)
	finish(t, root, corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4root"), c4v1.AnnotationOutputExpand: lane})
	finish(t, parse, corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4parse")})
	tick(t, q)
	tick(t, q)
	want := []string{"plan", "parse", "docs", "report", "docs-api", "docs-guide", "worker-1", "late", "tail", "lane"}
	if names := planNames(t, q, ns+"/comp"); !slices.Equal(names, want) {
		t.Fatalf("units %v, want %v", names, want)
	}
	got = get(t, ns, "comp")
	if units := podUnits(unitPods(t, ns, "comp")); slices.Contains(units, 2) || slices.Contains(units, 3) || !slices.Contains(units, 4) || slices.Contains(units, 5) || !slices.Contains(units, 8) {
		t.Fatalf("pods of %v: docs-api runs once parse has, and tail after the withdrawn report; withdrawn and held units do not", units)
	}

	// A restart replays the calls, the append and the expansion in their
	// order, from the log; a key listed again is not applied again.
	before := got.Status.UnitStates
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	planTo(t, ns, "comp", c4v1.WorkPlanRequest{Key: "k3", Document: split, Units: []string{"docs-api", "docs-guide"}})
	tick(t, q)
	if names := planNames(t, q, ns+"/comp"); !slices.Equal(names, want) {
		t.Fatalf("after restart units %v, want %v", names, want)
	}
	plan, _ = q.PlanOf(ns + "/comp")
	if plan.Units[2].Withdrawn == nil || !slices.Equal(plan.Units[2].Withdrawn.By, []int{4, 5}) || plan.Units[4].Held || !plan.Units[5].Held ||
		!slices.Equal(plan.Units[7].After, []int{6}) {
		t.Fatalf("after restart: docs %+v docs-api %+v docs-guide %+v late %+v", plan.Units[2], plan.Units[4], plan.Units[5], plan.Units[7])
	}
	got = get(t, ns, "comp")
	if got.Status.Plan.Edits != 4 || got.Status.Units.Total != 10 || got.Status.UnitStates["withdrawn"] != before["withdrawn"] || got.Status.UnitStates["held"] != before["held"] {
		t.Fatalf("after restart plan %+v units %+v states %v, before %v", got.Status.Plan, got.Status.Units, got.Status.UnitStates, before)
	}
}

// A lead's job whose units left are all held is idle, waiting for its
// lead to release them; closed, they never run, and the job finishes. A
// job that is not a lead's takes no plan.
func TestPlanParkedLeadJob(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	leadJob(t, ns, "comp", q, *now)
	plain := job(ns, "noor-graph", "graph", "cpu-"+ns, map[string]string{"estimateSeconds": "60"}, now.Add(48*time.Hour))
	plain.Annotations = map[string]string{c4v1.AnnotationOwner: "noor"}
	if err := k8s.Create(ctx, plain); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	later := putDoc(t, st, `{"version": 1, "units": [{"name": "later", "held": true}, {"name": "after-later", "held": true, "after": ["later"]}]}`)
	planTo(t, ns, "comp", c4v1.WorkPlanRequest{Key: "k1", Document: later, Units: []string{"later", "after-later"}})
	planTo(t, ns, "noor-graph", c4v1.WorkPlanRequest{Key: "k1", Document: later, Units: []string{"later", "after-later"}})
	*now = now.Add(time.Minute)
	finish(t, podOfUnit(t, ns, "comp", 0), corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4root")})
	tick(t, q)
	tick(t, q)
	got := get(t, ns, "comp")
	if got.Status.Phase != "Running" || got.Status.Lead.State != c4v1.LeadIdle {
		t.Fatalf("parked: phase %s lead %+v", got.Status.Phase, got.Status.Lead)
	}
	if c := openCondition(t, got); !strings.Contains(c.Message, "every worker left is planned and held: nothing to place until its lead releases them") {
		t.Fatalf("open %+v", c)
	}
	if p := get(t, ns, "noor-graph").Status.Plan; p == nil || p.Recent[0].Units != 0 ||
		p.Recent[0].Error != "noor-graph is not a lead's job: only a lead plans its own job (c4_plan)" {
		t.Fatalf("a plan for a job that is not a lead's: %+v", p)
	}

	closeJob(t, ns, "comp", c4v1.WorkCloseRequest{Why: "done", Actor: "noor", At: metav1.Time{Time: *now}})
	tick(t, q)
	if got := get(t, ns, "comp"); got.Status.Phase != "Succeeded" || got.Status.Lead.State != c4v1.LeadClosed {
		t.Fatalf("closed: phase %s lead %+v", got.Status.Phase, got.Status.Lead)
	}

	// Signing on: a request naming no record is not carried out.
	q.Roots = x509.NewCertPool()
	planTo(t, ns, "comp", c4v1.WorkPlanRequest{Key: "unsigned", Document: later})
	tick(t, q)
	if p := get(t, ns, "comp").Status.Plan; p.Edits != 1 {
		t.Fatalf("an unsigned plan was carried out: %+v", p)
	}
}
