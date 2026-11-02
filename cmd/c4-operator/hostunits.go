package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A host unit has no pod-bound token: it runs on its owner's machine, under
// the host runner, which makes its key and asks the workload issuer, at
// the HUD's address, to certify it (PEOPLE-KEYS 5.1, K5 item 2). The
// runner signs the request with its own runner/<node> key (request/v1),
// so no token stands in for its key. The issuer certifies only a unit pod
// bound to that runner's Node, of a job whose owner is among the Node's
// owners, and only while it runs, for an hour; the leaf names the runner
// (urn:c4:runner:<node>), since the key is issued through it and held by
// it. The runner renews it at half-life.

// hostUnitTTL is how long a host unit's certificate lives.
const hostUnitTTL = time.Hour

// hostUnitRequest is what a runner sends for its unit's certificate.
type hostUnitRequest struct {
	PublicKey []byte `json:"publicKey"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
}

// hostUnitIssuer serves POST /v1/unit-certificate (the HUD mounts it at
// /workload/v1/unit-certificate): a host unit's certificate, for its
// runner. pods reads pods, Nodes and WorkJobs (the manager's API reader).
func hostUnitIssuer(s *signing, pods client.Reader) http.Handler {
	seen := provenance.NewMemorySeen()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/unit-certificate" {
			http.Error(w, "POST /v1/unit-certificate", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<12+1))
		if err != nil || len(body) > 1<<12 {
			http.Error(w, "send {publicKey, namespace, pod}, at most 4 KiB", http.StatusBadRequest)
			return
		}
		node, err := hostRunnerOf(s, r, body, seen)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		var req hostUnitRequest
		if err := json.Unmarshal(body, &req); err != nil || len(req.PublicKey) != ed25519.PublicKeySize || req.Namespace == "" || req.Pod == "" {
			http.Error(w, "send {publicKey, namespace, pod}: a 32-byte Ed25519 public key and the unit's pod", http.StatusBadRequest)
			return
		}
		identity, err := hostUnitIdentity(r, pods, node, req.Namespace, req.Pod)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		leaf, err := s.ca.IssueHostUnit(ed25519.PublicKey(req.PublicKey), identity, node, hostUnitTTL, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			Identity string   `json:"identity"`
			Chain    [][]byte `json:"chain"`
		}{identity, [][]byte{leaf.Raw, s.ca.Cert.Raw}})
	})
}

// hostRunnerOf is the Node of the host runner that signed request r, whose
// body is body: a request/v1 signed as runner/<node>, held to the request,
// taken once.
func hostRunnerOf(s *signing, r *http.Request, body []byte, seen provenance.SeenSet) (string, error) {
	h := r.Header.Get(schema.RequestHeader)
	if h == "" {
		return "", fmt.Errorf("a host unit's certificate is asked for by its runner, signing the request as itself (%s)", schema.RequestHeader)
	}
	data, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		return "", fmt.Errorf("the signed request (%s) is not base64", schema.RequestHeader)
	}
	v, req, err := provenance.VerifyRequest(data, s.pool, time.Now(), seen)
	if err != nil {
		return "", fmt.Errorf("the signed request does not verify: %v", err)
	}
	if err := req.Matches(r.Method, r.RequestURI, body); err != nil {
		return "", fmt.Errorf("the signed request is not this request: %v", err)
	}
	node, ok := strings.CutPrefix(v.Identity, provenance.IdentityBase+"runner/")
	if !ok || node == "" || strings.Contains(node, "/") {
		return "", fmt.Errorf("%s is not a host runner: a host unit's certificate is its runner's to ask for", v.Identity)
	}
	return node, nil
}

// hostUnitIdentity is the unit a host runner on Node node may have
// certified for pod ns/name: a live unit pod bound to that Node, of a job
// whose owner is among the Node's owners.
func hostUnitIdentity(r *http.Request, pods client.Reader, node, ns, name string) (string, error) {
	ctx := r.Context()
	var pod corev1.Pod
	if err := pods.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("pod %s/%s is gone", ns, name)
		}
		return "", fmt.Errorf("reading pod %s/%s: %w", ns, name, err)
	}
	if pod.Spec.NodeName != node {
		return "", fmt.Errorf("pod %s/%s is not bound to %s", ns, name, node)
	}
	unit, err := podUnit(&pod)
	if err != nil {
		return "", err
	}
	var n corev1.Node
	if err := pods.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return "", fmt.Errorf("reading node %s: %w", node, err)
	}
	var owners []string
	if err := json.Unmarshal([]byte(n.Annotations[c4v1.AnnotationNodeOwners]), &owners); err != nil || len(owners) == 0 {
		return "", fmt.Errorf("%s names no owner: it is not enrolled", node)
	}
	ref := metav1.GetControllerOf(&pod)
	var job c4v1.WorkJob
	if err := pods.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &job); err != nil || job.UID != ref.UID {
		return "", fmt.Errorf("pod %s/%s's job %s is not here", ns, name, ref.Name)
	}
	owner := job.Annotations[c4v1.AnnotationOwner]
	if owner == "" || !slices.Contains(owners, owner) {
		return "", fmt.Errorf("job %s/%s is %q's, and %s runs only its owners' work (%s)", ns, ref.Name, owner, node, strings.Join(owners, ", "))
	}
	return unit, nil
}
