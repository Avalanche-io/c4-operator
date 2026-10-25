package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// testInstallerToken is a test machine's installer token.
const testInstallerToken = "6f3a1c9e2b7d4f80a1c3e5b7d9f1a3c5"

// guarded is the test farm with its development viewers behind the
// installer token.
func guarded(t *testing.T) *Server {
	t.Helper()
	s := farm(t)
	s.DevViewersOpen, s.InstallerToken = false, []byte(testInstallerToken)
	return s
}

// signsIn lets bearer tokens sign in as dana, allowed every write.
func signsIn(s *Server) {
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: "dana"}}}, nil
	})
	kube.PrependReactor("create", "subjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	s.Kube = kube
}

func icall(s *Server, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// A development viewer needs the installer's secret: ?as= without it is
// refused, saying how to get it; a request naming no viewer signs in as
// on any farm (a bearer token), and is no one without one, never the
// wrangler by default. The token itself opens any viewer.
func TestDevViewersNeedTheInstallerToken(t *testing.T) {
	s := guarded(t)
	w := icall(s, http.MethodGet, "/api/v1/snapshot?as=wrangler", nil)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "development viewers need this machine's installer token: run c4 farm open") {
		t.Fatalf("?as= without the token: %d %s", w.Code, w.Body)
	}
	if w := icall(s, http.MethodGet, "/api/v1/snapshot?as=wrangler", map[string]string{InstallerHeader: strings.Repeat("0", 32)}); w.Code != http.StatusUnauthorized {
		t.Errorf("?as= with a wrong token: %d %s", w.Code, w.Body)
	}
	if w := icall(s, http.MethodGet, "/api/v1/snapshot", nil); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "wrangler") {
		t.Errorf("no ?as=, no sign-in: %d %s", w.Code, w.Body)
	}
	if v, err := s.viewer(httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)); err == nil || v != nil {
		t.Errorf("no ?as=, no sign-in: %+v %v, want no one", v, err)
	}

	signsIn(s)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	r.Header.Set("Authorization", "Bearer person-token")
	if v, err := s.viewer(r); err != nil || v.User != "dana" || isDev(v) {
		t.Errorf("a bearer token on a development farm: %+v %v", v, err)
	}
	if got := s.authorization(nil, "patch", "workjobs", "show-a", "x"); strings.Contains(got, "development") {
		t.Errorf("a signed-in viewer's authorization: %s", got)
	}

	r = httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?as=artist", nil)
	r.Header.Set(InstallerHeader, testInstallerToken)
	if v, err := s.viewer(r); err != nil || v.User != "maya" || v.DevBy != byInstallerToken {
		t.Errorf("?as=artist with the token: %+v %v", v, err)
	}
	if s.DevViewers["artist"].DevBy != "" {
		t.Error("marking the viewer changed the shared one")
	}
	if w := icall(s, http.MethodGet, "/api/v1/snapshot?as=wrangler", map[string]string{InstallerHeader: testInstallerToken}); w.Code != http.StatusOK {
		t.Errorf("the snapshot with the token: %d %s", w.Code, w.Body)
	}
}

// With open development viewers (tests, demos) anyone is any viewer, as
// before the token: no ?as= is the wrangler, and a browser's write needs
// no header.
func TestOpenDevViewers(t *testing.T) {
	s := farm(t)
	if !s.DevViewersOpen {
		t.Fatal("the test farm's viewers are open")
	}
	v, err := s.viewer(httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil))
	if err != nil || v.User != "dana" || v.DevBy != devOpen {
		t.Errorf("no ?as= on an open farm: %+v %v", v, err)
	}
	if v, err := s.viewer(httptest.NewRequest(http.MethodPost, "/api/v1/jobs?as=artist", nil)); err != nil || v.User != "maya" {
		t.Errorf("?as=artist on an open farm: %+v %v", v, err)
	}
}

// The browser's way in: the token asks for a single-use code (a cookie is
// not enough), and /installer trades it for this farm's cookie, derived
// from the token and never the token, then sends the browser on to a path
// on this HUD only.
func TestInstallerCodeAndCookie(t *testing.T) {
	s := guarded(t)
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	cookie := s.installerCookieName() + "=" + s.installerCookieValue()
	if w := icall(s, http.MethodPost, "/api/v1/installer/code", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a code without the token: %d %s", w.Code, w.Body)
	}
	if w := icall(s, http.MethodPost, "/api/v1/installer/code", map[string]string{"Cookie": cookie, HUDHeader: "1"}); w.Code != http.StatusUnauthorized {
		t.Errorf("a code on the cookie alone: %d %s", w.Code, w.Body)
	}
	code := func() string {
		t.Helper()
		w := icall(s, http.MethodPost, "/api/v1/installer/code", map[string]string{InstallerHeader: testInstallerToken})
		if w.Code != http.StatusOK {
			t.Fatalf("a code with the token: %d %s", w.Code, w.Body)
		}
		var out struct{ Code string }
		if err := jsonDecode(w, &out); err != nil || len(out.Code) < 16 {
			t.Fatalf("code %q %v", out.Code, err)
		}
		return out.Code
	}

	for _, c := range []struct{ next, want string }{
		{"/#/job/ns/n", "/#/job/ns/n"},
		{"/needs?x=1#/me", "/needs?x=1#/me"},
		{"//evil.example", "/"},
		{"/\\evil.example", "/"},
		{"https://x.example", "/"},
		{"/\t/evil.example", "/"},
		{"", "/"},
	} {
		w := icall(s, http.MethodGet, "/installer?code="+code()+"&next="+url.QueryEscape(c.next), nil)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != c.want {
			t.Errorf("next %q: %d to %q, want %q", c.next, w.Code, w.Header().Get("Location"), c.want)
		}
		got := w.Result().Cookies()
		if len(got) != 1 {
			t.Fatalf("cookies %v", got)
		}
		ck := got[0]
		if !strings.HasPrefix(ck.Name, "c4_installer_") || len(ck.Name) != len("c4_installer_")+8 || ck.Value != s.installerCookieValue() ||
			strings.Contains(ck.Value, testInstallerToken) || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" ||
			ck.MaxAge != 365*24*3600 {
			t.Errorf("the cookie %+v", ck)
		}
	}

	// A code is good once, and for two minutes.
	once := code()
	if w := icall(s, http.MethodGet, "/installer?code="+once, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d", w.Code)
	}
	if w := icall(s, http.MethodGet, "/installer?code="+once, nil); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Run c4 farm open again") || len(w.Result().Cookies()) != 0 {
		t.Errorf("second use: %d %s", w.Code, w.Body)
	}
	late := code()
	now = now.Add(2*time.Minute + time.Second)
	if w := icall(s, http.MethodGet, "/installer?code="+late, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("an expired code: %d %s", w.Code, w.Body)
	}
	if w := icall(s, http.MethodGet, "/installer?code=nope", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("an unknown code: %d", w.Code)
	}
	// Only a few are outstanding at once.
	for range 3 * installerCodesMax {
		code()
	}
	if n := len(s.installerCodes); n > installerCodesMax {
		t.Errorf("%d codes outstanding", n)
	}

	// The cookie opens a development viewer for reading; another farm's
	// cookie does not.
	if w := icall(s, http.MethodGet, "/api/v1/snapshot?as=wrangler", map[string]string{"Cookie": cookie}); w.Code != http.StatusOK {
		t.Errorf("a GET on the cookie: %d %s", w.Code, w.Body)
	}
	other := &Server{InstallerToken: []byte(strings.Repeat("ab", 16))}
	if w := icall(s, http.MethodGet, "/api/v1/snapshot?as=wrangler", map[string]string{"Cookie": other.installerCookieName() + "=" + other.installerCookieValue()}); w.Code != http.StatusUnauthorized {
		t.Errorf("another farm's cookie: %d", w.Code)
	}
	if w := icall(s, http.MethodGet, "/api/v1/snapshot?as=wrangler", map[string]string{"Cookie": s.installerCookieName() + "=" + testInstallerToken}); w.Code != http.StatusUnauthorized {
		t.Errorf("the token as the cookie's value: %d", w.Code)
	}
}

// A write the cookie alone authorizes must come from the HUD's own page
// (X-C4-HUD), which another site's page cannot send without a preflight
// the HUD never answers. With it, it is the installer's, graded claimed.
func TestCookieWritesNeedTheHUDHeader(t *testing.T) {
	s, roots := bindFarm(t)
	cookie := s.installerCookieName() + "=" + s.installerCookieValue()
	body := `{"owner": "noor", "to": "c4-local/*"}`
	code, out := bindCall(s, http.MethodPost, "wrangler", body, map[string]string{InstallerHeader: "", "Cookie": cookie})
	if code != http.StatusForbidden || out["error"] != "a write from the browser needs the HUD's own page" {
		t.Fatalf("a cookie-only write: %d %v", code, out)
	}
	code, out = bindCall(s, http.MethodPost, "wrangler", body, map[string]string{InstallerHeader: "", "Cookie": cookie, HUDHeader: "1"})
	if code != http.StatusOK {
		t.Fatalf("a cookie write from the HUD's page: %d %v", code, out)
	}
	p := bindRecord(t, s, roots)
	if p.Custody != CustodyClaimed || !strings.Contains(p.Authenticated, "installer's secret on this machine (claimed)") {
		t.Errorf("the record of a cookie write: %+v", p)
	}
	// No CORS: nothing answers a preflight with Access-Control-Allow-*.
	w := icall(s, http.MethodOptions, "/api/v1/farm/binding", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
	for k := range w.Header() {
		if strings.HasPrefix(k, "Access-Control-") {
			t.Errorf("a preflight answered %s", k)
		}
	}
}

// onePerson needs a HUD guarded by an installer token: binding with it is
// refused elsewhere, a one-person binding does not make its owner every
// role there, and the binding's view says whether it holds, and why.
func TestOnePersonNeedsTheInstallerToken(t *testing.T) {
	s, _ := bindFarm(t)
	ctx := context.Background()
	s.DevViewers, s.InstallerToken = nil, nil
	signsIn(s)
	if code, out := bindCall(s, http.MethodPost, "", `{"owner": "noor", "to": "c4-local/*", "onePerson": true}`,
		map[string]string{InstallerHeader: "", "Authorization": "Bearer person-token"}); code != http.StatusForbidden ||
		out["error"] != "onePerson: this HUD has no installer token, so the farm is not one person's" {
		t.Fatalf("onePerson without a token: %d %v", code, out)
	}

	b := &c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: c4v1.FarmBindingName},
		Spec: c4v1.DeploymentBindingSpec{Owner: "noor", To: "c4-local/*", OnePerson: true, For: boundKinds, Start: metav1.Now(), By: "noor"}}
	if err := s.Client.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	noor := Decider{User: "noor", Role: "farm-admin"}
	if got := s.singlePerson(ctx); got != "" {
		t.Errorf("a one-person binding without a token: %q", got)
	}
	if as, err := s.mayDecide("show-a", "person:wrangler", noor); err == nil || !strings.Contains(err.Error(), "decide.standIn: deny") {
		t.Errorf("without a token, the owner decides a role's wait: %q %v", as, err)
	}
	if v := s.bindingView(b); v.OnePersonInEffect || v.OnePersonNote != "not in effect: this HUD has no installer token" {
		t.Errorf("the view without a token: %+v", v)
	}
	s.DevViewersOpen = true
	if got := s.singlePerson(ctx); got != "" {
		t.Errorf("a one-person binding with open viewers: %q", got)
	}

	s.DevViewersOpen, s.InstallerToken = false, []byte(testInstallerToken)
	if got := s.singlePerson(ctx); got != "noor" {
		t.Errorf("a one-person binding with a token: %q", got)
	}
	if v := s.bindingView(b); !v.OnePersonInEffect || v.OnePersonNote != "while this HUD is reached only with this machine's installer token" {
		t.Errorf("the view with a token: %+v", v)
	}
	shared := b.DeepCopy()
	shared.Spec.OnePerson = false
	if v := s.bindingView(shared); v.OnePersonInEffect || v.OnePersonNote != "" {
		t.Errorf("a shared farm's view: %+v", v)
	}
}

// --single-person-farm holds only where the HUD has an installer token,
// or open development viewers (tests): elsewhere a name on the command
// line makes no one every role.
func TestSinglePersonFlagNeedsTheInstallerToken(t *testing.T) {
	s := farm(t)
	s.SinglePerson = "noor"
	ctx := context.Background()
	for _, c := range []struct {
		open  bool
		token string
		want  string
	}{{true, "", "noor"}, {false, testInstallerToken, "noor"}, {false, "", ""}} {
		s.DevViewersOpen, s.InstallerToken = c.open, []byte(c.token)
		if got := s.singlePerson(ctx); got != c.want {
			t.Errorf("open %v, token %v: %q, want %q", c.open, c.token != "", got, c.want)
		}
	}
}

func jsonDecode(w *httptest.ResponseRecorder, v any) error {
	return json.Unmarshal(w.Body.Bytes(), v)
}

// bindRecord is the farm binding's signed record, verified.
func bindRecord(t *testing.T, s *Server, roots *x509.CertPool) controlPredicate {
	t.Helper()
	var b c4v1.DeploymentBinding
	if err := s.Client.Get(context.Background(), k8stypes.NamespacedName{Name: c4v1.FarmBindingName}, &b); err != nil {
		t.Fatal(err)
	}
	v, err := provenance.VerifyBundle(b.Spec.Signed, roots)
	if err != nil {
		t.Fatal(err)
	}
	var p controlPredicate
	if err := json.Unmarshal(v.Predicate, &p); err != nil {
		t.Fatal(err)
	}
	return p
}
