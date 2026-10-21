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
		Annotations: map[string]string{c4v1.AnnotationOwner: "joshua"}}}
	yes := true
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: "team", UID: job.UID, Controller: &yes}}
	docs := &fakeDocs{}
	r, c := hostRunner(t, e, p)
	if err := c.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	r.Signer, r.Docs, r.DocsProject, r.Version, r.Farm, r.Owners = sg, docs, "saltmarsh", "test", "saltmarsh", []string{"joshua"}
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
	if ch == nil || ch.Act != "build" || ch.By.Unit != "show-a/team@5c1d7e/3" || ch.By.Agent != "lead" || ch.For.Party != "joshua" ||
		len(ch.Using) != 1 || ch.Using[0].SignIn != "saltmarsh/runner/host" || ch.Using[0].Holder != "joshua" ||
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
