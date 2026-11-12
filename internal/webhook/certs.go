package webhook

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TLSSecret is the Secret, in the operator's namespace, that holds the
// webhook's serving certificate and the CA that signed it. Every manager
// serves the same one, so the webhook configuration's CA bundle is right
// for all of them at once: during a rolling update a request reaching the
// old manager or the new one is admitted alike. Until 2026-11-12 each
// manager made its own CA at start and put it in the bundle, and the one
// still running failed TLS until it stopped (failurePolicy Fail).
//
// The CA's private key is not kept: it signs the serving certificate once
// and is dropped. A new pair is made only when the Secret's is missing,
// unreadable, for other service names, or within renewBefore of expiry.
const TLSSecret = "c4-webhook-tls"

const (
	secretCA   = "ca.crt"
	secretCert = "tls.crt"
	secretKey  = "tls.key"
)

// renewBefore is how long before the serving certificate expires a new
// pair is made.
const renewBefore = 30 * 24 * time.Hour

// BootstrapCerts gives the webhook server its serving certificate, from
// the Secret TLSSecret (made by the first manager that finds none), writes
// it to dir, and puts its CA into the MutatingWebhookConfiguration, ahead
// of the CAs the bundle held before (bundle).
func BootstrapCerts(ctx context.Context, c client.Client, dir, service, namespace, configName string) error {
	names := dnsNames(service, namespace)
	pair, err := sharedPair(ctx, c, namespace, names)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), pair.cert, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), pair.key, 0o600); err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cfg admissionv1.MutatingWebhookConfiguration
		if err := c.Get(ctx, types.NamespacedName{Name: configName}, &cfg); err != nil {
			return fmt.Errorf("webhook configuration %s: %w", configName, err)
		}
		changed := false
		for i := range cfg.Webhooks {
			b := bundle(pair.ca, cfg.Webhooks[i].ClientConfig.CABundle)
			if !bytes.Equal(b, cfg.Webhooks[i].ClientConfig.CABundle) {
				cfg.Webhooks[i].ClientConfig.CABundle = b
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return c.Update(ctx, &cfg)
	})
}

type tlsPair struct{ ca, cert, key []byte }

// sharedPair reads the Secret's pair, or makes one and stores it. Two
// managers starting together both create; the one refused takes the
// winner's. A pair that will not serve these names is replaced, guarded
// by the Secret's resourceVersion, and the loser of that race reads again.
func sharedPair(ctx context.Context, c client.Client, namespace string, names []string) (tlsPair, error) {
	key := types.NamespacedName{Namespace: namespace, Name: TLSSecret}
	for range 5 {
		var s corev1.Secret
		err := c.Get(ctx, key, &s)
		switch {
		case apierrors.IsNotFound(err):
			p, err := newPair(names)
			if err != nil {
				return tlsPair{}, err
			}
			s = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: TLSSecret}, Type: corev1.SecretTypeTLS, Data: p.data()}
			if err := c.Create(ctx, &s); apierrors.IsAlreadyExists(err) {
				continue
			} else if err != nil {
				return tlsPair{}, fmt.Errorf("webhook TLS Secret %s: %w", TLSSecret, err)
			}
			return p, nil
		case err != nil:
			return tlsPair{}, fmt.Errorf("webhook TLS Secret %s: %w", TLSSecret, err)
		}
		p := tlsPair{ca: s.Data[secretCA], cert: s.Data[secretCert], key: s.Data[secretKey]}
		if p.serves(names, time.Now()) {
			return p, nil
		}
		np, err := newPair(names)
		if err != nil {
			return tlsPair{}, err
		}
		s.Data = np.data()
		s.Type = corev1.SecretTypeTLS
		if err := c.Update(ctx, &s); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return tlsPair{}, fmt.Errorf("webhook TLS Secret %s: %w", TLSSecret, err)
		}
		return np, nil
	}
	return tlsPair{}, fmt.Errorf("webhook TLS Secret %s: it kept changing while it was read", TLSSecret)
}

func (p tlsPair) data() map[string][]byte {
	return map[string][]byte{secretCA: p.ca, secretCert: p.cert, secretKey: p.key}
}

// serves says whether the pair is whole, its key is the certificate's, its
// CA signed it, it names every one of names, and it is good past
// renewBefore.
func (p tlsPair) serves(names []string, now time.Time) bool {
	kp, err := tls.X509KeyPair(p.cert, p.key)
	if err != nil {
		return false
	}
	leaf, err := x509.ParseCertificate(kp.Certificate[0])
	if err != nil {
		return false
	}
	ca, err := parseCert(p.ca)
	if err != nil || leaf.CheckSignatureFrom(ca) != nil {
		return false
	}
	for _, n := range names {
		if !slices.Contains(leaf.DNSNames, n) {
			return false
		}
	}
	return now.Add(renewBefore).Before(leaf.NotAfter)
}

func dnsNames(service, namespace string) []string {
	host := service + "." + namespace + ".svc"
	return []string{service, service + "." + namespace, host, host + ".cluster.local"}
}

// newPair makes a CA and a serving certificate it signs, valid 10 years.
func newPair(names []string) (tlsPair, error) {
	return newPairUntil(names, time.Now().AddDate(10, 0, 0))
}

func newPairUntil(names []string, notAfter time.Time) (tlsPair, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsPair{}, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "c4-operator webhook CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return tlsPair{}, err
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsPair{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: names[2]},
		DNSNames:     names,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tlsPair{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tlsPair{}, err
	}
	return tlsPair{
		ca:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// bundle is ca, then the CAs the bundle held before that are still good,
// at most two: a manager serving one of their certificates (started before
// the Secret, or before a renewal) is admitted while it runs, however often
// the new one restarts. A CA kept after its manager stopped trusts nothing
// anyone can still sign: its key was never kept.
func bundle(ca, before []byte) []byte {
	out := append([]byte(nil), ca...)
	kept := 0
	for rest := before; kept < 2; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		prev := pem.EncodeToMemory(block)
		if bytes.Equal(prev, ca) {
			continue
		}
		if c, err := x509.ParseCertificate(block.Bytes); err != nil || !c.IsCA || time.Now().After(c.NotAfter) {
			continue
		}
		out = append(out, prev...)
		kept++
	}
	return out
}

func parseCert(p []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(p)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}
