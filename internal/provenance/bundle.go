package provenance

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/schema"
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
	// PredicateBook: one sealed segment of the farm book, the farm-wide log
	// of farm-level acts (internal/farmbook). Signed by the queue, which
	// seals it.
	PredicateBook = schema.BookPredicateType
	// PredicateBookEntry: an entry the people's issuer wrote for the farm
	// book, as written (its subject the entry's C4 ID without this
	// signature). Signed by the issuer, so the queue seals only what it
	// wrote.
	PredicateBookEntry = "https://cccc.io/provenance/book-entry/v1"
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
// A confirmed act (act/v1) has two signatures over its one payload, the
// device key's and then its touch key's, each with its key ID; the touch
// key's chain is TouchCertificateChain. A browser's touch key is a
// passkey, whose signature is a WebAuthn assertion: TouchWebAuthn carries
// what it signed beside its DER signature (webauthn.go).
type VerificationMaterial struct {
	X509CertificateChain  CertificateChain  `json:"x509CertificateChain"`
	TouchCertificateChain *CertificateChain `json:"touchCertificateChain,omitempty"`
	TouchWebAuthn         *WebAuthn         `json:"touchWebAuthn,omitempty"`
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
	// Payload is the DSSE payload as signed; PayloadHash of it is the key
	// the farm takes an act once by.
	Payload []byte
	// Farm is the farm the signer's certificate chains to: the C4 ID of
	// the CA certificate that issued its leaf (FarmID).
	Farm string
	// Device is the device a person's leaf names, its own key's ID, and
	// Grant the grant an agent's or a CI job's leaf names.
	Device string
	Grant  string
	// Runner is the host runner (its Node) a host unit's leaf names: the
	// unit's key was issued through it and is held by it.
	Runner string
	// Binding is the deployment binding the signer's leaf names, by its
	// record's C4 ID (CertBinding): "" for a signer certified unbound.
	Binding string
	// Syncs are the Trackers a tracker sync's leaf names (CertSyncs,
	// <namespace>/<tracker>): the only ones it relays for.
	Syncs []string
	// TouchDevice is a confirmed act's touch key, by its device ID: the
	// second of its two signatures.
	TouchDevice string
	// TouchAssertion is a passkey touch's origin and relying party, when
	// the touch key signed as WebAuthn; CheckTouchKey holds them to the
	// passkey's device record.
	TouchAssertion *Assertion
	// Expires is when the first of the signer's certificates ends: how
	// long the farm keeps the record's payload hash.
	Expires time.Time
}

// Confirmed reports whether a person's touch key signed the record too.
func (v *Verified) Confirmed() bool { return v.TouchDevice != "" }

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

// VerifyBundle checks a bundle: the signatures with the leaf's key, then
// the certificate chain to roots at the record's signing time.
//
// The leaf's key type picks the rule. A farm part's Ed25519 leaf passes
// when any signature in the envelope is its own, as before personal keys.
// A device's P-256 leaf signs alone: exactly one signature, its own,
// keyed by its leaf's fingerprint. A confirmed act, whose bundle carries a
// touch key's chain, has exactly two: the device key's, then the touch
// key's, each keyed by its leaf's fingerprint. An extra, missing or
// unknown signature, or two in the other order, refuses the record; it
// is never skipped (PEOPLE-KEYS-2 10.4 item 5). A passkey's touch is a
// WebAuthn assertion over the PAE's challenge (TouchWebAuthn,
// VerifyWebAuthn), never a raw signature.
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
	leaf, inter, err := parseChain(b.VerificationMaterial.X509CertificateChain)
	if err != nil {
		return nil, err
	}
	payload, err := base64.StdEncoding.DecodeString(b.DSSEEnvelope.Payload)
	if err != nil {
		return nil, err
	}
	msg := PAE(b.DSSEEnvelope.PayloadType, payload)
	sigs := b.DSSEEnvelope.Signatures
	var assertion *Assertion
	if b.VerificationMaterial.TouchWebAuthn != nil && b.VerificationMaterial.TouchCertificateChain == nil {
		return nil, fmt.Errorf("%w: a WebAuthn assertion is a touch key's, and the bundle has no touch key", ErrUntrusted)
	}
	var touch *x509.Certificate
	var touchInter *x509.CertPool
	_, device := leaf.PublicKey.(*ecdsa.PublicKey)
	switch {
	case b.VerificationMaterial.TouchCertificateChain != nil:
		if touch, touchInter, err = parseChain(*b.VerificationMaterial.TouchCertificateChain); err != nil {
			return nil, fmt.Errorf("touch key: %w", err)
		}
		if _, ok := touch.PublicKey.(*ecdsa.PublicKey); !ok || !device {
			return nil, fmt.Errorf("%w: a confirmed act is signed by a person's device key and touch key, both P-256", ErrUntrusted)
		}
		if len(sigs) != 2 {
			return nil, fmt.Errorf("%w: a confirmed act has exactly two signatures, the device key's and then the touch key's, not %d", ErrUntrusted, len(sigs))
		}
		if err := signedBy(leaf, sigs[0], msg); err != nil {
			return nil, fmt.Errorf("the first signature, the device key's: %w", err)
		}
		if w := b.VerificationMaterial.TouchWebAuthn; w != nil {
			assertion, err = signedByPasskey(touch, sigs[1], msg, *w)
		} else {
			err = signedBy(touch, sigs[1], msg)
		}
		if err != nil {
			return nil, fmt.Errorf("the second signature, the touch key's: %w", err)
		}
	case device:
		if len(sigs) != 1 {
			return nil, fmt.Errorf("%w: a device key signs alone, with exactly one signature, not %d", ErrUntrusted, len(sigs))
		}
		if err := signedBy(leaf, sigs[0], msg); err != nil {
			return nil, err
		}
	default:
		pub, ok := leaf.PublicKey.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("the signer's key is neither Ed25519 nor P-256")
		}
		signed := false
		for _, s := range sigs {
			sig, err := base64.StdEncoding.DecodeString(s.Sig)
			if err == nil && ed25519.Verify(pub, msg, sig) {
				signed = true
				break
			}
		}
		if !signed {
			return nil, ErrUntrusted
		}
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
	farm, err := chainsAt(leaf, inter, roots, when.SignedAt)
	if err != nil {
		return nil, err
	}
	v := &Verified{Identity: CertIdentity(leaf), PredicateType: st.PredicateType, Subjects: st.Subject,
		Predicate: st.Predicate, SignedAt: when.SignedAt, Payload: payload, Farm: farm,
		Device: CertDevice(leaf), Grant: CertGrant(leaf), Runner: CertRunner(leaf), Binding: CertBinding(leaf), Syncs: CertSyncs(leaf), Expires: leaf.NotAfter}
	if v.Device != "" && v.Device != CertDeviceID(leaf) {
		return nil, fmt.Errorf("%w: the leaf names device %s, but its key is device %s", ErrUntrusted, v.Device, CertDeviceID(leaf))
	}
	if touch == nil {
		return v, nil
	}
	// The touch key is the same person's, on the same farm, and was valid
	// when the act was signed; its device record names the device it
	// confirms for (CheckTouchKey, against the device records).
	if st.PredicateType != PredicateAct {
		return nil, fmt.Errorf("%w: only an act is confirmed with a touch, not a %s record", ErrUntrusted, st.PredicateType)
	}
	touchFarm, err := chainsAt(touch, touchInter, roots, when.SignedAt)
	if err != nil {
		return nil, fmt.Errorf("touch key: %w", err)
	}
	switch {
	case touchFarm != farm:
		return nil, fmt.Errorf("%w: the touch key is another farm's", ErrUntrusted)
	case CertIdentity(touch) != v.Identity:
		return nil, fmt.Errorf("%w: the touch key is %s's, the device key %s's", ErrUntrusted, CertIdentity(touch), v.Identity)
	case v.Device == "" || CertDevice(touch) != CertDeviceID(touch):
		return nil, fmt.Errorf("%w: a confirmed act's leaves each name their own device", ErrUntrusted)
	case CertDevice(touch) == v.Device:
		return nil, fmt.Errorf("%w: the touch key is the device key itself", ErrUntrusted)
	}
	v.TouchDevice, v.TouchAssertion = CertDevice(touch), assertion
	if touch.NotAfter.Before(v.Expires) {
		v.Expires = touch.NotAfter
	}
	return v, nil
}

// parseChain reads a certificate chain: its leaf, and the rest as
// intermediates.
func parseChain(chain CertificateChain) (*x509.Certificate, *x509.CertPool, error) {
	certs := chain.Certificates
	if len(certs) == 0 {
		return nil, nil, errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(certs[0].RawBytes)
	if err != nil {
		return nil, nil, err
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		if ic, err := x509.ParseCertificate(c.RawBytes); err == nil {
			inter.AddCert(ic)
		}
	}
	return leaf, inter, nil
}

// chainsAt checks that leaf chains to roots at the time the record claims
// and returns its farm.
func chainsAt(leaf *x509.Certificate, inter, roots *x509.CertPool, at time.Time) (string, error) {
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: at,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}})
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrChain, err)
	}
	if len(chains[0]) < 2 {
		return "", fmt.Errorf("%w: the leaf is a root", ErrChain)
	}
	return FarmID(chains[0][1]), nil
}

// signedBy checks one signature of an envelope against a device's leaf:
// keyed by the leaf's fingerprint, in its one form (VerifyP256).
func signedBy(leaf *x509.Certificate, s Signature, msg []byte) error {
	if s.KeyID != Fingerprint(leaf) {
		return fmt.Errorf("%w: signature key ID %.16s is not its leaf's fingerprint", ErrUntrusted, s.KeyID)
	}
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil {
		return fmt.Errorf("%w: the signature is not base64", ErrSignatureForm)
	}
	return VerifyP256(leaf.PublicKey.(*ecdsa.PublicKey), msg, sig)
}

// signedByPasskey checks a passkey touch's signature: keyed by its leaf's
// fingerprint, a WebAuthn assertion over msg's challenge.
func signedByPasskey(leaf *x509.Certificate, s Signature, msg []byte, w WebAuthn) (*Assertion, error) {
	if s.KeyID != Fingerprint(leaf) {
		return nil, fmt.Errorf("%w: signature key ID %.16s is not its leaf's fingerprint", ErrUntrusted, s.KeyID)
	}
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil {
		return nil, fmt.Errorf("%w: the signature is not base64", ErrSignatureForm)
	}
	return VerifyWebAuthn(leaf.PublicKey.(*ecdsa.PublicKey), msg, w, sig)
}

// FarmID is a farm's ID: the C4 ID of its CA's certificate (DER), the CA
// that issues its leaves. It differs per farm, and an act names the farm
// it was made for by it (PEOPLE-KEYS-2 10.4 item 7).
func FarmID(ca *x509.Certificate) string { return c4.Identify(bytes.NewReader(ca.Raw)).String() }

// CheckSigner applies the farm's policy: who may sign each kind of record.
// A task record must be signed by the node it says it ran on; decisions of
// the controller, the queue and the HUD by those components; imports and
// ingests by a workload identity; device records by the people's issuer;
// an act by the identity in its signer field, with the kinds that need a
// person or a touch (ActNeeds) refused to anyone else.
func CheckSigner(v *Verified) error {
	want := ""
	switch v.PredicateType {
	case PredicateTask:
		var p TaskPredicate
		if err := json.Unmarshal(v.Predicate, &p); err != nil {
			return err
		}
		want = NodeIdentity(p.Runtime.Node)
		if v.Identity == RunnerIdentity(p.Runtime.Node) {
			// a host runner's record of a unit it ran there
			want = v.Identity
		}
	case PredicateResolution:
		want = ComponentIdentity("controller")
	case PredicateJob, PredicateDecision, PredicateBook:
		want = ComponentIdentity("queue")
	case PredicateControl:
		// The HUD's, citing the act it carried out (CheckCitation).
		want = ComponentIdentity("hud")
	case PredicateAct:
		// The actor's own key, or a tracker's relay (checkAct).
		_, err := checkAct(v)
		return err
	case PredicateRequest:
		// The client that sent it (checkRequest): never an act.
		_, err := checkRequest(v)
		return err
	case PredicateDevice, PredicateBookEntry:
		want = ComponentIdentity("issuer")
	case PredicateImport, PredicateIngest:
		if len(v.Syncs) > 0 {
			// a tracker's sync signs the decisions it relays, nothing else
			return fmt.Errorf("%s records are an importer's, and %s is the sync of tracker %s", v.PredicateType, v.Identity, strings.Join(v.Syncs, ", "))
		}
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
