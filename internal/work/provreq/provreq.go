// Package provreq fulfils CapacityRequests through Cluster Autoscaler's
// ProvisioningRequests (autoscaling.x-k8s.io/v1, DESIGN 10.12), for burst
// pools whose provider is provisioning-request.
//
// A pool's request for N slots is N/slotsPerNode nodes. Each burst
// decision (each generation of the pool's CapacityRequest) gets fresh
// ProvisioningRequests, because their spec is immutable and a booking
// lasts ten minutes:
//
//  1. check-capacity.autoscaling.x-k8s.io: is there room on existing
//     nodes? If Provisioned, the room is booked against other requests,
//     and the queue's units take it.
//  2. If not, best-effort-atomic-scale-up.autoscaling.x-k8s.io: scale one
//     node group for all of it, or none.
//
// Both point at the pool's PodTemplate: a pod per node (required pod
// anti-affinity on the hostname) with the pool's node selector and
// tolerations and a token request, so count means nodes whatever their
// size. A request whose booking expired, or that failed, while the pool
// is still short, is replaced by a new pair after a short backoff; one the
// pool no longer needs is deleted.
package provreq

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Provider is the PoolBurst provider this actuator serves.
const Provider = "provisioning-request"

// The ProvisioningRequest API and its classes.
var GVK = schema.GroupVersionKind{Group: "autoscaling.x-k8s.io", Version: "v1", Kind: "ProvisioningRequest"}

const (
	ClassCheck = "check-capacity.autoscaling.x-k8s.io"
	ClassScale = "best-effort-atomic-scale-up.autoscaling.x-k8s.io"

	// Conditions CA sets (cluster-autoscaler/apis/provisioningrequest).
	CondProvisioned     = "Provisioned"
	CondBookingExpired  = "BookingExpired"
	CondFailed          = "Failed"
	CondCapacityRevoked = "CapacityRevoked"

	// LabelPool names the pool a request or template is for, and
	// AnnotationRound which round of the current decision it belongs to.
	LabelPool       = "c4.avalanche.io/provreq-pool"
	AnnotationRound = "c4.avalanche.io/provreq-round"
)

// retryAfter is the backoff before a failed or expired round is replaced.
const retryAfter = time.Minute

// Actuator is the ProvisioningRequest capacity provider.
type Actuator struct {
	Client client.Client
	Log    logr.Logger
	// Namespace holds the ProvisioningRequests and PodTemplates (the
	// operator's).
	Namespace string
	Every     time.Duration
	Now       func() time.Time
}

// NeedLeaderElection makes the actuator run on one replica only.
func (a *Actuator) NeedLeaderElection() bool { return true }

// Start runs until ctx ends.
func (a *Actuator) Start(ctx context.Context) error {
	if a.Every <= 0 {
		a.Every = 5 * time.Second
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	t := time.NewTicker(a.Every)
	defer t.Stop()
	for {
		if err := a.Tick(ctx); err != nil && ctx.Err() == nil {
			a.Log.Error(err, "provisioning-request tick")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick reconciles every provisioning-request pool once.
func (a *Actuator) Tick(ctx context.Context) error {
	if a.Now == nil {
		a.Now = time.Now
	}
	var pools c4v1.PoolList
	var crs c4v1.CapacityRequestList
	var nodes corev1.NodeList
	for _, l := range []client.ObjectList{&pools, &crs, &nodes} {
		if err := a.Client.List(ctx, l); err != nil {
			return err
		}
	}
	prs := &unstructured.UnstructuredList{}
	prs.SetGroupVersionKind(GVK.GroupVersion().WithKind("ProvisioningRequestList"))
	if err := a.Client.List(ctx, prs, client.InNamespace(a.Namespace), client.HasLabels{LabelPool}); err != nil {
		return err
	}
	byPool := map[string][]*unstructured.Unstructured{}
	for i := range prs.Items {
		pr := &prs.Items[i]
		byPool[pr.GetLabels()[LabelPool]] = append(byPool[pr.GetLabels()[LabelPool]], pr)
	}
	for i := range pools.Items {
		pl := &pools.Items[i]
		if b := pl.Spec.Burst; b == nil || b.Provider != Provider {
			continue
		}
		var cr *c4v1.CapacityRequest
		for j := range crs.Items {
			if crs.Items[j].Name == pl.Name {
				cr = &crs.Items[j]
			}
		}
		if err := a.pool(ctx, pl, cr, readyNodes(pl, nodes.Items), byPool[pl.Name]); err != nil {
			a.Log.Error(err, "pool", "pool", pl.Name)
		}
	}
	return nil
}

func readyNodes(pl *c4v1.Pool, nodes []corev1.Node) int {
	sel := labels.SelectorFromSet(pl.Spec.NodeSelector)
	n := 0
	for i := range nodes {
		node := &nodes[i]
		if !sel.Matches(labels.Set(node.Labels)) || node.Spec.Unschedulable {
			continue
		}
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				n++
			}
		}
	}
	return n
}

// pool moves one pool's requests along.
func (a *Actuator) pool(ctx context.Context, pl *c4v1.Pool, cr *c4v1.CapacityRequest, ready int, prs []*unstructured.Unstructured) error {
	per := int(max(1, pl.Spec.SlotsPerNode))
	want := 0
	if cr != nil && cr.Spec.Approved {
		want = min(int(pl.Spec.Burst.MaxNodes), (int(cr.Spec.Slots)+per-1)/per)
	}
	if want <= ready {
		// Enough nodes: no booking needed. Scale-down is the autoscaler's
		// own (DESIGN 10.12).
		for _, pr := range prs {
			a.delete(ctx, pr)
		}
		if cr != nil && want > 0 {
			a.state(ctx, cr, "Provisioned", ready*per, fmt.Sprintf("%d nodes ready", ready))
		}
		return nil
	}
	gen := cr.Generation
	round := a.round(cr, prs)
	base := fmt.Sprintf("c4-%s-g%d-r%d", pl.Name, gen, round)
	// Requests of an earlier decision or round are no longer wanted.
	var check, scale *unstructured.Unstructured
	for _, pr := range prs {
		switch pr.GetName() {
		case base + "-check":
			check = pr
		case base + "-scale":
			scale = pr
		default:
			a.delete(ctx, pr)
		}
	}
	if err := a.template(ctx, pl); err != nil {
		return err
	}
	if check == nil {
		a.state(ctx, cr, "Provisioning", ready*per, fmt.Sprintf("asking whether %d nodes fit (%s)", want, ClassCheck))
		return a.create(ctx, pl, base+"-check", ClassCheck, want, round)
	}
	switch {
	case cond(check, CondProvisioned) == metav1.ConditionTrue && !expired(check):
		a.state(ctx, cr, "Provisioning", ready*per, fmt.Sprintf("room for %d nodes on existing capacity, booked until %s", want, bookedUntil(check)))
		return nil
	case cond(check, CondProvisioned) == metav1.ConditionTrue && expired(check):
		return a.next(ctx, pl, cr, want, base, check, round, ready*per, "the booking on existing capacity expired")
	case cond(check, CondProvisioned) != metav1.ConditionFalse && cond(check, CondFailed) != metav1.ConditionTrue:
		a.state(ctx, cr, "Provisioning", ready*per, fmt.Sprintf("asking whether %d nodes fit (%s)", want, ClassCheck))
		return nil
	}
	// No room: scale up.
	if scale == nil {
		a.state(ctx, cr, "Provisioning", ready*per, fmt.Sprintf("no room on existing capacity; scaling up to %d nodes (%s)", want, ClassScale))
		return a.create(ctx, pl, base+"-scale", ClassScale, want, round)
	}
	switch {
	case cond(scale, CondFailed) == metav1.ConditionTrue:
		return a.next(ctx, pl, cr, want, base, scale, round, ready*per, "scale-up failed: "+message(scale, CondFailed))
	case cond(scale, CondCapacityRevoked) == metav1.ConditionTrue:
		return a.next(ctx, pl, cr, want, base, scale, round, ready*per, "capacity revoked: "+message(scale, CondCapacityRevoked))
	case cond(scale, CondProvisioned) == metav1.ConditionTrue && expired(scale):
		return a.next(ctx, pl, cr, want, base, scale, round, ready*per, "the scale-up's booking expired")
	case cond(scale, CondProvisioned) == metav1.ConditionTrue:
		a.state(ctx, cr, "Provisioning", ready*per, fmt.Sprintf("scaled up for %d nodes, booked until %s", want, bookedUntil(scale)))
	default:
		a.state(ctx, cr, "Provisioning", ready*per, fmt.Sprintf("scaling up to %d nodes (%s)", want, ClassScale))
	}
	return nil
}

// round is the current round of the decision: the newest request's,
// counted afresh for a new generation.
func (a *Actuator) round(cr *c4v1.CapacityRequest, prs []*unstructured.Unstructured) int {
	r := 0
	prefix := fmt.Sprintf("-g%d-r", cr.Generation)
	for _, pr := range prs {
		name := pr.GetName()
		i := strings.LastIndex(name, prefix)
		if i < 0 {
			continue
		}
		rest := name[i+len(prefix):]
		rest = rest[:strings.IndexByte(rest+"-", '-')]
		if n, err := strconv.Atoi(rest); err == nil && n > r {
			r = n
		}
	}
	return r
}

// next starts a new round once the last one is retryAfter old.
func (a *Actuator) next(ctx context.Context, pl *c4v1.Pool, cr *c4v1.CapacityRequest, want int, base string, last *unstructured.Unstructured, round, slots int, why string) error {
	at := last.GetCreationTimestamp().Time
	for _, c := range conditions(last) {
		if c.LastTransitionTime.After(at) {
			at = c.LastTransitionTime.Time
		}
	}
	if a.Now().Sub(at) < retryAfter {
		a.state(ctx, cr, "Provisioning", slots, why+"; asking again shortly")
		return nil
	}
	a.state(ctx, cr, "Provisioning", slots, why+"; asking again")
	a.Log.Info("new round", "capacityRequest", cr.Name, "round", round+1, "why", why)
	// The new round starts with its own check; this round's requests go
	// next tick.
	name := strings.TrimSuffix(base, fmt.Sprintf("-r%d", round)) + fmt.Sprintf("-r%d-check", round+1)
	return a.create(ctx, pl, name, ClassCheck, want, round+1)
}

// TemplateName is the pool's PodTemplate.
func TemplateName(pool string) string { return "c4-slot-" + pool }

// template makes the pool's PodTemplate: one token pod per node.
func (a *Actuator) template(ctx context.Context, pl *c4v1.Pool) error {
	name := TemplateName(pl.Name)
	var pt corev1.PodTemplate
	err := a.Client.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: name}, &pt)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	lbl := map[string]string{LabelPool: pl.Name}
	pt = corev1.PodTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: a.Namespace, Name: name, Labels: lbl},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}, Spec: corev1.PodSpec{
			NodeSelector: pl.Spec.NodeSelector,
			Tolerations:  pl.Spec.Tolerations,
			Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: lbl}, TopologyKey: corev1.LabelHostname}}}},
			Containers: []corev1.Container{{Name: "slot", Image: "registry.k8s.io/pause:3.10",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")}}}},
		}}}
	return a.Client.Create(ctx, &pt)
}

func (a *Actuator) create(ctx context.Context, pl *c4v1.Pool, name, class string, nodes, round int) error {
	pr := &unstructured.Unstructured{}
	pr.SetGroupVersionKind(GVK)
	pr.SetNamespace(a.Namespace)
	pr.SetName(name)
	pr.SetLabels(map[string]string{LabelPool: pl.Name})
	pr.SetAnnotations(map[string]string{AnnotationRound: strconv.Itoa(round)})
	spec := map[string]any{
		"provisioningClassName": class,
		"podSets":               []any{map[string]any{"podTemplateRef": map[string]any{"name": TemplateName(pl.Name)}, "count": int64(nodes)}},
	}
	if class == ClassScale && pl.Spec.Burst.ProvisioningSeconds > 0 {
		// Retry the scale-up no longer than a few provisioning delays.
		spec["parameters"] = map[string]any{"ValidUntilSeconds": strconv.Itoa(int(4 * pl.Spec.Burst.ProvisioningSeconds))}
	}
	pr.Object["spec"] = spec
	if err := a.Client.Create(ctx, pr); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	a.Log.Info("provisioning request", "name", name, "class", class, "nodes", nodes)
	return nil
}

func (a *Actuator) delete(ctx context.Context, pr *unstructured.Unstructured) {
	if err := a.Client.Delete(ctx, pr); err != nil && !apierrors.IsNotFound(err) {
		a.Log.Error(err, "delete provisioning request", "name", pr.GetName())
	}
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
	if err := a.Client.Status().Patch(ctx, cr, patch); err != nil {
		a.Log.Error(err, "capacity request status", "name", cr.Name)
	}
}

func conditions(pr *unstructured.Unstructured) []metav1.Condition {
	raw, _, _ := unstructured.NestedSlice(pr.Object, "status", "conditions")
	var out []metav1.Condition
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		c := metav1.Condition{}
		c.Type, _ = m["type"].(string)
		s, _ := m["status"].(string)
		c.Status = metav1.ConditionStatus(s)
		c.Reason, _ = m["reason"].(string)
		c.Message, _ = m["message"].(string)
		if t, ok := m["lastTransitionTime"].(string); ok {
			if at, err := time.Parse(time.RFC3339, t); err == nil {
				c.LastTransitionTime = metav1.Time{Time: at}
			}
		}
		out = append(out, c)
	}
	return out
}

func cond(pr *unstructured.Unstructured, t string) metav1.ConditionStatus {
	for _, c := range conditions(pr) {
		if c.Type == t {
			return c.Status
		}
	}
	return metav1.ConditionUnknown
}

func message(pr *unstructured.Unstructured, t string) string {
	for _, c := range conditions(pr) {
		if c.Type == t {
			return c.Message
		}
	}
	return ""
}

func expired(pr *unstructured.Unstructured) bool {
	return cond(pr, CondBookingExpired) == metav1.ConditionTrue
}

// bookedUntil is when a provisioned request's ten-minute booking ends.
func bookedUntil(pr *unstructured.Unstructured) string {
	for _, c := range conditions(pr) {
		if c.Type == CondProvisioned && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Add(10 * time.Minute).UTC().Format(time.RFC3339)
		}
	}
	return "about ten minutes after provisioning"
}
