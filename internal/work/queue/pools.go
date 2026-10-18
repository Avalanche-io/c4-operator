package queue

import (
	"context"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// pool is a Pool during a tick.
type pool struct {
	spec      *c4v1.Pool
	nodes     []string // ready, provisioned nodes
	spare     []string // burst nodes not provisioned
	perNode   int
	slots     int
	used      int
	requested int // burst slots requested
	cost      float64
	burstCost float64
}

// pools sizes every pool from its nodes, advertises each node's slots as
// an extended resource, and writes Pool status.
func (q *Queue) pools(ctx context.Context, list []c4v1.Pool, nodes []corev1.Node, pods []corev1.Pod, crs []c4v1.CapacityRequest) map[string]*pool {
	out := map[string]*pool{}
	for i := range list {
		ps := &list[i]
		p := &pool{spec: ps, perNode: int(max(1, ps.Spec.SlotsPerNode)), cost: floatOr(ps.Spec.CostPerSlotHour, 0)}
		if b := ps.Spec.Burst; b != nil {
			p.burstCost = floatOr(b.CostPerSlotHour, p.cost)
		}
		sel := labels.SelectorFromSet(ps.Spec.NodeSelector)
		for n := range nodes {
			node := &nodes[n]
			if !sel.Matches(labels.Set(node.Labels)) {
				continue
			}
			if tainted(node, c4v1.TaintUnprovisioned) {
				p.spare = append(p.spare, node.Name)
				continue
			}
			if !ready(node) || node.Spec.Unschedulable {
				continue
			}
			n := nodeSlots(node, p.perNode)
			p.nodes = append(p.nodes, node.Name)
			p.slots += n
			q.advertise(ctx, node, n)
		}
		sort.Strings(p.nodes)
		sort.Strings(p.spare)
		out[ps.Name] = p
	}
	for i := range pods {
		pd := &pods[i]
		p := out[pd.Labels[c4v1.LabelPool]]
		if p == nil || pd.Status.Phase == corev1.PodSucceeded || pd.Status.Phase == corev1.PodFailed {
			continue
		}
		n := 1
		if len(pd.Spec.Containers) > 0 {
			if qty, ok := pd.Spec.Containers[0].Resources.Requests[SlotResource]; ok {
				n = int(qty.Value())
			}
		}
		p.used += n
	}
	for _, cr := range crs {
		if p := out[cr.Spec.Pool]; p != nil && cr.Spec.Approved {
			p.requested += int(cr.Spec.Slots)
		}
	}
	for _, p := range out {
		st := c4v1.PoolStatus{Nodes: int32(len(p.nodes)), Slots: int32(p.slots), Used: int32(p.used),
			Requested: int32(p.requested)}
		if p.spec.Spec.Burst != nil {
			st.Provisioned = int32(len(p.nodes))
		}
		if st != p.spec.Status {
			patch := client.MergeFrom(p.spec.DeepCopy())
			p.spec.Status = st
			if err := q.Client.Status().Patch(ctx, p.spec, patch); err != nil {
				q.Log.Error(err, "pool status", "pool", p.spec.Name)
			}
		}
	}
	return out
}

// nodeSlots is a node's slots: its pool's SlotsPerNode, lowered by the
// node's c4.avalanche.io/slot-limit label when it has one (a host runner under
// a power cap). The label never raises a node above its pool's shape.
func nodeSlots(node *corev1.Node, perNode int) int {
	v, ok := node.Labels[c4v1.LabelSlotLimit]
	if !ok {
		return perNode
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return perNode
	}
	return min(n, perNode)
}

// advertise sets the node's slot capacity, the way a device plugin would
// advertise devices, so kube-scheduler counts slots as the queue does.
func (q *Queue) advertise(ctx context.Context, node *corev1.Node, slots int) {
	want := resource.MustParse(strconv.Itoa(slots))
	if have, ok := node.Status.Capacity[SlotResource]; ok && have.Cmp(want) == 0 {
		return
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Status.Capacity == nil {
		node.Status.Capacity = corev1.ResourceList{}
	}
	node.Status.Capacity[SlotResource] = want
	if err := q.Client.Status().Patch(ctx, node, patch); err != nil {
		q.Log.Error(err, "advertise slots", "node", node.Name)
	}
}

func ready(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func tainted(n *corev1.Node, key string) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == key {
			return true
		}
	}
	return false
}
