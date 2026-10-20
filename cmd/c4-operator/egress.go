package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Avalanche-io/c4-operator/internal/egress"
)

// runEgressProxy is c4-operator egress-proxy: the farm's egress proxy for
// units whose tool profile allows hosts (internal/egress).
func runEgressProxy(args []string) error {
	fs := flag.NewFlagSet("egress-proxy", flag.ExitOnError)
	addr := fs.String("addr", ":3128", "listen address")
	namespace := fs.String("namespace", env("POD_NAMESPACE", "c4-system"), "the operator's namespace, where the farm's tool profiles live")
	ttl := fs.Duration("ttl", 30*time.Second, "how long a client address's unit and profile are kept")
	fs.Parse(args)

	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("egress")
	direct, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: newScheme()})
	if err != nil {
		return err
	}
	p := &egress.Proxy{Resolve: &egress.Cluster{Client: direct, Namespace: *namespace, TTL: *ttl}, Log: log}
	srv := &http.Server{Addr: *addr, Handler: p, ReadHeaderTimeout: 30 * time.Second}
	ctx := ctrl.SetupSignalHandler()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
