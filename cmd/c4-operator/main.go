// Command c4-operator is the manager, the node agent, and the init
// container in one binary.
//
//	c4-operator manager   controller + pod webhook (Deployment)
//	c4-operator agent     node cache + view builder (DaemonSet)
//	c4-operator ensure    ask the node agent for a view (init container)
//	c4-operator tree-id   content-level C4 ID of a directory
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/agent"
	"github.com/Avalanche-io/c4-operator/internal/cache"
	"github.com/Avalanche-io/c4-operator/internal/controller"
	"github.com/Avalanche-io/c4-operator/internal/manifest"
	c4webhook "github.com/Avalanche-io/c4-operator/internal/webhook"
)

const usage = `usage: c4-operator <command> [flags]

commands:
  manager   run the controller and the pod webhook
  agent     run the node cache agent
  ensure    ask the node agent to materialize a view (init container)
  tree-id   print the content-level C4 ID of a directory
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "manager":
		err = runManager(args)
	case "agent":
		err = runAgent(args)
	case "ensure":
		err = runEnsure(args)
	case "tree-id":
		err = runTreeID(args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "c4-operator "+cmd+": "+err.Error())
		os.Exit(1)
	}
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := c4v1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runManager(args []string) error {
	fs := flag.NewFlagSet("manager", flag.ExitOnError)
	metricsAddr := fs.String("metrics-addr", ":8080", "metrics listen address (0 disables)")
	probeAddr := fs.String("probe-addr", ":8081", "health probe listen address")
	webhookPort := fs.Int("webhook-port", 9443, "webhook server port")
	certDir := fs.String("cert-dir", filepath.Join(os.TempDir(), "c4-operator-webhook"), "where the generated webhook certificate is written")
	service := fs.String("webhook-service", "c4-operator-webhook", "Service in front of the webhook")
	namespace := fs.String("namespace", env("POD_NAMESPACE", "c4-system"), "namespace the manager runs in")
	webhookConfig := fs.String("webhook-config", "c4-operator", "MutatingWebhookConfiguration to keep the CA in")
	initImage := fs.String("init-image", env("INIT_IMAGE", "c4-operator:dev"), "image for the injected init container")
	hostRoot := fs.String("host-root", "/var/lib/c4", "node path of the agent cache")
	retry := fs.Duration("retry", time.Minute, "recheck interval for Materializations that failed for a reason that may heal")
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

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		WebhookServer:          webhook.NewServer(webhook.Options{Port: *webhookPort, CertDir: *certDir}),
		Client:                 client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
	})
	if err != nil {
		return err
	}
	r := &controller.MaterializationReconciler{Client: mgr.GetClient(), Retry: *retry}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}
	mgr.GetWebhookServer().Register("/mutate-pod", &webhook.Admission{Handler: &c4webhook.PodMutator{
		Client:    mgr.GetClient(),
		Decoder:   admission.NewDecoder(scheme),
		InitImage: *initImage,
		HostRoot:  *hostRoot,
	}})
	mgr.AddHealthzCheck("ping", healthz.Ping)
	mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker())
	log.Info("starting", "initImage", *initImage, "hostRoot", *hostRoot)
	return mgr.Start(ctx)
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	node := fs.String("node", os.Getenv("NODE_NAME"), "this node's name")
	root := fs.String("root", "/var/lib/c4", "cache root on the node")
	size := fs.String("cache-size", "20Gi", "object byte budget")
	workers := fs.Int("fetch-workers", 8, "concurrent fetches and checks")
	gcEvery := fs.Duration("gc-interval", 30*time.Second, "eviction interval")
	socket := fs.String("socket", "", "unix socket (default <root>/agent/agent.sock)")
	fs.Parse(args)

	ctrl.SetLogger(zap.New())
	log := ctrl.Log.WithName("agent")
	if *node == "" {
		return errors.New("--node or NODE_NAME is required")
	}
	budget, err := resource.ParseQuantity(*size)
	if err != nil {
		return fmt.Errorf("--cache-size: %w", err)
	}
	if *socket == "" {
		*socket = filepath.Join(*root, "agent", "agent.sock")
	}
	cl, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: newScheme()})
	if err != nil {
		return err
	}
	c, err := cache.Open(*root, budget.Value(), log.WithName("cache"))
	if err != nil {
		return err
	}
	a := &agent.Agent{Client: cl, Cache: c, Node: *node, FetchWorkers: *workers, GCInterval: *gcEvery, Log: log}
	ctx := ctrl.SetupSignalHandler()
	go a.RunGC(ctx)
	log.Info("serving", "node", *node, "root", *root, "budget", budget.String(), "cached", c.Bytes(), "socket", *socket)
	return a.Serve(ctx, *socket)
}

func runEnsure(args []string) error {
	fs := flag.NewFlagSet("ensure", flag.ExitOnError)
	socket := fs.String("socket", "/run/c4/agent.sock", "agent socket")
	namespace := fs.String("namespace", os.Getenv("POD_NAMESPACE"), "Materialization namespace")
	name := fs.String("name", "", "Materialization name")
	view := fs.String("view", "", "view ID the pod was admitted for")
	wait := fs.Duration("connect-wait", 5*time.Minute, "how long to wait for the agent to come up")
	fs.Parse(args)
	if *name == "" || *view == "" || *namespace == "" {
		return errors.New("--namespace, --name, and --view are required")
	}

	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", *socket)
		},
	}}
	body, _ := json.Marshal(agent.Request{Namespace: *namespace, Name: *name, View: *view})
	deadline := time.Now().Add(*wait)
	var resp *http.Response
	for {
		var err error
		resp, err = hc.Post("http://agent/v1/ensure", "application/json", bytes.NewReader(body))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node agent unreachable at %s: %w", *socket, err)
		}
		fmt.Fprintf(os.Stderr, "waiting for node agent: %v\n", err)
		time.Sleep(2 * time.Second)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r agent.Response
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("agent replied %s: %s", resp.Status, raw)
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	fmt.Printf("view %s ready in %dms: %d files, %d bytes; %d objects (%d cached, %d fetched, %d refetched after failing verification), %d bytes fetched\n",
		r.View, r.Millis, r.Files, r.Bytes, r.Objects, r.Hits, r.Misses, r.Refetched, r.BytesFetched)
	fmt.Println(string(raw))
	return nil
}

func runTreeID(args []string) error {
	fs := flag.NewFlagSet("tree-id", flag.ExitOnError)
	expect := fs.String("expect", "", "fail unless the tree has this ID")
	hold := fs.Duration("hold", 0, "keep running this long after printing (for demos)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: c4-operator tree-id [--expect ID] [--hold DURATION] DIR")
	}
	id, err := manifest.ScanContentID(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Println(id)
	if *expect != "" && id.String() != *expect {
		return fmt.Errorf("tree is %s, expected %s", id, *expect)
	}
	if *hold > 0 {
		time.Sleep(*hold)
	}
	return nil
}
