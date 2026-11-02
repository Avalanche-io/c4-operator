package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A client that isn't a browser may sign each request it sends (request/v1,
// X-C4-Request) with its own key, in place of a bearer token (PEOPLE-KEYS
// 2.4). The HUD verifies it, holds it to the request it came with (method,
// target and body, byte for byte), and takes it once. Until K8 a request
// with no signature is read as before, and only a host runner's signed
// request is taken: its runner/<node> certificate, which the workload
// issuer gave it on its own account's token, makes it that account,
// c4-hosts/<node>, as its bearer token does. A person's, an agent's or a
// unit's signed request comes with K8.

// maxSignedBody bounds the body the HUD reads to check a signed request.
const maxSignedBody = 32 << 20

// requestViewer is the viewer of a request that carries its signature h.
func (s *Server) requestViewer(r *http.Request, h string) (*api.Viewer, error) {
	data, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("the signed request (%s) is not base64", schema.RequestHeader)
	}
	body, err := exactBody(r)
	if err != nil {
		return nil, err
	}
	v, req, err := provenance.VerifyRequest(data, s.trust(r.Context()).roots, time.Now(), s.actsSeen())
	if err != nil {
		return nil, fmt.Errorf("the signed request does not verify: %v", err)
	}
	if err := req.Matches(r.Method, r.RequestURI, body); err != nil {
		return nil, fmt.Errorf("the signed request is not this request: %v", err)
	}
	node, ok := strings.CutPrefix(v.Identity, provenance.IdentityBase+"runner/")
	if !ok || node == "" || strings.Contains(node, "/") {
		return nil, fmt.Errorf("a signed request by %s is not taken here yet: only a host runner's is, until every client signs", v.Identity)
	}
	// its machine's own account, as a reviewed token of that account
	// would be: the same user and groups, so access() reads them alike
	return s.access(r.Context(), HostAccountPrefix+node,
		[]string{"system:serviceaccounts", "system:serviceaccounts:" + hostsNamespace, "system:authenticated"})
}

// exactBody is the request's body as sent, byte for byte (empty for
// none), put back for the handler to read.
func exactBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxSignedBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSignedBody {
		return nil, errors.New("a signed request's body is over 32 MiB")
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

// signedKey holds a signed request's caller in its context.
type signedKey struct{}

type signedCaller struct {
	v   *api.Viewer
	err error
}

// signedOnce checks a signed request's signature once, before any handler
// asks who calls: a request is taken once, so a handler that asks twice
// would otherwise refuse its own request the second time.
func (s *Server) signedOnce(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sig := r.Header.Get(schema.RequestHeader); sig != "" {
			v, err := s.requestViewer(r, sig)
			r = r.WithContext(context.WithValue(r.Context(), signedKey{}, &signedCaller{v: v, err: err}))
		}
		h.ServeHTTP(w, r)
	})
}
