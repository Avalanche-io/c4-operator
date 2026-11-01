package server

import (
	"net"
	"net/http"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

// The people's issuer at the HUD's origin (/issuer/v1/people/,
// internal/hud/API.md, "The people's issuer"). It certifies on ID tokens,
// installer tokens and codes, never on the HUD's word; the HUD only tells
// it whom a read is for, the person signed in here, so it can keep each
// person's ID tokens and addresses to them and the farm's admins.

// proxiedKey marks a request that carried the SSO proxy's secret.
type proxiedKey struct{}

// peopleIssuer serves s.People with the request's caller and the client's
// address in its context.
func (s *Server) peopleIssuer() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := people.WithClientAddr(r.Context(), clientAddr(r))
		if who := s.peopleCaller(r); who != "" {
			ctx = people.WithCaller(ctx, who)
		}
		s.People.ServeHTTP(w, r.WithContext(ctx))
	})
}

// clientAddr is where a request came from, for the issuer's two-places
// notice: the first X-Forwarded-For address on a request that carried the
// SSO proxy's secret, which only the proxy sends, and the remote address
// on any other, whose X-Forwarded-For anyone could write.
func clientAddr(r *http.Request) string {
	if p, _ := r.Context().Value(proxiedKey{}).(bool); p {
		if first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
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
