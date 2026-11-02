package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A host runner has its own unit's key certified at the HUD, on a request
// it signed as itself: the leaf is the unit's attempt and names the
// runner. Another runner, another owner's job, a pod that ended, a request
// with no signature or one taken already are refused.
func TestHostUnitCertificates(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	ca, err := provenance.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	yes := true
	job := func(name, owner string) *c4v1.WorkJob {
		return &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name, UID: types.UID("9c1d2e7f-0000-4000-8000-0000000000" + name[len(name)-2:]),
			Annotations: map[string]string{c4v1.AnnotationOwner: owner}}}
	}
	pod := func(name string, j *c4v1.WorkJob, node string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name, UID: types.UID("uid-" + name),
			Labels:      map[string]string{c4v1.LabelJob: j.Name, c4v1.LabelUnit: "2"},
			Annotations: map[string]string{c4v1.AnnotationAttempt: "1"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: j.Name,
				UID: j.UID, Controller: &yes}}},
			Spec: corev1.PodSpec{NodeName: node}, Status: corev1.PodStatus{Phase: phase}}
	}
	danas, noors := job("port-01", "dana"), job("sync-02", "noor")
	node := func(name, owners string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{c4v1.AnnotationNodeOwners: owners}}}
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(danas, noors,
		pod("port-u2", danas, "laptop-1", corev1.PodRunning), pod("sync-u2", noors, "laptop-1", corev1.PodRunning),
		pod("done-u2", danas, "laptop-1", corev1.PodSucceeded), pod("away-u2", danas, "laptop-2", corev1.PodRunning),
		node("laptop-1", `["dana"]`), node("laptop-2", `["dana"]`)).Build()
	srv := httptest.NewServer(http.StripPrefix("/workload", hostUnitIssuer(&signing{ca: ca, pool: pool}, reader)))
	defer srv.Close()
	runner := func(n string) *provenance.Signer {
		s, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.RunnerIdentity(n), time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	laptop1 := runner("laptop-1")
	pub, _, _ := ed25519.GenerateKey(nil)
	const target = "/workload/v1/unit-certificate"
	ask := func(as *provenance.Signer, podName string, sig *string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"publicKey": []byte(pub), "namespace": "show-a", "pod": podName})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+target, bytes.NewReader(body))
		h := ""
		if as != nil {
			if h, err = provenance.SignRequest(ctx, as, http.MethodPost, target, body, time.Now()); err != nil {
				t.Fatal(err)
			}
			req.Header.Set(schema.RequestHeader, h)
		}
		if sig != nil {
			if *sig != "" {
				req.Header.Set(schema.RequestHeader, *sig)
			}
			*sig = h
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	var sig string
	code, body := ask(laptop1, "port-u2", &sig)
	var out struct {
		Identity string
		Chain    [][]byte
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || len(out.Chain) != 2 {
		t.Fatalf("its own unit: %d %s", code, body)
	}
	leaf, _ := x509.ParseCertificate(out.Chain[0])
	if want := provenance.UnitIdentity("show-a", "port-01", "2", string(danas.UID), 1); out.Identity != want ||
		provenance.CertIdentity(leaf) != want || provenance.CertRunner(leaf) != "laptop-1" || !leaf.PublicKey.(ed25519.PublicKey).Equal(pub) {
		t.Fatalf("leaf %s runner %q", provenance.CertIdentity(leaf), provenance.CertRunner(leaf))
	}
	// the same signed request again
	if code, body := ask(nil, "port-u2", &sig); code != http.StatusUnauthorized || !strings.Contains(body, "taken once") {
		t.Errorf("a replay: %d %s", code, body)
	}
	for name, c := range map[string]struct {
		as   *provenance.Signer
		pod  string
		code int
		why  string
	}{
		"another runner's unit": {runner("laptop-2"), "port-u2", http.StatusForbidden, "is not bound to laptop-2"},
		"another owner's job":   {laptop1, "sync-u2", http.StatusForbidden, `is "noor"'s`},
		"an ended attempt":      {laptop1, "done-u2", http.StatusForbidden, "has ended"},
		"a pod elsewhere":       {laptop1, "away-u2", http.StatusForbidden, "is not bound to laptop-1"},
		"no signature":          {nil, "port-u2", http.StatusUnauthorized, "signing the request as itself"},
		"not a runner": {func() *provenance.Signer {
			s, _ := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.NodeIdentity("w1"), time.Hour))
			return s
		}(), "port-u2", http.StatusUnauthorized, ""},
	} {
		if code, body := ask(c.as, c.pod, nil); code != c.code || !strings.Contains(body, c.why) {
			t.Errorf("%s: %d %s, want %d with %q", name, code, body, c.code, c.why)
		}
	}
}
