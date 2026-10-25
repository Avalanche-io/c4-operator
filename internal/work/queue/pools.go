package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
	site      string // from Queue.Placement; "" without one
	// A host pool (one tolerating c4v1.TaintHostRunner) runs its units on
	// people's own machines. Each unit is created bound to one of them
	// (hostNode), so no runner binds a pod and none can claim another
	// person's: owners and free are each ready node's, by name.
	host   bool
	owners map[string][]string
	free   map[string]int
	// unenrolled are the machines no one has enrolled through the HUD (no
	// node-owners): true where they serve their pool's one owner, false
	// where the pool has several and so they serve no one.
	unenrolled map[string]bool
}

// hostNode is the machine of a host pool that a unit of owner's needing
// slots is created on: one whose owners include owner, with the most
// free slots, then the first by name. With none, it says why.
func (p *pool) hostNode(owner string, slots int) (string, string) {
	best, room, serves, waiting := "", 0, false, false
	for _, n := range p.nodes {
		if serving, ok := p.unenrolled[n]; ok && !serving {
			waiting = true
		}
		if owner == "" || !slices.Contains(p.owners[n], owner) {
			continue
		}
		serves = true
		if f := p.free[n]; f >= slots && f > room {
			best, room = n, f
		}
	}
	switch {
	case best != "":
		return best, ""
	case owner == "":
		return "", fmt.Sprintf("pool %s runs on people's own machines, and this job has no owner", p.spec.Name)
	case !serves && waiting:
		return "", fmt.Sprintf("no machine of %s's in pool %s has been enrolled", owner, p.spec.Name)
	case !serves:
		return "", fmt.Sprintf("no machine in pool %s runs %s's work", p.spec.Name, owner)
	}
	need := "a free slot"
	if slots > 1 {
		need = fmt.Sprintf("%d free slots", slots)
	}
	return "", fmt.Sprintf("no machine of %s's in pool %s has %s", owner, p.spec.Name, need)
}

// nodeOwners is whose work a host runner's Node runs: its node-owners
// annotation, which only the HUD writes on a person's signed enroll. A
// Node without one (enrolled before that route) serves its pool's owner
// when the pool has exactly one, the farm admin's word that no host token
// can write; with several owners it serves no one, since one machine's
// sign-in would run all of their work. enrolled says which it was.
//
// TODO(enroll route): when the HUD's signed enroll lands, a Node without
// node-owners serves no one, and this fallback goes in the same landing.
func nodeOwners(node *corev1.Node, ps *c4v1.Pool) (owners []string, enrolled bool) {
	v, ok := node.Annotations[c4v1.AnnotationNodeOwners]
	if !ok {
		if len(ps.Spec.Owners) == 1 {
			return ps.Spec.Owners, false
		}
		return nil, false
	}
	// a JSON list, like every owner list; one that does not read serves no one
	if json.Unmarshal([]byte(v), &owners) != nil {
		return nil, true
	}
	return slices.DeleteFunc(owners, func(o string) bool { return o == "" }), true
}

// basis says, for a unit on a machine no one has enrolled through the
// HUD, where its owner came from; "" for an enrolled machine.
func (p *pool) basis(node string) string {
	if !p.unenrolled[node] {
		return ""
	}
	return "its pool " + p.spec.Name + "'s owner: no one has enrolled this machine"
}

func tolerates(ts []corev1.Toleration, key string) bool {
	for _, t := range ts {
		if t.Key == key {
			return true
		}
	}
	return false
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
		if tolerates(ps.Spec.Tolerations, c4v1.TaintHostRunner) {
			p.host, p.owners, p.free, p.unenrolled = true, map[string][]string{}, map[string]int{}, map[string]bool{}
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
			if p.host && node.Labels[c4v1.LabelHost] != node.Name {
				// A runner names its own Node when it creates it, so only
				// a Node whose host label is its own name is the machine
				// the pool was enrolled with.
				continue
			}
			n := nodeSlots(node, p.perNode)
			if p.host {
				owners, enrolled := nodeOwners(node, ps)
				p.owners[node.Name], p.free[node.Name] = owners, n
				if !enrolled {
					p.unenrolled[node.Name] = len(owners) > 0
				}
			}
			p.nodes = append(p.nodes, node.Name)
			p.slots += n
			q.advertise(ctx, node, n)
		}
		sort.Strings(p.nodes)
		sort.Strings(p.spare)
		if q.Placement != nil {
			var regions []string
			for n := range nodes {
				if sel.Matches(labels.Set(nodes[n].Labels)) {
					if r := nodes[n].Labels[corev1.LabelTopologyRegion]; r != "" {
						regions = append(regions, r)
					}
				}
			}
			p.site = q.Placement.Site(ps, regions)
		}
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
		if _, ok := p.free[pd.Spec.NodeName]; ok {
			p.free[pd.Spec.NodeName] -= n
		}
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
