package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A unit pod a host runner ends gets a task record signed as the runner's
// node: what it left (its output view and session checkpoint), stored in
// the project's store and named on the pod (output-provenance), as a node
// daemon records a pod's capture. It verifies against the farm's root, and
// its signer is the node the record says.
func TestHostRunnerSignsTaskRecords(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m := e.register("lead", sess)
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sg, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.RunnerIdentity("host"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	view := c4.Identify(strings.NewReader("the session's view"))
	head := c4.Identify(strings.NewReader("the chain head"))
	p := hostPod()
	p.Labels[c4v1.LabelUnit] = "3"
	p.Annotations[c4v1.AnnotationOutputTree] = view.String()
	p.Annotations[c4v1.AnnotationOutputCheckpoint] = m.Session + "#4:" + head.String()
	job := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "team", UID: "5c1d7e00-0000-4000-8000-000000000001",
		Annotations: map[string]string{c4v1.AnnotationOwner: "noor"}}}
	yes := true
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: "team", UID: job.UID, Controller: &yes}}
	docs := &fakeDocs{}
	r, c := hostRunner(t, e, p)
	if err := c.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	r.Signer, r.Docs, r.DocsProject, r.Version, r.Farm, r.Owners = sg, docs, "saltmarsh", "test", "saltmarsh", []string{"noor"}
	if err := r.finish(ctx, getPod(t, c), corev1.PodSucceeded, "Completed", "done", 0); err != nil {
		t.Fatal(err)
	}
	got := getPod(t, c)
	rec := got.Annotations[c4v1.AnnotationOutputProvenance]
	if rec == "" || rec != docs.id.String() || docs.project != "saltmarsh" {
		t.Fatalf("record %q, stored %s in %q", rec, docs.id, docs.project)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	v, err := provenance.VerifyBundle(docs.data, roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := provenance.CheckSigner(v); err != nil {
		t.Fatalf("signer: %v", err)
	}
	var pred provenance.TaskPredicate
	if err := json.Unmarshal(v.Predicate, &pred); err != nil {
		t.Fatal(err)
	}
	if pred.Builder.ID != hostBuilder || pred.Runtime.Node != "host" || pred.Invocation.Job != "team" || pred.Invocation.Unit != "3" ||
		pred.Output.View != view.String() || pred.Invocation.Project != "saltmarsh" {
		t.Fatalf("predicate %+v", pred)
	}
	ch := pred.Chain
	if ch == nil || ch.Act != "build" || ch.By.Unit != "show-a/team@5c1d7e/3" || ch.By.Agent != "lead" || ch.For.Party != "noor" ||
		len(ch.Using) != 1 || ch.Using[0].SignIn != "saltmarsh/runner/host" || ch.Using[0].Holder != "noor" ||
		len(ch.On) != 1 || ch.On[0].Deployment != "saltmarsh/runner/host" || ch.Accountable || ch.Gap != "deployment saltmarsh/runner/host has no binding" {
		t.Fatalf("chain %+v", ch)
	}
	if b, _ := json.Marshal(ch); true {
		if _, errs := schema.ParseChain(b); len(errs) > 0 {
			t.Fatalf("the chain is not chain/1: %v", errs)
		}
	}
	// once: ending the pod again signs nothing new
	if err := r.finish(ctx, getPod(t, c), corev1.PodSucceeded, "Completed", "done", 0); err != nil {
		t.Fatal(err)
	}
	if getPod(t, c).Annotations[c4v1.AnnotationOutputProvenance] != rec {
		t.Fatal("signed twice")
	}
}

// The holder is the runner's, never the job's: a runner shared by two
// owners shows dana's job using noor's sign-in, the fact the chain exists
// to show. With several owners and no --holder, the chain says it cannot
// tell. Every chain, the fallbacks too, parses as chain/1.
func TestChainHolderIsTheRunners(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	yes := true
	job := func(name, owner string) *c4v1.WorkJob {
		ann := map[string]string{}
		if owner != "" {
			ann[c4v1.AnnotationOwner] = owner
		}
		return &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name, UID: types.UID("5c1d7e00-0000-4000-8000-00000000000" + name[len(name)-1:]), Annotations: ann}}
	}
	podOf := func(j *c4v1.WorkJob) *corev1.Pod {
		p := hostPod()
		p.Labels[c4v1.LabelUnit] = "2"
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: j.Name, UID: j.UID, Controller: &yes}}
		return p
	}
	danas, nobodys := job("dana-1", "dana"), job("none-2", "")
	r, c := hostRunner(t, e, hostPod())
	for _, j := range []*c4v1.WorkJob{danas, nobodys} {
		if err := c.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	r.Farm, r.Node, r.Owners, r.Holder = "saltmarsh", "laptop-1", []string{"noor", "dana"}, "noor"
	valid := func(name string, ch *schema.Chain) {
		t.Helper()
		b, _ := json.Marshal(ch)
		if _, errs := schema.ParseChain(b); len(errs) > 0 {
			t.Errorf("%s: not chain/1: %v\n%s", name, errs, b)
		}
	}

	ch := r.chainOf(ctx, podOf(danas))
	if ch.For.Party != "dana" || ch.Using[0].Holder != "noor" || ch.On[0].Owner != "noor" {
		t.Errorf("shared runner: %+v", ch)
	}
	valid("shared runner", ch)

	r.Holder = ""
	if ch := r.chainOf(ctx, podOf(danas)); ch.Using[0].Holder != "" || ch.Using[0].HolderSubject != "unknown" {
		t.Errorf("two owners, no --holder: %+v", ch.Using)
	}
	r.Owners = []string{"noor"}
	if ch := r.chainOf(ctx, podOf(danas)); ch.Using[0].Holder != "noor" {
		t.Errorf("one owner is the holder: %+v", ch.Using)
	}

	ch = r.chainOf(ctx, podOf(nobodys))
	if ch.For.Subject != "no owner recorded on job show-a/none-2" || ch.Gap != "job show-a/none-2 has no owner" {
		t.Errorf("no owner: %+v", ch)
	}
	valid("no owner", ch)

	loose := hostPod()
	ch = r.chainOf(ctx, loose)
	if ch.By.Deployment != "saltmarsh/runner/laptop-1" || ch.For.Subject == "" {
		t.Errorf("no job: %+v", ch)
	}
	valid("no job", ch)
}

// A lead's worker's chain names the act of its lead that added it: the
// submission that made the lead's job, for its first workers, or the
// append that added it, by its index; a job that is not a lead's names
// none. The chain stays chain/1.
func TestLeadWorkerChainNamesItsAdd(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	yes := true
	id := func(s string) string { return c4.Identify(strings.NewReader(s)).String() }
	first, appended, other := id("the submission"), id("the append"), id("another job's submission")
	lead := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "lead-1a2b3c4d5e", UID: types.UID("5c1d7e00-0000-4000-8000-0000000000aa"),
		Annotations: map[string]string{c4v1.AnnotationOwner: "dana", c4v1.AnnotationLeadSession: "session-1", "c4.avalanche.io/submission-record": first}}}
	plain := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "dana-plain", UID: types.UID("5c1d7e00-0000-4000-8000-0000000000bb"),
		Annotations: map[string]string{c4v1.AnnotationOwner: "dana", "c4.avalanche.io/submission-record": other}}}
	lead.Status.Appends = []c4v1.WorkAppend{{Key: "call-2", First: 4, Units: 2, Control: appended, Record: id("the queue's decision")}}
	r, c := hostRunner(t, e, hostPod())
	for _, j := range []*c4v1.WorkJob{lead, plain} {
		if err := c.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	r.Farm, r.Node, r.Owners, r.Holder = "saltmarsh", "laptop-1", []string{"dana"}, "dana"
	podOf := func(j *c4v1.WorkJob, unit string) *corev1.Pod {
		p := hostPod()
		p.Labels[c4v1.LabelUnit] = unit
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: j.Name, UID: j.UID, Controller: &yes}}
		return p
	}
	added := func(ch *schema.Chain) string {
		for _, d := range ch.Under {
			if d.Kind == "lead-worker" {
				return d.Record
			}
		}
		return ""
	}
	for _, c := range []struct {
		j    *c4v1.WorkJob
		unit string
		want string
	}{{lead, "2", first}, {lead, "5", appended}, {plain, "2", ""}} {
		ch := r.chainOf(ctx, podOf(c.j, c.unit))
		if got := added(ch); got != c.want || ch.Under[0].Job != "show-a/"+c.j.Name {
			t.Errorf("%s unit %s: added by %q, want %q; under %+v", c.j.Name, c.unit, got, c.want, ch.Under)
		}
		b, _ := json.Marshal(ch)
		if _, errs := schema.ParseChain(b); len(errs) > 0 {
			t.Errorf("%s unit %s: not chain/1: %v\n%s", c.j.Name, c.unit, errs, b)
		}
	}
}
