// Package server is the HUD's backend (DESIGN.md section 11): it serves
// the web app and the HUD API (internal/hud/API.md) from the cluster's own
// state: WorkJobs, Pools, Projects, CapacityRequests, Nodes, unit pods, and
// the metrics of the node daemons and the regional tier. It runs in the
// manager, next to the queue.
//
// Viewers: a bearer token (TokenReview), or headers from a trusted SSO
// proxy, or, in development only, ?as= with configured viewers. A viewer
// sees the projects whose namespaces it may list WorkJobs in
// (SubjectAccessReview). Controls are checked with a SubjectAccessReview
// for the write they make, made by the HUD's service account, and recorded
// as Kubernetes Events naming the user.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4-operator/internal/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// Server is the HUD backend.
type Server struct {
	Client client.Client        // the manager's cached client
	Kube   kubernetes.Interface // logs, events, token and access reviews, c4-system pods
	Queue  *queue.Queue         // burst spend (optional)
	Stores queue.StoreFunc      // project stores, for previews and objects
	Web    fs.FS                // the web app
	Loc    *time.Location       // the farm's time zone
	// DevViewers enables ?as= with these viewers (tests and demos only).
	DevViewers map[string]*api.Viewer
	// TrustProxy reads X-Forwarded-User and X-Forwarded-Groups from an
	// SSO proxy in front of the HUD.
	TrustProxy bool
	Namespace  string // the operator's namespace
	Log        logr.Logger
	// Signer signs a control record for each action a person or agent
	// takes through the HUD or MCP; nil signs none.
	Signer *provenance.Signer
	// Sites place the farm's nodes on the HUD's map (--hud-sites); nil
	// leaves the map out.
	Sites []api.Site

	mu      sync.RWMutex
	m       *model
	version uint64
	meter   meter
	hist    history
	viewers map[string]accessEntry
	// previewCache indexes preview proxies by output view.
	previewCache map[string]map[int]string
	trusted      trustSet
	keysUntil    time.Time
}

type accessEntry struct {
	viewer *api.Viewer
	until  time.Time
}

// Handler returns the HUD's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/graph", s.handleGraph)
	mux.HandleFunc("GET /api/v1/book", s.handleBook)
	mux.HandleFunc("GET /api/v1/jobs/{ns}/{name}", s.handleJob)
	mux.HandleFunc("GET /api/v1/jobs/{ns}/{name}/units/{i}", s.handleUnit)
	mux.HandleFunc("GET /api/v1/jobs/{ns}/{name}/preview", s.handlePreview)
	mux.HandleFunc("GET /api/v1/objects/{project}/{id}", s.handleObject)
	mux.HandleFunc("GET /api/v1/views/{project}/diff", s.handleDiff)
	mux.HandleFunc("GET /api/v1/trace/{ns}/{name}", s.handleTrace)
	mux.HandleFunc("GET /api/v1/custody/{ns}/{name}", s.handleCustody)
	mux.HandleFunc("POST /api/v1/jobs/{ns}/{name}/nudge", s.handleNudge)
	mux.HandleFunc("POST /api/v1/jobs/{ns}/{name}/requeue", s.handleRequeue)
	mux.HandleFunc("POST /api/v1/jobs/{ns}/{name}/blacklist", s.handleBlacklist)
	mux.HandleFunc("POST /api/v1/nodes/{name}/disable", s.handleDisable)
	mux.HandleFunc("POST /api/v1/capacity/{pool}/approve", s.handleApprove)
	files := http.FileServer(http.FS(s.Web))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	return mux
}

// Start collects the farm's state once a second until ctx ends. It runs
// on every replica: the HUD is read-mostly and needs no leader.
func (s *Server) Start(ctx context.Context) error {
	if s.Loc == nil {
		s.Loc = time.UTC
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if err := s.collect(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error(err, "collect")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// NeedLeaderElection lets every replica serve the HUD.
func (s *Server) NeedLeaderElection() bool { return false }

var errUnauthenticated = errors.New("sign in: send a bearer token, or reach the HUD through the SSO proxy")

// viewer resolves who is asking.
func (s *Server) viewer(r *http.Request) (*api.Viewer, error) {
	if s.DevViewers != nil {
		v, ok := s.DevViewers[r.URL.Query().Get("as")]
		if !ok {
			v, ok = s.DevViewers["wrangler"]
		}
		if ok {
			return s.expand(v), nil
		}
	}
	var user string
	var groups []string
	switch {
	case s.TrustProxy && r.Header.Get("X-Forwarded-User") != "":
		user = r.Header.Get("X-Forwarded-User")
		for _, g := range strings.Split(r.Header.Get("X-Forwarded-Groups"), ",") {
			if g = strings.TrimSpace(g); g != "" {
				groups = append(groups, g)
			}
		}
	default:
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.Kube == nil {
			return nil, errUnauthenticated
		}
		tr, err := s.Kube.AuthenticationV1().TokenReviews().Create(r.Context(),
			&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: tok}}, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		if !tr.Status.Authenticated {
			return nil, errUnauthenticated
		}
		user, groups = tr.Status.User.Username, tr.Status.User.Groups
	}
	return s.access(r.Context(), user, groups)
}

// access lists the projects a user may see: those with a namespace where
// the user may list WorkJobs. Cached for a minute.
func (s *Server) access(ctx context.Context, user string, groups []string) (*api.Viewer, error) {
	key := user + "|" + strings.Join(groups, ",")
	s.mu.Lock()
	if e, ok := s.viewers[key]; ok && time.Now().Before(e.until) {
		s.mu.Unlock()
		return e.viewer, nil
	}
	m := s.m
	s.mu.Unlock()
	v := &api.Viewer{User: user, Projects: []string{}, All: true}
	if m != nil {
		for _, p := range m.projects {
			ok := false
			for _, ns := range p.Spec.Namespaces {
				allowed, err := s.allowed(ctx, user, groups, ns, "list", "workjobs", "")
				if err != nil {
					return nil, err
				}
				if allowed {
					ok = true
					break
				}
			}
			if ok {
				v.Projects = append(v.Projects, p.Name)
			} else {
				v.All = false
			}
		}
	}
	s.mu.Lock()
	if s.viewers == nil {
		s.viewers = map[string]accessEntry{}
	}
	s.viewers[key] = accessEntry{viewer: v, until: time.Now().Add(time.Minute)}
	s.mu.Unlock()
	return v, nil
}

// allowed asks the API server whether a user may make a request.
func (s *Server) allowed(ctx context.Context, user string, groups []string, ns, verb, resource, name string) (bool, error) {
	if s.Kube == nil {
		return false, nil
	}
	group := "c4.avalanche.io"
	if resource == "nodes" {
		group = ""
	}
	sar := &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{User: user, Groups: groups,
		ResourceAttributes: &authzv1.ResourceAttributes{Namespace: ns, Verb: verb, Group: group, Resource: resource, Name: name}}}
	out, err := s.Kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return out.Status.Allowed, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// withViewer resolves the viewer or answers 401.
func (s *Server) withViewer(w http.ResponseWriter, r *http.Request) *api.Viewer {
	v, err := s.viewer(r)
	if err != nil {
		fail(w, http.StatusUnauthorized, "%v", err)
		return nil
	}
	return v
}

func (s *Server) current() *model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	m := s.current()
	if m == nil {
		fail(w, http.StatusServiceUnavailable, "the HUD is still reading the farm")
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot(m, v))
}

// handleEvents streams snapshots: at most one a second, only when the
// viewer's snapshot changed, with keepalives in between.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	var last []byte
	idle := 0
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if m := s.current(); m != nil {
			snap := s.snapshot(m, v)
			body, _ := json.Marshal(snap)
			// now changes every second; compare the rest.
			cmp, _ := json.Marshal(struct {
				*api.Snapshot
				Now string `json:"now"`
			}{snap, ""})
			switch {
			case string(cmp) != string(last):
				fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", body)
				last, idle = cmp, 0
			case idle >= 15:
				fmt.Fprint(w, ": keepalive\n\n")
				idle = 0
			default:
				idle++
			}
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}

// expand gives a development viewer configured with every project ("*")
// the projects that exist now.
func (s *Server) expand(v *api.Viewer) *api.Viewer {
	if !v.All || len(v.Projects) > 0 {
		return v
	}
	out := &api.Viewer{User: v.User, All: true, Projects: []string{}}
	if m := s.current(); m != nil {
		for _, p := range m.projects {
			out.Projects = append(out.Projects, p.Name)
		}
	}
	return out
}
