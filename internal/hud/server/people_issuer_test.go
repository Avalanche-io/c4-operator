package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
