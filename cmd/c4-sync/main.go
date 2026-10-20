// Command c4-sync keeps trackers and the farm in step (proposals/
// integrations ADAPTER.md). It reconciles the experimental Tracker and
// TrackerLink objects: tracker entities become jobs, job states go back as
// work states, and decisions people make in the tracker are relayed to the
// jobs' waits. It talks to the farm only through the HUD API, as its own
// identity, and to each tracker through that Tracker's adapter.
//
//	c4-sync --hud http://c4-hud.c4-system --hud-token-file /var/run/c4-sync/token
//
// Deliveries are received at /webhooks/{namespace}/{tracker} on --listen;
// /healthz answers ok. Adapters built in: flow, github, mlflow.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm"
	_ "github.com/Avalanche-io/c4-operator/internal/tracker/flow"
	_ "github.com/Avalanche-io/c4-operator/internal/tracker/github"
	_ "github.com/Avalanche-io/c4-operator/internal/tracker/mlflow"
	"github.com/Avalanche-io/c4-operator/internal/tracker/syncer"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "c4-sync:", err)
		os.Exit(1)
	}
}

type options struct {
	hud, hudPublic, tokenFile, listen, agent, devViewer string
	interval                                            time.Duration
}

func parse(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("c4-sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.hud, "hud", os.Getenv("C4_HUD"), "the HUD's base URL (C4_HUD)")
	fs.StringVar(&o.tokenFile, "hud-token-file", os.Getenv("C4_HUD_TOKEN_FILE"), "a file holding c4-sync's bearer token for the HUD, reread on every use (C4_HUD_TOKEN_FILE)")
	fs.StringVar(&o.listen, "listen", ":8090", "where to receive tracker deliveries (/webhooks/{namespace}/{tracker})")
	fs.StringVar(&o.agent, "agent", "c4-sync", "the name the HUD books c4-sync's controls under (X-C4-Agent)")
	fs.DurationVar(&o.interval, "interval", time.Minute, "time between passes over every Tracker")
	fs.StringVar(&o.hudPublic, "hud-public-url", os.Getenv("C4_HUD_PUBLIC_URL"), "the HUD's address as people reach it, for links written to trackers (default --hud)")
	fs.StringVar(&o.devViewer, "hud-dev-viewer", "", "development only: the HUD viewer (?as=) to act as, on a HUD started with --hud-dev-viewers")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if *showVersion {
		fmt.Fprintln(stderr, "c4-sync", version, "adapters:", strings.Join(tracker.Kinds(), ", "))
		return o, errVersion
	}
	if o.hud == "" {
		return o, errors.New("--hud is required")
	}
	if o.interval < 5*time.Second {
		return o, errors.New("--interval must be at least 5s")
	}
	return o, nil
}

var errVersion = errors.New("version")

func run(args []string, stderr io.Writer) error {
	o, err := parse(args, stderr)
	if errors.Is(err, errVersion) {
		return nil
	}
	if err != nil {
		return err
	}
	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("c4-sync")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := c4v1.AddToScheme(scheme); err != nil {
		return err
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	s := &syncer.Syncer{
		Client: k8s,
		Farm: &farm.HUD{Base: o.hud, Agent: o.agent, As: o.devViewer, Public: o.hudPublic, Token: func(context.Context) (string, error) {
			if o.tokenFile == "" {
				return "", nil
			}
			b, err := os.ReadFile(o.tokenFile)
			return strings.TrimSpace(string(b)), err
		}},
		Log: log,
	}
	ctx := ctrl.SetupSignalHandler()
	mux := http.NewServeMux()
	mux.Handle("/webhooks/", s.Webhooks())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: o.listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error(err, "webhook listener")
		}
	}()
	log.Info("started", "version", version, "hud", o.hud, "adapters", tracker.Kinds(), "interval", o.interval)
	return s.Run(ctx, o.interval)
}
