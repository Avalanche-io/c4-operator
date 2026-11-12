package webhook

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	testNS       = "c4-system"
	testSecretNS = "c4-signing"
	testSvc      = "c4-operator-webhook"
	testConfig   = "c4-operator"
)

func envClient(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		matches, _ := filepath.Glob("../../bin/k8s/k8s/*")
		if len(matches) == 0 {
			t.Skip("envtest binaries not found; run `make envtest`")
		}
		abs, _ := filepath.Abs(matches[len(matches)-1])
		t.Setenv("KUBEBUILDER_ASSETS", abs)
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Stop() })
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, ns := range []string{testNS, testSecretNS} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	none := admissionv1.SideEffectClassNone
	path := "/mutate-pod"
	cfgObj := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: testConfig},
		Webhooks: []admissionv1.MutatingWebhook{{Name: "pods.c4.avalanche.io", SideEffects: &none, AdmissionReviewVersions: []string{"v1"},
			ClientConfig: admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{Namespace: testNS, Name: testSvc, Path: &path}}}}}
	if err := c.Create(ctx, cfgObj); err != nil {
		t.Fatal(err)
	}
	return c
}

// start is one manager's start: its certificate directory's files, and
// the configuration's bundle after it.
func start(t *testing.T, c client.Client) (cert, key, bundle []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := BootstrapCerts(context.Background(), c, dir, testSvc, testNS, testSecretNS, testConfig); err != nil {
		t.Fatal(err)
	}
	cert, _ = os.ReadFile(filepath.Join(dir, "tls.crt"))
	key, _ = os.ReadFile(filepath.Join(dir, "tls.key"))
	return cert, key, caBundle(t, c)
}

func caBundle(t *testing.T, c client.Client) []byte {
	t.Helper()
	var cfg admissionv1.MutatingWebhookConfiguration
	if err := c.Get(context.Background(), types.NamespacedName{Name: testConfig}, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Webhooks[0].ClientConfig.CABundle
}

// verifies says whether a client trusting bundle accepts cert for the
// webhook's service name, as the API server does.
func verifies(t *testing.T, bundle, cert []byte) bool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		return false
	}
	block, _ := pem.Decode(cert)
	if block == nil {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: testSvc + "." + testNS + ".svc"})
	return err == nil
}

// Two managers serve the same certificate, which the bundle verifies. The
// start that made the pair keeps the CA of a manager from before the
// Secret, so it is admitted through the update; the next start drops it
// (as revoking a leaked key needs), and starts after that change nothing.
func TestEveryManagerServesOneCertificate(t *testing.T) {
	c := envClient(t)
	ctx := context.Background()

	// A manager from before: its own CA in the bundle, its certificate.
	old, err := newPair(dnsNames(testSvc, testNS))
	if err != nil {
		t.Fatal(err)
	}
	var cfg admissionv1.MutatingWebhookConfiguration
	if err := c.Get(ctx, types.NamespacedName{Name: testConfig}, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Webhooks[0].ClientConfig.CABundle = old.ca
	if err := c.Update(ctx, &cfg); err != nil {
		t.Fatal(err)
	}

	certA, keyA, b1 := start(t, c)
	if !verifies(t, b1, certA) || !verifies(t, b1, old.cert) {
		t.Fatalf("after the start that made the pair the bundle verifies the new certificate %v, the old manager's %v", verifies(t, b1, certA), verifies(t, b1, old.cert))
	}
	certB, keyB, b2 := start(t, c) // a second manager, or the first again
	if !bytes.Equal(certA, certB) || !bytes.Equal(keyA, keyB) {
		t.Fatal("two managers serve different certificates")
	}
	if !verifies(t, b2, certB) || verifies(t, b2, old.cert) {
		t.Fatalf("after the next start the bundle verifies the pair %v, the old manager's %v (want true, false)", verifies(t, b2, certB), verifies(t, b2, old.cert))
	}
	var before, after admissionv1.MutatingWebhookConfiguration
	c.Get(ctx, types.NamespacedName{Name: testConfig}, &before)
	start(t, c)
	c.Get(ctx, types.NamespacedName{Name: testConfig}, &after)
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatal("a start with nothing to change changed the webhook configuration")
	}
}

// A leaked key is revoked by deleting the Secret and starting twice: the
// first start keeps the leaked CA for managers still on it, the second
// trusts only the new pair.
func TestDeletingTheSecretRevokesInTwoStarts(t *testing.T) {
	c := envClient(t)
	ctx := context.Background()
	leaked, _, _ := start(t, c)
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: testSecretNS, Name: TLSSecret}, &s); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, &s); err != nil {
		t.Fatal(err)
	}
	cert1, _, b1 := start(t, c)
	if bytes.Equal(cert1, leaked) || !verifies(t, b1, cert1) || !verifies(t, b1, leaked) {
		t.Fatal("the first start after the deletion did not make a new pair beside the old one")
	}
	_, _, b2 := start(t, c)
	if verifies(t, b2, leaked) || !verifies(t, b2, cert1) {
		t.Fatal("the second start still trusts the leaked certificate")
	}
}

// The bundle is built from certificates, not text: a CA without a final
// newline, or with CRLF lines, and earlier CAs repeated or expired, give a
// bundle that verifies, each CA once, at most two kept.
func TestTheBundleIsBuiltFromCertificates(t *testing.T) {
	pairs := make([]tlsPair, 4)
	for i := range pairs {
		var err error
		if pairs[i], err = newPair(dnsNames(testSvc, testNS)); err != nil {
			t.Fatal(err)
		}
	}
	expired, err := newPairUntil(dnsNames(testSvc, testNS), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ca := bytes.TrimRight(pairs[0].ca, "\n")
	before := append(append(append(bytes.ReplaceAll(pairs[1].ca, []byte("\n"), []byte("\r\n")), pairs[1].ca...), expired.ca...), append(pairs[2].ca, pairs[3].ca...)...)
	b := bundle(ca, before)
	for i, want := range []bool{true, true, true, false} {
		if got := verifies(t, b, pairs[i].cert); got != want {
			t.Errorf("pair %d verified %v, want %v", i, got, want)
		}
	}
	if verifies(t, b, expired.cert) {
		t.Error("an expired CA was kept")
	}
	if n := bytes.Count(b, []byte("BEGIN CERTIFICATE")); n != 3 {
		t.Errorf("%d certificates in the bundle, want 3 (the pair's and two before)", n)
	}
}

// A Secret whose pair does not serve the webhook's names, or expires
// within renewBefore, is replaced.
func TestAPairForOtherNamesIsReplaced(t *testing.T) {
	for name, make := range map[string]func() (tlsPair, error){
		"other names": func() (tlsPair, error) { return newPair(dnsNames("another-service", testNS)) },
		"expiring":    func() (tlsPair, error) { return newPairUntil(dnsNames(testSvc, testNS), time.Now().Add(renewBefore/2)) },
		"another CA's": func() (tlsPair, error) {
			a, err := newPair(dnsNames(testSvc, testNS))
			if err != nil {
				return a, err
			}
			b, err := newPair(dnsNames(testSvc, testNS))
			a.ca = b.ca // the leaf is not this CA's
			return a, err
		},
	} {
		t.Run(name, func(t *testing.T) { replaced(t, make) })
	}
}

func replaced(t *testing.T, make func() (tlsPair, error)) {
	c := envClient(t)
	ctx := context.Background()
	other, err := make()
	if err != nil {
		t.Fatal(err)
	}
	// Opaque, as one made by hand may be: its type is immutable, and the
	// replacement must leave it.
	if err := c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testSecretNS, Name: TLSSecret}, Type: corev1.SecretTypeOpaque, Data: other.data()}); err != nil {
		t.Fatal(err)
	}
	cert, _, b := start(t, c)
	if bytes.Equal(cert, other.cert) || !verifies(t, b, cert) {
		t.Fatal("the pair in the Secret was served, not replaced")
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: testSecretNS, Name: TLSSecret}, &s); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.Data[secretCert], cert) {
		t.Fatal("the Secret does not hold the pair served")
	}
}

// Two managers starting at once both find no Secret; they serve one pair.
func TestTwoFirstStartsServeOnePair(t *testing.T) {
	c := envClient(t)
	var wg sync.WaitGroup
	certs := make([][]byte, 4)
	for i := range certs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := t.TempDir()
			if err := BootstrapCerts(context.Background(), c, dir, testSvc, testNS, testSecretNS, testConfig); err != nil {
				t.Error(err)
				return
			}
			certs[i], _ = os.ReadFile(filepath.Join(dir, "tls.crt"))
		}()
	}
	wg.Wait()
	b := caBundle(t, c)
	for i, cert := range certs {
		if !bytes.Equal(cert, certs[0]) {
			t.Fatalf("manager %d serves another certificate", i)
		}
		if !verifies(t, b, cert) {
			t.Fatalf("the bundle does not verify manager %d's certificate", i)
		}
	}
}

// With signing off there is no Secret: a manager makes its own pair, and
// the bundle is its CA alone.
func TestWithoutASigningNamespaceEachStartMakesItsOwn(t *testing.T) {
	c := envClient(t)
	dir := t.TempDir()
	if err := BootstrapCerts(context.Background(), c, dir, testSvc, testNS, "", testConfig); err != nil {
		t.Fatal(err)
	}
	cert, _ := os.ReadFile(filepath.Join(dir, "tls.crt"))
	b := caBundle(t, c)
	if !verifies(t, b, cert) || bytes.Count(b, []byte("BEGIN CERTIFICATE")) != 1 {
		t.Fatal("the bundle is not the start's own CA")
	}
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testSecretNS, Name: TLSSecret}, &s); err == nil {
		t.Fatal("a Secret was made with signing off")
	}
}
