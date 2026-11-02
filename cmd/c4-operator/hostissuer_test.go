package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// The HUD serves the workload issuer to host runners, which reach the farm
// only at its address: a host runner's own token (audience c4-signer) gets
// its runner/<node> certificate there, and any other account, an importer
// included, is refused and asks the issuer inside the cluster.
func TestHostIssuerCertifiesOnlyHostRunners(t *testing.T) {
	now := time.Now()
	ca, err := provenance.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	show := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-a"}, Spec: c4v1.ProjectSpec{
		Namespaces: []string{"show-a"}, Importers: []c4v1.Importer{{Namespace: "show-a", ServiceAccount: "import-git"}}}}
	projects := fake.NewClientBuilder().WithScheme(scheme).WithObjects(show).Build()
	accounts := map[string]string{"runner": "system:serviceaccount:c4-hosts:laptop-1", "importer": "system:serviceaccount:show-a:import-git"}
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		tr := a.(ktesting.CreateAction).GetObject().(*authnv1.TokenReview)
		user, ok := accounts[tr.Spec.Token]
		if !ok || len(tr.Spec.Audiences) != 1 || tr.Spec.Audiences[0] != AudienceSigner {
			return true, &authnv1.TokenReview{}, nil
		}
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true, Audiences: tr.Spec.Audiences,
			User: authnv1.UserInfo{Username: user}}}, nil
	})
	srv := httptest.NewServer(http.StripPrefix("/workload", hostIssuer(&signing{ca: ca}, kube, projects, "c4-system")))
	defer srv.Close()
	as := func(token string) provenance.IssueFunc {
		return provenance.RemoteIssuer(srv.URL+"/workload", func() (string, error) { return token, nil }, nil)
	}
	s, err := provenance.NewSigner(context.Background(), as("runner"))
	if err != nil || s.Identity() != provenance.RunnerIdentity("laptop-1") {
		t.Fatalf("the host runner: %v", err)
	}
	for _, token := range []string{"importer", "forged"} {
		if _, err := provenance.NewSigner(context.Background(), as(token)); err == nil {
			t.Errorf("%s was certified at the HUD", token)
		}
	}
}
