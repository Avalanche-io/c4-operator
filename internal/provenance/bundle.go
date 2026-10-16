package provenance

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Predicate types. Each names what kind of entity signs it.
const (
	// PredicateTask: what a unit read, where it ran, what it wrote. Signed
	// by the node daemon that captured it.
	PredicateTask = PredicateType
	// PredicateResolution: which view a Materialization resolved to, under
	// which grant, with every object present. Signed by the controller.
	PredicateResolution = "https://cccc.io/provenance/resolution/v1"
	// PredicateJob: a finished job's output view, chain and record index,
	// and the scheduling decisions that touched it. Signed by the queue.
	PredicateJob = "https://cccc.io/provenance/job/v1"
	// PredicateDecision: one preemption or yield the queue decided, and for
	// whom. Signed by the queue.
	PredicateDecision = "https://cccc.io/provenance/decision/v1"
	// PredicateControl: an action a person or agent took through the HUD or
	// MCP, as the HUD authenticated and authorized it. Signed by the HUD.
	PredicateControl = "https://cccc.io/provenance/control/v1"
	// PredicateImport: a git commit imported as a view. Signed by the
	// importer's workload identity.
	PredicateImport = "https://cccc.io/provenance/import/v1"
	// PredicateIngest: a directory ingested as a view. Signed by the
	// ingesting workload's identity.
	PredicateIngest = "https://cccc.io/provenance/ingest/v1"
)

// BundleMediaType identifies an attestation bundle: a DSSE envelope and the
// certificate chain of its signer, shaped like a Sigstore bundle without a
// transparency-log entry.
const BundleMediaType = "application/vnd.cccc.attestation.v1+json"

// Bundle is a signed statement with its verification material.
type Bundle struct {
	MediaType            string               `json:"mediaType"`
	DSSEEnvelope         Envelope             `json:"dsseEnvelope"`
	VerificationMaterial VerificationMaterial `json:"verificationMaterial"`
}

// VerificationMaterial carries the signer's certificate chain, leaf first.
type VerificationMaterial struct {
	X509CertificateChain CertificateChain `json:"x509CertificateChain"`
}

// CertificateChain is DER certificates, leaf first.
type CertificateChain struct {
	Certificates []RawCertificate `json:"certificates"`
}

// RawCertificate is one DER certificate.
type RawCertificate struct {
	RawBytes []byte `json:"rawBytes"`
}

// AnyStatement is an in-toto Statement with any predicate.
type AnyStatement struct {
	Type          string          `json:"_type"`
	Subject       []Subject       `json:"subject"`
	PredicateType string          `json:"predicateType"`
	Predicate     json.RawMessage `json:"predicate"`
}

// SignStatement signs a statement as s. The predicate must marshal to a
// JSON object; SignStatement adds signedAt to it, the time the signature
// claims and the time the certificate must have been valid at.
func SignStatement(ctx context.Context, s *Signer, predicateType string, subjects []Subject, predicate any, now time.Time) ([]byte, error) {
	raw, err := json.Marshal(predicate)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("predicate is not a JSON object: %w", err)
	}
	m["signedAt"] = now.UTC().Format(time.RFC3339Nano)
	pred, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(AnyStatement{Type: StatementType, Subject: subjects, PredicateType: predicateType, Predicate: pred})
	if err != nil {
		return nil, err
	}
	key, chain, err := s.current(ctx, now)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(key, pae(PayloadType, payload))
	b := Bundle{MediaType: BundleMediaType,
		DSSEEnvelope: Envelope{PayloadType: PayloadType, Payload: base64.StdEncoding.EncodeToString(payload),
			Signatures: []Signature{{KeyID: Fingerprint(chain[0]), Sig: base64.StdEncoding.EncodeToString(sig)}}}}
	for _, c := range chain {
		b.VerificationMaterial.X509CertificateChain.Certificates = append(b.VerificationMaterial.X509CertificateChain.Certificates, RawCertificate{RawBytes: c.Raw})
	}
	return json.Marshal(b)
}

// Verified is a record whose signature and certificate chain checked out.
type Verified struct {
	// Identity is the signer: a URI under IdentityBase, or "key:ID" for a
	// record signed with a bare farm key (the format before signers had
	// certificates).
	Identity      string
	PredicateType string
	Subjects      []Subject
	Predicate     json.RawMessage
	SignedAt      time.Time
}

// Subject returns the digest of the named subject.
func (v *Verified) Subject(name string) string {
	for _, s := range v.Subjects {
		if s.Name == name {
			return s.Digest["c4"]
		}
	}
	return ""
}

// HasSubject reports whether a subject has this C4 ID.
func (v *Verified) HasSubject(id string) bool {
	for _, s := range v.Subjects {
		if s.Digest["c4"] == id {
			return true
		}
	}
	return false
}

// VerifyRecord checks a record: a bundle against the farm's roots, or a
// bare envelope against legacy keys.
func VerifyRecord(data []byte, roots *x509.CertPool, legacy map[string]ed25519.PublicKey) (*Verified, error) {
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("not a record: %w", err)
	}
	if probe.MediaType == "" {
		st, signer, err := Verify(data, legacy)
		if err != nil {
			return nil, err
		}
		pred, _ := json.Marshal(st.Predicate)
		return &Verified{Identity: "key:" + signer, PredicateType: st.PredicateType, Subjects: st.Subject,
			Predicate: pred, SignedAt: st.Predicate.Runtime.FinishedOn}, nil
	}
	return VerifyBundle(data, roots)
}

// ErrChain means the signer's certificate does not chain to the farm's
// roots at the time the record claims.
var ErrChain = errors.New("the signer's certificate does not chain to a trusted root")

// VerifyBundle checks a bundle: the certificate chain to roots at the
// record's signing time, then the signature with the leaf's key.
func VerifyBundle(data []byte, roots *x509.CertPool) (*Verified, error) {
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	if b.MediaType != BundleMediaType {
		return nil, fmt.Errorf("media type %q, want %q", b.MediaType, BundleMediaType)
	}
	if b.DSSEEnvelope.PayloadType != PayloadType {
		return nil, fmt.Errorf("payload type %q", b.DSSEEnvelope.PayloadType)
	}
	certs := b.VerificationMaterial.X509CertificateChain.Certificates
	if len(certs) == 0 {
		return nil, errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(certs[0].RawBytes)
	if err != nil {
		return nil, err
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		if ic, err := x509.ParseCertificate(c.RawBytes); err == nil {
			inter.AddCert(ic)
		}
	}
	payload, err := base64.StdEncoding.DecodeString(b.DSSEEnvelope.Payload)
	if err != nil {
		return nil, err
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("the signer's key is not Ed25519")
	}
	signed := false
	for _, s := range b.DSSEEnvelope.Signatures {
		sig, err := base64.StdEncoding.DecodeString(s.Sig)
		if err == nil && ed25519.Verify(pub, pae(b.DSSEEnvelope.PayloadType, payload), sig) {
			signed = true
			break
		}
	}
	if !signed {
		return nil, ErrUntrusted
	}
	var st AnyStatement
	if err := json.Unmarshal(payload, &st); err != nil {
		return nil, err
	}
	if st.Type != StatementType {
		return nil, fmt.Errorf("statement type %q", st.Type)
	}
	var when struct {
		SignedAt time.Time `json:"signedAt"`
	}
	if err := json.Unmarshal(st.Predicate, &when); err != nil || when.SignedAt.IsZero() {
		return nil, errors.New("the record does not say when it was signed")
	}
	// The certificate must have been valid when the record says it was
	// signed. Without a timestamp authority or a transparency log, that
	// time is the signer's own claim (DESIGN.md section 9.2).
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: when.SignedAt,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrChain, err)
	}
	return &Verified{Identity: CertIdentity(leaf), PredicateType: st.PredicateType, Subjects: st.Subject,
		Predicate: st.Predicate, SignedAt: when.SignedAt}, nil
}

// CheckSigner applies the farm's policy: who may sign each kind of record.
// A task record must be signed by the node it says it ran on; decisions of
// the controller, the queue and the HUD by those components; imports and
// ingests by a workload identity.
func CheckSigner(v *Verified) error {
	want := ""
	switch v.PredicateType {
	case PredicateTask:
		var p TaskPredicate
		if err := json.Unmarshal(v.Predicate, &p); err != nil {
			return err
		}
		want = NodeIdentity(p.Runtime.Node)
	case PredicateResolution:
		want = ComponentIdentity("controller")
	case PredicateJob, PredicateDecision:
		want = ComponentIdentity("queue")
	case PredicateControl:
		want = ComponentIdentity("hud")
	case PredicateImport, PredicateIngest:
		if strings.HasPrefix(v.Identity, IdentityBase+"sa/") {
			return nil
		}
		return fmt.Errorf("%s records are signed by a workload identity, not %s", v.PredicateType, v.Identity)
	default:
		return fmt.Errorf("unknown predicate type %q", v.PredicateType)
	}
	if strings.HasPrefix(v.Identity, "key:") {
		// A record signed with a bare farm key names no signer; it verifies,
		// but the policy cannot say who signed it.
		return fmt.Errorf("signed with a farm key, not by %s", want)
	}
	if v.Identity != want {
		return fmt.Errorf("signed by %s, the policy wants %s", v.Identity, want)
	}
	return nil
}

// RootsFromPEM reads a trust bundle of root certificates.
func RootsFromPEM(text []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(text) {
		return nil, errors.New("no certificates in the trust bundle")
	}
	return pool, nil
}
