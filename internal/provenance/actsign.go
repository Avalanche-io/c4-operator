package provenance

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Avalanche-io/c4-operator/schema"
)

// A client's side of an act (PEOPLE-KEYS-2 2.1): a person's act, signed
// with their device key on this machine and, for a touch-list act, their
// touch key too, as the bundle X-C4-Act carries.

// P256Signer is a device key that signs in the farm's one form, raw r||s
// with a low S over the SHA-256 of what it signs (a keystore key).
type P256Signer interface {
	Sign(msg []byte) ([]byte, error)
}

// DeviceCert is a device key's certificate chain, DER, leaf first, as the
// people's issuer certified it.
type DeviceCert struct {
	// Identity and Device are the leaf's, copied as certified.
	Identity string
	Device   string
	Chain    [][]byte
}

// PersonAct is what a person signs.
type PersonAct struct {
	// Farm is the farm's ID, which the act is made for.
	Farm    string
	Subject Subject
	Act     string
	// Params are the request's body as the client sends it.
	Params json.RawMessage
	// Person is who acts, and for whom.
	Person string
	// As is the role, where one applies.
	As string
	// Confirmed signs with the touch key too.
	Confirmed bool
	SignedAt  time.Time
}

// SignPersonAct signs a person's act with their device key and, when the
// act is confirmed, their touch key: an act/v1 bundle.
func SignPersonAct(a PersonAct, device P256Signer, cert DeviceCert, touch P256Signer, touchCert *DeviceCert) ([]byte, error) {
	if a.Confirmed && (touch == nil || touchCert == nil) {
		return nil, errors.New("a confirmed act needs the person's touch key")
	}
	if len(cert.Chain) == 0 {
		return nil, errors.New("the device key has no certificate: enrol it first")
	}
	grade := schema.GradeSigned
	if a.Confirmed {
		grade = schema.GradeConfirmed
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
	payload, err := json.Marshal(schema.Act{Type: schema.StatementType, PredicateType: PredicateAct,
		Subject: []schema.Subject{{Name: a.Subject.Name, Digest: a.Subject.Digest}},
		Predicate: schema.ActPredicate{Farm: a.Farm, Act: a.Act, Params: params, As: a.As, Device: cert.Device,
			Nonce: base64.RawURLEncoding.EncodeToString(nonce), Signer: cert.Identity, SignedAt: at.UTC().Format(time.RFC3339Nano),
			Chain: schema.ActChain{By: schema.Actor{Person: a.Person, Grade: grade}, For: schema.For{Party: a.Person, Grade: grade}}}})
	if err != nil {
		return nil, err
	}
	b := Bundle{MediaType: BundleMediaType, DSSEEnvelope: Envelope{PayloadType: PayloadType, Payload: base64.StdEncoding.EncodeToString(payload)}}
	chain, leaf, err := certChain(cert.Chain)
	if err != nil {
		return nil, fmt.Errorf("the device key's certificate: %w", err)
	}
	b.VerificationMaterial.X509CertificateChain = *chain
	pae := PAE(PayloadType, payload)
	sig, err := device.Sign(pae)
	if err != nil {
		return nil, err
	}
	b.DSSEEnvelope.Signatures = append(b.DSSEEnvelope.Signatures, Signature{KeyID: Fingerprint(leaf), Sig: base64.StdEncoding.EncodeToString(sig)})
	if a.Confirmed {
		tchain, tleaf, err := certChain(touchCert.Chain)
		if err != nil {
			return nil, fmt.Errorf("the touch key's certificate: %w", err)
		}
		b.VerificationMaterial.TouchCertificateChain = tchain
		tsig, err := touch.Sign(pae)
		if err != nil {
			return nil, err
		}
		b.DSSEEnvelope.Signatures = append(b.DSSEEnvelope.Signatures, Signature{KeyID: Fingerprint(tleaf), Sig: base64.StdEncoding.EncodeToString(tsig)})
	}
	return json.Marshal(b)
}

// certChain is a DER chain as a bundle carries it, and its leaf.
func certChain(der [][]byte) (*CertificateChain, *x509.Certificate, error) {
	if len(der) == 0 {
		return nil, nil, errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return nil, nil, err
	}
	c := &CertificateChain{}
	for _, d := range der {
		c.Certificates = append(c.Certificates, RawCertificate{RawBytes: d})
	}
	return c, leaf, nil
}
