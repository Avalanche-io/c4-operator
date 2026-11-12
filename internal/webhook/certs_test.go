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
	testNS     = "c4-system"
	testSvc    = "c4-operator-webhook"
	testConfig = "c4-operator"
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
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}); err != nil {
		t.Fatal(err)
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
	if err := BootstrapCerts(context.Background(), c, dir, testSvc, testNS, testConfig); err != nil {
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

// Two managers serve the same certificate, which the bundle verifies, and
// the second start leaves the configuration as it was: a rolling update
// fails no admission. A manager made before the Secret (its own CA in the
// bundle) is admitted until it stops, however often the new one restarts.
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
		t.Fatalf("after the first start the bundle verifies the new certificate %v, the old manager's %v", verifies(t, b1, certA), verifies(t, b1, old.cert))
	}
	var before admissionv1.MutatingWebhookConfiguration
	c.Get(ctx, types.NamespacedName{Name: testConfig}, &before)

	certB, keyB, b2 := start(t, c) // a second manager, or the first again
	if !bytes.Equal(certA, certB) || !bytes.Equal(keyA, keyB) {
		t.Fatal("two managers serve different certificates")
	}
	var after admissionv1.MutatingWebhookConfiguration
	c.Get(ctx, types.NamespacedName{Name: testConfig}, &after)
	if after.ResourceVersion != before.ResourceVersion || !bytes.Equal(b1, b2) {
		t.Fatal("a second start changed the webhook configuration")
	}
	if !verifies(t, b2, old.cert) {
		t.Fatal("a restart dropped the CA of a manager from before")
	}
}

// A Secret whose pair does not serve the webhook's names, or expires
// within renewBefore, is replaced.
func TestAPairForOtherNamesIsReplaced(t *testing.T) {
	for name, make := range map[string]func() (tlsPair, error){
		"other names": func() (tlsPair, error) { return newPair(dnsNames("another-service", testNS)) },
		"expiring":    func() (tlsPair, error) { return newPairUntil(dnsNames(testSvc, testNS), time.Now().Add(renewBefore/2)) },
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
	if err := c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: TLSSecret}, Type: corev1.SecretTypeTLS, Data: other.data()}); err != nil {
		t.Fatal(err)
	}
	cert, _, b := start(t, c)
	if bytes.Equal(cert, other.cert) || !verifies(t, b, cert) {
		t.Fatal("the pair in the Secret was served, not replaced")
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: TLSSecret}, &s); err != nil {
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
			if err := BootstrapCerts(context.Background(), c, dir, testSvc, testNS, testConfig); err != nil {
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
