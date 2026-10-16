// Package agent is the per-node half of the operator. It runs as a
// DaemonSet, owns the node's content cache, and answers one question
// from init containers over a unix socket: "make view V of
// Materialization N present on this node". It fetches only the objects
// the node lacks, verifies everything it serves, builds the tree by
// hardlink, and reports per-node hits and misses into the
// Materialization's status.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"
	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/cache"
	"github.com/Avalanche-io/c4-operator/internal/controller"
	"github.com/Avalanche-io/c4-operator/internal/manifest"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/tree"
)

// +kubebuilder:rbac:groups=c4.avalanche.io,resources=materializations,verbs=get
// +kubebuilder:rbac:groups=c4.avalanche.io,resources=materializations/status,verbs=get;update
// +kubebuilder:rbac:groups="",resources=configmaps;secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=pods,verbs=list

// Agent serves one node.
type Agent struct {
	Client       client.Client // uncached
	Cache        *cache.Cache
	Node         string
	FetchWorkers int
	GCInterval   time.Duration
	Log          logr.Logger

	gcKick chan struct{}
}

// Request asks for a view.
type Request struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	View      string `json:"view"`
}

// Response reports what serving it took.
type Response struct {
	View         string `json:"view"`
	Files        int64  `json:"files"`
	Bytes        int64  `json:"bytes"`
	Objects      int    `json:"objects"`
	Hits         int64  `json:"hits"`
	Misses       int64  `json:"misses"`
	Refetched    int64  `json:"refetched"`
	BytesFetched int64  `json:"bytesFetched"`
	Built        bool   `json:"built"`
	Linked       int64  `json:"linked"`
	Copied       int64  `json:"copied"`
	Millis       int64  `json:"millis"`
	Error        string `json:"error,omitempty"`
}

// ErrMissing is returned when the store lacks content the view needs.
var ErrMissing = errors.New("content missing from store")

// Ensure makes the requested view present and valid on this node.
func (a *Agent) Ensure(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	resp := Response{View: req.View}
	viewID, err := c4.Parse(req.View)
	if err != nil {
		return resp, fmt.Errorf("view: %w", err)
	}
	var mat c4v1.Materialization
	key := types.NamespacedName{Namespace: req.Namespace, Name: req.Name}
	if err := a.Client.Get(ctx, key, &mat); err != nil {
		return resp, fmt.Errorf("materialization %s: %w", key, err)
	}

	unlockView := a.Cache.ViewLock(viewID)
	defer unlockView()

	v, err := a.Cache.LoadViewDesc(viewID)
	if err != nil {
		v, err = a.resolve(ctx, &mat, viewID)
		if err != nil {
			return resp, err
		}
	}
	t := v.Totals()
	resp.Files, resp.Bytes = t.Files, t.Bytes

	release := a.Cache.BuildLock()
	defer release()

	src := &lazySource{open: func() (storeurl.Source, error) { return controller.OpenStore(ctx, a.Client, &mat) }}
	objs := v.Objects()
	resp.Objects = len(objs)

	// Preflight: before fetching a byte, confirm the store has everything
	// this node lacks. A missing object fails the request here, so the
	// workload never starts on a partial tree.
	if missing, err := a.preflight(v, objs, src); err != nil {
		return resp, err
	} else if len(missing) > 0 {
		return resp, fmt.Errorf("%w: %d of %d objects, e.g. %s", ErrMissing, len(missing), len(objs), missing[0])
	}

	full := mat.Spec.Verify == c4v1.VerifyFull
	if err := a.fetchAll(objs, src, full, &resp); err != nil {
		return resp, err
	}

	dir := a.Cache.ViewDir(viewID)
	if reason := tree.Check(dir, v, a.Cache.Path); reason != "" {
		if reason != "not built" {
			a.Log.Info("rebuilding view", "view", req.View, "reason", reason)
		}
		bs, err := tree.Build(dir, v, a.Cache.Path, cache.Sentinel)
		if err != nil {
			return resp, fmt.Errorf("build view: %w", err)
		}
		resp.Built, resp.Linked, resp.Copied = true, bs.Linked, bs.Copied
	}

	var retain time.Duration
	if mat.Spec.RetainOnNode != nil {
		retain = mat.Spec.RetainOnNode.Duration
	}
	a.Cache.TouchView(viewID, cache.ViewMeta{Materialization: key.String(), Retain: retain, LastUsed: time.Now()})
	resp.Millis = time.Since(start).Milliseconds()
	a.kickGC()
	return resp, nil
}

// resolve derives the view from the Materialization and checks it is
// the one the pod was admitted for.
func (a *Agent) resolve(ctx context.Context, mat *c4v1.Materialization, want c4.ID) (*manifest.View, error) {
	data, sourceID, err := controller.LoadSource(ctx, a.Client, mat)
	if err != nil {
		return nil, err
	}
	f, err := manifest.NewFilter(mat.Spec.Include)
	if err != nil {
		return nil, err
	}
	src, err := controller.OpenStore(ctx, a.Client, mat)
	if err != nil {
		return nil, err
	}
	var res *manifest.Resolved
	if data != nil {
		res, err = manifest.FromBytes(data, src, f)
	} else {
		res, err = manifest.FromID(sourceID, src, f)
	}
	if err != nil {
		return nil, err
	}
	if res.View.ID != want {
		return nil, fmt.Errorf("materialization %s/%s now resolves to view %s, but this pod was admitted for %s; recreate the pod",
			mat.Namespace, mat.Name, res.View.ID, want)
	}
	if err := a.Cache.SaveViewDesc(res.View); err != nil {
		a.Log.Error(err, "saving view description", "view", want.String())
	}
	return res.View, nil
}

func (a *Agent) preflight(v *manifest.View, objs []manifest.Object, src *lazySource) ([]string, error) {
	var need []manifest.Object
	for _, o := range objs {
		if !a.Cache.Present(o.ID, o.Size) {
			need = append(need, o)
		}
	}
	if len(need) == 0 {
		return nil, nil
	}
	if _, err := src.get(); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	missing := map[c4.ID]bool{}
	a.parallel(len(need), func(i int) error {
		if !src.Has(need[i].ID) {
			mu.Lock()
			missing[need[i].ID] = true
			mu.Unlock()
		}
		return nil
	})
	var out []string
	for _, f := range v.Files {
		if missing[f.ID] {
			out = append(out, f.Path+" ("+f.ID.String()+")")
		}
	}
	return out, nil
}

func (a *Agent) fetchAll(objs []manifest.Object, src *lazySource, full bool, resp *Response) error {
	var hits, misses, refetched, fetched atomic.Int64
	err := a.parallel(len(objs), func(i int) error {
		r, err := a.Cache.Ensure(objs[i].ID, objs[i].Size, src, full)
		if err != nil {
			return err
		}
		if r.Hit {
			hits.Add(1)
			return nil
		}
		misses.Add(1)
		fetched.Add(r.Fetched)
		if r.Refetched {
			refetched.Add(1)
		}
		return nil
	})
	resp.Hits, resp.Misses, resp.Refetched, resp.BytesFetched = hits.Load(), misses.Load(), refetched.Load(), fetched.Load()
	return err
}

// parallel runs fn over [0,n) on FetchWorkers goroutines and returns the
// first error.
func (a *Agent) parallel(n int, fn func(int) error) error {
	workers := a.FetchWorkers
	if workers <= 0 {
		workers = 8
	}
	var (
		wg    sync.WaitGroup
		next  atomic.Int64
		once  sync.Once
		first error
	)
	for range min(workers, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				if err := fn(i); err != nil {
					once.Do(func() { first = err })
					return
				}
			}
		}()
	}
	wg.Wait()
	return first
}

// Report records a request in the Materialization's per-node status.
func (a *Agent) Report(ctx context.Context, req Request, resp Response, failure error) {
	key := types.NamespacedName{Namespace: req.Namespace, Name: req.Name}
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var mat c4v1.Materialization
		if err := a.Client.Get(ctx, key, &mat); err != nil {
			return err
		}
		var ns *c4v1.NodeCacheStatus
		for i := range mat.Status.Nodes {
			if mat.Status.Nodes[i].Node == a.Node {
				ns = &mat.Status.Nodes[i]
			}
		}
		if ns == nil {
			mat.Status.Nodes = append(mat.Status.Nodes, c4v1.NodeCacheStatus{Node: a.Node})
			ns = &mat.Status.Nodes[len(mat.Status.Nodes)-1]
		}
		ns.ViewID = req.View
		ns.Requests++
		ns.Hits += resp.Hits
		ns.Misses += resp.Misses
		ns.Refetched += resp.Refetched
		ns.BytesFetched += resp.BytesFetched
		ns.LastRequest = metav1.Now()
		ns.LastError = ""
		if failure != nil {
			ns.LastError = failure.Error()
		} else {
			ns.BytesMaterialized += resp.Bytes
			ns.LastReadyMillis = resp.Millis
		}
		return a.Client.Status().Update(ctx, &mat)
	})
	if err != nil {
		a.Log.Error(err, "reporting node status", "materialization", key.String())
	}
}

// Serve answers init containers on a unix socket until ctx ends.
func (a *Agent) Serve(ctx context.Context, socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	// Any process that can reach the socket can ask for a view; the
	// socket is only reachable through the agent hostPath, which the
	// webhook mounts into init containers.
	os.Chmod(socket, 0o666)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /v1/ensure", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log := a.Log.WithValues("materialization", req.Namespace+"/"+req.Name, "view", req.View)
		log.Info("ensure")
		resp, err := a.Ensure(r.Context(), req)
		status := http.StatusOK
		if err != nil {
			resp.Error = err.Error()
			status = http.StatusConflict
			log.Error(err, "ensure failed")
		} else {
			log.Info("ready", "millis", resp.Millis, "hits", resp.Hits, "misses", resp.Misses,
				"refetched", resp.Refetched, "bytesFetched", resp.BytesFetched, "built", resp.Built)
		}
		a.Report(context.Background(), req, resp, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	})

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a *Agent) kickGC() {
	if a.gcKick == nil {
		return
	}
	select {
	case a.gcKick <- struct{}{}:
	default:
	}
}

// RunGC evicts on an interval and after each request until ctx ends.
func (a *Agent) RunGC(ctx context.Context) {
	a.gcKick = make(chan struct{}, 1)
	interval := a.GCInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.gcKick:
		}
		a.gcOnce(ctx)
	}
}

func (a *Agent) gcOnce(ctx context.Context) {
	now := time.Now()
	views, err := a.Cache.Views()
	if err != nil {
		a.Log.Error(err, "listing views")
		return
	}
	inUse, err := a.viewsInUse(ctx)
	if err != nil {
		// Unknown use: protect every view, evict only unlinked objects.
		a.Log.Error(err, "listing pods; protecting all views this round")
		inUse = map[c4.ID]bool{}
		for _, v := range views {
			inUse[v.ID] = true
		}
	}
	// A view in use was last used now; retention runs from when the
	// last pod using it goes away.
	for _, v := range views {
		if inUse[v.ID] {
			v.Meta.LastUsed = now
			a.Cache.TouchView(v.ID, v.Meta)
		}
	}
	st, err := a.Cache.Evict(inUse, now)
	if err != nil {
		a.Log.Error(err, "eviction")
		return
	}
	if st.Objects > 0 || st.Views > 0 || st.Over > 0 {
		a.Log.Info("eviction", "objects", st.Objects, "bytes", st.ObjectBytes, "views", st.Views,
			"cacheBytes", a.Cache.Bytes(), "budget", a.Cache.Budget(), "over", st.Over)
	}
}

func (a *Agent) viewsInUse(ctx context.Context) (map[c4.ID]bool, error) {
	var pods corev1.PodList
	if err := a.Client.List(ctx, &pods, client.MatchingFields{"spec.nodeName": a.Node}); err != nil {
		return nil, err
	}
	out := map[c4.ID]bool{}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		id, err := c4.Parse(p.Annotations[c4v1.AnnotationView])
		if err != nil {
			continue
		}
		out[id] = true
	}
	return out, nil
}

// lazySource opens the store on first use, so a fully warm node never
// touches the store or its credentials.
type lazySource struct {
	open func() (storeurl.Source, error)
	once sync.Once
	src  storeurl.Source
	err  error
}

func (l *lazySource) get() (storeurl.Source, error) {
	l.once.Do(func() { l.src, l.err = l.open() })
	return l.src, l.err
}

func (l *lazySource) Open(id c4.ID) (io.ReadCloser, error) {
	s, err := l.get()
	if err != nil {
		return nil, err
	}
	return s.Open(id)
}

func (l *lazySource) Has(id c4.ID) bool {
	s, err := l.get()
	if err != nil {
		return false
	}
	return s.Has(id)
}
