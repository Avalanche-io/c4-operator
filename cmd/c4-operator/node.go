package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/auth"
	"github.com/Avalanche-io/c4-operator/internal/fill"
	"github.com/Avalanche-io/c4-operator/internal/membership"
	"github.com/Avalanche-io/c4-operator/internal/node"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/tier"
)

func runNode(args []string) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	nodeName := fs.String("node", os.Getenv("NODE_NAME"), "this node's name")
	root := fs.String("root", "/var/lib/c4", "cache root on the node")
	size := fs.String("cache-size", "20Gi", "object cache budget")
	peerAddr := fs.String("peer-addr", ":7444", "tier API listen address (the daemon runs with hostNetwork)")
	peerPort := fs.Int("peer-port", 7444, "port peers dial")
	csiSocket := fs.String("csi-socket", "/csi/csi.sock", "CSI socket")
	namespace := fs.String("namespace", env("POD_NAMESPACE", "c4-system"), "the operator's namespace")
	localityKey := fs.String("locality-label", c4v1.LabelLocality, "node label that groups nodes into localities")
	homesK := fs.Int("homes", 1, "rendezvous homes per object in a locality")
	regionalR := fs.Int("regional-homes", 2, "regional homes per object")
	regionalService := fs.String("regional-service", "c4-regional", "regional tier Service")
	tokenFile := fs.String("tier-token", "/var/run/secrets/c4-tier/token", "projected token for the tier API (audience c4-tier)")
	workers := fs.Int("fetch-workers", 16, "parallel object fetches per publish")
	maxFills := fs.Int("max-fills", 64, "concurrent upstream fills (admission control)")
	maxWaiters := fs.Int("max-waiters", 4096, "concurrent waiters on fills (admission control)")
	maxServing := fs.Int("max-serving", 256, "concurrent responses to peers")
	retain := fs.Duration("retain", 10*time.Minute, "keep unused layers at least this long")
	gcEvery := fs.Duration("gc-interval", 30*time.Second, "eviction interval")
	noAuth := fs.Bool("insecure-no-auth", false, "skip TokenReview (tests only)")
	signerURL := fs.String("signer-url", "http://c4-signer.c4-system.svc", "the farm's certificate issuer; task records are signed as this node (empty turns records off)")
	signerToken := fs.String("signer-token", "/var/run/secrets/c4-signer/token", "projected token that proves this daemon's identity to the issuer (audience c4-signer)")
	fs.Parse(args)

	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("node")
	if *nodeName == "" {
		return errors.New("--node or NODE_NAME is required")
	}
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
	locality, err := membership.Locality(ctx, direct, *nodeName, *localityKey)
	if err != nil {
		return err
	}

	// Bounded watches: the Nodes of this locality, the regional tier's
	// EndpointSlices, and Projects (cluster-scoped and few). Nothing else.
	watch, err := cache.New(cfg, cache.Options{
		Scheme: scheme,
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Node{}:               {Label: labels.SelectorFromSet(labels.Set{*localityKey: locality})},
			&discoveryv1.EndpointSlice{}: {Namespaces: map[string]cache.Config{*namespace: {}}, Label: labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: *regionalService})},
			&c4v1.Project{}:              {},
		},
	})
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

	d := &node.Daemon{
		Cfg: node.Config{
			NodeName: *nodeName, Root: *root, Budget: budget.Value(), PeerAddr: *peerAddr, PeerPort: *peerPort,
			Namespace: *namespace, HomesK: *homesK, RegionalR: *regionalR, RegionalService: *regionalService,
			FetchWorkers: *workers, Limits: fill.Limits{Fills: *maxFills, Waiters: *maxWaiters}, MaxServing: *maxServing,
			Retain: *retain, GCInterval: *gcEvery, Version: version,
		},
		Kube:     direct,
		Projects: &project.Registry{Cache: watch, Secrets: direct, Namespace: *namespace},
		Members: &membership.Members{
			Cache: watch, LocalityKey: *localityKey, Locality: locality, PeerPort: *peerPort,
			RegionalNamespace: *namespace, RegionalService: *regionalService,
		},
		Token: (&tier.TokenFile{Path: *tokenFile}).Token,
		Log:   log,
	}
	if *signerURL != "" {
		// The node's key is made here and never leaves the process; the
		// issuer binds it to this node's identity for a day at a time.
		tf := &tier.TokenFile{Path: *signerToken}
		token := func() (string, error) {
			if t := tf.Token(); t != "" {
				return t, nil
			}
			return "", fmt.Errorf("no token at %s", *signerToken)
		}
		d.Cfg.Signer = provenance.LazySigner(provenance.RemoteIssuer(*signerURL, token, nil))
		go func() {
			if err := d.Cfg.Signer.Ready(ctx, 5*time.Second); err != nil {
				log.Error(err, "task records")
				return
			}
			log.Info("signing task records", "as", d.Cfg.Signer.Identity())
		}()
	} else {
		log.Info("no signer; task records are off")
	}
	if !*noAuth {
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return err
		}
		d.Reviewer = &auth.Reviewer{Client: cs, Tier: "node"}
	}
	if err := d.Open(); err != nil {
		return err
	}
	log.Info("serving", "node", *nodeName, "locality", locality, "root", *root, "budget", budget.String(), "csi", *csiSocket, "peers", *peerAddr)
	go func() {
		if err := d.ServePeers(ctx); err != nil {
			log.Error(err, "peer server")
			os.Exit(1)
		}
	}()
	go d.RunGC(ctx)
	go d.RunProgress(ctx, 2*time.Second)
	return d.ServeCSI(ctx, *csiSocket)
}
