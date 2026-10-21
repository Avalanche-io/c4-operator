package auth

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MetricsReader is the ClusterRole (config/rbac) that may get /metrics.
// A scraper's service account is bound to it.
const MetricsReader = "c4-metrics-reader"

// metricsCacheTTL is how long an access decision for /metrics is kept.
const metricsCacheTTL = time.Minute

// MetricsGuard admits a request for /metrics only from a caller the API
// server knows and lets get the path: TokenReview of its bearer token
// (the API server's audience), then SubjectAccessReview of the non-resource
// URL, the check controller-runtime's metrics filter makes. Decisions are
// cached for a minute.
type MetricsGuard struct {
	Reviewer *Reviewer

	mu    sync.Mutex
	cache map[string]metricsDecision
}

type metricsDecision struct {
	allowed bool
	until   time.Time
}

// Wrap guards next.
func (g *MetricsGuard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token, found := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if !found || token == "" {
			http.Error(w, "bearer token required: scrape with a service account bound to the "+MetricsReader+" ClusterRole, sending its token", http.StatusUnauthorized)
			return
		}
		id, err := g.Reviewer.Review(req.Context(), token, AudienceAPI)
		if errors.Is(err, ErrInvalid) {
			http.Error(w, "token is not valid: send a service account token for the API server, from an account bound to the "+MetricsReader+" ClusterRole", http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, "token review failed", http.StatusServiceUnavailable)
			return
		}
		verb := strings.ToLower(req.Method)
		allowed, err := g.allowed(req, id, verb)
		if err != nil {
			http.Error(w, "access review failed", http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			http.Error(w, id.User+" may not "+verb+" "+req.URL.Path+" (bind it to the "+MetricsReader+" ClusterRole)", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (g *MetricsGuard) allowed(req *http.Request, id *Identity, verb string) (bool, error) {
	key := id.User + "\x00" + verb + "\x00" + req.URL.Path
	now := time.Now()
	g.mu.Lock()
	if d, ok := g.cache[key]; ok && now.Before(d.until) {
		g.mu.Unlock()
		return d.allowed, nil
	}
	g.mu.Unlock()
	sar, err := g.Reviewer.Client.AuthorizationV1().SubjectAccessReviews().Create(req.Context(), &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:                  id.User,
			Groups:                id.Groups,
			NonResourceAttributes: &authzv1.NonResourceAttributes{Path: req.URL.Path, Verb: verb},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	g.mu.Lock()
	if g.cache == nil || len(g.cache) >= 1000 {
		g.cache = map[string]metricsDecision{}
	}
	g.cache[key] = metricsDecision{allowed: sar.Status.Allowed, until: now.Add(metricsCacheTTL)}
	g.mu.Unlock()
	return sar.Status.Allowed, nil
}
