package provenance

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/schema"
)

// TestLeafNamesItsDevice: a person's leaf names its identity as before and
// its device in a further URI SAN, and the device ID is its own key's.
func TestLeafNamesItsDevice(t *testing.T) {
	now := time.Now()
	ca, err := NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	device, grant := schema.DeviceID(spki), "c4"+"5grant"
	uris, err := LeafURIs(UserIdentity("noor"), device, grant)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: UserIdentity("noor")},
		URIs: uris, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(12 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertIdentity(leaf); got != UserIdentity("noor") {
		t.Errorf("identity %q", got)
	}
	if CertDevice(leaf) != device || CertDeviceID(leaf) != device {
		t.Errorf("device %q, its key's %q, want %q", CertDevice(leaf), CertDeviceID(leaf), device)
	}
	if CertGrant(leaf) != grant {
		t.Errorf("grant %q", CertGrant(leaf))
	}
	if uris, _ := LeafURIs(NodeIdentity("node-3"), "", ""); len(uris) != 1 {
		t.Errorf("a farm part's leaf has only its identity, got %v", uris)
	}
}

func TestIdentities(t *testing.T) {
	if got := UnitIdentity("saltmarsh", "sm0140-light", "7", "9b2e4d71", 1); got != "https://cccc.io/id/unit/saltmarsh/sm0140-light/7@9b2e4d71/1" {
		t.Errorf("unit %s", got)
	}
	if got := AgentIdentity("assistant"); got != "https://cccc.io/id/agent/assistant" {
		t.Errorf("agent %s", got)
	}
	if PayloadHash([]byte("{}")) != "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" {
		t.Errorf("payload hash %s", PayloadHash([]byte("{}")))
	}
}

// A relay signs an act with its own Ed25519 key (SignAct): by a tracker
// user, signed by its sa/ identity, for the farm that certified it; the
// verifier takes it once, and a second farm's roots refuse it.
func TestRelaySignsAnAct(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	ca, err := NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	sync := func(trackers ...string) *Signer {
		s, err := NewSigner(ctx, func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
			leaf, err := ca.IssueBoundRelay(pub, ServiceAccountIdentity("c4-system", "c4-sync"), Bound{}, trackers, time.Hour, now)
			return []*x509.Certificate{leaf, ca.Cert}, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := sync("show-a/flow")
	on := ActSubject("workjob", "show-a", "sh040-light-v007", "9b2e4d71-0c3a-4f8e-a6d2-5b1e7c9f3a10", strings.Repeat("ab", 32))
	p := schema.ActPredicate{Act: "decide", Params: json.RawMessage(`{"do":"approve","unit":"review"}`),
		Chain: schema.ActChain{By: schema.Actor{Tracker: "flow", User: "dana", Grade: schema.GradeRelayed},
			For: schema.For{Party: "dana", Grade: schema.GradeRelayed}}}
	data, err := SignAct(ctx, s, on, p, now)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	v, a, err := VerifyAct(data, roots, now, NewMemorySeen())
	if err != nil {
		t.Fatal(err)
	}
	if v.Identity != ServiceAccountIdentity("c4-system", "c4-sync") || a.Predicate.Farm != FarmID(ca.Cert) || a.Predicate.Nonce == "" ||
		a.Subject[0].Name != on.Name {
		t.Fatalf("signed as %s for farm %s: %+v", v.Identity, a.Predicate.Farm, a.Predicate)
	}
	other, _ := NewCA(now)
	elsewhere := x509.NewCertPool()
	elsewhere.AddCert(other.Cert)
	if _, _, err := VerifyAct(data, elsewhere, now, NewMemorySeen()); err == nil {
		t.Fatal("another farm took the relay's act")
	}
	// A sync certified for another Tracker, or for none, relays nothing
	// for this one: its leaf names the Trackers it relays for.
	for name, s := range map[string]*Signer{"another tracker's sync": sync("show-a/kitsu"), "another namespace's": sync("show-b/flow"), "no tracker's": sync()} {
		data, err := SignAct(ctx, s, on, p, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := VerifyAct(data, roots, now, NewMemorySeen()); err == nil || !strings.Contains(err.Error(), "signed by its sync") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A tracker's sync signs only the decisions it relays: an import or an
// ingest record signed by a leaf that names a Tracker is refused, where
// an importer's (naming none) is taken.
func TestSyncSignsNoImports(t *testing.T) {
	now := time.Now()
	ca, err := NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		syncs []string
		ok    bool
	}{"an importer": {nil, true}, "a tracker's sync": {[]string{"show-a/flow"}, false}} {
		pub, _, _ := ed25519.GenerateKey(nil)
		leaf, err := ca.IssueBoundRelay(pub, ServiceAccountIdentity("show-a", "import-git"), Bound{}, c.syncs, time.Hour, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{PredicateImport, PredicateIngest} {
			v := &Verified{Identity: CertIdentity(leaf), PredicateType: kind, Relays: CertRelays(leaf)}
			if err := CheckSigner(v); (err == nil) != c.ok {
				t.Errorf("%s, %s: %v", name, kind, err)
			}
		}
	}
}
