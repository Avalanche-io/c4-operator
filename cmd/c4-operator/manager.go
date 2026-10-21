package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	"github.com/Avalanche-io/c4-operator/internal/auth"
	"github.com/Avalanche-io/c4-operator/internal/controller"
	"github.com/Avalanche-io/c4-operator/internal/hud"
	hudserver "github.com/Avalanche-io/c4-operator/internal/hud/server"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/farmkey"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/ring"
	"github.com/Avalanche-io/c4-operator/internal/tier"
	c4webhook "github.com/Avalanche-io/c4-operator/internal/webhook"
	"github.com/Avalanche-io/c4-operator/internal/work/kindsim"
	"github.com/Avalanche-io/c4-operator/internal/work/placement"
	"github.com/Avalanche-io/c4-operator/internal/work/provreq"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/tools"
	"github.com/Avalanche-io/c4-operator/work/sched"
	"github.com/Avalanche-io/c4-operator/work/topology"
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
	unitPriority := fs.String("unit-priority-class", "c4-unit", "PriorityClass for unit pods (preemptionPolicy Never: only c4 preempts them); empty sets none")
	policy := fs.String("work-policy", "objective", "scheduling policy: objective, edf, fifo")
	hudService := fs.String("hud-service", "c4-hud:80", "the HUD's Service, [<namespace>/]<service>:<port>: units whose tool grants c4 reach it")
	egressProxy := fs.String("egress-proxy", "c4-egress:3128", "the farm's egress proxy, [<namespace>/]<service>:<port> (config/egress): units whose tool allows hosts reach them through it; empty for none")
	provider := fs.String("capacity-provider", "", "fulfil CapacityRequests in-process, comma-separated: kind-sim (tests), provisioning-request (Cluster Autoscaler); empty for an external provider")
	hudAddr := fs.String("hud-addr", ":8090", "HUD listen address (empty disables the HUD)")
	hudDev := fs.String("hud-dev-viewers", "", "development only: viewers selected by ?as=, as name[@role]=project|project;... (* for all projects; a name that is a role, such as wrangler, has that role)")
	hudSandbox := fs.String("hud-sandbox", "", "development only, with --hud-dev-viewers: the stand-in tracker's URL (c4 farm up --tracker), served at /sandbox/ on the HUD's port")
	hudProxy := fs.Bool("hud-trust-proxy", false, "read the viewer from X-Forwarded-User and X-Forwarded-Groups (an SSO proxy in front of the HUD); needs --hud-proxy-secret-file")
	hudProxySecret := fs.String("hud-proxy-secret-file", "", "a file holding the secret the SSO proxy sends in X-C4-Proxy-Secret on every request; a request naming a viewer without it is refused")
	hudRoles := fs.String("hud-role-groups", "", "the studio's groups and the HUD roles they start their members at, as group=role,... (roles: artist, supervisor, producer, wrangler, farm-admin, facilities, pipeline-td); groups named c4:role:<role> need no entry")
	singlePerson := fs.String("single-person-farm", "", "a local farm's installer, who plays every role there and may decide a wait addressed to any of them, recorded as standing in for that role; empty on a shared farm")
	standIn := fs.String("decide-stand-in", "deny", "the farm's limit on deciding a person's wait for them (decide.standIn): deny, or allow so a project may choose (Project spec.work.policy)")
	hudImageRegistry := fs.String("hud-image-registry", "ghcr.io/avalanche-io", "where a tool's c4/<name> images are: c4/<name> is <registry>/c4-<name>:<tag>; empty for images loaded onto the nodes (the local farm), named c4-<name>:<tag>")
	hudPlainRegistries := fs.String("hud-plain-registries", "", "registries a tool's images are pinned from without TLS, host[:port],... (a local farm's registry); loopback always is")
	hudImageTag := fs.String("hud-image-tag", version, "the tag of a tool's c4/<name> images that name none: by default the operator's own version, so a release's tools run its own images")
	hudSites := fs.String("hud-sites", "", "JSON file of the farm's sites (name, label, region, lat, lon, kind onprem or cloud, localities) for the HUD's map; nodes join a site by their topology.kubernetes.io/region label")
	farmTZ := fs.String("tz", "UTC", "the farm's time zone, for times the queue and the HUD show")
	staging := fs.Bool("staging", false, "experimental: let the work queue move jobs' inputs to pools at other sites over the links in the --hud-sites topology file")
	tierToken := fs.String("tier-token-file", "/var/run/secrets/c4-tier/token", "with --staging: the operator's service account token (audience c4-tier) for the sites' regional replicas")
	signingNS := fs.String("signing-namespace", "c4-signing", "namespace of the farm's signing CA, readable only by the manager (empty turns signing off)")
	signerAddr := fs.String("signer-addr", ":8092", "certificate issuer listen address, for node daemons and importers")
	telemetryRing := fs.Bool("telemetry-ring", false, "keep the last two hours of the work queue's decisions, redacted, in memory, so a diagnostic bundle (GET /api/v1/diagnose) covers the moment a problem happened; off, a bundle holds the present moment alone. Nothing leaves the farm either way; C4_TELEMETRY=off holds it off")
	holderTransfer := fs.String("holder-transfer", "disallow", "holder-transfer credential leases, a holder's own sign-in materialized for the holder's own unit pods: allow or disallow")
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
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr, FilterProvider: metricsGuard(kube)},
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
	if *holderTransfer != "allow" && *holderTransfer != "disallow" {
		return fmt.Errorf("--holder-transfer: %q, want allow or disallow", *holderTransfer)
	}
	if err := (&controller.CredentialLeaseReconciler{Client: mgr.GetClient(), HolderTransfer: *holderTransfer == "allow", SinglePerson: *singlePerson}).SetupWithManager(mgr); err != nil {
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
			Stores: stores, Signer: sig.queue, Roots: sig.pool, PolicyName: *policy, Namespace: *namespace,
			// Units' reported progress, filled by the HUD's scrape of the
			// node daemons (without the HUD, units are estimated from
			// elapsed time alone).
			Progress: &queue.ProgressCache{}, Safepoints: &queue.SafepointCache{}, PriorityClass: *unitPriority,
			HolderTransfer: *holderTransfer == "allow", EgressProxy: *egressProxy, HUDService: *hudService}
		// The farm's telemetry ring (DESIGN.md section 6.9): the queue tells
		// it each decision; the HUD writes bundles from it. Its key is read,
		// or made, the first time it is needed.
		ns := *namespace
		q.Recorder = ring.New(ring.TierFrom(*telemetryRing), telemetry.PolicyOf(*policy), func(ctx context.Context) (telemetry.Key, error) {
			return farmkey.LoadOrCreate(ctx, kube, ns)
		})
		if err := mgr.Add(q); err != nil {
			return err
		}
		if *staging {
			if err := addStaging(mgr, q, stores, *hudSites, *tierToken); err != nil {
				return err
			}
		}
	}
	if *hudAddr != "" {
		proxy, err := proxySecret(*hudProxy, *hudProxySecret)
		if err != nil {
			return err
		}
		if err := addHUD(mgr, kube, q, stores, sig, *hudAddr, *hudDev, proxy, *hudSites, *hudRoles, *farmTZ, *namespace, *tierToken, *standIn, *singlePerson, *hudSandbox, hudImages{registry: *hudImageRegistry, tag: *hudImageTag, plain: splitList(*hudPlainRegistries)}); err != nil {
			return err
		}
	}
	if sig.ca != nil && *signerAddr != "" {
		if err := mgr.Add(issuerRunnable(sig, kube, mgr.GetAPIReader(), *namespace, *signerAddr)); err != nil {
			return err
		}
	}
	for _, p := range strings.Split(*provider, ",") {
		// Each actuator acts only on the burst pools that name it.
		var r manager.Runnable
		switch strings.TrimSpace(p) {
		case "":
			continue
		case "kind-sim":
			r = &kindsim.Actuator{Client: mgr.GetClient(), Log: ctrl.Log.WithName("kind-sim")}
		case provreq.Provider:
			r = &provreq.Actuator{Client: mgr.GetClient(), Log: ctrl.Log.WithName("provisioning-request"), Namespace: *namespace}
		default:
			return fmt.Errorf("--capacity-provider %q: want kind-sim or provisioning-request", p)
		}
		if err := mgr.Add(r); err != nil {
			return err
		}
	}
	mgr.GetWebhookServer().Register("/mutate-pod", &webhook.Admission{Handler: &c4webhook.PodMutator{Decoder: admission.NewDecoder(scheme)}})
	// The manager creates WorkJobs for signed-in HUD and MCP users and for
	// bundles, and names their owner; everyone else is stamped with their
	// own name, on jobs and on bundles alike.
	self := "system:serviceaccount:" + *namespace + ":" + env("POD_SERVICE_ACCOUNT", "c4-manager")
	owner := &webhook.Admission{Handler: &c4webhook.Owner{Delegates: []string{self}}}
	mgr.GetWebhookServer().Register("/mutate-workjob", owner)
	mgr.GetWebhookServer().Register("/mutate-agentbundle", owner)
	mgr.GetWebhookServer().Register("/mutate-credentiallease", owner)
	mgr.AddHealthzCheck("ping", healthz.Ping)
	mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker())
	log.Info("starting", "namespace", *namespace)
	return mgr.Start(ctx)
}

// addHUD runs the HUD backend: a collector on every replica and an HTTP
// server for the web app and the HUD API.
// hudImages is where a published tool's c4/<name> images are (tools.go).
type hudImages struct {
	registry, tag string
	plain         []string
}

// loopbackOnly reports whether addr listens only on a loopback address
// (127.0.0.0/8, ::1 or localhost); an empty host listens everywhere.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// proxySecret reads the SSO proxy's secret: nil when the HUD trusts no
// proxy. Trusting one without a secret is refused, since anyone who
// reached the HUD past the proxy could then send its headers.
func proxySecret(trust bool, file string) ([]byte, error) {
	switch {
	case !trust && file == "":
		return nil, nil
	case !trust:
		return nil, errors.New("--hud-proxy-secret-file needs --hud-trust-proxy")
	case file == "":
		return nil, errors.New("--hud-trust-proxy needs --hud-proxy-secret-file: the secret the proxy sends in X-C4-Proxy-Secret, so headers from anyone else are refused")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("--hud-proxy-secret-file: %w", err)
	}
	b = bytes.TrimSpace(b)
	if len(b) < 16 {
		return nil, errors.New("--hud-proxy-secret-file: the secret is shorter than 16 bytes")
	}
	return b, nil
}

func addHUD(mgr ctrl.Manager, kube kubernetes.Interface, q *queue.Queue, stores queue.StoreFunc, sig *signing, addr, dev string, proxy []byte, sites, roles, tz, namespace, tierToken, standIn, singlePerson, sandbox string, images hudImages) error {
	if standIn != "allow" && standIn != "deny" {
		return fmt.Errorf("--decide-stand-in: allow or deny, not %q", standIn)
	}
	roleGroups, err := parseRoleGroups(roles)
	if err != nil {
		return fmt.Errorf("--hud-role-groups: %w", err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("--tz: %w", err)
	}
	web, err := fs.Sub(hud.Web, "web")
	if err != nil {
		return err
	}
	srv := &hudserver.Server{ImageRegistry: images.registry, ImageTag: images.tag, PlainRegistries: images.plain, Client: mgr.GetClient(), Kube: kube, Stores: stores, Web: web, Loc: loc,
		TrustProxy: proxy != nil, ProxySecret: proxy, Namespace: namespace, Log: ctrl.Log.WithName("hud"), Signer: sig.hud, RoleGroups: roleGroups, StandIn: standIn, SinglePerson: singlePerson,
		// What units report comes from the node daemons' /units, for the
		// operator's tier token alone; their metrics need an API token
		// the c4-metrics-reader role is bound to.
		TierToken: (&tier.TokenFile{Path: tierToken}).Token, MetricsToken: apiToken(mgr.GetConfig())}
	if strings.HasPrefix(version, "v") {
		// the release's own tools, built with their tool.lock: c4 farm
		// up publishes these trees, tested in this release
		if srv.ReleaseTrees, err = tools.ReleaseTrees(version); err != nil {
			return fmt.Errorf("the release's tools: %w", err)
		}
	}
	if q != nil {
		// Without the work system the HUD has no queue to ask (a nil
		// *queue.Queue in the interface would not compare equal to nil).
		srv.Queue = q
		srv.DryRun = hudserver.QueueDryRun(q)
		srv.DryRunAll = hudserver.QueueDryRunAll(q)
		srv.Forecast = func() api.CapacityForecast { return forecastView(q.Forecast()) }
		srv.Progress = q.Progress
		srv.Safepoints = q.Safepoints
		srv.Plans = q.PlanOf
		if r, ok := q.Recorder.(*ring.Ring); ok {
			srv.Telemetry = r
		}
	}
	if dev != "" {
		srv.DevViewers, err = parseViewers(dev)
		if err != nil {
			return err
		}
		// Whether only this machine reaches the HUD is its listen
		// address's to say, never a request's.
		srv.Loopback = loopbackOnly(addr)
	}
	if sandbox != "" {
		if srv.DevViewers == nil {
			return errors.New("--hud-sandbox is for a development farm: it needs --hud-dev-viewers")
		}
		if srv.Sandbox, err = url.Parse(sandbox); err != nil || srv.Sandbox.Host == "" {
			return fmt.Errorf("--hud-sandbox %q: want the sandbox's URL", sandbox)
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

// addStaging gives the work queue a Placement (experimental): pools at
// sites, what each site lacks of jobs' inputs, and moving them over the
// topology's links (internal/work/placement).
func addStaging(mgr ctrl.Manager, q *queue.Queue, stores func(context.Context, string) (*storeurl.Sharded, error), topoFile, tokenFile string) error {
	if topoFile == "" {
		return errors.New("--staging needs --hud-sites: a topology file with the sites' links and regional replicas")
	}
	tp, err := topology.Load(topoFile)
	if err != nil {
		return fmt.Errorf("--hud-sites: %w", err)
	}
	if len(tp.Links) == 0 {
		return errors.New("--staging: the topology file has no links")
	}
	token := func() string {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	reg := &placement.Regional{Topo: tp, Client: &http.Client{Timeout: 30 * time.Second}, Token: token}
	p := &placement.Provider{Topo: tp, Views: placement.StoreViews{Stores: stores}, Holdings: reg, Warmer: reg,
		Cost: q.RecomputeCost, Recomputer: q, Log: ctrl.Log.WithName("placement")}
	q.Placement = p
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		p.Run(ctx, 15*time.Second)
		return nil
	}))
}

// parseRoleGroups reads --hud-role-groups: group=role,...
func parseRoleGroups(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(spec, ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		g, r, ok := strings.Cut(kv, "=")
		if !ok || g == "" || !slices.Contains(api.Roles, r) {
			return nil, fmt.Errorf("%q: want group=role, role one of %s", kv, strings.Join(api.Roles, ", "))
		}
		out[g] = r
	}
	return out, nil
}

// forecastView is the queue's last forecast pass as the HUD API names it.
func forecastView(f queue.Forecast) api.CapacityForecast {
	out := api.CapacityForecast{Jobs: f.Jobs}
	if !f.At.IsZero() {
		at := f.At.UTC()
		out.At = &at
	}
	for _, b := range f.Bursts {
		out.Bursts = append(out.Bursts, api.ForecastBurst{Pool: b.Pool, Slots: b.Slots, Live: b.Live})
	}
	for _, w := range f.Worse {
		out.Worse = append(out.Worse, api.ForecastRisk{Job: w.Job, Risk: w.Risk, Live: w.Live})
	}
	return out
}

// splitList is a comma-separated flag's values, trimmed, empty ones left
// out.
func splitList(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// metricsGuard puts the operator's /metrics behind the check the node
// daemons and regional replicas make (auth.MetricsGuard): a token the API
// server accepts, whose subject may get /metrics.
func metricsGuard(kube kubernetes.Interface) func(*rest.Config, *http.Client) (metricsserver.Filter, error) {
	g := &auth.MetricsGuard{Reviewer: &auth.Reviewer{Client: kube, Tier: "manager"}}
	return func(*rest.Config, *http.Client) (metricsserver.Filter, error) {
		return func(_ logr.Logger, h http.Handler) (http.Handler, error) { return g.Wrap(h), nil }, nil
	}
}

// apiToken is the operator's own API token (its service account's, in a
// pod), which the HUD presents to the node daemons' and regional
// replicas' /metrics.
func apiToken(cfg *rest.Config) func() string {
	if cfg.BearerTokenFile != "" {
		return (&tier.TokenFile{Path: cfg.BearerTokenFile}).Token
	}
	return func() string { return cfg.BearerToken }
}
