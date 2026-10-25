package queue

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/egress"
)

// Tools (DESIGN 10.6). A unit that names a tool in its
// reasoning.tools runs with it: the tool's view mounted read-only at
// $C4_TOOLS, and egress to what the tool lists and nothing else. The
// queue finds the tool in the job's namespace, else in the operator's
// (the farm's defaults), and before it releases the unit makes the view's
// Materialization and the tool's NetworkPolicy in the job's namespace,
// which selects the unit pods labelled with the tool. Address blocks and
// cluster services are allowed directly; hosts only through the farm's
// egress proxy (internal/egress, Queue.EgressProxy), which the policy allows
// and the unit's HTTPS_PROXY and HTTP_PROXY name. A tool job's units
// (spec.tool) run under its tool the same way, in the job's own policy and
// with the networks the job names (jobToolReady); a unit of a tool job
// cannot name another tool. Under all of these, every namespace with units
// has a policy denying its unit pods egress (UnitsPolicyName), so a unit
// reaches only what its tool allows, and a unit with no tool reaches
// nothing.

// ToolsMount is where a unit's tool view is mounted ($C4_TOOLS).
const ToolsMount = "/etc/c4/tools"

// toolsName is the Materialization a job's units mount tool name's
// view through.
func toolsName(job, tool string) string { return fmt.Sprintf("%s-tools-%s", job, tool) }

// toolEnv is the unit's variable naming sidecar name's address.
func toolEnv(name string) string {
	return "C4_TOOL_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

// portName is a container port name (at most 15 characters).
func portName(name string) string {
	if len(name) > 15 {
		return name[:15]
	}
	return name
}

// toolsPolicyName is the NetworkPolicy of tool name in a namespace.
func toolsPolicyName(tool string) string { return "c4-tools-" + tool }

// hostRules reports whether a tool allows hosts, reached through the
// egress proxy.
func hostRules(p *c4v1.Tool) bool {
	for _, e := range p.Spec.Egress {
		if e.Host != "" {
			return true
		}
	}
	return false
}

// egressProxy is the farm's egress proxy: its Service's namespace and
// name, and its port.
func (q *Queue) egressProxy() (ns, name string, port int32, err error) {
	ref, ps, ok := strings.Cut(q.EgressProxy, ":")
	n, perr := strconv.Atoi(ps)
	if !ok || perr != nil || n <= 0 || n > 65535 || ref == "" {
		return "", "", 0, fmt.Errorf("egress proxy %q: want [<namespace>/]<service>:<port>", q.EgressProxy)
	}
	ns, name, ok = strings.Cut(ref, "/")
	if !ok {
		ns, name = q.Namespace, ref
	}
	return ns, name, int32(n), nil
}

// hudService is the HUD's Service: its namespace and name, and port.
func (q *Queue) hudService() (ns, name string, port int32, err error) {
	ref, ps, ok := strings.Cut(q.HUDService, ":")
	n, perr := strconv.Atoi(ps)
	if !ok || perr != nil || n <= 0 || n > 65535 || ref == "" {
		return "", "", 0, fmt.Errorf("HUD service %q: want [<namespace>/]<service>:<port>", q.HUDService)
	}
	ns, name, ok = strings.Cut(ref, "/")
	if !ok {
		ns, name = q.Namespace, ref
	}
	return ns, name, int32(n), nil
}

// hudURL is the HUD's API for a unit with a c4 grant ($C4_HUD).
func (q *Queue) hudURL() string {
	ns, name, port, err := q.hudService()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("http://%s.%s.svc:%d/api/v1", name, ns, port)
}

// proxyURL is the egress proxy's address for a unit's HTTPS_PROXY: its
// Service's cluster IP, so a unit that reaches only hosts needs no DNS
// (the proxy resolves them), else the Service's name. byIP says which.
func (q *Queue) proxyURL(ctx context.Context) (url string, byIP bool) {
	ns, name, port, err := q.egressProxy()
	if err != nil {
		return "", false
	}
	var svc corev1.Service
	if err := q.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &svc); err == nil {
		if ip := net.ParseIP(svc.Spec.ClusterIP); ip != nil {
			return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), true
		}
	}
	return fmt.Sprintf("http://%s.%s.svc:%d", name, ns, port), false
}

// clusterDNS is the cluster's DNS servers as a policy peer: CoreDNS (or
// kube-dns) in kube-system, by the label both carry.
var clusterDNS = networkingv1.NetworkPolicyPeer{
	NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
	PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}}}

// servicePeer is the pods a cluster service selects, as a policy peer.
func (q *Queue) servicePeer(ctx context.Context, ns, name string) (networkingv1.NetworkPolicyPeer, error) {
	var svc corev1.Service
	if err := q.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &svc); err != nil {
		return networkingv1.NetworkPolicyPeer{}, fmt.Errorf("service %s/%s: %w", ns, name, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return networkingv1.NetworkPolicyPeer{}, fmt.Errorf("service %s/%s selects no pods", ns, name)
	}
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: svc.Spec.Selector}}, nil
}

// toolObject finds the tool a unit names: the job's namespace first,
// then the operator's.
func (q *Queue) toolObject(ctx context.Context, ns, name string) (*c4v1.Tool, error) {
	for _, in := range []string{ns, q.Namespace} {
		if in == "" {
			continue
		}
		var p c4v1.Tool
		err := q.Client.Get(ctx, types.NamespacedName{Namespace: in, Name: name}, &p)
		if err == nil {
			return &p, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("tool %s does not exist in %s or %s", name, ns, q.Namespace)
}

// toolsReady prepares unit idx's tool, if it names one: its view's
// Materialization, Ready, and its NetworkPolicy. It returns the tool
// (nil for none), and false with why while the unit must wait.
func (q *Queue) toolsReady(ctx context.Context, v *view, idx int) (*c4v1.Tool, bool, string) {
	name := v.mem.plan.Units[idx].Reasoning.Tools
	if jt := v.job.Spec.Tool; jt != nil {
		// A tool job's units run under its tool alone: a unit (or an
		// expansion) naming another would reach what that one allows.
		if name != "" && name != jt.Name {
			return nil, false, fmt.Sprintf("unit %d names tool %s, but %s runs under tool %s, and a tool job's units run under its tool alone: leave reasoning.tools out",
				idx, name, v.job.Name, jt.Name)
		}
		return q.jobToolReady(ctx, v.job)
	}
	if name == "" {
		return nil, true, ""
	}
	p, err := q.toolObject(ctx, v.job.Namespace, name)
	if err != nil {
		return nil, false, err.Error()
	}
	for _, sc := range p.Spec.Sidecars {
		if sc.Name == "" || sc.Image == "" || sc.Port <= 0 || sc.Port > 65535 {
			return nil, false, fmt.Sprintf("tool %s: a sidecar needs a name, an image and a port", name)
		}
	}
	if p.Spec.C4 != "" && q.HUDService == "" {
		return nil, false, fmt.Sprintf("tool %s grants c4 %s, which needs the HUD (--hud-service)", name, p.Spec.C4)
	}
	if hostRules(p) && q.EgressProxy == "" {
		return nil, false, fmt.Sprintf("tool %s allows hosts, which need the farm's egress proxy (--egress-proxy)", name)
	}
	if p.Spec.View != "" {
		ready, err := q.ensureMaterialization(ctx, v.job, toolsName(v.job.Name, name), p.Spec.View, "")
		if err != nil {
			return nil, false, fmt.Sprintf("tool %s's view: %v", name, err)
		}
		if !ready {
			return nil, false, fmt.Sprintf("tool %s's view is not ready", name)
		}
	}
	if err := q.ensureToolsPolicy(ctx, v.job.Namespace, p); err != nil {
		return nil, false, fmt.Sprintf("tool %s's network policy: %v", name, err)
	}
	return p, true, ""
}

// jobToolPolicyName is a tool job's own NetworkPolicy (a job's name is at
// most 50 characters, so this fits a name's 63).
func jobToolPolicyName(job string) string { return "c4-job-tool-" + job }

// jobToolReady prepares a tool job's units' tool (spec.tool): the networks
// it declares, as the farm defines them, in the job's own NetworkPolicy,
// which selects only the job's units and goes with the job (the
// namespace's UnitsPolicyName still denies them egress once it is gone).
// With no network declared the policy allows nothing: the units are
// sealed. The
// tool's tree is already one of the job's inputs, at ToolsMount, so
// nothing is mounted here. It returns the tool as its units see it.
func (q *Queue) jobToolReady(ctx context.Context, j *c4v1.WorkJob) (*c4v1.Tool, bool, string) {
	name := j.Spec.Tool.Name
	nets := egress.Networks{}
	if q.Namespace != "" {
		var err error
		if nets, err = egress.LoadNetworks(ctx, q.Client, q.Namespace); err != nil {
			return nil, false, "the farm's networks: " + err.Error()
		}
	}
	rules, missing := nets.Expand(j.Spec.Tool.Network)
	if len(missing) > 0 {
		return nil, false, egress.Missing(name, missing, q.Namespace)
	}
	p := &c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: c4v1.ToolSpec{Egress: rules}}
	if hostRules(p) && q.EgressProxy == "" {
		return nil, false, fmt.Sprintf("tool %s reaches hosts, which need the farm's egress proxy (--egress-proxy)", name)
	}
	np, err := q.toolsPolicy(ctx, j.Namespace, p)
	if err != nil {
		return nil, false, fmt.Sprintf("tool %s's network policy: %v", name, err)
	}
	np.Name = jobToolPolicyName(j.Name)
	np.Labels = map[string]string{c4v1.LabelTool: name, c4v1.LabelJob: j.Name}
	np.Spec.PodSelector = metav1.LabelSelector{MatchLabels: map[string]string{c4v1.LabelJob: j.Name, c4v1.LabelJobTool: "true"}}
	np.OwnerReferences = []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: j.Name, UID: j.UID}}
	if err := q.ensurePolicy(ctx, np); err != nil {
		return nil, false, fmt.Sprintf("tool %s's network policy: %v", name, err)
	}
	return p, true, ""
}

// toolsPolicy is tool p's NetworkPolicy in namespace ns: egress to the
// address blocks and services it lists, to the egress proxy if it lists
// hosts, and the cluster's DNS servers when the unit names anything
// itself; nothing else.
func (q *Queue) toolsPolicy(ctx context.Context, ns string, p *c4v1.Tool) (*networkingv1.NetworkPolicy, error) {
	// A tool job's units carry the tool's label too, under their job's
	// own policy (jobToolPolicy), so this one leaves them out.
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: toolsPolicyName(p.Name),
		Labels: map[string]string{c4v1.LabelTool: p.Name}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{c4v1.LabelTool: p.Name},
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: c4v1.LabelJobTool, Operator: metav1.LabelSelectorOpDoesNotExist}}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{},
		}}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	named := false
	for _, e := range p.Spec.Egress {
		var rule networkingv1.NetworkPolicyEgressRule
		switch {
		case e.Host != "":
			continue // through the proxy, below
		case e.CIDR != "":
			rule.To = []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: e.CIDR}}}
		case e.Service != "":
			sns, sname, ok := strings.Cut(e.Service, "/")
			if !ok {
				return nil, fmt.Errorf("service %q: want <namespace>/<name>", e.Service)
			}
			peer, err := q.servicePeer(ctx, sns, sname)
			if err != nil {
				return nil, err
			}
			rule.To = []networkingv1.NetworkPolicyPeer{peer}
		default:
			return nil, fmt.Errorf("an egress rule names no host, cidr or service")
		}
		for _, port := range e.Ports {
			pt := intstr.FromInt32(port)
			rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &pt})
		}
		np.Spec.Egress = append(np.Spec.Egress, rule)
		named = true
	}
	if hostRules(p) {
		pns, pname, port, err := q.egressProxy()
		if err != nil {
			return nil, err
		}
		peer, err := q.servicePeer(ctx, pns, pname)
		if err != nil {
			return nil, fmt.Errorf("the egress proxy: %w", err)
		}
		pp := intstr.FromInt32(port)
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{peer},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &pp}}})
	}
	if p.Spec.C4 != "" {
		// The HUD, on the port its pods listen on.
		hns, hname, port, err := q.hudService()
		if err != nil {
			return nil, err
		}
		var svc corev1.Service
		if err := q.Client.Get(ctx, types.NamespacedName{Namespace: hns, Name: hname}, &svc); err != nil {
			return nil, fmt.Errorf("the HUD: %w", err)
		}
		peer, err := q.servicePeer(ctx, hns, hname)
		if err != nil {
			return nil, fmt.Errorf("the HUD: %w", err)
		}
		target := intstr.FromInt32(port)
		for _, sp := range svc.Spec.Ports {
			if sp.Port == port && sp.TargetPort.IntValue() > 0 {
				target = intstr.FromInt32(int32(sp.TargetPort.IntValue()))
			}
		}
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{peer},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &target}}})
	}
	// DNS, from the cluster's DNS servers alone, for what a unit reaches by
	// name itself: an address block or service rule, the HUD, or the proxy
	// when it has no cluster IP. A unit that reaches only hosts names the
	// proxy by its IP and needs none.
	_, proxyByIP := q.proxyURL(ctx)
	if named || p.Spec.C4 != "" || (hostRules(p) && !proxyByIP) {
		dns := intstr.FromInt32(53)
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{clusterDNS},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}}})
	}
	return np, nil
}

// UnitsPolicyName is the NetworkPolicy in every namespace with units that
// denies the unit pods egress. NetworkPolicies add up, so a unit reaches
// what its tool's policy or its tool job's policy allows and nothing else;
// a unit with neither, or whose job's policy is gone (a job being
// deleted), reaches nothing.
const UnitsPolicyName = "c4-units-deny-egress"

// unitsPolicy is namespace ns's unit egress default: every unit pod (the
// queue labels each with its job and its unit), egress, no rule.
func unitsPolicy(ns string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: UnitsPolicyName,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "c4-operator"}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: c4v1.LabelJob, Operator: metav1.LabelSelectorOpExists},
				{Key: c4v1.LabelUnit, Operator: metav1.LabelSelectorOpExists}}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{},
		}}
}

// sealNamespaces writes (or puts back) the unit egress default in every
// namespace with a job or a unit pod, and in every project's namespace,
// and says per namespace why it could not.
func (q *Queue) sealNamespaces(ctx context.Context, jobs []c4v1.WorkJob, pods []corev1.Pod, nsProject map[string]string) map[string]error {
	nss := map[string]bool{}
	for i := range jobs {
		nss[jobs[i].Namespace] = true
	}
	for i := range pods {
		nss[pods[i].Namespace] = true
	}
	for ns := range nsProject {
		nss[ns] = true
	}
	out := make(map[string]error, len(nss))
	for ns := range nss {
		err := q.ensurePolicy(ctx, unitsPolicy(ns))
		out[ns] = err
		if err != nil && !apierrors.IsNotFound(err) {
			q.Log.Error(err, "unit egress policy", "namespace", ns)
		}
	}
	return out
}

// unsealedWhy is why a unit in ns waits for its namespace's unit egress
// default.
func unsealedWhy(ns string, err error) string {
	if err == nil {
		return fmt.Sprintf("the policy denying %s's units egress (%s) is not written yet", ns, UnitsPolicyName)
	}
	return fmt.Sprintf("the policy denying %s's units egress (%s) cannot be written: %v", ns, UnitsPolicyName, err)
}

// ensureToolsPolicy makes or updates tool p's NetworkPolicy in ns.
func (q *Queue) ensureToolsPolicy(ctx context.Context, ns string, p *c4v1.Tool) error {
	want, err := q.toolsPolicy(ctx, ns, p)
	if err != nil {
		return err
	}
	return q.ensurePolicy(ctx, want)
}

// ensurePolicy makes or updates a NetworkPolicy.
func (q *Queue) ensurePolicy(ctx context.Context, want *networkingv1.NetworkPolicy) error {
	var cur networkingv1.NetworkPolicy
	err := q.Client.Get(ctx, types.NamespacedName{Namespace: want.Namespace, Name: want.Name}, &cur)
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

// applyTools gives unit pod pod of job its tool: the label its
// NetworkPolicy selects (and, for a tool job's tool, the label its job's
// policy selects), the view mounted at $C4_TOOLS, for hosts the
// egress proxy as its HTTPS_PROXY and HTTP_PROXY, and its sidecars, native
// sidecars that start before the unit and end with it.
func applyTools(pod *corev1.Pod, job string, p *c4v1.Tool, jobTool bool, proxy, hud string) {
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[c4v1.LabelTool] = p.Name
	if jobTool {
		pod.Labels[c4v1.LabelJobTool] = "true"
	}
	if p.Spec.C4 != "" && hud != "" {
		// A token for this pod alone, for the HUD alone, renewed by the
		// kubelet: the unit's way to c4 (its grant is read at the HUD).
		exp := int64(3600)
		if pod.Spec.ServiceAccountName == "" {
			// a projected token needs a named account (admission names
			// "default" too, where it runs)
			pod.Spec.ServiceAccountName = "default"
		}
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "c4-hud-token", VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: c4v1.HUDAudience, ExpirationSeconds: &exp, Path: "token"}}}}}})
		for k := range pod.Spec.Containers {
			pod.Spec.Containers[k].VolumeMounts = append(pod.Spec.Containers[k].VolumeMounts,
				corev1.VolumeMount{Name: "c4-hud-token", MountPath: "/etc/c4/hud", ReadOnly: true})
			pod.Spec.Containers[k].Env = append(pod.Spec.Containers[k].Env, corev1.EnvVar{Name: "C4_HUD", Value: hud},
				corev1.EnvVar{Name: "C4_HUD_TOKEN_FILE", Value: "/etc/c4/hud/token"})
		}
	}
	always := corev1.ContainerRestartPolicyAlways
	no := false
	for _, sc := range p.Spec.Sidecars {
		c := corev1.Container{Name: "tool-" + sc.Name, Image: sc.Image, Args: sc.Args, RestartPolicy: &always,
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			Ports:           []corev1.ContainerPort{{Name: portName(sc.Name), ContainerPort: sc.Port}}}
		keys := make([]string, 0, len(sc.Env))
		for k := range sc.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: sc.Env[k]})
		}
		if p.Spec.View != "" {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "c4-tools", MountPath: ToolsMount, ReadOnly: true})
		}
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, c)
		for k := range pod.Spec.Containers {
			pod.Spec.Containers[k].Env = append(pod.Spec.Containers[k].Env,
				corev1.EnvVar{Name: toolEnv(sc.Name), Value: fmt.Sprintf("http://localhost:%d", sc.Port)})
		}
	}
	if hostRules(p) && proxy != "" {
		env := []corev1.EnvVar{}
		for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
			env = append(env, corev1.EnvVar{Name: k, Value: proxy})
		}
		for _, k := range []string{"NO_PROXY", "no_proxy"} {
			env = append(env, corev1.EnvVar{Name: k, Value: "localhost,127.0.0.1,.svc,.cluster.local"})
		}
		for c := range pod.Spec.Containers {
			pod.Spec.Containers[c].Env = append(pod.Spec.Containers[c].Env, env...)
		}
	}
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
