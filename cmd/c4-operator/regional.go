package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/auth"
	"github.com/Avalanche-io/c4-operator/internal/fill"
	"github.com/Avalanche-io/c4-operator/internal/metrics"
	"github.com/Avalanche-io/c4-operator/internal/objcache"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/tier"
)

// runRegional serves the regional cache tier: stateless, local-disk cache,
// fills from the project's sharded store with the project's credentials,
// ingests writes after hashing them.
func runRegional(args []string) error {
	fs := flag.NewFlagSet("regional", flag.ExitOnError)
	root := fs.String("root", "/cache", "cache directory (an emptyDir)")
	size := fs.String("cache-size", "50Gi", "cache budget")
	addr := fs.String("addr", ":7443", "tier API listen address")
	namespace := fs.String("namespace", env("POD_NAMESPACE", "c4-system"), "the operator's namespace")
	maxFills := fs.Int("max-fills", 128, "concurrent fills from the store (admission control)")
	maxWaiters := fs.Int("max-waiters", 16384, "concurrent waiters on fills")
	maxServing := fs.Int("max-serving", 2048, "concurrent responses")
	noAuth := fs.Bool("insecure-no-auth", false, "skip TokenReview (tests only)")
	fs.Parse(args)

	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("regional")
	budget, err := resource.ParseQuantity(*size)
	if err != nil {
		return fmt.Errorf("--cache-size: %w", err)
	}
	cfg := ctrl.GetConfigOrDie()
	scheme := newScheme()
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	ctx := ctrl.SetupSignalHandler()
	watch, err := cache.New(cfg, cache.Options{Scheme: scheme, ByObject: map[client.Object]cache.ByObject{&c4v1.Project{}: {}}})
	if err != nil {
		return err
	}
	if err := watch.IndexField(ctx, &c4v1.Project{}, project.NamespaceIndex, project.IndexNamespaces); err != nil {
		return err
	}
	go watch.Start(ctx)
	if !watch.WaitForCacheSync(ctx) {
		return errors.New("watch caches did not sync")
	}
	reg := &project.Registry{Cache: watch, Secrets: direct, Namespace: *namespace}
	caches, err := objcache.OpenSet(*root, objcache.Options{Mode: 0o444, Budget: budget.Value()})
	if err != nil {
		return err
	}
	srv := &tier.Server{
		Tier:    "regional",
		Caches:  caches,
		Fills:   fill.NewGroup(fill.Limits{Fills: *maxFills, Waiters: *maxWaiters}),
		Fetcher: &tier.Fetcher{Tier: "regional", Breakers: tier.NewBreakers(), Backoff: tier.Backoff{Base: 100 * time.Millisecond, Max: 5 * time.Second}},
		Partition: func(name string) (string, error) {
			p, err := reg.Get(context.Background(), name)
			if err != nil {
				return "", fmt.Errorf("project %s: %w", name, err)
			}
			return p.Partition(), nil
		},
		Route: func(r *http.Request, name string, id c4.ID) tier.Route {
			return func() []tier.Source {
				p, err := reg.Get(context.Background(), name)
				if err != nil {
					return nil
				}
				st, err := reg.Store(context.Background(), p)
				if err != nil {
					return nil
				}
				return []tier.Source{tier.ShardSource{Shard: st.For(id)}}
			}
		},
		Store: func(name string) (*storeurl.Sharded, error) {
			p, err := reg.Get(context.Background(), name)
			if err != nil {
				return nil, err
			}
			return reg.Store(context.Background(), p)
		},
		MaxServing: *maxServing,
		Log:        log,
	}
	if !*noAuth {
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return err
		}
		ns := *namespace
		srv.Authorize = (&auth.Reviewer{Client: cs, Tier: "regional"}).TierAuthorizer(ns+"/c4-node", ns+"/c4-hud", ns+"/c4-manager")
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				caches.Evict()
			}
		}
	}()
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.Handle("/", srv)
	hs := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Info("serving", "addr", *addr, "root", *root, "budget", budget.String())
	if err := hs.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
