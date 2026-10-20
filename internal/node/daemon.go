// Package node is the node daemon: one per node, the CSI node plugin, the
// node's object cache and layer builder, and a member of its locality's
// peer tier.
package node

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/singleflight"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/auth"
	"github.com/Avalanche-io/c4-operator/internal/fill"
	"github.com/Avalanche-io/c4-operator/internal/layer"
	"github.com/Avalanche-io/c4-operator/internal/membership"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/tier"
)

// Config configures a daemon.
type Config struct {
	NodeName  string
	Root      string // /var/lib/c4
	Budget    int64  // object cache bytes
	PeerAddr  string // tier API listen address, e.g. :7444 (hostNetwork)
	PeerPort  int    // the port peers dial
	Namespace string // the operator's namespace

	HomesK          int // rendezvous homes per object in a locality
	RegionalR       int // regional homes per object
	RegionalService string

	FetchWorkers int
	Limits       fill.Limits
	MaxServing   int

	Retain     time.Duration // layer retention after last use
	GCInterval time.Duration
	// PublishTimeout bounds the work behind one publish (fetch and build).
	// kubelet's call may time out sooner; its retry joins the work.
	PublishTimeout time.Duration
	// Signer signs a task record for every capture as this node (DESIGN.md
	// section 9.2), with a key that never leaves the daemon and a
	// short-lived certificate for the node's identity. Nil makes no
	// records.
	Signer *provenance.Signer
	// Version names this build in task records.
	Version string
	// PodsDir is kubelet's pods directory, where the daemon reaches a
	// unit's control emptyDir (default /var/lib/kubelet/pods).
	PodsDir string
}

// Daemon serves one node.
type Daemon struct {
	Cfg      Config
	Kube     client.Client // direct reads and pod patches
	Projects *project.Registry
	Members  *membership.Members
	Reviewer *auth.Reviewer // nil disables authentication (tests only)
	Token    tier.TokenFunc // this daemon's tier token
	Log      logr.Logger

	caches   *objcache.Set
	fills    *fill.Group
	fetcher  *tier.Fetcher
	breakers *tier.Breakers
	httpc    *http.Client
	uploader *tier.Uploader

	stacks   singleflight.Group
	captures singleflight.Group

	// units is what the running units last reported (GET /units).
	unitsMu sync.Mutex
	units   []UnitReport

	mu     sync.Mutex
	layers map[string]*layer.Store
	mats   map[string]matEntry
}

type matEntry struct {
	mat *c4v1.Materialization
	at  time.Time
}

// Open prepares the daemon's local state.
func (d *Daemon) Open() error {
	if d.Cfg.HomesK <= 0 {
		d.Cfg.HomesK = 1
	}
	if d.Cfg.RegionalR <= 0 {
		d.Cfg.RegionalR = 2
	}
	if d.Cfg.FetchWorkers <= 0 {
		d.Cfg.FetchWorkers = 16
	}
	if d.Cfg.PublishTimeout <= 0 {
		d.Cfg.PublishTimeout = 6 * time.Hour
	}
	set, err := objcache.OpenSet(d.Cfg.Root, objcache.Options{Mode: 0o666, ExecMode: 0o777, Budget: d.Cfg.Budget})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.volDir(), 0o700); err != nil {
		return err
	}
	d.caches = set
	d.fills = fill.NewGroup(d.Cfg.Limits)
	d.breakers = tier.NewBreakers()
	d.fetcher = &tier.Fetcher{Tier: "node", Breakers: d.breakers, Backoff: tier.Backoff{Base: 100 * time.Millisecond, Max: 5 * time.Second}}
	d.httpc = &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   64,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}}
	d.uploader = &tier.Uploader{Candidates: d.regionalForUpload, Breakers: d.breakers, Backoff: tier.Backoff{Base: 200 * time.Millisecond, Max: 5 * time.Second}, Client: d.httpc}
	d.layers = map[string]*layer.Store{}
	d.mats = map[string]matEntry{}
	return nil
}

func (d *Daemon) volDir() string { return filepath.Join(d.Cfg.Root, "vols") }

// projectCtx is what a publish needs about a project.
type projectCtx struct {
	p      *c4v1.Project
	part   string
	cache  *objcache.Cache
	layers *layer.Store
	direct *storeurl.Sharded // Direct projects only
}

func (d *Daemon) projectCtx(ctx context.Context, p *c4v1.Project) (*projectCtx, error) {
	part := p.Partition()
	cache, err := d.caches.Partition(part)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	ls, ok := d.layers[part]
	d.mu.Unlock()
	if !ok {
		ls, err = layer.Open(cache)
		if err != nil {
			return nil, err
		}
		d.mu.Lock()
		if existing, ok := d.layers[part]; ok {
			ls = existing
		} else {
			d.layers[part] = ls
		}
		d.mu.Unlock()
	}
	pc := &projectCtx{p: p, part: part, cache: cache, layers: ls}
	if p.AccessMode() == c4v1.AccessDirect {
		st, err := d.Projects.Store(ctx, p)
		if err != nil {
			return nil, err
		}
		pc.direct = st
	}
	return pc, nil
}

// materialization reads a Materialization from the API server's watch
// cache (resourceVersion 0), and remembers it for two seconds, so a
// thousand pods starting at once on a node cost one read.
func (d *Daemon) materialization(ctx context.Context, ns, name string) (*c4v1.Materialization, error) {
	key := ns + "/" + name
	d.mu.Lock()
	if e, ok := d.mats[key]; ok && time.Since(e.at) < 2*time.Second {
		d.mu.Unlock()
		return e.mat, nil
	}
	d.mu.Unlock()
	var mat c4v1.Materialization
	err := d.Kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &mat,
		&client.GetOptions{Raw: &metav1.GetOptions{ResourceVersion: "0"}})
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.mats[key] = matEntry{&mat, time.Now()}
	d.mu.Unlock()
	return &mat, nil
}

// ServePeers runs the tier API for peers until ctx ends.
func (d *Daemon) ServePeers(ctx context.Context) error {
	srv := &tier.Server{
		Tier:       "node",
		Caches:     d.caches,
		Fills:      d.fills,
		Fetcher:    d.fetcher,
		Partition:  d.partitionFor,
		Route:      d.peerRoute,
		MaxServing: d.Cfg.MaxServing,
		Log:        d.Log.WithName("peers"),
	}
	if d.Reviewer != nil {
		ns := d.Cfg.Namespace
		srv.Authorize = d.Reviewer.TierAuthorizer(ns+"/c4-node", ns+"/c4-regional")
	}
	hs := &http.Server{Addr: d.Cfg.PeerAddr, Handler: d.peerHandler(srv), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	err := hs.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// partitionFor maps a peer request's project to its partition.
func (d *Daemon) partitionFor(name string) (string, error) {
	p, err := d.Projects.Get(context.Background(), name)
	if err != nil {
		return "", fmt.Errorf("project %s: %w", name, err)
	}
	return p.Partition(), nil
}

// objectKey is the rendezvous key of an object in a partition.
func objectKey(part string, id c4.ID) []byte {
	return append([]byte(part+"/"), id[:]...)
}
