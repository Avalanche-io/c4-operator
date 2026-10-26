package queue

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
	"github.com/Avalanche-io/c4-operator/internal/work/policy"
)

// agentDoc is a generator's expansion into a block, a bundle member and a
// plain unit.
const agentDoc = `{"version": 1, "units": [
	{"name": "noor-block", "env": {"C4_AGENT_BLOCK": "1", "C4_BLOCK_PROMPT": "draft chapter one"}},
	{"name": "ivo-member", "env": {"C4_AGENT_MEMBER": "ivo"}},
	{"name": "plain", "work": 60}]}`

// plainDoc adds no agent units.
const plainDoc = `{"version": 1, "units": [{"name": "deep", "work": 60}, {"name": "beauty", "after": ["deep"]}]}`

// A worker may not start workers: a unit's graph job whose generator
// expands into agent units is refused them while the project's
// agents.submit is lead (the default), in the HUD's words. A person's job,
// a project that lets units submit agent work, and a unit's job that adds
// no agent units all expand as before.
func TestUnitJobExpandsIntoAgentUnits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		byUnit  bool
		submit  string // the project's agents.submit; "" for no project
		doc     string
		refused bool
	}{
		{name: "unit, lead by default", byUnit: true, doc: agentDoc, refused: true},
		{name: "unit, project lead", byUnit: true, submit: policy.SubmitLead, doc: agentDoc, refused: true},
		{name: "person", doc: agentDoc},
		{name: "unit, project anyone", byUnit: true, submit: policy.SubmitAnyone, doc: agentDoc},
		{name: "unit, no agent units", byUnit: true, doc: plainDoc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, q, now, st := graphFarm(t)
			ctx := context.Background()
			ca, err := provenance.NewCA(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if q.Signer, err = provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("queue"), time.Hour)); err != nil {
				t.Fatal(err)
			}
			if tc.submit != "" {
				pr := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-" + ns}, Spec: c4v1.ProjectSpec{Namespaces: []string{ns},
					Store: c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: "file:///tmp/none"}}},
					Work:  &c4v1.ProjectWork{Policy: &c4v1.ProjectPolicy{Values: map[string]string{policy.AgentsSubmit: tc.submit}}}}}
				if err := k8s.Create(ctx, pr); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { k8s.Delete(ctx, pr) })
			}
			j := job(ns, "lanes", "graph", "cpu-"+ns, nil, now.Add(4*time.Hour))
			j.Annotations = map[string]string{c4v1.AnnotationOwner: "dana"}
			if tc.byUnit {
				j.Annotations[c4v1.AnnotationSubmittedByUnit] = ns + "/plan/0"
			}
			if err := k8s.Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			tick(t, q)
			doc := putDoc(t, st, tc.doc)
			*now = now.Add(time.Minute)
			finish(t, podOfUnit(t, ns, "lanes", 0), corev1.PodSucceeded, *now, time.Minute,
				map[string]string{c4v1.AnnotationOutputTree: viewID("c4gen"), c4v1.AnnotationOutputExpand: doc})
			tick(t, q)
			got := get(t, ns, "lanes")
			if len(got.Status.Expansions) != 1 || got.Status.Expansions[0].Record == "" {
				t.Fatalf("expansions %+v", got.Status.Expansions)
			}
			x := got.Status.Expansions[0]
			c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionAgentWork)
			if !tc.refused {
				if x.Error != "" || got.Status.Units.Total != 1+int32(strings.Count(tc.doc, `"name"`)) || c != nil {
					t.Fatalf("not expanded: %+v units %+v condition %+v", x, got.Status.Units, c)
				}
				return
			}
			if x.Error != c4v1.LeadOnlyMessage || x.Units != 0 || got.Status.Units.Total != 1 || got.Status.Phase != "Failed" {
				t.Fatalf("not refused: %s %+v units %+v", got.Status.Phase, x, got.Status.Units)
			}
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "LeadOnly" ||
				!strings.Contains(c.Message, c4v1.LeadOnlyMessage) || !strings.Contains(c.Message, c4v1.LeadOnlyFix) {
				t.Fatalf("condition %+v", c)
			}
			// The signed decision names the agent units, the fix and the
			// policy value that decided. (The queue's clock is the test's,
			// before the signer's certificate, so the payload is read
			// without checking the chain.)
			cid, err := c4.Parse(x.Record)
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
			pred := string(payload)
			for _, want := range []string{`"expand"`, `"noor-block"`, `"ivo-member"`, `"agents.submit"`, c4v1.LeadOnlyFix, ns + "/plan/0"} {
				if !strings.Contains(pred, want) {
					t.Fatalf("record lacks %s: %s", want, pred)
				}
			}
			if strings.Contains(pred, `"plain"`) {
				t.Fatalf("record names a unit that runs no agent: %s", pred)
			}
			// A restart keeps the refusal and its condition.
			q = &Queue{Client: q.Client, Policy: q.Policy, Cfg: q.Cfg, Log: q.Log, Now: q.Now, CaptureWait: q.CaptureWait, Stores: q.Stores, Signer: q.Signer}
			tick(t, q)
			got = get(t, ns, "lanes")
			if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionAgentWork); c == nil || got.Status.Units.Total != 1 {
				t.Fatalf("after restart: units %+v condition %+v", got.Status.Units, c)
			}
		})
	}
}

// A refused verdict fails the review in the same way: a unit's job whose
// review carries an expansion into agent units, while agents.submit is
// lead, has its verdict refused whole. Nothing is added or passed, the
// review fails, the verdict's error and the AgentWork condition carry the
// HUD's words. A project set to anyone, and a person's job, expand.
func TestUnitJobVerdictExpandsIntoAgentUnits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		byUnit  bool
		submit  string // the project's agents.submit; "" for no project
		refused bool
	}{
		{name: "unit, lead by default", byUnit: true, refused: true},
		{name: "unit, project anyone", byUnit: true, submit: policy.SubmitAnyone},
		{name: "person"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, q, now, st := graphFarm(t)
			ctx := context.Background()
			ca, err := provenance.NewCA(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if q.Signer, err = provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("queue"), time.Hour)); err != nil {
				t.Fatal(err)
			}
			if tc.submit != "" {
				pr := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-" + ns}, Spec: c4v1.ProjectSpec{Namespaces: []string{ns},
					Store: c4v1.ProjectStore{Shards: []c4v1.StoreShard{{Name: "s0", URL: "file:///tmp/none"}}},
					Work:  &c4v1.ProjectWork{Policy: &c4v1.ProjectPolicy{Values: map[string]string{policy.AgentsSubmit: tc.submit}}}}}
				if err := k8s.Create(ctx, pr); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { k8s.Delete(ctx, pr) })
			}
			j := job(ns, "qc", "graph", "cpu-"+ns, nil, now.Add(8*time.Hour))
			j.Annotations = map[string]string{c4v1.AnnotationOwner: "dana"}
			if tc.byUnit {
				j.Annotations[c4v1.AnnotationSubmittedByUnit] = ns + "/plan/0"
			}
			if err := k8s.Create(ctx, j); err != nil {
				t.Fatal(err)
			}
			tick(t, q)
			// The root expands into a frame and a review after it, no agents.
			finishUnit(t, now, ns, "qc", 0, map[string]string{c4v1.AnnotationOutputExpand: putDoc(t, st,
				`{"version": 1, "units": [{"name": "f1", "label": "1001"}, {"name": "review", "after": ["f1"]}]}`)})
			tick(t, q)
			finishUnit(t, now, ns, "qc", 1, nil)
			tick(t, q)
			*now = now.Add(time.Minute)
			finishUnit(t, now, ns, "qc", 2, map[string]string{c4v1.AnnotationOutputVerdict: putDoc(t, st,
				`{"version": 1, "verdict": "pass", "items": [{"do": "expand", "expand": `+agentDoc+`}]}`)})
			tick(t, q)
			got := get(t, ns, "qc")
			if len(got.Status.Verdicts) != 1 {
				t.Fatalf("verdicts %+v", got.Status.Verdicts)
			}
			vd := got.Status.Verdicts[0]
			c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionAgentWork)
			if !tc.refused {
				if vd.Error != "" || vd.Expanded != 3 || len(got.Status.Expansions) != 2 || got.Status.Units.Total != 3+3 || c != nil {
					t.Fatalf("not expanded: %+v expansions %+v units %+v condition %+v", vd, got.Status.Expansions, got.Status.Units, c)
				}
				return
			}
			if !strings.Contains(vd.Error, c4v1.LeadOnlyMessage) || vd.Expanded != 0 || vd.Passed != 0 ||
				len(got.Status.Expansions) != 1 || got.Status.Units.Total != 3 ||
				!slices.Equal(states(t, got, "failed"), []int{2}) || got.Status.Phase != "Failed" {
				t.Fatalf("not refused: %s %+v expansions %+v units %+v states %v", got.Status.Phase, vd, got.Status.Expansions, got.Status.Units, got.Status.UnitStates)
			}
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "LeadOnly" ||
				!strings.Contains(c.Message, c4v1.LeadOnlyMessage) || !strings.Contains(c.Message, c4v1.LeadOnlyFix) ||
				!strings.Contains(c.Message, "unit review") {
				t.Fatalf("condition %+v", c)
			}
			// The signed verdict record names the agent units, the fix and
			// the policy value that decided, as a generator's expand does.
			cid, err := c4.Parse(vd.Record)
			if err != nil {
				t.Fatalf("verdict record %q: %v", vd.Record, err)
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
			pred := string(payload)
			for _, want := range []string{`"verdict"`, `"agentUnits"`, `"noor-block"`, `"agents.submit"`, c4v1.LeadOnlyFix} {
				if !strings.Contains(pred, want) {
					t.Fatalf("verdict record lacks %s: %s", want, pred)
				}
			}
		})
	}
}
