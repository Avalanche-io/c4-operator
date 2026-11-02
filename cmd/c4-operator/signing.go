package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
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
	AudienceSigner = c4v1.SignerAudience
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
	// people is the people's issuer (people.go), in the manager beside the
	// workload issuer and served only at the HUD's origin; the issuer's
	// listener stays the workloads'. Nil where the install names no
	// people's store.
	people http.Handler
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

// issuerRunnable serves the certificate issuer on addr. projects reads
// the Projects that name importers (the manager's API reader).
func issuerRunnable(s *signing, kube kubernetes.Interface, projects client.Reader, namespace, addr string) everyReplica {
	iss := workloadIssuer(s, kube, projects, namespace, nil)
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

// workloadIssuer is the farm's workload issuer: it certifies a reviewed
// token's account (audience c4-signer) as signerIdentity says, when only
// (if given) lets that account through.
func workloadIssuer(s *signing, kube kubernetes.Interface, projects client.Reader, namespace string, only func(*auth.Identity) error) *provenance.Issuer {
	rev := &auth.Reviewer{Client: kube, Tier: "signer"}
	return &provenance.Issuer{CA: s.ca, Identify: func(ctx context.Context, token string) (string, time.Duration, error) {
		id, err := rev.Review(ctx, token, AudienceSigner)
		if err != nil {
			return "", 0, err
		}
		if only != nil {
			if err := only(id); err != nil {
				return "", 0, err
			}
		}
		return signerIdentity(ctx, projects, id, namespace)
	}}
}

// hostIssuer is the workload issuer as the HUD serves it to host runners,
// at its own address (/workload/v1/certificate): a machine outside the
// cluster reaches the farm only there. It certifies host runners'
// accounts (c4-hosts/<node>) alone; every other workload asks the issuer
// inside the cluster.
func hostIssuer(s *signing, kube kubernetes.Interface, projects client.Reader, namespace string) *provenance.Issuer {
	return workloadIssuer(s, kube, projects, namespace, func(id *auth.Identity) error {
		if id.Namespace != hostsNamespace {
			return fmt.Errorf("%s is not a host runner's account: the HUD's issuer certifies host runners only", id.User)
		}
		return nil
	})
}

// hostsNamespace holds the host runners' accounts, one per machine.
const hostsNamespace = "c4-hosts"

// signerIdentity is the identity the issuer certifies a reviewed token's
// account as, and for how long. A node daemon is certified as its node
// for a day; a host runner (c4-hosts/<node>) as runner/<node> for an
// hour; a unit, on its own pod's token, as that unit's attempt for an
// hour (unitIdentity); a workload as itself for an hour, and only if a
// project names it among its importers. Anything else is refused.
func signerIdentity(ctx context.Context, projects client.Reader, id *auth.Identity, namespace string) (string, time.Duration, error) {
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
	case id.Namespace == hostsNamespace:
		// A host runner is its machine's own account, named for its
		// Node (config/rbac/host-runner.yaml).
		return provenance.RunnerIdentity(id.Account), time.Hour, nil
	case id.PodName != "":
		// A token bound to a pod: a unit's is certified as that unit,
		// never as its account, so a unit is only ever itself.
		if unit, ok, err := unitIdentity(ctx, projects, id); ok || err != nil {
			return unit, time.Hour, err
		}
	}
	var list c4v1.ProjectList
	if err := projects.List(ctx, &list); err != nil {
		return "", 0, fmt.Errorf("reading the projects' importers: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.Imports(id.Namespace, id.Account) {
			return provenance.ServiceAccountIdentity(id.Namespace, id.Account), time.Hour, nil
		}
	}
	return "", 0, fmt.Errorf("%s is not an importer of a project: a project names the accounts that sign its imports and ingests (spec.importers)", id.User)
}

// unitIdentity is the identity of the unit whose pod a reviewed token is
// bound to, unit/<ns>/<job>/<unit>@<job UID>/<attempt> (PEOPLE-KEYS 5.1),
// for its act/v1 records only: CheckSigner gives a unit/ leaf no other
// kind. ok is false for a pod that is no unit's, which is judged as its
// account is. The pod is read now, so a token outliving its pod, or one
// for a finished attempt, certifies nothing.
func unitIdentity(ctx context.Context, r client.Reader, id *auth.Identity) (string, bool, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: id.Namespace, Name: id.PodName}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return "", true, fmt.Errorf("pod %s/%s is gone", id.Namespace, id.PodName)
		}
		return "", true, fmt.Errorf("reading pod %s/%s: %w", id.Namespace, id.PodName, err)
	}
	job := pod.Labels[c4v1.LabelJob]
	if job == "" {
		return "", false, nil
	}
	ref := metav1.GetControllerOf(&pod)
	attempt, err := strconv.Atoi(pod.Annotations[c4v1.AnnotationAttempt])
	switch {
	case string(pod.UID) != id.PodUID:
		return "", true, fmt.Errorf("the token is bound to pod %s/%s %s, which is now another pod", id.Namespace, id.PodName, id.PodUID)
	case ref == nil || ref.Kind != "WorkJob" || ref.Name != job || ref.UID == "":
		return "", true, fmt.Errorf("pod %s/%s is not a unit of job %s", id.Namespace, id.PodName, job)
	case pod.Labels[c4v1.LabelUnit] == "":
		return "", true, fmt.Errorf("pod %s/%s names no unit", id.Namespace, id.PodName)
	case err != nil || attempt < 1:
		return "", true, fmt.Errorf("pod %s/%s names no attempt", id.Namespace, id.PodName)
	case pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || pod.DeletionTimestamp != nil:
		return "", true, fmt.Errorf("unit %s/%s's attempt %d has ended", job, pod.Labels[c4v1.LabelUnit], attempt)
	}
	// a host runner's unit is certified only through its runner, which
	// asks for units bound to its own Node, of its owners' jobs
	if host, err := hostNode(ctx, r, pod.Spec.NodeName); err != nil {
		return "", true, err
	} else if host {
		return "", true, fmt.Errorf("pod %s/%s runs on host runner %s: its certificate comes through that runner", id.Namespace, id.PodName, pod.Spec.NodeName)
	}
	return provenance.UnitIdentity(id.Namespace, job, pod.Labels[c4v1.LabelUnit], string(ref.UID), attempt), true, nil
}

// hostNode reports whether Node name is a host runner's machine: its
// taint or its host label says so. A pod bound to no Node is on none.
func hostNode(ctx context.Context, r client.Reader, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	var n corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("node %s is gone", name)
		}
		return false, fmt.Errorf("reading node %s: %w", name, err)
	}
	if n.Labels[c4v1.LabelHost] != "" {
		return true, nil
	}
	for _, t := range n.Spec.Taints {
		if t.Key == c4v1.TaintHostRunner {
			return true, nil
		}
	}
	return false, nil
}

// everyReplica runs on every manager replica, leader or not: the issuer
// is stateless and the Service spreads requests over all replicas.
type everyReplica func(ctx context.Context) error

func (f everyReplica) Start(ctx context.Context) error { return f(ctx) }
func (f everyReplica) NeedLeaderElection() bool        { return false }
