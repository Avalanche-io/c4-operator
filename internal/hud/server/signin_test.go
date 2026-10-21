package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// noAccess answers every SubjectAccessReview with no.
func noAccess(kube *kfake.Clientset) *kfake.Clientset {
	kube.PrependReactor("create", "subjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{}, nil
	})
	return kube
}

// apiToken is a bearer token whose claims name aud (none for "").
func apiToken(sub, aud string) string {
	c := `{"sub":"` + sub + `"}`
	if aud != "" {
		c = `{"aud":["` + aud + `"],"sub":"` + sub + `"}`
	}
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(c)) + ".sig"
}

// A client may sign in with a token minted for the HUD alone (audience
// c4.avalanche.io/hud-api), which the API server would refuse, so a HUD
// that leaked it would leak nothing it could use elsewhere. The review
// asks for that audience. A token for the API server (a person's sign-in)
// is still reviewed at the API server's own audience.
func TestSignInWithAHUDToken(t *testing.T) {
	s := farm(t)
	s.DevViewers = nil
	kube := noAccess(kfake.NewSimpleClientset())
	kube.PrependReactor("create", "tokenreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		tr := a.(ktesting.CreateAction).GetObject().(*authnv1.TokenReview)
		claims, _ := base64.RawURLEncoding.DecodeString(strings.Split(tr.Spec.Token, ".")[1])
		forHUDAPI := strings.Contains(string(claims), c4v1.HUDAPIAudience)
		// The API server accepts a token only for an audience the
		// review names (its own when the review names none).
		switch {
		case forHUDAPI && slices.Contains(tr.Spec.Audiences, c4v1.HUDAPIAudience):
			return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true, Audiences: []string{c4v1.HUDAPIAudience},
				User: authnv1.UserInfo{Username: "system:serviceaccount:c4-system:c4-sync"}}}, nil
		case !forHUDAPI && len(tr.Spec.Audiences) == 0:
			return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true,
				User: authnv1.UserInfo{Username: "alice"}}}, nil
		}
		return true, &authnv1.TokenReview{}, nil
	})
	s.Kube = kube
	for _, c := range []struct{ tok, want string }{
		{apiToken("c4-sync", c4v1.HUDAPIAudience), "system:serviceaccount:c4-system:c4-sync"},
		{apiToken("alice", ""), "alice"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
		r.Header.Set("Authorization", "Bearer "+c.tok)
		v, err := s.viewer(r)
		if err != nil || v.User != c.want {
			t.Errorf("signed in as %v (err %v), want %s", v, err, c.want)
		}
	}
}

// X-Forwarded-User names the viewer only on a request that carries the
// proxy's secret: anyone who could reach the HUD past the proxy could
// otherwise sign in as anyone by sending the header.
func TestProxyHeadersNeedTheProxysSecret(t *testing.T) {
	s := farm(t)
	s.DevViewers = nil
	s.Kube = noAccess(kfake.NewSimpleClientset())
	s.TrustProxy = true
	s.ProxySecret = []byte("the-proxys-secret")
	get := func(secret string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
		r.Header.Set("X-Forwarded-User", "mallory")
		if secret != "" {
			r.Header.Set(ProxySecretHeader, secret)
		}
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if code := get(""); code != http.StatusUnauthorized {
		t.Errorf("X-Forwarded-User with no secret got %d, want 401", code)
	}
	if code := get("a-guess"); code != http.StatusUnauthorized {
		t.Errorf("X-Forwarded-User with the wrong secret got %d, want 401", code)
	}
	if code := get("the-proxys-secret"); code != http.StatusOK {
		t.Errorf("X-Forwarded-User with the proxy's secret got %d, want 200", code)
	}
}

// A development farm's stand-in tracker is served at /sandbox/ as it is,
// with nothing of the person's sign-in; a farm without development viewers
// serves no sandbox, whatever it is given.
func TestSandboxBehindTheHUD(t *testing.T) {
	var got *http.Request
	sb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte("sandbox"))
	}))
	defer sb.Close()
	s := farm(t)
	s.Web = fstest.MapFS{"index.html": {Data: []byte("hud")}}
	s.Sandbox, _ = url.Parse(sb.URL)
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/sandbox/issues/3?as=artist", nil)
		r.Header.Set("Authorization", "Bearer the-persons-token")
		r.Header.Set("Cookie", "session=x")
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := get(); w.Body.String() != "sandbox" || got == nil || got.URL.Path != "/sandbox/issues/3" || got.URL.RawQuery != "as=artist" ||
		got.Header.Get("Authorization") != "" || got.Header.Get("Cookie") != "" {
		t.Fatalf("proxied: %d %q %+v", w.Code, w.Body.String(), got)
	}
	got = nil
	s.DevViewers = nil
	if w := get(); got != nil || w.Body.String() == "sandbox" {
		t.Fatalf("a farm without development viewers served the sandbox: %d", w.Code)
	}
}
