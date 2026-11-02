package provenance

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/schema"
)

// A signed request (request/v1, X-C4-Request) is one HTTP request signed
// by the client that isn't a browser and sent it: a host runner, a unit,
// an agent's MCP server, a person's CLI. It proves who calls, at the call;
// it is never an act, so it carries out nothing a person decides, and a
// request bundle never verifies as one.

// requestSigners are the identities that may sign a request: the farm's
// clients. The farm's own parts (nodes, components, importers) call
// nothing as a client and sign no request.
var requestSigners = []string{"user/", "agent/", "unit/", "runner/"}

// checkRequest checks a verified request/v1 envelope's statement: its
// shape, its signer the leaf's identity, its farm the leaf's farm, a
// person's device the leaf's, and its signer one of the farm's clients.
func checkRequest(v *Verified) (*schema.Request, error) {
	r, errs := schema.ParseRequest(v.Payload)
	if errs != nil {
		return nil, fmt.Errorf("not a signed request: %s", errs[0].Message)
	}
	p := r.Predicate
	client := false
	for _, k := range requestSigners {
		client = client || strings.HasPrefix(v.Identity, IdentityBase+k)
	}
	switch {
	case p.Signer != v.Identity:
		return nil, fmt.Errorf("the request names %s as its signer, and %s signed it", p.Signer, v.Identity)
	case p.Farm != v.Farm:
		return nil, fmt.Errorf("the request is for farm %s, and its certificate is from %s", p.Farm, v.Farm)
	case p.Device != v.Device:
		return nil, fmt.Errorf("the request names device %q, and its certificate %q", p.Device, v.Device)
	case !client:
		return nil, fmt.Errorf("%s is not one of the farm's clients, and signs no request", v.Identity)
	}
	return r, nil
}

// VerifyRequest checks a signed request as the farm takes it: the bundle
// (VerifyBundle), a request/v1 record, its signer (checkRequest), its
// signedAt within ActSkew of now, and taken once by its payload hash, kept
// for as long as its certificates live. Matches, the caller's, holds it to
// the request it came with.
func VerifyRequest(data []byte, roots *x509.CertPool, now time.Time, seen SeenSet) (*Verified, *schema.Request, error) {
	v, err := VerifyBundle(data, roots)
	if err != nil {
		return nil, nil, err
	}
	if v.PredicateType != PredicateRequest {
		return nil, nil, fmt.Errorf("a %s record, not a signed request", v.PredicateType)
	}
	r, err := checkRequest(v)
	if err != nil {
		return nil, nil, err
	}
	if d := now.Sub(v.SignedAt); d > ActSkew || d < -ActSkew {
		return nil, nil, fmt.Errorf("the request was signed at %s, %s from %s", v.SignedAt.UTC().Format(time.RFC3339), d.Round(time.Second), now.UTC().Format(time.RFC3339))
	}
	until := v.Expires
	if t := v.SignedAt.Add(ActSkew); t.After(until) {
		until = t
	}
	if err := seen.Take(PayloadHash(v.Payload), until); err != nil {
		return nil, nil, fmt.Errorf("the request was taken once already (payload %.16s): %w", PayloadHash(v.Payload), err)
	}
	return v, r, nil
}

// SignRequest signs an HTTP request as s (request/v1): its method, its
// target in origin form exactly as it will be sent (escaped path and,
// with a query, ? and the raw query) and its body, byte for byte. It
// returns the value of X-C4-Request. Every request is signed afresh: the
// farm takes each once.
func SignRequest(ctx context.Context, s *Signer, method, target string, body []byte, now time.Time) (string, error) {
	farm, err := s.Farm(ctx, now) // also obtains the certificate
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	p := schema.RequestPredicate{Farm: farm, Method: method, Path: target, Signer: s.Identity(),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce)}
	b, err := SignStatement(ctx, s, PredicateRequest, []Subject{{Name: "body", Digest: map[string]string{"sha256": schema.BodyDigest(body)}}}, p, now)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
