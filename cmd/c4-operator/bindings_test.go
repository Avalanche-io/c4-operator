package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/bindings"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
)

func testBinding(name, owner, to, record string, start time.Time) *c4v1.DeploymentBinding {
	return &c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: c4v1.DeploymentBindingSpec{
		Owner: owner, To: to, For: []string{"task/v1"}, Start: metav1.NewTime(start), By: owner, Record: record}}
}

// The workload issuer certifies a deployment under the binding in force
// for it: the most specific, matched byte for byte, else the farm
// binding. Its leaf names the binding and never outlives the binding's
// end; a binding ended early stops certifying under it at once; an
// identity no binding covers is still certified, naming none.
func TestIssuerCertifiesUnderTheBindingInForce(t *testing.T) {
	now := time.Now()
	ca, err := provenance.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	farmID, runnerID, tenID := provtest.ID("the farm binding"), provtest.ID("laptop-1's binding"), provtest.ID("laptop-10's binding")
	runner := testBinding("runner-laptop-1", "noor", "studio/runner/laptop-1", runnerID, now.Add(-time.Minute))
	end := metav1.NewTime(now.Add(30 * time.Minute).Truncate(time.Second))
	runner.Spec.End = &end
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		tr := a.(ktesting.CreateAction).GetObject().(*authnv1.TokenReview)
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true, Audiences: tr.Spec.Audiences,
			User: authnv1.UserInfo{Username: "system:serviceaccount:c4-hosts:laptop-1"}}}, nil
	})
	sig := &signing{ca: ca}
	sig.useBindings(&bindings.Resolver{Client: reader})
	srv := httptest.NewServer(http.StripPrefix("/workload", hostIssuer(sig, kube, reader, "c4-system")))
	defer srv.Close()
	leaf := func() *provenanceLeaf {
		t.Helper()
		s, err := provenance.NewSigner(context.Background(), provenance.RemoteIssuer(srv.URL+"/workload", func() (string, error) { return "runner", nil }, nil))
		if err != nil {
			t.Fatal(err)
		}
		return leafOf(t, s)
	}

	if l := leaf(); l.binding != "" {
		t.Errorf("no binding yet, and the leaf names %q", l.binding)
	}
	for _, o := range []client.Object{testBinding("farm", "dana", "studio/*", farmID, now.Add(-time.Hour)), runner,
		testBinding("runner-laptop-10", "lee", "studio/runner/laptop-10", tenID, now.Add(-time.Minute))} {
		if err := reader.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	l := leaf()
	if l.binding != runnerID {
		t.Errorf("runner/laptop-1's leaf names %q, not its own binding", l.binding)
	}
	if l.notAfter.After(end.Time) {
		t.Errorf("the leaf ends %s, past its binding's end %s", l.notAfter, end.Time)
	}
	runner.Ended = &c4v1.DeploymentBindingEnd{At: metav1.NewTime(now), By: "dana", Record: provtest.ID("its end")}
	if err := reader.Update(context.Background(), runner); err != nil {
		t.Fatal(err)
	}
	if l := leaf(); l.binding != farmID {
		t.Errorf("after its binding ended, the leaf names %q, not the farm binding", l.binding)
	}
}

// Today nothing changes: with only the farm binding, every identity the
// farm certifies is bound by it, a node, a host runner, a unit, an
// importer and each of the manager's components; and the components,
// certified in this process before the install bound the farm, re-certify
// under it once it is.
func TestTodayEveryIdentityStaysBound(t *testing.T) {
	now := time.Now()
	ca, err := provenance.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	sig := &signing{ca: ca}
	hud, err := provenance.NewSigner(context.Background(), provenance.BoundLocalIssuer(ca, provenance.ComponentIdentity("hud"), 24*time.Hour, sig.bind))
	if err != nil {
		t.Fatal(err)
	}
	sig.hud = hud
	defer func(d time.Duration) { componentRecheck = d }(componentRecheck)
	componentRecheck = 0
	r := &bindings.Resolver{Client: reader}
	sig.useBindings(r)
	if l := leafOf(t, hud); l.binding != "" {
		t.Fatalf("the HUD's leaf names %q before the farm is bound", l.binding)
	}
	farmID := provtest.ID("the farm binding")
	if err := reader.Create(context.Background(), testBinding("farm", "dana", "studio/*", farmID, now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{provenance.NodeIdentity("laptop-1"), provenance.RunnerIdentity("laptop-1"),
		provenance.UnitIdentity("show-a", "sh040", "u-1", "9b2e4d71", 1), provenance.ServiceAccountIdentity("show-a", "import-git"),
		provenance.ComponentIdentity("hud"), provenance.ComponentIdentity("queue"), provenance.ComponentIdentity("controller")} {
		b, err := sig.bind(context.Background(), id)
		if err != nil || b.ID != farmID {
			t.Errorf("%s is bound by %q (%v), not the farm binding", id, b.ID, err)
		}
	}
	if l := leafOf(t, hud); l.binding != farmID {
		t.Errorf("the HUD's leaf names %q once the farm is bound, not the farm binding", l.binding)
	}
}

type provenanceLeaf struct {
	binding  string
	notAfter time.Time
}

// leafOf signs once as s and reads back the leaf it signed with.
func leafOf(t *testing.T, s *provenance.Signer) *provenanceLeaf {
	t.Helper()
	rec, err := provenance.SignStatement(context.Background(), s, provenance.PredicateControl, []provenance.Subject{{Name: "x", Digest: map[string]string{"sha256": "00"}}},
		map[string]any{"action": "probe"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var b provenance.Bundle
	if err := json.Unmarshal(rec, &b); err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(b.VerificationMaterial.X509CertificateChain.Certificates[0].RawBytes)
	if err != nil {
		t.Fatal(err)
	}
	return &provenanceLeaf{binding: provenance.CertBinding(leaf), notAfter: leaf.NotAfter}
}

// Each component re-checks its binding at its own pace: the controller,
// queue and HUD signing in turn each re-certify under the farm binding at
// their first signature after it, none starved by another's check.
func TestEachComponentRechecksItsOwnBinding(t *testing.T) {
	now := time.Now()
	ca, err := provenance.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	sig := &signing{ca: ca}
	for name, dst := range map[string]**provenance.Signer{"controller": &sig.controller, "queue": &sig.queue, "hud": &sig.hud} {
		if *dst, err = provenance.NewSigner(context.Background(), provenance.BoundLocalIssuer(ca, provenance.ComponentIdentity(name), 24*time.Hour, sig.bind)); err != nil {
			t.Fatal(err)
		}
	}
	defer func(d time.Duration) { componentRecheck = d }(componentRecheck)
	componentRecheck = time.Hour
	farmID := provtest.ID("the farm binding")
	if err := reader.Create(context.Background(), testBinding("farm", "dana", "studio/*", farmID, now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	sig.useBindings(&bindings.Resolver{Client: reader})
	for name, s := range map[string]*provenance.Signer{"controller": sig.controller, "queue": sig.queue, "hud": sig.hud} {
		if l := leafOf(t, s); l.binding != farmID {
			t.Errorf("the %s's leaf names %q, not the farm binding", name, l.binding)
		}
	}
}

// Bindings that can't be read never stop the farm signing: the issuer
// certifies the identity unbound, as one no binding covers, so its
// records are labelled, never attributed to a binding that may have ended.
func TestUnreadableBindingsCertifyUnbound(t *testing.T) {
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		tr := a.(ktesting.CreateAction).GetObject().(*authnv1.TokenReview)
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true, Audiences: tr.Spec.Audiences,
			User: authnv1.UserInfo{Username: "system:serviceaccount:c4-hosts:laptop-1"}}}, nil
	})
	// a reader that doesn't know DeploymentBindings: every list fails
	reader := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	sig := &signing{ca: ca}
	sig.useBindings(&bindings.Resolver{Client: reader})
	if _, err := sig.bind(context.Background(), provenance.RunnerIdentity("laptop-1")); err == nil {
		t.Fatal("the bindings read")
	}
	srv := httptest.NewServer(http.StripPrefix("/workload", hostIssuer(sig, kube, reader, "c4-system")))
	defer srv.Close()
	s, err := provenance.NewSigner(context.Background(), provenance.RemoteIssuer(srv.URL+"/workload", func() (string, error) { return "runner", nil }, nil))
	if err != nil {
		t.Fatalf("refused while the bindings can't be read: %v", err)
	}
	if l := leafOf(t, s); l.binding != "" {
		t.Errorf("the leaf names %q", l.binding)
	}
}
