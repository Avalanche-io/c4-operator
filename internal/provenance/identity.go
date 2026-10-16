package provenance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Every entity that does work signs its own records (DESIGN.md section
// 9.2): each node daemon, each manager component, each importer and, where
// they hold a key, each person. An identity is a URI in the signer's
// certificate, issued by the farm's signing CA.
const IdentityBase = "https://cccc.io/id/"

// NodeIdentity is a node daemon's identity.
func NodeIdentity(node string) string { return IdentityBase + "node/" + node }

// ComponentIdentity is a manager component's identity: controller, queue,
// hud.
func ComponentIdentity(name string) string { return IdentityBase + "component/" + name }

// ServiceAccountIdentity is a workload's identity, for example an importer.
func ServiceAccountIdentity(namespace, name string) string {
	return IdentityBase + "sa/" + namespace + "/" + name
}

// SignerNamespace returns the namespace of a workload identity, or "".
func SignerNamespace(identity string) string {
	rest, ok := strings.CutPrefix(identity, IdentityBase+"sa/")
	if !ok {
		return ""
	}
	ns, _, _ := strings.Cut(rest, "/")
	return ns
}

// UserIdentity is a person's identity, for records a person signs with
// their own key.
func UserIdentity(user string) string { return IdentityBase + "user/" + url.PathEscape(user) }

// CA is the farm's signing CA. Its root key signs short-lived certificates
// that bind each signer's key to its identity. In production the root key
// lives in a KMS or HSM and never leaves it; here it is a Secret only the
// manager can read.
type CA struct {
	Cert *x509.Certificate
	Key  ed25519.PrivateKey
}

// NewCA makes a root valid for ten years.
func NewCA(now time.Time) (*CA, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "C4 farm signing CA", Organization: []string{"The Cinema Content Creation Cloud"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// PEM encodes the CA's certificate and key.
func (ca *CA) PEM() (certPEM, keyPEM []byte, err error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(ca.Key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// ParseCA reads a CA from PEM.
func ParseCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("CA: not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("CA: the key is not Ed25519")
	}
	return &CA{Cert: cert, Key: key}, nil
}

// Issue binds pub to identity for ttl.
func (ca *CA) Issue(pub ed25519.PublicKey, identity string, ttl time.Duration, now time.Time) (*x509.Certificate, error) {
	u, err := url.Parse(identity)
	if err != nil || !strings.HasPrefix(identity, IdentityBase) {
		return nil, fmt.Errorf("identity %q is not under %s", identity, IdentityBase)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: identity},
		URIs:         []*url.URL{u},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// CertIdentity is the identity a certificate binds.
func CertIdentity(c *x509.Certificate) string {
	for _, u := range c.URIs {
		if s := u.String(); strings.HasPrefix(s, IdentityBase) {
			return s
		}
	}
	return ""
}

// IssueFunc obtains a certificate chain (leaf first) for a public key.
type IssueFunc func(ctx context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error)

// Signer signs as one identity with a key that never leaves its process.
// It renews its certificate at half its lifetime.
type Signer struct {
	issue IssueFunc

	mu    sync.Mutex
	key   ed25519.PrivateKey
	chain []*x509.Certificate
}

// NewSigner makes a key and obtains its first certificate.
func NewSigner(ctx context.Context, issue IssueFunc) (*Signer, error) {
	s := &Signer{issue: issue}
	return s, s.renew(ctx)
}

// LazySigner obtains its certificate when it first signs, or when Ready
// gets one. A daemon that starts before the issuer can still start.
func LazySigner(issue IssueFunc) *Signer { return &Signer{issue: issue} }

// Ready tries to obtain a certificate every interval until one is issued
// or ctx ends.
func (s *Signer) Ready(ctx context.Context, every time.Duration) error {
	for {
		if s.Identity() != "" {
			return nil
		}
		err := s.renew(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no signing certificate: %w", err)
		case <-time.After(every):
		}
	}
}

func (s *Signer) renew(ctx context.Context) error {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	chain, err := s.issue(ctx, pub)
	if err != nil {
		return err
	}
	if len(chain) == 0 || !chain[0].PublicKey.(ed25519.PublicKey).Equal(pub) {
		return errors.New("the issued certificate does not bind this key")
	}
	s.mu.Lock()
	s.key, s.chain = key, chain
	s.mu.Unlock()
	return nil
}

// Identity is the identity this signer signs as.
func (s *Signer) Identity() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chain) == 0 {
		return ""
	}
	return CertIdentity(s.chain[0])
}

// current returns the key and chain, renewing past half-life.
func (s *Signer) current(ctx context.Context, now time.Time) (ed25519.PrivateKey, []*x509.Certificate, error) {
	s.mu.Lock()
	var leaf *x509.Certificate
	if len(s.chain) > 0 {
		leaf = s.chain[0]
	}
	s.mu.Unlock()
	switch {
	case leaf == nil:
		if err := s.renew(ctx); err != nil {
			return nil, nil, fmt.Errorf("no signing certificate yet: %w", err)
		}
	case now.After(leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) / 2)):
		if err := s.renew(ctx); err != nil && now.After(leaf.NotAfter) {
			return nil, nil, fmt.Errorf("signing certificate expired and renewal failed: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key, s.chain, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

// Fingerprint names a certificate (the SHA-256 of its DER).
func Fingerprint(c *x509.Certificate) string {
	h := sha256.Sum256(c.Raw)
	return fmt.Sprintf("%x", h[:])
}
