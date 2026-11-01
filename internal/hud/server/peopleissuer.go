package server

import (
	"net/http"

	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

// The people's issuer at the HUD's origin (/issuer/v1/people/,
// internal/hud/API.md, "The people's issuer"). It certifies on ID tokens,
// installer tokens and codes, never on the HUD's word; the HUD only tells
// it whom a read is for, the person signed in here, so it can keep each
// person's ID tokens and addresses to them and the farm's admins.

// peopleIssuer serves s.People with the request's caller in its context.
func (s *Server) peopleIssuer() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if who := s.peopleCaller(r); who != "" {
			r = r.WithContext(people.WithCaller(r.Context(), who))
		}
		s.People.ServeHTTP(w, r)
	})
}

// peopleCaller is the person a request is signed in as here, or "": a
// unit is no one's caller, and a development viewer is the farm's one
// person only on this machine's installer secret, since roles are views
// of that person.
func (s *Server) peopleCaller(r *http.Request) string {
	v, err := s.viewer(r)
	switch {
	case err != nil || v == nil || v.Unit != nil:
		return ""
	case isDev(v) && !byInstaller(v):
		return ""
	case isDev(v):
		return s.singlePerson(r.Context())
	}
	return v.User
}
