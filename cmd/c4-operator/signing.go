package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Avalanche-io/c4-operator/internal/auth"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// Signing (DESIGN.md section 9.2): the farm's signing CA lives in a
// Secret in its own namespace, which only the manager may read. The
// manager issues short-lived certificates to signers that prove who they
// are with a projected service-account token (audience c4-signer), and
// signs as its own components with certificates it issues to itself.
const (
	// AudienceSigner is the token audience the issuer accepts.
	AudienceSigner = "c4-signer"
	caSecret       = "c4-signing-ca"
	trustConfigMap = "c4-provenance"
)

type signing struct {
	ca         *provenance.CA
	controller *provenance.Signer
	queue      *provenance.Signer
	hud        *provenance.Signer
	roots      []byte // PEM trust bundle
	pool       *x509.CertPool
}

// setupSigning loads or creates the CA, publishes its certificate, and
// makes the components' signers.
func setupSigning(ctx context.Context, kube kubernetes.Interface, caNamespace, namespace string) (*signing, error) {
	ca, err := loadOrCreateCA(ctx, kube, caNamespace)
	if err != nil {
		return nil, fmt.Errorf("signing CA: %w", err)
	}
	certPEM, _, err := ca.PEM()
	if err != nil {
		return nil, err
	}
	roots, err := publishRoot(ctx, kube, namespace, certPEM)
	if err != nil {
		return nil, fmt.Errorf("publish the signing root: %w", err)
	}
	pool, err := provenance.RootsFromPEM(roots)
	if err != nil {
		return nil, err
	}
	s := &signing{ca: ca, roots: roots, pool: pool}
	for name, dst := range map[string]**provenance.Signer{"controller": &s.controller, "queue": &s.queue, "hud": &s.hud} {
		sg, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity(name), 24*time.Hour))
		if err != nil {
			return nil, err
		}
		*dst = sg
	}
	return s, nil
}

func loadOrCreateCA(ctx context.Context, kube kubernetes.Interface, ns string) (*provenance.CA, error) {
	sec, err := kube.CoreV1().Secrets(ns).Get(ctx, caSecret, metav1.GetOptions{})
	if err == nil {
		return provenance.ParseCA(sec.Data["tls.crt"], sec.Data["tls.key"])
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := ca.PEM()
	if err != nil {
		return nil, err
	}
	_, err = kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: caSecret, Namespace: ns},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Another replica made it first.
		return loadOrCreateCA(ctx, kube, ns)
	}
	return ca, err
}

// publishRoot adds the CA certificate to the farm's trust bundle (the
// c4-provenance ConfigMap, key ca.crt) and returns the bundle. Roots are
// only added, so records signed under an earlier CA still verify.
func publishRoot(ctx context.Context, kube kubernetes.Interface, ns string, certPEM []byte) ([]byte, error) {
	cms := kube.CoreV1().ConfigMaps(ns)
	cm, err := cms.Get(ctx, trustConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: trustConfigMap, Namespace: ns}, Data: map[string]string{"ca.crt": string(certPEM)}}
		if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return nil, err
		}
		return certPEM, nil
	}
	if err != nil {
		return nil, err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	bundle := cm.Data["ca.crt"]
	if !strings.Contains(bundle, strings.TrimSpace(string(certPEM))) {
		bundle += string(certPEM)
		cm.Data["ca.crt"] = bundle
		if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return nil, err
		}
	}
	return []byte(bundle), nil
}

// issuerRunnable serves the certificate issuer on addr.
func issuerRunnable(s *signing, kube kubernetes.Interface, namespace, addr string) everyReplica {
	rev := &auth.Reviewer{Client: kube, Tier: "signer"}
	iss := &provenance.Issuer{CA: s.ca, Identify: func(ctx context.Context, token string) (string, time.Duration, error) {
		id, err := rev.Review(ctx, token, AudienceSigner)
		if err != nil {
			return "", 0, err
		}
		switch {
		case id.Namespace == "":
			// People sign with their own keys through an OIDC-backed
			// issuer (designed, DESIGN.md section 9.2); this issuer
			// serves workloads.
			return "", 0, fmt.Errorf("%s is not a service account", id.User)
		case id.Namespace == namespace && id.Account == "c4-node":
			if id.NodeName == "" {
				return "", 0, errors.New("a node daemon's token must be bound to its pod (it names no node)")
			}
			return provenance.NodeIdentity(id.NodeName), 24 * time.Hour, nil
		case id.Namespace == namespace && (id.Account == "c4-manager" || id.Account == "c4-regional"):
			return "", 0, fmt.Errorf("%s does not sign records", id.User)
		}
		return provenance.ServiceAccountIdentity(id.Namespace, id.Account), time.Hour, nil
	}}
	return everyReplica(func(ctx context.Context) error {
		hs := &http.Server{Addr: addr, Handler: iss, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			hs.Shutdown(sctx)
		}()
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
}

// everyReplica runs on every manager replica, leader or not: the issuer
// is stateless and the Service spreads requests over all replicas.
type everyReplica func(ctx context.Context) error

func (f everyReplica) Start(ctx context.Context) error { return f(ctx) }
func (f everyReplica) NeedLeaderElection() bool        { return false }
