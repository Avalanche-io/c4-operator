package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/controller"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4webhook "github.com/Avalanche-io/c4-operator/internal/webhook"
	"github.com/Avalanche-io/c4-operator/internal/work/kindsim"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/internal/work/sched"
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
	namespace := fs.String("namespace", env("POD_NAMESPACE", "c4-system"), "the operator's namespace (holds store credentials)")
	webhookConfig := fs.String("webhook-config", "c4-operator", "MutatingWebhookConfiguration to keep the CA in")
	retry := fs.Duration("retry", time.Minute, "recheck interval for Materializations that failed for a reason that may heal")
	work := fs.Bool("work", true, "run the work queue (WorkJobs, Pools, CapacityRequests)")
	tick := fs.Duration("work-tick", 2*time.Second, "work queue decision period")
	policy := fs.String("work-policy", "objective", "scheduling policy: objective, edf, fifo")
	provider := fs.String("capacity-provider", "", "fulfil CapacityRequests in-process: kind-sim (tests), or empty for an external provider")
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
	if err := (&controller.MaterializationReconciler{Client: mgr.GetClient(), Projects: reg, Retry: *retry}).SetupWithManager(mgr); err != nil {
		return err
	}
	if *work {
		pol, err := policyFor(*policy)
		if err != nil {
			return err
		}
		q := &queue.Queue{Client: mgr.GetClient(), Every: *tick, Policy: pol, Cfg: sched.DefaultConfig(), Log: ctrl.Log.WithName("queue"),
			Stores: func(ctx context.Context, name string) (*storeurl.Sharded, error) {
				p, err := reg.Get(ctx, name)
				if apierrors.IsNotFound(err) {
					return nil, nil
				}
				if err != nil {
					return nil, err
				}
				return reg.Store(ctx, p)
			}}
		if err := mgr.Add(q); err != nil {
			return err
		}
	}
	if *provider == "kind-sim" {
		if err := mgr.Add(&kindsim.Actuator{Client: mgr.GetClient(), Log: ctrl.Log.WithName("kind-sim")}); err != nil {
			return err
		}
	}
	mgr.GetWebhookServer().Register("/mutate-pod", &webhook.Admission{Handler: &c4webhook.PodMutator{Decoder: admission.NewDecoder(scheme)}})
	mgr.AddHealthzCheck("ping", healthz.Ping)
	mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker())
	log.Info("starting", "namespace", *namespace)
	return mgr.Start(ctx)
}
