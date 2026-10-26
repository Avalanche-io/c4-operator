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

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// appendTo lists append requests on a job, as the HUD writes them.
func appendTo(t *testing.T, ns, name string, reqs ...c4v1.WorkAppendRequest) {
	t.Helper()
	j := get(t, ns, name)
	var list []c4v1.WorkAppendRequest
	_ = json.Unmarshal([]byte(j.Annotations[c4v1.AnnotationAppend]), &list)
	for i, r := range reqs {
		if r.Token == "" {
			r.Token = strconv.FormatInt(time.Now().UnixNano()+int64(i), 36)
		}
		list = append(list, r)
	}
	b, _ := json.Marshal(list)
	if j.Annotations == nil {
		j.Annotations = map[string]string{}
	}
	j.Annotations[c4v1.AnnotationAppend] = string(b)
	if err := k8s.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func planNames(t *testing.T, q *Queue, key string) []string {
	t.Helper()
	p, _ := q.PlanOf(key)
	if p == nil {
		t.Fatalf("no plan of %s", key)
	}
	var out []string
	for _, u := range p.Units {
		out = append(out, u.Name)
	}
	return out
}

// An owner's append adds its units after the job's, with the next
// indexes, under the root; they are released like any other unit. A
// generator's expansion after it, and another append after that, take
// the indexes after theirs, and a restart of the queue rebuilds the same
// table and applies no key twice, even when the request is listed again.
func TestAppendAddsUnits(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	j := job(ns, "noor-lead", "graph", "cpu-"+ns, map[string]string{"estimateSeconds": "60"}, now.Add(4*time.Hour))
	j.Annotations = map[string]string{c4v1.AnnotationOwner: "noor"}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	root := podOfUnit(t, ns, "noor-lead", 0)

	w1 := putDoc(t, st, `{"version": 1, "dependents": [], "units": [
		{"name": "worker-1", "work": 60, "group": "turn 3", "params": {"turn": "3"}},
		{"name": "worker-1-check", "after": ["worker-1"]}]}`)
	appendTo(t, ns, "noor-lead", c4v1.WorkAppendRequest{Key: "k1", Document: w1, Units: []string{"worker-1", "worker-1-check"}, Actor: "noor"})
	tick(t, q)
	got := get(t, ns, "noor-lead")
	if len(got.Status.Appends) != 1 || got.Status.Appends[0] != (c4v1.WorkAppend{Key: "k1", Document: w1, First: 1, Units: 2, At: got.Status.Appends[0].At}) {
		t.Fatalf("appends %+v", got.Status.Appends)
	}
	if got.Status.Units.Total != 3 {
		t.Fatalf("units %+v", got.Status.Units)
	}
	plan, _ := q.PlanOf(ns + "/noor-lead")
	if u := plan.Units[1]; u.Name != "worker-1" || u.Parent != 0 || u.Depth != 1 || u.Group != "turn 3" || len(u.After) != 0 {
		t.Fatalf("worker-1 %+v", u)
	}
	if u := plan.Units[2]; u.Name != "worker-1-check" || !slices.Equal(u.After, []int{1}) {
		t.Fatalf("worker-1-check %+v", u)
	}
	// Released as any unit: worker-1 runs beside the root; its check waits.
	if units := podUnits(unitPods(t, ns, "noor-lead")); !slices.Contains(units, 1) || slices.Contains(units, 2) {
		t.Fatalf("running %v", units)
	}
	if b := states(t, got, "blocked"); !slices.Equal(b, []int{2}) {
		t.Fatalf("blocked %v, states %v", b, got.Status.UnitStates)
	}

	// The root expands after it: its children come after the appended.
	lane := putDoc(t, st, `{"version": 1, "units": [{"name": "lane", "work": 60}]}`)
	*now = now.Add(time.Minute)
	finish(t, root, corev1.PodSucceeded, *now, time.Minute, map[string]string{c4v1.AnnotationOutputTree: viewID("c4root"), c4v1.AnnotationOutputExpand: lane})
	tick(t, q)
	if got := get(t, ns, "noor-lead"); len(got.Status.Expansions) != 1 || got.Status.Expansions[0].First != 3 {
		t.Fatalf("expansions %+v", got.Status.Expansions)
	}
	w2 := putDoc(t, st, `{"version": 1, "dependents": [], "units": [{"name": "worker-2", "after": ["lane"]}]}`)
	appendTo(t, ns, "noor-lead", c4v1.WorkAppendRequest{Key: "k2", Document: w2, Units: []string{"worker-2"}, Actor: "agent/claude for noor"})
	tick(t, q)
	want := []string{"plan", "worker-1", "worker-1-check", "lane", "worker-2"}
	if names := planNames(t, q, ns+"/noor-lead"); !slices.Equal(names, want) {
		t.Fatalf("units %v, want %v", names, want)
	}
	if got := get(t, ns, "noor-lead"); len(got.Status.Appends) != 2 || got.Status.Appends[1].First != 4 || got.Status.Appends[1].Units != 1 {
		t.Fatalf("appends %+v", got.Status.Appends)
	}

	// A restart replays both appends and the expansion in their order, and
	// a key listed again is not applied again.
	q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores}
	appendTo(t, ns, "noor-lead", c4v1.WorkAppendRequest{Key: "k1", Document: w1, Units: []string{"worker-1", "worker-1-check"}, Actor: "noor"})
	tick(t, q)
	if names := planNames(t, q, ns+"/noor-lead"); !slices.Equal(names, want) {
		t.Fatalf("after restart units %v, want %v", names, want)
	}
	plan, _ = q.PlanOf(ns + "/noor-lead")
	if !slices.Equal(plan.Units[4].After, []int{3}) {
		t.Fatalf("after restart worker-2 waits on %v", plan.Units[4].After)
	}
	if got := get(t, ns, "noor-lead"); len(got.Status.Appends) != 2 || got.Status.Units.Total != 5 {
		t.Fatalf("after restart appends %+v units %+v", got.Status.Appends, got.Status.Units)
	}
}

// A refused append adds nothing and says why, in status and in a signed
// decision: agent units on a unit's job (a worker may not start workers),
// a name the job already has, and units for a job that is not a graph.
// With signing on, a request without a record is not applied at all.
func TestAppendRefused(t *testing.T) {
	ns, q, now, st := graphFarm(t)
	ctx := context.Background()
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if q.Signer, err = provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("queue"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	j := job(ns, "ivo-port", "graph", "cpu-"+ns, nil, now.Add(4*time.Hour))
	j.Annotations = map[string]string{c4v1.AnnotationOwner: "ivo", c4v1.AnnotationSubmittedByUnit: ns + "/plan/0"}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	r := job(ns, "dana-render", "render", "cpu-"+ns, map[string]string{"frames": "1-2"}, now.Add(4*time.Hour))
	if err := k8s.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	agents := putDoc(t, st, `{"version": 1, "dependents": [], "units": [
		{"name": "noor-block", "env": {"C4_AGENT_BLOCK": "1"}}, {"name": "plain"}]}`)
	taken := putDoc(t, st, `{"version": 1, "dependents": [], "units": [{"name": "plan"}]}`)
	appendTo(t, ns, "ivo-port", c4v1.WorkAppendRequest{Key: "agents", Document: agents, Units: []string{"noor-block", "plain"}, Actor: "ivo"},
		c4v1.WorkAppendRequest{Key: "taken", Document: taken, Units: []string{"plan"}, Actor: "ivo"})
	appendTo(t, ns, "dana-render", c4v1.WorkAppendRequest{Key: "k", Document: taken, Units: []string{"plan"}, Actor: "dana"})
	tick(t, q)
	got := get(t, ns, "ivo-port")
	if len(got.Status.Appends) != 2 || got.Status.Units.Total != 1 {
		t.Fatalf("appends %+v units %+v", got.Status.Appends, got.Status.Units)
	}
	a := got.Status.Appends[0]
	if a.Units != 0 || a.Record == "" || !strings.HasPrefix(a.Error, c4v1.LeadOnlyMessage) || !strings.Contains(a.Error, "noor-block") ||
		strings.Contains(a.Error, "plain") || !strings.Contains(a.Error, c4v1.LeadOnlyFix) {
		t.Fatalf("agent units on a unit's job: %+v", a)
	}
	if b := got.Status.Appends[1]; b.Units != 0 || !strings.Contains(b.Error, "unit plan is already in the job") {
		t.Fatalf("a taken name: %+v", b)
	}
	if rg := get(t, ns, "dana-render"); len(rg.Status.Appends) != 1 || !strings.Contains(rg.Status.Appends[0].Error, "only a graph job takes units") {
		t.Fatalf("a render job: %+v", rg.Status.Appends)
	}
	// The signed decision is kind append and names the agent units.
	cid, err := c4.Parse(a.Record)
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
	for _, want := range []string{`"decision":"append"`, `"key":"agents"`, `"noor-block"`, `"agents.submit"`, agents} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("record lacks %s: %s", want, payload)
		}
	}

	// Signing on: a request naming no record is left where it is.
	q.Roots = x509.NewCertPool()
	plain := putDoc(t, st, `{"version": 1, "dependents": [], "units": [{"name": "unsigned"}]}`)
	appendTo(t, ns, "ivo-port", c4v1.WorkAppendRequest{Key: "unsigned", Document: plain, Units: []string{"unsigned"}, Actor: "ivo"})
	tick(t, q)
	if got := get(t, ns, "ivo-port"); len(got.Status.Appends) != 2 || got.Status.Units.Total != 1 {
		t.Fatalf("an unsigned append was applied: %+v", got.Status.Appends)
	}
}
