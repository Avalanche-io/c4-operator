package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

// The people's issuer is served at the HUD's own origin, under
// /issuer/v1/people/, with the prefix taken off, so the web app's key,
// which lives in the HUD's storage, enrols without leaving the origin; a
// HUD without one answers its paths as no API call.
func TestPeopleIssuerAtTheHUDsOrigin(t *testing.T) {
	var got string
	s := farm(t)
	s.People = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusTeapot)
	})
	h := s.Handler()
	for _, c := range []struct{ method, path, want string }{
		{"GET", "/issuer/v1/people/config", "GET /v1/people/config"},
		{"POST", "/issuer/v1/people/enroll", "POST /v1/people/enroll"},
	} {
		got = ""
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, strings.NewReader("{}")))
		if w.Code != http.StatusTeapot || got != c.want {
			t.Errorf("%s %s: %d, the issuer saw %q", c.method, c.path, w.Code, got)
		}
	}
	s.People = nil
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/issuer/v1/people/enroll", strings.NewReader("{}")))
	if w.Code == http.StatusTeapot || w.Code == http.StatusOK {
		t.Errorf("a HUD with no people's issuer: %d", w.Code)
	}
}

// The HUD tells the people's issuer whom a read is for: the person signed
// in (a bearer token's, here dana), the farm's one person for a
// development viewer on this machine's installer secret, and no one for a
// request that signs in as no one.
func TestPeopleIssuerHearsTheCaller(t *testing.T) {
	s := guarded(t)
	s.SinglePerson = "noor"
	signsIn(s)
	var got string
	s.People = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = people.CallerFrom(r.Context()) })
	for _, c := range []struct {
		headers map[string]string
		want    string
	}{
		{map[string]string{"Authorization": "Bearer dana-token"}, "dana"},
		{map[string]string{InstallerHeader: testInstallerToken}, "noor"},
		{nil, ""},
	} {
		got = "unset"
		icall(s, "GET", "/issuer/v1/people/index", c.headers)
		if got != c.want {
			t.Errorf("%v: the issuer heard %q, want %q", c.headers, got, c.want)
		}
	}
}

// The people's issuer hears where a request came from: the SSO proxy's
// X-Forwarded-For only on a request carrying the proxy's secret, and the
// remote address on any other, whose X-Forwarded-For anyone could write.
func TestPeopleIssuerTrustsForwardedForFromTheProxy(t *testing.T) {
	s := farm(t)
	s.TrustProxy, s.ProxySecret = true, []byte("a-proxy-secret-for-tests")
	var got string
	s.People = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = people.ClientAddrFrom(r.Context()) })
	for _, c := range []struct {
		headers map[string]string
		want    string
	}{
		{map[string]string{"X-Forwarded-For": "198.51.100.7, 10.0.0.2", ProxySecretHeader: "a-proxy-secret-for-tests"}, "198.51.100.7"},
		{map[string]string{"X-Forwarded-For": "198.51.100.7"}, "192.0.2.1"},
		{map[string]string{"X-Forwarded-For": "198.51.100.7", ProxySecretHeader: "a guess"}, "192.0.2.1"},
	} {
		got = ""
		icall(s, "GET", "/issuer/v1/people/config", c.headers)
		if got != c.want {
			t.Errorf("%v: the issuer heard %q, want %q", c.headers, got, c.want)
		}
	}
}

// An open development viewer, which anyone who reaches the HUD may be, is
// no one's caller.
func TestOpenDevViewerIsNoCaller(t *testing.T) {
	s := farm(t)
	s.SinglePerson = "noor"
	if !s.DevViewersOpen {
		t.Skip("the test farm's development viewers are not open")
	}
	got := "unset"
	s.People = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = people.CallerFrom(r.Context()) })
	icall(s, "GET", "/issuer/v1/people/index?as=wrangler", nil)
	if got != "" {
		t.Errorf("an open development viewer is the issuer's caller %q", got)
	}
}

// Every HUD response, the web app's pages above all, tells the browser to
// send no Referer: a page's address can carry the one-time browser code
// or the provider's sign-in answer for a moment, and neither may leave
// with a link or a fetch to another site.
func TestHUDSendsNoReferrer(t *testing.T) {
	s := farm(t)
	s.Web = fstest.MapFS{"index.html": {Data: []byte("hud")}}
	h := s.Handler()
	for _, path := range []string{"/", "/api/v1/snapshot", "/issuer/v1/people/config"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("GET %s: Referrer-Policy %q, want no-referrer", path, got)
		}
	}
}
