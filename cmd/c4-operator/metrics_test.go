package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// The operator's own /metrics goes only to a caller the API server lets
// get it.
func TestOperatorMetricsGuard(t *testing.T) {
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		tr := a.(ktesting.CreateAction).GetObject().(*authnv1.TokenReview)
		if len(tr.Spec.Audiences) != 0 || (tr.Spec.Token != "scraper" && tr.Spec.Token != "nobody") {
			return true, &authnv1.TokenReview{}, nil
		}
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true,
			User: authnv1.UserInfo{Username: "system:serviceaccount:monitoring:" + tr.Spec.Token}}}, nil
	})
	kube.PrependReactor("create", "subjectaccessreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		sar := a.(ktesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		sar.Status.Allowed = sar.Spec.User == "system:serviceaccount:monitoring:scraper" && sar.Spec.NonResourceAttributes.Path == "/metrics"
		return true, sar, nil
	})
	filter, err := metricsGuard(kube)(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := filter(logr.Discard(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	if err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]int{"": 401, "forged": 401, "nobody": 403, "scraper": 200} {
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%q: %d, want %d", token, w.Code, want)
		}
		// A refusal says which role a scraper needs.
		if want != 200 && !strings.Contains(w.Body.String(), "c4-metrics-reader") {
			t.Errorf("%q: the refusal names no role: %s", token, w.Body.String())
		}
	}
}
