// Package jobspec builds the pod a job's units run, the same way for
// `c4 farm submit` and MCP's c4_submit_job: one container under Pod
// Security restricted (non-root 65532, no capabilities, RuntimeDefault
// seccomp), which is all a project namespace admits.
package jobspec

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

// UID is the user every unit runs as unless a template says otherwise.
const UID = 65532

// Container describes the one container of a unit.
type Container struct {
	Image   string
	Command []string
	Args    []string
	Env     map[string]string
	// Requests are resource requests (cpu, memory, nvidia.com/gpu).
	Requests map[string]string
}

// Pod returns the unit pod template for one container.
func Pod(c Container) (corev1.PodTemplateSpec, error) {
	if strings.TrimSpace(c.Image) == "" {
		return corev1.PodTemplateSpec{}, fmt.Errorf("an image is required")
	}
	ct := corev1.Container{Name: "unit", Image: c.Image, Command: c.Command, Args: c.Args,
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasPrefix(k, "C4_") {
			return corev1.PodTemplateSpec{}, fmt.Errorf("%s: C4_ variables are set by the queue", k)
		}
		ct.Env = append(ct.Env, corev1.EnvVar{Name: k, Value: c.Env[k]})
	}
	if len(c.Requests) > 0 {
		ct.Resources.Requests = corev1.ResourceList{}
		for k, v := range c.Requests {
			q, err := resource.ParseQuantity(v)
			if err != nil {
				return corev1.PodTemplateSpec{}, fmt.Errorf("request %s=%s: %v", k, v, err)
			}
			ct.Resources.Requests[corev1.ResourceName(k)] = q
		}
	}
	return Restrict(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{ct}}}), nil
}

// Restrict fills in what Pod Security restricted requires where a
// template leaves it unset; it never loosens what a template sets.
func Restrict(t corev1.PodTemplateSpec) corev1.PodTemplateSpec {
	t = *t.DeepCopy()
	if t.Spec.RestartPolicy == "" {
		t.Spec.RestartPolicy = corev1.RestartPolicyNever
	}
	sc := t.Spec.SecurityContext
	if sc == nil {
		sc = &corev1.PodSecurityContext{}
		t.Spec.SecurityContext = sc
	}
	if sc.RunAsNonRoot == nil {
		sc.RunAsNonRoot = ptr.To(true)
	}
	if sc.RunAsUser == nil {
		sc.RunAsUser = ptr.To[int64](UID)
	}
	if sc.SeccompProfile == nil {
		sc.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	for i := range t.Spec.Containers {
		c := &t.Spec.Containers[i]
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		if c.SecurityContext.AllowPrivilegeEscalation == nil {
			c.SecurityContext.AllowPrivilegeEscalation = ptr.To(false)
		}
		if c.SecurityContext.Capabilities == nil {
			c.SecurityContext.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
		}
	}
	return t
}
