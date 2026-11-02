package provenance

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/Avalanche-io/c4-operator/schema"
)

// An agent's side of an act (PEOPLE-KEYS-2 5.2, K5): the agent signs with
// its own key, certified as agent/<name> under a person's grant, never the
// person's. The act's chain names the agent as by, the grant's person as
// for, and the grant as under, which is what the verifier holds an agent's
// act to (checkAct). A person's act is signed the same way into the same
// bundle; the two builders should share one once both are on main.

// AgentCert is an agent key's certificate as the people's issuer gave it:
// the leaf's identity, copied, the grant it names and whose grant that is,
// and the chain, DER, leaf first.
type AgentCert struct {
	Identity string
	Agent    string
	Grant    string
	Person   string
	Chain    [][]byte
}

// AgentAct is what an agent signs.
type AgentAct struct {
	// Farm is the farm's ID, which the act is made for.
	Farm    string
	Subject Subject
	Act     string
	// Params are the request's body exactly as the client sends it.
	Params   json.RawMessage
	SignedAt time.Time
}

// SignAgentAct signs an agent's act with its key, which signs as the
// keystore's do (raw r||s, low S, over the SHA-256 of what it signs): an
// act/v1 bundle, graded signed, as X-C4-Act carries it. An agent's act
// names no device: that field is a person's device key's.
func SignAgentAct(a AgentAct, key interface{ Sign([]byte) ([]byte, error) }, cert AgentCert) ([]byte, error) {
	if len(cert.Chain) == 0 || cert.Grant == "" || cert.Person == "" {
		return nil, errors.New("the agent's key has no certificate under a grant")
	}
	leaf, err := x509.ParseCertificate(cert.Chain[0])
	if err != nil {
		return nil, err
	}
	params := a.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	at := a.SignedAt
	if at.IsZero() {
		at = time.Now()
	}
	g := schema.GradeSigned
	payload, err := json.Marshal(schema.Act{Type: schema.StatementType, PredicateType: PredicateAct,
		Subject: []schema.Subject{{Name: a.Subject.Name, Digest: a.Subject.Digest}},
		Predicate: schema.ActPredicate{Farm: a.Farm, Act: a.Act, Params: params,
			Nonce: base64.RawURLEncoding.EncodeToString(nonce), Signer: cert.Identity, SignedAt: at.UTC().Format(time.RFC3339Nano),
			Chain: schema.ActChain{By: schema.Actor{Agent: cert.Agent, Grade: g}, For: schema.For{Party: cert.Person, Grade: g},
				Under: []schema.Delegation{{Grant: cert.Grant, Kind: "sign", From: cert.Person, To: "agent " + cert.Agent}}}}})
	if err != nil {
		return nil, err
	}
	sig, err := key.Sign(PAE(PayloadType, payload))
	if err != nil {
		return nil, err
	}
	b := Bundle{MediaType: BundleMediaType, DSSEEnvelope: Envelope{PayloadType: PayloadType,
		Payload:    base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{KeyID: Fingerprint(leaf), Sig: base64.StdEncoding.EncodeToString(sig)}}}}
	for _, der := range cert.Chain {
		b.VerificationMaterial.X509CertificateChain.Certificates = append(b.VerificationMaterial.X509CertificateChain.Certificates, RawCertificate{RawBytes: der})
	}
	return json.Marshal(b)
}
