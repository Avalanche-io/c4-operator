// Package auth checks the identities the system acts for.
//
// Pods prove who they are to the CSI driver with a projected
// service-account token (audience c4.avalanche.io) that kubelet hands the
// driver at NodePublishVolume. Node daemons, regional replicas and the HUD
// prove who they are to each other with their own projected tokens
// (audience c4-tier). Both are validated with TokenReview and cached
// until the token expires. A metrics scraper presents its default
// service-account token, which must also be let get /metrics
// (MetricsGuard).
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Avalanche-io/c4-operator/internal/metrics"
)

// Audiences.
const (
	AudiencePod  = "c4.avalanche.io"
	AudienceTier = "c4-tier"
)

// Extra keys TokenReview sets for pod-bound tokens.
const (
	ExtraPodName  = "authentication.kubernetes.io/pod-name"
	ExtraPodUID   = "authentication.kubernetes.io/pod-uid"
	ExtraNodeName = "authentication.kubernetes.io/node-name"
)

// Identity is a validated token's subject.
type Identity struct {
	User      string // system:serviceaccount:<namespace>:<name>
	Namespace string
	Account   string
	PodName   string
	PodUID    string
	NodeName  string
	Groups    []string
	Expires   time.Time
}

// Reviewer validates tokens with TokenReview and caches the results.
type Reviewer struct {
	Client kubernetes.Interface
	// MaxCache bounds the cache (entries); 0 means 10,000.
	MaxCache int
	// Tier labels metrics.
	Tier string

	mu    sync.Mutex
	cache map[[32]byte]*Identity
}

// ErrInvalid means the token did not validate for the audience.
var ErrInvalid = errors.New("token is not valid")

// AudienceAPI is the API server's own audience: a pod's default
// service-account token, which a metrics scraper presents.
const AudienceAPI = ""

// Review validates token for audience (AudienceAPI: for the API server's
// own audiences).
func (r *Reviewer) Review(ctx context.Context, token, audience string) (*Identity, error) {
	if token == "" {
		return nil, fmt.Errorf("%w: no token", ErrInvalid)
	}
	key := sha256.Sum256([]byte(audience + "\x00" + token))
	r.mu.Lock()
	if id, ok := r.cache[key]; ok && time.Now().Before(id.Expires) {
		r.mu.Unlock()
		metrics.TierAuth.WithLabelValues(r.Tier, "cached").Inc()
		return id, nil
	}
	r.mu.Unlock()

	spec := authnv1.TokenReviewSpec{Token: token}
	if audience != AudienceAPI {
		spec.Audiences = []string{audience}
	}
	tr, err := r.Client.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{Spec: spec}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("token review: %w", err)
	}
	if !tr.Status.Authenticated {
		metrics.TierAuth.WithLabelValues(r.Tier, "invalid").Inc()
		return nil, fmt.Errorf("%w: %s", ErrInvalid, tr.Status.Error)
	}
	if audience != AudienceAPI && !contains(tr.Status.Audiences, audience) {
		metrics.TierAuth.WithLabelValues(r.Tier, "invalid").Inc()
		return nil, fmt.Errorf("%w: audience %v", ErrInvalid, tr.Status.Audiences)
	}
	id := &Identity{User: tr.Status.User.Username, Groups: tr.Status.User.Groups, Expires: expiry(token)}
	if ns, sa, ok := splitServiceAccount(id.User); ok {
		id.Namespace, id.Account = ns, sa
	}
	extra := tr.Status.User.Extra
	id.PodName = first(extra[ExtraPodName])
	id.PodUID = first(extra[ExtraPodUID])
	id.NodeName = first(extra[ExtraNodeName])

	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[[32]byte]*Identity{}
	}
	limit := r.MaxCache
	if limit <= 0 {
		limit = 10000
	}
	if len(r.cache) >= limit {
		now := time.Now()
		for k, v := range r.cache {
			if now.After(v.Expires) || len(r.cache) >= limit {
				delete(r.cache, k)
			}
		}
	}
	r.cache[key] = id
	r.mu.Unlock()
	metrics.TierAuth.WithLabelValues(r.Tier, "ok").Inc()
	return id, nil
}

// TierAuthorizer admits requests whose bearer token belongs to one of the
// allowed service accounts ("namespace/name").
func (r *Reviewer) TierAuthorizer(allowed ...string) func(*http.Request) (string, int, error) {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	return func(req *http.Request) (string, int, error) {
		token, found := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if !found {
			metrics.TierAuth.WithLabelValues(r.Tier, "denied").Inc()
			return "", http.StatusUnauthorized, errors.New("bearer token required")
		}
		id, err := r.Review(req.Context(), token, AudienceTier)
		if err != nil {
			return "", http.StatusUnauthorized, err
		}
		if !ok[id.Namespace+"/"+id.Account] {
			metrics.TierAuth.WithLabelValues(r.Tier, "denied").Inc()
			return id.User, http.StatusForbidden, fmt.Errorf("%s may not use the tier API", id.User)
		}
		return id.User, 0, nil
	}
}

// PodClaims are what kubelet says about the pod in a CSI volume context.
type PodClaims struct {
	Name, Namespace, UID, ServiceAccount string
}

// TokensKey is where kubelet puts service-account tokens for a CSI
// driver: in the request's secrets with serviceAccountTokenInSecrets, or
// in the volume context without it.
const TokensKey = "csi.storage.k8s.io/serviceAccount.tokens"

// VerifyPod checks a CSI request: the pod's token must validate for the
// pod audience and name the same namespace, pod and UID that kubelet put
// in the volume context.
func (r *Reviewer) VerifyPod(ctx context.Context, volumeContext, secrets map[string]string) (*Identity, error) {
	claims := PodClaims{
		Name:           volumeContext["csi.storage.k8s.io/pod.name"],
		Namespace:      volumeContext["csi.storage.k8s.io/pod.namespace"],
		UID:            volumeContext["csi.storage.k8s.io/pod.uid"],
		ServiceAccount: volumeContext["csi.storage.k8s.io/serviceAccount.name"],
	}
	if claims.Name == "" || claims.Namespace == "" || claims.UID == "" {
		return nil, errors.New("volume context lacks pod information; the CSIDriver needs podInfoOnMount: true")
	}
	raw := secrets[TokensKey]
	if raw == "" {
		raw = volumeContext[TokensKey]
	}
	token, err := tokenFor(raw, AudiencePod)
	if err != nil {
		return nil, err
	}
	id, err := r.Review(ctx, token, AudiencePod)
	if err != nil {
		return nil, err
	}
	switch {
	case id.Namespace != claims.Namespace:
		return nil, fmt.Errorf("token is for namespace %q, volume context says %q", id.Namespace, claims.Namespace)
	case id.PodName != claims.Name || id.PodUID != claims.UID:
		return nil, fmt.Errorf("token is bound to pod %s/%s (%s), volume context says %s (%s)", id.Namespace, id.PodName, id.PodUID, claims.Name, claims.UID)
	case claims.ServiceAccount != "" && id.Account != claims.ServiceAccount:
		return nil, fmt.Errorf("token is for service account %q, volume context says %q", id.Account, claims.ServiceAccount)
	}
	return id, nil
}

// tokenFor extracts one audience's token from the JSON kubelet passes as
// csi.storage.k8s.io/serviceAccount.tokens.
func tokenFor(raw, audience string) (string, error) {
	if raw == "" {
		return "", errors.New("no service account token in the volume context; the CSIDriver needs tokenRequests for audience " + audience)
	}
	var m map[string]struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", fmt.Errorf("service account tokens: %w", err)
	}
	t, ok := m[audience]
	if !ok || t.Token == "" {
		return "", fmt.Errorf("no token for audience %s", audience)
	}
	return t.Token, nil
}

func splitServiceAccount(user string) (ns, name string, ok bool) {
	rest, found := strings.CutPrefix(user, "system:serviceaccount:")
	if !found {
		return "", "", false
	}
	ns, name, ok = strings.Cut(rest, ":")
	return ns, name, ok
}

// expiry reads a JWT's exp claim without verifying it (TokenReview did),
// capped at an hour so a revoked token is not trusted for long.
func expiry(token string) time.Time {
	limit := time.Now().Add(time.Hour)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Now().Add(time.Minute)
	}
	payload, err := decodeSegment(parts[1])
	if err != nil {
		return time.Now().Add(time.Minute)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Now().Add(time.Minute)
	}
	exp := time.Unix(claims.Exp, 0)
	if exp.After(limit) {
		return limit
	}
	return exp
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func first(v authnv1.ExtraValue) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
