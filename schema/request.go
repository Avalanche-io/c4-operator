package schema

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"slices"
)

// A signed request (request/v1): one HTTP request, signed by the client
// that sends it, for every client that isn't a browser. It proves who is
// calling, for this farm, now, and nothing more. It is never an act: a
// request that changes something carries its act beside it (X-C4-Act), and
// neither stands in for the other.
const RequestPredicateType = "https://cccc.io/provenance/request/v1"

// RequestHeader carries a request's signed request: its bundle, base64.
const RequestHeader = "X-C4-Request"

//go:embed request-1.schema.json
var requestSchema []byte

// RequestSchema is the JSON Schema of a signed request (request/v1), as GET
// /schema/request serves it.
func RequestSchema() []byte { return slices.Clone(requestSchema) }

// Request is a signed request as its client signed it: the statement in a
// DSSE payload. Its one subject is the body, by its SHA-256.
type Request struct {
	Type          string           `json:"_type"`
	Subject       []Subject        `json:"subject"`
	PredicateType string           `json:"predicateType"`
	Predicate     RequestPredicate `json:"predicate"`
}

// RequestPredicate is what a signed request says.
type RequestPredicate struct {
	// Farm is the C4 ID of the farm CA's root certificate.
	Farm   string `json:"farm"`
	Method string `json:"method"`
	// Path is the request target exactly as sent, in origin form: the
	// escaped path and, with a query, ? and the raw query.
	Path string `json:"path"`
	// Signer is the leaf certificate's identity.
	Signer string `json:"signer"`
	// Device is a person's device key that signed, by its device ID.
	Device   string `json:"device,omitempty"`
	Nonce    string `json:"nonce"`
	SignedAt string `json:"signedAt"`
}

// ParseRequest reads a signed request statement and checks its shape.
// Whether its signer's certificate is in force, its clock and taking it
// once are the verifier's; Matches holds it to the request it came with.
func ParseRequest(b []byte) (*Request, []Error) {
	var r Request
	if errs := parse(b, "request", &r); errs != nil {
		return nil, errs
	}
	return &r, nil
}

// BodyDigest is a body's digest as a signed request names it: the SHA-256
// of its bytes, hex.
func BodyDigest(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// NewRequest is the statement a client signs for one request: its body by
// BodyDigest, and the predicate with the request's method and target.
func NewRequest(p RequestPredicate, body []byte) Request {
	return Request{Type: StatementType, PredicateType: RequestPredicateType, Predicate: p,
		Subject: []Subject{{Name: "body", Digest: map[string]string{"sha256": BodyDigest(body)}}}}
}

// Matches holds a signed request to the request it came with: the same
// method, the same target byte for byte (as the request line has it,
// http.Request.RequestURI), and the same body.
func (r *Request) Matches(method, target string, body []byte) error {
	p := r.Predicate
	switch {
	case p.Method != method:
		return fmt.Errorf("the signed request is a %s, and this request a %s", p.Method, method)
	case p.Path != target:
		return fmt.Errorf("the signed request is for %s, and this request for %s", p.Path, target)
	case len(r.Subject) != 1 || r.Subject[0].Digest["sha256"] != BodyDigest(body):
		return fmt.Errorf("the signed request's body is not this request's")
	}
	return nil
}
