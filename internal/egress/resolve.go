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
// unit's profile: the pod's c4.avalanche.io/tool-profile, found in its
// namespace, else in the operator's. Answers are kept for TTL.
type Cluster struct {
	Client client.Client
	// Namespace is the operator's, where the farm's profiles live.
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

// Rules is the unit at ip and its profile's egress.
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
		profile := p.Labels[c4v1.LabelToolProfile]
		u := Unit{Namespace: p.Namespace, Job: p.Labels[c4v1.LabelJob], Unit: p.Labels[c4v1.LabelUnit], Pod: p.Name, Profile: profile}
		if profile == "" {
			return u, nil, errors.New("unit " + p.Namespace + "/" + p.Name + " has no tool profile")
		}
		for _, ns := range []string{p.Namespace, c.Namespace} {
			if ns == "" {
				continue
			}
			var tp c4v1.ToolProfile
			err := c.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: profile}, &tp)
			if err == nil {
				return u, tp.Spec.Egress, nil
			}
			if !apierrors.IsNotFound(err) {
				return u, nil, err
			}
		}
		return u, nil, fmt.Errorf("tool profile %s does not exist", profile)
	}
	return Unit{}, nil, fmt.Errorf("no unit pod at %s", ip)
}
