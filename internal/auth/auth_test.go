package auth

import (
	"net/http"
	"strings"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// reviewer answers TokenReview with the service account a token names:
// the token "sa:<namespace>:<name>" is that account's, for the tier.
func reviewer() *Reviewer {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		ns, name, ok := strings.Cut(strings.TrimPrefix(tr.Spec.Token, "sa:"), ":")
		tr.Status = authnv1.TokenReviewStatus{
			Authenticated: ok,
			Audiences:     tr.Spec.Audiences,
			User:          authnv1.UserInfo{Username: "system:serviceaccount:" + ns + ":" + name},
		}
		return true, tr, nil
	})
	return &Reviewer{Client: cs, Tier: "test"}
}

// Each account may use the tier API only with the methods it is listed
// for; an account listed bare may use any; an account not listed, none.
func TestTierAuthorizerMethods(t *testing.T) {
	authorize := reviewer().TierAuthorizer("c4-system/c4-node:GET,HEAD,PUT", "c4-system/c4-manager:GET,HEAD,POST", "c4-system/c4-regional")
	for _, c := range []struct {
		account, method string
		status          int
	}{
		{"c4-node", http.MethodGet, 0},
		{"c4-node", http.MethodPut, 0},
		{"c4-node", http.MethodPost, http.StatusForbidden},
		{"c4-manager", http.MethodHead, 0},
		{"c4-manager", http.MethodPost, 0},
		{"c4-manager", http.MethodPut, http.StatusForbidden},
		{"c4-manager", http.MethodDelete, http.StatusForbidden},
		{"c4-regional", http.MethodPut, 0},
		{"c4-hud", http.MethodGet, http.StatusForbidden},
	} {
		req, _ := http.NewRequest(c.method, "http://tier/", nil)
		req.Header.Set("Authorization", "Bearer sa:c4-system:"+c.account)
		_, status, err := authorize(req)
		if status != c.status {
			t.Errorf("%s %s: status %d (%v), want %d", c.account, c.method, status, err, c.status)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, "http://tier/", nil)
	if _, status, _ := authorize(req); status != http.StatusUnauthorized {
		t.Errorf("no token: %d", status)
	}
}
