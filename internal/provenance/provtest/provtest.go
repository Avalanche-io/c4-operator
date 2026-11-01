// Package provtest makes scratch keys for tests of personal keys
// (PEOPLE-KEYS-2 10.4 item 16): a farm CA, a person's P-256 device key
// with its leaf, and a touch key; an agent's, a unit's and a relay's
// leaves; and act/v1 bundles they sign, signed or confirmed. Every key is
// made when the test runs and lives only in memory, so no test needs a
// development viewer or a key on disk.
//
//	f := provtest.NewFarm(t, time.Now())
//	dana := f.Person(t, "dana", true)
//	bundle := f.SignAct(t, dana, provtest.Act{Act: "grant", Params: grant, Confirmed: true})
//	v, act, err := provenance.VerifyAct(bundle, f.Roots, time.Now(), provenance.NewMemorySeen())
package provtest

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// Farm is a scratch farm: its CA, the roots a verifier trusts, its ID as
// an act names it, and the time its leaves are issued at.
type Farm struct {
	CA    *provenance.CA
	Roots *x509.CertPool
	ID    string
	Now   time.Time
}

// NewFarm makes a scratch farm whose leaves are issued at now.
func NewFarm(t testing.TB, now time.Time) *Farm {
	t.Helper()
	ca, err := provenance.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	return &Farm{CA: ca, Roots: roots, ID: provenance.FarmID(ca.Cert), Now: now}
}

// Key is one signer's scratch key and its chain, leaf first: a P-256 key
// (a person's device, an agent) or an Ed25519 one (a unit, a farm part).
type Key struct {
	Identity string
	Device   string
	Grant    string
	P256     *ecdsa.PrivateKey
	Ed25519  ed25519.PrivateKey
	Chain    []*x509.Certificate
	// Touch is a person's touch key, a device of its own.
	Touch *Key

	by schema.Actor
}

// Leaf is the key's leaf certificate.
func (k *Key) Leaf() *x509.Certificate { return k.Chain[0] }

// Person makes a person's device key and its leaf (user/<name>, naming
// the device), valid 12 hours, and a touch key with its own leaf when
// touch is set.
func (f *Farm) Person(t testing.TB, name string, touch bool) *Key {
	t.Helper()
	k := f.p256(t, provenance.UserIdentity(name), true, "", 12*time.Hour)
	k.by = schema.Actor{Person: name}
	if touch {
		k.Touch = f.p256(t, k.Identity, true, "", 12*time.Hour)
	}
	return k
}

// Agent makes an agent's key and its leaf (agent/<name>), naming the
// grant it acts under, valid 12 hours. An empty grant makes one up.
func (f *Farm) Agent(t testing.TB, name, grant string) *Key {
	t.Helper()
	if grant == "" {
		grant = ID("grant for " + name)
	}
	k := f.p256(t, provenance.AgentIdentity(name), false, grant, 12*time.Hour)
	k.by = schema.Actor{Agent: name}
	return k
}

// Unit makes a unit's Ed25519 key and its leaf for one attempt, valid an
// hour.
func (f *Farm) Unit(t testing.TB, namespace, job, unit, jobUID string, attempt int) *Key {
	t.Helper()
	k := f.Part(t, provenance.UnitIdentity(namespace, job, unit, jobUID, attempt))
	k.by = schema.Actor{Unit: schema.UnitRef(namespace, job, jobUID, unit)}
	return k
}

// Relay makes a tracker relay's Ed25519 key and leaf (sa/<ns>/<account>);
// its acts are a tracker user's (by tracker and user), graded relayed.
func (f *Farm) Relay(t testing.TB, namespace, account, tracker, user string) *Key {
	t.Helper()
	k := f.Part(t, provenance.ServiceAccountIdentity(namespace, account))
	k.by = schema.Actor{Tracker: tracker, User: user}
	return k
}

// Part makes a farm part's Ed25519 key and leaf for identity, valid an
// hour.
func (f *Farm) Part(t testing.TB, identity string) *Key {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := f.CA.Issue(pub, identity, time.Hour, f.Now)
	if err != nil {
		t.Fatal(err)
	}
	return &Key{Identity: identity, Ed25519: key, Chain: []*x509.Certificate{leaf, f.CA.Cert}}
}

func (f *Farm) p256(t testing.TB, identity string, device bool, grant string, ttl time.Duration) *Key {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k := &Key{Identity: identity, Grant: grant, P256: key}
	if device {
		spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		k.Device = schema.DeviceID(spki)
	}
	leaf, err := f.CA.IssueLeaf(&key.PublicKey, identity, k.Device, grant, ttl, f.Now)
	if err != nil {
		t.Fatal(err)
	}
	k.Chain = []*x509.Certificate{leaf, f.CA.Cert}
	return k
}

// Act is an act to sign. A zero field takes its default: a job subject,
// approve, {} as params, the chain the signer's kind implies (for its
// person, or noor), the farm's clock, and signed rather than confirmed.
type Act struct {
	Subject provenance.Subject
	Act     string
	Params  any
	// For is the party the act is for.
	For string
	// Chain replaces the implied chain.
	Chain *schema.ActChain
	// Signer replaces the leaf's identity as the act's signer field.
	Signer   string
	SignedAt time.Time
	// Confirmed signs with the person's touch key too, and grades the act
	// confirmed.
	Confirmed bool
}

// JobUID and JobSpecDigest are the scratch job an act is on by default,
// show-a/sh040-light-v007.
const JobUID = "9b2e4d71-0c3a-4f8e-a6d2-5b1e7c9f3a10"

// JobSpecDigest is the default job's spec digest.
var JobSpecDigest = sha256Hex("the spec of show-a/sh040-light-v007")

// Statement is the act's DSSE payload as k signs it.
func (f *Farm) Statement(t testing.TB, k *Key, a Act) []byte {
	t.Helper()
	if a.Subject.Name == "" {
		a.Subject = provenance.ActSubject("workjob", "show-a", "sh040-light-v007", JobUID, JobSpecDigest)
	}
	if a.Act == "" {
		a.Act = "approve"
	}
	if a.Params == nil {
		a.Params = map[string]any{}
	}
	if a.SignedAt.IsZero() {
		a.SignedAt = f.Now
	}
	if a.Signer == "" {
		a.Signer = k.Identity
	}
	params, err := json.Marshal(a.Params)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	p := schema.ActPredicate{Farm: f.ID, Act: a.Act, Params: params, Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		Signer: a.Signer, SignedAt: a.SignedAt.UTC().Format(time.RFC3339Nano)}
	if k.Device != "" {
		p.Device = k.Device
	}
	if a.Chain != nil {
		p.Chain = *a.Chain
	} else {
		p.Chain = k.chain(a)
	}
	b, err := json.Marshal(schema.Act{Type: schema.StatementType, PredicateType: provenance.PredicateAct, Predicate: p,
		Subject: []schema.Subject{{Name: a.Subject.Name, Digest: a.Subject.Digest}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// chain is the chain a signer's kind implies.
func (k *Key) chain(a Act) schema.ActChain {
	by, grade := k.by, schema.GradeSigned
	if a.Confirmed {
		grade = schema.GradeConfirmed
	}
	party := a.For
	switch {
	case party != "":
	case by.Person != "":
		party = by.Person
	case by.Unit != "":
		party = "dana"
	default:
		party = "noor"
	}
	c := schema.ActChain{By: by, For: schema.For{Party: party, Grade: grade}}
	switch {
	case by.Agent != "":
		c.Under = []schema.Delegation{{Grant: k.Grant, Kind: "sign", From: party, To: "agent " + by.Agent}}
	case by.Unit != "":
		job, _, _ := strings.Cut(by.Unit, "@")
		c.Under = []schema.Delegation{{Job: job, Kind: "ownership", From: party}}
	case by.User != "":
		grade = schema.GradeRelayed
		c.For.Grade = grade
		c.Under = []schema.Delegation{{Record: ID("people-map " + by.User), Kind: "people-map"}}
	}
	c.By.Grade = grade
	return c
}

// SignAct signs the act as k: a bundle, confirmed with k's touch key
// when a.Confirmed.
func (f *Farm) SignAct(t testing.TB, k *Key, a Act) []byte {
	t.Helper()
	return Sign(t, f.Statement(t, k, a), k, a.Confirmed)
}

// Sign signs a payload as k, and with k's touch key too when confirmed:
// each signature keyed by its leaf's fingerprint, the device key's
// first, the touch key's chain in touchCertificateChain.
func Sign(t testing.TB, payload []byte, k *Key, confirmed bool) []byte {
	t.Helper()
	b := provenance.Bundle{MediaType: provenance.BundleMediaType, DSSEEnvelope: provenance.Envelope{PayloadType: provenance.PayloadType,
		Payload: base64.StdEncoding.EncodeToString(payload)}}
	signers := []*Key{k}
	if confirmed {
		if k.Touch == nil {
			t.Fatal("a confirmed act needs a person with a touch key")
		}
		signers = append(signers, k.Touch)
		b.VerificationMaterial.TouchCertificateChain = chainOf(k.Touch)
	}
	b.VerificationMaterial.X509CertificateChain = *chainOf(k)
	for _, s := range signers {
		b.DSSEEnvelope.Signatures = append(b.DSSEEnvelope.Signatures, provenance.Signature{KeyID: provenance.Fingerprint(s.Leaf()),
			Sig: base64.StdEncoding.EncodeToString(s.SignRaw(t, provenance.PAE(provenance.PayloadType, payload)))})
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// SignRaw signs msg with k's key: raw r||s with a low S for P-256.
func (k *Key) SignRaw(t testing.TB, msg []byte) []byte {
	t.Helper()
	if k.P256 == nil {
		return ed25519.Sign(k.Ed25519, msg)
	}
	sig, err := provenance.SignP256(k.P256, msg)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func chainOf(k *Key) *provenance.CertificateChain {
	c := &provenance.CertificateChain{}
	for _, cert := range k.Chain {
		c.Certificates = append(c.Certificates, provenance.RawCertificate{RawBytes: cert.Raw})
	}
	return c
}

// ID is a made-up C4 ID: the C4 ID of s.
func ID(s string) string { return c4.Identify(strings.NewReader(s)).String() }

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
