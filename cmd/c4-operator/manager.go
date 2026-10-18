package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/controller"
	"github.com/Avalanche-io/c4-operator/internal/hud"
	hudserver "github.com/Avalanche-io/c4-operator/internal/hud/server"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4webhook "github.com/Avalanche-io/c4-operator/internal/webhook"
	"github.com/Avalanche-io/c4-operator/internal/work/kindsim"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// unitPods selects pods the work queue created.
func unitPods() labels.Selector {
	req, err := labels.NewRequirement(c4v1.LabelJob, selection.Exists, nil)
	if err != nil {
		panic(err)
	}
	return labels.NewSelector().Add(*req)
}

func policyFor(name string) (sched.Policy, error) {
	switch name {
	case "objective", "":
		return sched.Decide, nil
	case "edf":
		return sched.EDF, nil
	case "fifo":
		return sched.FIFO, nil
	}
	return nil, fmt.Errorf("--work-policy %q: want objective, edf or fifo", name)
}

func runManager(args []string) error {
	fs := flag.NewFlagSet("manager", flag.ExitOnError)
	metricsAddr := fs.String("metrics-addr", ":8080", "metrics listen address (0 disables)")
	probeAddr := fs.String("probe-addr", ":8081", "health probe listen address")
	webhookPort := fs.Int("webhook-port", 9443, "webhook server port")
	certDir := fs.String("cert-dir", filepath.Join(os.TempDir(), "c4-operator-webhook"), "where the generated webhook certificate is written")
	service := fs.String("webhook-service", "c4-operator-webhook", "Service in front of the webhook")
	namespace := fs.String("namespace", env("POD_NAMESPACE", "c4-system"), "the operator's namespace (holds store credentials and the work queue's calibration pointers)")
	webhookConfig := fs.String("webhook-config", "c4-operator", "MutatingWebhookConfiguration to keep the CA in")
	retry := fs.Duration("retry", time.Minute, "recheck interval for Materializations that failed for a reason that may heal")
	work := fs.Bool("work", true, "run the work queue (WorkJobs, Pools, CapacityRequests)")
	tick := fs.Duration("work-tick", 2*time.Second, "work queue decision period")
	policy := fs.String("work-policy", "objective", "scheduling policy: objective, edf, fifo")
	provider := fs.String("capacity-provider", "", "fulfil CapacityRequests in-process: kind-sim (tests), or empty for an external provider")
	hudAddr := fs.String("hud-addr", ":8090", "HUD listen address (empty disables the HUD)")
	hudDev := fs.String("hud-dev-viewers", "", "development only: viewers selected by ?as=, as name[@role]=project|project;... (* for all projects; a name that is a role, such as wrangler, has that role)")
	hudProxy := fs.Bool("hud-trust-proxy", false, "read the viewer from X-Forwarded-User and X-Forwarded-Groups (an SSO proxy in front of the HUD)")
	hudSites := fs.String("hud-sites", "", "JSON file of the farm's sites (name, label, region, lat, lon, kind onprem or cloud, localities) for the HUD's map; nodes join a site by their topology.kubernetes.io/region label")
	farmTZ := fs.String("tz", "UTC", "the farm's time zone, for times the queue and the HUD show")
	signingNS := fs.String("signing-namespace", "c4-signing", "namespace of the farm's signing CA, readable only by the manager (empty turns signing off)")
	signerAddr := fs.String("signer-addr", ":8092", "certificate issuer listen address, for node daemons and importers")
	fs.Parse(args)

	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("manager")
	scheme := newScheme()
	cfg := ctrl.GetConfigOrDie()

	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	ctx := ctrl.SetupSignalHandler()
	if err := c4webhook.BootstrapCerts(ctx, direct, *certDir, *service, *namespace, *webhookConfig); err != nil {
		return fmt.Errorf("webhook certificates: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	// Every component that decides something signs it as itself
	// (DESIGN.md section 9.2).
	sig := &signing{}
	if *signingNS != "" {
		if sig, err = setupSigning(ctx, kube, *signingNS, *namespace); err != nil {
			return err
		}
		log.Info("signing", "ca", provenance.Fingerprint(sig.ca.Cert)[:16], "issuer", *signerAddr)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		WebhookServer:          webhook.NewServer(webhook.Options{Port: *webhookPort, CertDir: *certDir}),
		// Secrets are never cached: credentials are read on demand, from
		// the operator's namespace only.
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
		// The queue watches only its own unit pods, not every pod.
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}: {Label: unitPods()},
		}},
	})
	if err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &c4v1.Project{}, project.NamespaceIndex, project.IndexNamespaces); err != nil {
		return err
	}
	reg := &project.Registry{Cache: mgr.GetClient(), Secrets: mgr.GetAPIReader(), Namespace: *namespace}
	if err := (&controller.ProjectReconciler{Client: mgr.GetClient(), Projects: reg}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&controller.MaterializationReconciler{Client: mgr.GetClient(), Projects: reg, Retry: *retry,
		Signer: sig.controller, Roots: sig.pool}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&controller.AgentBundleReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return err
	}
	var q *queue.Queue
	stores := func(ctx context.Context, name string) (*storeurl.Sharded, error) {
		p, err := reg.Get(ctx, name)
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return reg.Store(ctx, p)
	}
	if *work {
		pol, err := policyFor(*policy)
		if err != nil {
			return err
		}
		loc, err := time.LoadLocation(*farmTZ)
		if err != nil {
			return fmt.Errorf("--tz: %w", err)
		}
		q = &queue.Queue{Loc: loc, Client: mgr.GetClient(), Every: *tick, Policy: pol, Cfg: sched.DefaultConfig(), Log: ctrl.Log.WithName("queue"),
			Stores: stores, Signer: sig.queue, PolicyName: *policy, Namespace: *namespace,
			// Units' reported progress, filled by the HUD's scrape of the
			// node daemons (without the HUD, units are estimated from
			// elapsed time alone).
			Progress: &queue.ProgressCache{}}
		if err := mgr.Add(q); err != nil {
			return err
		}
	}
	if *hudAddr != "" {
		if err := addHUD(mgr, kube, q, stores, sig, *hudAddr, *hudDev, *hudProxy, *hudSites, *farmTZ, *namespace); err != nil {
			return err
		}
	}
	if sig.ca != nil && *signerAddr != "" {
		if err := mgr.Add(issuerRunnable(sig, kube, *namespace, *signerAddr)); err != nil {
			return err
		}
	}
	if *provider == "kind-sim" {
		if err := mgr.Add(&kindsim.Actuator{Client: mgr.GetClient(), Log: ctrl.Log.WithName("kind-sim")}); err != nil {
			return err
		}
	}
	mgr.GetWebhookServer().Register("/mutate-pod", &webhook.Admission{Handler: &c4webhook.PodMutator{Decoder: admission.NewDecoder(scheme)}})
	// The manager creates WorkJobs for signed-in HUD and MCP users and names
	// them as owner; everyone else is stamped with their own name.
	self := "system:serviceaccount:" + *namespace + ":" + env("POD_SERVICE_ACCOUNT", "c4-manager")
	mgr.GetWebhookServer().Register("/mutate-workjob", &webhook.Admission{Handler: &c4webhook.WorkJobOwner{Decoder: admission.NewDecoder(scheme), Delegates: []string{self}}})
	mgr.AddHealthzCheck("ping", healthz.Ping)
	mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker())
	log.Info("starting", "namespace", *namespace)
	return mgr.Start(ctx)
}

// addHUD runs the HUD backend: a collector on every replica and an HTTP
// server for the web app and the HUD API.
func addHUD(mgr ctrl.Manager, kube kubernetes.Interface, q *queue.Queue, stores queue.StoreFunc, sig *signing, addr, dev string, proxy bool, sites, tz, namespace string) error {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("--tz: %w", err)
	}
	web, err := fs.Sub(hud.Web, "web")
	if err != nil {
		return err
	}
	srv := &hudserver.Server{Client: mgr.GetClient(), Kube: kube, Stores: stores, Web: web, Loc: loc,
		TrustProxy: proxy, Namespace: namespace, Log: ctrl.Log.WithName("hud"), Signer: sig.hud}
	if q != nil {
		// Without the work system the HUD has no queue to ask (a nil
		// *queue.Queue in the interface would not compare equal to nil).
		srv.Queue = q
		srv.DryRun = hudserver.QueueDryRun(q)
		srv.Progress = q.Progress
	}
	if dev != "" {
		srv.DevViewers, err = parseViewers(dev)
		if err != nil {
			return err
		}
	}
	if sites != "" {
		if srv.Sites, err = hudserver.LoadSites(sites); err != nil {
			return fmt.Errorf("--hud-sites: %w", err)
		}
	}
	if err := mgr.Add(srv); err != nil {
		return err
	}
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			hs.Shutdown(sctx)
		}()
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}))
}

// parseViewers reads "wrangler=*;artist=show-a|show-b".
func parseViewers(s string) (map[string]*api.Viewer, error) {
	out := map[string]*api.Viewer{}
	for _, item := range strings.Split(s, ";") {
		name, projects, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--hud-dev-viewers %q: want name=project|project;...", s)
		}
		// name@role=projects gives the viewer a role; a name that is a
		// role (wrangler, artist) has that role.
		name, role, _ := strings.Cut(name, "@")
		if role == "" && slices.Contains(api.Roles, name) {
			role = name
		}
		v := &api.Viewer{User: name, Projects: []string{}, Role: role}
		if projects == "*" {
			v.All = true
		} else {
			v.Projects = strings.Split(projects, "|")
		}
		out[name] = v
	}
	return out, nil
}
