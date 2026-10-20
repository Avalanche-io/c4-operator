package queue

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Tool profiles (DESIGN 10.6). A unit that names a profile in its
// reasoning.tools runs with it: the profile's view mounted read-only at
// $C4_TOOLS, and egress to what the profile lists and nothing else. The
// queue finds the profile in the job's namespace, else in the operator's
// (the farm's defaults), and before it releases the unit makes the view's
// Materialization and the profile's NetworkPolicy in the job's namespace,
// which selects the unit pods labelled with the profile. Units without a
// profile run as before.

// ToolsMount is where a unit's tool profile view is mounted ($C4_TOOLS).
const ToolsMount = "/etc/c4/tools"

// toolsName is the Materialization a job's units mount profile name's
// view through.
func toolsName(job, profile string) string { return fmt.Sprintf("%s-tools-%s", job, profile) }

// toolsPolicyName is the NetworkPolicy of profile name in a namespace.
func toolsPolicyName(profile string) string { return "c4-tools-" + profile }

// toolProfile finds the profile a unit names: the job's namespace first,
// then the operator's.
func (q *Queue) toolProfile(ctx context.Context, ns, name string) (*c4v1.ToolProfile, error) {
	for _, in := range []string{ns, q.Namespace} {
		if in == "" {
			continue
		}
		var p c4v1.ToolProfile
		err := q.Client.Get(ctx, types.NamespacedName{Namespace: in, Name: name}, &p)
		if err == nil {
			return &p, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("tool profile %s does not exist in %s or %s", name, ns, q.Namespace)
}

// toolsReady prepares unit idx's tool profile, if it names one: its view's
// Materialization, Ready, and its NetworkPolicy. It returns the profile
// (nil for none), and false with why while the unit must wait.
func (q *Queue) toolsReady(ctx context.Context, v *view, idx int) (*c4v1.ToolProfile, bool, string) {
	name := v.mem.plan.Units[idx].Reasoning.Tools
	if name == "" {
		return nil, true, ""
	}
	p, err := q.toolProfile(ctx, v.job.Namespace, name)
	if err != nil {
		return nil, false, err.Error()
	}
	if p.Spec.View != "" {
		ready, err := q.ensureMaterialization(ctx, v.job, toolsName(v.job.Name, name), p.Spec.View, "")
		if err != nil {
			return nil, false, fmt.Sprintf("tool profile %s's view: %v", name, err)
		}
		if !ready {
			return nil, false, fmt.Sprintf("tool profile %s's view is not ready", name)
		}
	}
	if err := q.ensureToolsPolicy(ctx, v.job.Namespace, p); err != nil {
		return nil, false, fmt.Sprintf("tool profile %s's network policy: %v", name, err)
	}
	return p, true, ""
}

// toolsPolicy is profile p's NetworkPolicy in namespace ns: egress to what
// it lists, and with any rule the cluster's DNS; nothing else.
func (q *Queue) toolsPolicy(ctx context.Context, ns string, p *c4v1.ToolProfile) (*networkingv1.NetworkPolicy, error) {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: toolsPolicyName(p.Name),
		Labels: map[string]string{c4v1.LabelToolProfile: p.Name}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{c4v1.LabelToolProfile: p.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{},
		}}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	for _, e := range p.Spec.Egress {
		var rule networkingv1.NetworkPolicyEgressRule
		switch {
		case e.CIDR != "":
			rule.To = []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: e.CIDR}}}
		case e.Service != "":
			sns, sname, ok := strings.Cut(e.Service, "/")
			if !ok {
				return nil, fmt.Errorf("service %q: want <namespace>/<name>", e.Service)
			}
			var svc corev1.Service
			if err := q.Client.Get(ctx, types.NamespacedName{Namespace: sns, Name: sname}, &svc); err != nil {
				return nil, fmt.Errorf("service %s: %w", e.Service, err)
			}
			if len(svc.Spec.Selector) == 0 {
				return nil, fmt.Errorf("service %s selects no pods", e.Service)
			}
			rule.To = []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": sns}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: svc.Spec.Selector}}}
		default:
			return nil, fmt.Errorf("an egress rule names no cidr or service")
		}
		for _, port := range e.Ports {
			pt := intstr.FromInt32(port)
			rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &pt})
		}
		np.Spec.Egress = append(np.Spec.Egress, rule)
	}
	if len(np.Spec.Egress) > 0 {
		dns := intstr.FromInt32(53)
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}}})
	}
	return np, nil
}

// ensureToolsPolicy makes or updates profile p's NetworkPolicy in ns.
func (q *Queue) ensureToolsPolicy(ctx context.Context, ns string, p *c4v1.ToolProfile) error {
	want, err := q.toolsPolicy(ctx, ns, p)
	if err != nil {
		return err
	}
	var cur networkingv1.NetworkPolicy
	err = q.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: want.Name}, &cur)
	switch {
	case apierrors.IsNotFound(err):
		return q.Client.Create(ctx, want)
	case err != nil:
		return err
	case equality.Semantic.DeepEqual(cur.Spec, want.Spec):
		return nil
	}
	cur.Spec = want.Spec
	return q.Client.Update(ctx, &cur)
}

// applyTools gives unit pod pod of job its tool profile: the label its
// NetworkPolicy selects, and the view mounted at $C4_TOOLS.
func applyTools(pod *corev1.Pod, job string, p *c4v1.ToolProfile) {
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[c4v1.LabelToolProfile] = p.Name
	if p.Spec.View == "" {
		return
	}
	ro := true
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "c4-tools", VolumeSource: corev1.VolumeSource{
		CSI: &corev1.CSIVolumeSource{Driver: c4v1.DriverName, ReadOnly: &ro,
			VolumeAttributes: map[string]string{c4v1.VolumeAttrMaterialization: toolsName(job, p.Name)}}}})
	for c := range pod.Spec.Containers {
		pod.Spec.Containers[c].VolumeMounts = append(pod.Spec.Containers[c].VolumeMounts,
			corev1.VolumeMount{Name: "c4-tools", MountPath: ToolsMount, ReadOnly: true})
		pod.Spec.Containers[c].Env = append(pod.Spec.Containers[c].Env, corev1.EnvVar{Name: "C4_TOOLS", Value: ToolsMount})
	}
}
