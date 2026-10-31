package provenance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
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
