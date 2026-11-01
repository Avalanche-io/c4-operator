package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
