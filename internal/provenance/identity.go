package provenance

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
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

	"github.com/Avalanche-io/c4-operator/schema"
)

// Every entity that does work signs its own records (DESIGN.md section
// 9.2): each node daemon, each manager component, each importer and, where
// they hold a key, each person. An identity is a URI in the signer's
// certificate, issued by the farm's signing CA.
const IdentityBase = "https://cccc.io/id/"

// NodeIdentity is a node daemon's identity.
func NodeIdentity(node string) string { return IdentityBase + "node/" + node }

// RunnerIdentity is a host runner's identity: the person's own machine,
// running their members under their own sign-in. It is a deployment apart
// from a node daemon on the same Node, owned by that person, not the farm.
func RunnerIdentity(node string) string { return IdentityBase + "runner/" + node }

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

// Issue binds pub to identity for ttl. pub is a farm part's Ed25519 key
// or a device's ECDSA P-256 key; the CA itself is Ed25519 either way.
func (ca *CA) Issue(pub crypto.PublicKey, identity string, ttl time.Duration, now time.Time) (*x509.Certificate, error) {
	return ca.IssueLeaf(pub, identity, "", "", ttl, now)
}

// IssueLeaf is Issue with a leaf's further URI SANs (LeafURIs): the device
// a person's key is, or the grant an agent or CI job acts under. A device
// key is P-256, and the device it names must be its own key's ID, so a
// leaf can never name another device.
func (ca *CA) IssueLeaf(pub crypto.PublicKey, identity, device, grant string, ttl time.Duration, now time.Time) (*x509.Certificate, error) {
	if !strings.HasPrefix(identity, IdentityBase) {
		return nil, fmt.Errorf("identity %q is not under %s", identity, IdentityBase)
	}
	switch k := pub.(type) {
	case ed25519.PublicKey:
		if len(k) != ed25519.PublicKeySize {
			return nil, errors.New("an Ed25519 key is 32 bytes")
		}
		if device != "" {
			return nil, errors.New("a device key is P-256, not Ed25519")
		}
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() {
			return nil, errors.New("a device key is P-256")
		}
	default:
		return nil, fmt.Errorf("a leaf's key is Ed25519 or ECDSA P-256, not %T", pub)
	}
	if device != "" {
		spki, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, err
		}
		if own := schema.DeviceID(spki); device != own {
			return nil, fmt.Errorf("the leaf names device %s, but its key is device %s", device, own)
		}
	}
	uris, err := LeafURIs(identity, device, grant)
	if err != nil {
		return nil, fmt.Errorf("identity %q is not under %s: %v", identity, IdentityBase, err)
	}
	return ca.issue(pub, identity, uris, ttl, now)
}

// Bound is the deployment binding in force for an identity when it is
// certified: its record's C4 ID and its end (zero for a standing
// binding). The zero Bound is no binding.
type Bound struct {
	ID  string
	End time.Time
}

// BindFunc finds the binding in force now for a deployment's identity:
// the zero Bound for an identity no binding covers.
type BindFunc func(ctx context.Context, identity string) (Bound, error)

// IssueBound certifies a farm part's Ed25519 key as identity under the
// binding in force for it (DESIGN 3.3): the leaf names the binding
// (BindingURN) and never outlives its end. An unbound identity (the zero
// Bound) is certified as Issue does, naming none, so its records are
// labelled, not refused, until strict accountability (K8).
func (ca *CA) IssueBound(pub ed25519.PublicKey, identity string, b Bound, ttl time.Duration, now time.Time) (*x509.Certificate, error) {
	return ca.IssueBoundRelay(pub, identity, b, nil, ttl, now)
}

// IssueBoundRelay is IssueBound for a tracker's sync: the leaf also names
// each Tracker that names it as its sync (RelayURN, <namespace>/<tracker>).
func (ca *CA) IssueBoundRelay(pub ed25519.PublicKey, identity string, b Bound, syncs []string, ttl time.Duration, now time.Time) (*x509.Certificate, error) {
	if b.ID == "" && len(syncs) == 0 {
		return ca.Issue(pub, identity, ttl, now)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("an Ed25519 key is 32 bytes")
	}
	if !b.End.IsZero() {
		if !b.End.After(now) {
			return nil, fmt.Errorf("binding %.16s ended at %s", b.ID, b.End.UTC().Format(time.RFC3339))
		}
		ttl = min(ttl, b.End.Sub(now))
	}
	uris, err := LeafURIs(identity, "", "")
	if err != nil {
		return nil, fmt.Errorf("identity %q is not under %s: %v", identity, IdentityBase, err)
	}
	if !strings.HasPrefix(identity, IdentityBase) {
		return nil, fmt.Errorf("identity %q is not under %s", identity, IdentityBase)
	}
	if b.ID != "" {
		u, err := url.Parse(BindingURN + b.ID)
		if err != nil {
			return nil, err
		}
		uris = append(uris, u)
	}
	for _, t := range syncs {
		ns, name, ok := strings.Cut(t, "/")
		if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("a sync names a Tracker as <namespace>/<tracker>, not %q", t)
		}
		u, err := url.Parse(RelayURN + t)
		if err != nil {
			return nil, err
		}
		uris = append(uris, u)
	}
	return ca.issue(pub, identity, uris, ttl, now)
}

// IssueHostUnit certifies a host unit's Ed25519 key as identity (a unit's
// attempt), naming the host runner it was issued through and is held by
// (RunnerURN, the runner's Node).
func (ca *CA) IssueHostUnit(pub ed25519.PublicKey, identity, node string, ttl time.Duration, now time.Time) (*x509.Certificate, error) {
	if _, ok := ParseUnitIdentity(identity); !ok {
		return nil, fmt.Errorf("%s is not a unit", identity)
	}
	if node == "" || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("a host unit's leaf names its runner and holds a 32-byte Ed25519 key")
	}
	uris, err := LeafURIs(identity, "", "")
	if err != nil {
		return nil, err
	}
	r, err := url.Parse(RunnerURN + url.PathEscape(node))
	if err != nil {
		return nil, err
	}
	return ca.issue(pub, identity, append(uris, r), ttl, now)
}

func (ca *CA) issue(pub crypto.PublicKey, identity string, uris []*url.URL, ttl time.Duration, now time.Time) (*x509.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: identity},
		URIs:         uris,
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
	stale func(leaf *x509.Certificate) bool

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

// RenewWhen makes the signer renew before it signs whenever stale says
// its leaf is out of date, not only past half its life: a component
// certified in its own process re-certifies when the binding in force
// for it changes. A failed renewal keeps the leaf it has while it lasts.
func (s *Signer) RenewWhen(stale func(leaf *x509.Certificate) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stale = stale
}

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

// Farm is the ID of the farm s's certificate is from: the C4 ID of its
// chain's root, the farm's CA (FarmID), which an act names.
func (s *Signer) Farm(ctx context.Context, now time.Time) (string, error) {
	_, chain, err := s.current(ctx, now)
	if err != nil {
		return "", err
	}
	return FarmID(chain[len(chain)-1]), nil
}

// Leaf is the signer's current certificate, nil before its first.
func (s *Signer) Leaf() *x509.Certificate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chain) == 0 {
		return nil
	}
	return s.chain[0]
}

// Renew obtains a new key and certificate now, as the signer does at half
// its certificate's life: for a leaf that must name something it was
// certified without (a Tracker named its sync since).
func (s *Signer) Renew(ctx context.Context) error { return s.renew(ctx) }

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
	stale := s.stale
	s.mu.Unlock()
	switch {
	case leaf == nil:
		if err := s.renew(ctx); err != nil {
			return nil, nil, fmt.Errorf("no signing certificate yet: %w", err)
		}
	case now.After(leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore)/2)) || stale != nil && stale(leaf):
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
