package egress

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Cluster resolves a client address to the unit pod that has it, and the
// unit's tool: the pod's c4.avalanche.io/tool, found in its namespace,
// else in the operator's; for a tool job's unit, the networks its job's
// tool declares, as the farm defines them. Answers are kept for TTL.
type Cluster struct {
	Client client.Client
	// Namespace is the operator's, where the farm's tools live.
	Namespace string
	TTL       time.Duration
	Now       func() time.Time

	mu   sync.Mutex
	seen map[string]answer
}

type answer struct {
	unit  Unit
	rules []c4v1.ToolEgress
	err   error
	at    time.Time
}

func (c *Cluster) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Rules is the unit at ip and its tool's egress.
func (c *Cluster) Rules(ctx context.Context, ip string) (Unit, []c4v1.ToolEgress, error) {
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	c.mu.Lock()
	if a, ok := c.seen[ip]; ok && c.now().Sub(a.at) < ttl {
		c.mu.Unlock()
		return a.unit, a.rules, a.err
	}
	c.mu.Unlock()
	u, rules, err := c.resolve(ctx, ip)
	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]answer{}
	}
	c.seen[ip] = answer{unit: u, rules: rules, err: err, at: c.now()}
	c.mu.Unlock()
	return u, rules, err
}

func (c *Cluster) resolve(ctx context.Context, ip string) (Unit, []c4v1.ToolEgress, error) {
	var pods corev1.PodList
	if err := c.Client.List(ctx, &pods, client.MatchingFields{"status.podIP": ip}, client.HasLabels{c4v1.LabelJob}); err != nil {
		return Unit{}, nil, err
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodPending {
			continue
		}
		tool := p.Labels[c4v1.LabelTool]
		u := Unit{Namespace: p.Namespace, Job: p.Labels[c4v1.LabelJob], Unit: p.Labels[c4v1.LabelUnit], Pod: p.Name, Tool: tool,
			JobTool: p.Labels[c4v1.LabelJobTool] != ""}
		if tool == "" {
			return u, nil, errors.New("unit " + p.Namespace + "/" + p.Name + " has no tool")
		}
		if u.JobTool {
			// A tool job's unit: the networks its job's tool declares.
			var j c4v1.WorkJob
			if err := c.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: u.Job}, &j); err != nil {
				return u, nil, fmt.Errorf("job %s/%s: %w", p.Namespace, u.Job, err)
			}
			if j.Spec.Tool == nil || j.Spec.Tool.Name != tool {
				return u, nil, fmt.Errorf("job %s/%s does not run under tool %s", p.Namespace, u.Job, tool)
			}
			nets, err := LoadNetworks(ctx, c.Client, c.Namespace)
			if err != nil {
				return u, nil, err
			}
			rules, missing := nets.Expand(j.Spec.Tool.Network)
			if len(missing) > 0 {
				return u, nil, errors.New(Missing(tool, missing, c.Namespace))
			}
			return u, rules, nil
		}
		for _, ns := range []string{p.Namespace, c.Namespace} {
			if ns == "" {
				continue
			}
			var tp c4v1.Tool
			err := c.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: tool}, &tp)
			if err == nil {
				return u, tp.Spec.Egress, nil
			}
			if !apierrors.IsNotFound(err) {
				return u, nil, err
			}
		}
		return u, nil, fmt.Errorf("tool %s does not exist", tool)
	}
	return Unit{}, nil, fmt.Errorf("no unit pod at %s", ip)
}

// Advise says what would let a refused unit reach host on port: for a
// tool job, the network to declare in its tool.yaml, or that a farm admin
// must define one first; for an agent's Tool, the rule to add.
func (c *Cluster) Advise(ctx context.Context, u Unit, host string, port int) string {
	if !u.JobTool {
		return fmt.Sprintf("add {host: %s, ports: [%d]} to Tool %s's spec.egress", host, port, u.Tool)
	}
	nets, err := LoadNetworks(ctx, c.Client, c.Namespace)
	if err == nil {
		if names := nets.Offering(host, port); len(names) > 0 {
			return fmt.Sprintf("add needs: {network: [%s]} to %s's tool.yaml and publish it again", names[0], u.Tool)
		}
	}
	return fmt.Sprintf("ask a farm admin to define a network for %s:%d in the %s ConfigMap, then add needs: {network: [<its name>]} to %s's tool.yaml",
		host, port, c4v1.NetworksConfigMap, u.Tool)
}
