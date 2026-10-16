// Package kindsim fulfils CapacityRequests on a kind cluster, where nodes
// cannot be created: a burst pool's nodes exist from the start, tainted
// c4.avalanche.io/unprovisioned, and "provisioning" one removes the taint
// after the pool's provisioning delay. Shrinking taints idle nodes again.
//
// It exercises the same interface a real provider adapter implements
// (DESIGN.md section 10.12): it reads CapacityRequests and reports
// fulfilment in their status. Cluster Autoscaler ProvisioningRequests and
// Karpenter NodePools are the production adapters.
package kindsim

import (
	"context"
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// AnnotationSince records when the current shortfall was first seen.
const AnnotationSince = "c4.avalanche.io/kind-sim-since"

// Actuator is the kind-sim capacity provider.
type Actuator struct {
	Client client.Client
	Log    logr.Logger
	Every  time.Duration
	Now    func() time.Time
}

// NeedLeaderElection makes the actuator run on one replica only.
func (a *Actuator) NeedLeaderElection() bool { return true }

// Start runs until ctx ends.
func (a *Actuator) Start(ctx context.Context) error {
	if a.Every <= 0 {
		a.Every = 2 * time.Second
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	t := time.NewTicker(a.Every)
	defer t.Stop()
	for {
		if err := a.Tick(ctx); err != nil && ctx.Err() == nil {
			a.Log.Error(err, "kind-sim tick")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick reconciles every kind-sim pool once.
func (a *Actuator) Tick(ctx context.Context) error {
	var pools c4v1.PoolList
	var crs c4v1.CapacityRequestList
	var nodes corev1.NodeList
	var pods corev1.PodList
	for _, l := range []client.ObjectList{&pools, &crs, &nodes} {
		if err := a.Client.List(ctx, l); err != nil {
			return err
		}
	}
	if err := a.Client.List(ctx, &pods, client.HasLabels{c4v1.LabelJob}); err != nil {
		return err
	}
	busy := map[string]bool{}
	for _, p := range pods.Items {
		if p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			busy[p.Spec.NodeName] = true
		}
	}
	now := a.Now()
	for i := range pools.Items {
		pl := &pools.Items[i]
		b := pl.Spec.Burst
		if b == nil || b.Provider != "kind-sim" {
			continue
		}
		var cr *c4v1.CapacityRequest
		for j := range crs.Items {
			if crs.Items[j].Name == pl.Name {
				cr = &crs.Items[j]
			}
		}
		per := int(max(1, pl.Spec.SlotsPerNode))
		want := 0
		if cr != nil && cr.Spec.Approved {
			want = min(int(b.MaxNodes), (int(cr.Spec.Slots)+per-1)/per)
		}
		sel := labels.SelectorFromSet(pl.Spec.NodeSelector)
		var up, spare []*corev1.Node
		for n := range nodes.Items {
			node := &nodes.Items[n]
			if !sel.Matches(labels.Set(node.Labels)) {
				continue
			}
			if hasTaint(node) {
				spare = append(spare, node)
			} else {
				up = append(up, node)
			}
		}
		sort.Slice(spare, func(x, y int) bool { return spare[x].Name < spare[y].Name })
		sort.Slice(up, func(x, y int) bool { return up[x].Name > up[y].Name })
		switch {
		case len(up) < want && len(spare) > 0:
			since := sinceOf(cr, now)
			if cr != nil && cr.Annotations[AnnotationSince] == "" {
				a.annotate(ctx, cr, now)
			}
			if now.Sub(since) < time.Duration(b.ProvisioningSeconds)*time.Second {
				a.state(ctx, cr, "Provisioning", len(up)*per, "waiting for the provisioning delay")
				continue
			}
			for _, node := range spare[:min(len(spare), want-len(up))] {
				if err := a.setTaint(ctx, node, false); err != nil {
					return err
				}
				up = append(up, node)
				a.Log.Info("provisioned", "pool", pl.Name, "node", node.Name)
			}
		case len(up) > want:
			for _, node := range up {
				if len(up) <= want {
					break
				}
				if busy[node.Name] {
					continue
				}
				if err := a.setTaint(ctx, node, true); err != nil {
					return err
				}
				up = up[1:]
				a.Log.Info("released", "pool", pl.Name, "node", node.Name)
			}
		}
		if cr != nil {
			if len(up) >= want && cr.Annotations[AnnotationSince] != "" {
				a.annotate(ctx, cr, time.Time{})
			}
			st := "Provisioned"
			if len(up) < want {
				st = "Provisioning"
			}
			a.state(ctx, cr, st, len(up)*per, "")
		}
	}
	return nil
}

func hasTaint(n *corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == c4v1.TaintUnprovisioned {
			return true
		}
	}
	return false
}

func (a *Actuator) setTaint(ctx context.Context, n *corev1.Node, on bool) error {
	patch := client.MergeFrom(n.DeepCopy())
	var taints []corev1.Taint
	for _, t := range n.Spec.Taints {
		if t.Key != c4v1.TaintUnprovisioned {
			taints = append(taints, t)
		}
	}
	if on {
		taints = append(taints, corev1.Taint{Key: c4v1.TaintUnprovisioned, Value: "true", Effect: corev1.TaintEffectNoSchedule})
	}
	n.Spec.Taints = taints
	return a.Client.Patch(ctx, n, patch)
}

func sinceOf(cr *c4v1.CapacityRequest, now time.Time) time.Time {
	if cr == nil {
		return now
	}
	t, err := time.Parse(time.RFC3339, cr.Annotations[AnnotationSince])
	if err != nil {
		return now
	}
	return t
}

func (a *Actuator) annotate(ctx context.Context, cr *c4v1.CapacityRequest, at time.Time) {
	patch := client.MergeFrom(cr.DeepCopy())
	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	if at.IsZero() {
		delete(cr.Annotations, AnnotationSince)
	} else {
		cr.Annotations[AnnotationSince] = at.UTC().Format(time.RFC3339)
	}
	a.Client.Patch(ctx, cr, patch)
}

func (a *Actuator) state(ctx context.Context, cr *c4v1.CapacityRequest, state string, slots int, msg string) {
	if cr == nil {
		return
	}
	st := c4v1.CapacityRequestStatus{State: state, ProvisionedSlots: int32(slots), Message: msg}
	if cr.Status == st {
		return
	}
	patch := client.MergeFrom(cr.DeepCopy())
	cr.Status = st
	a.Client.Status().Patch(ctx, cr, patch)
}
