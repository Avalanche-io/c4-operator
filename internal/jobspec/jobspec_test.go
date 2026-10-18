package jobspec

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestPodIsRestricted(t *testing.T) {
	p, err := Pod(Container{Image: "ghcr.io/avalanche-io/c4-job-hydra:v0.1.0", Command: []string{"c4-render"},
		Env: map[string]string{"RENDERER": "cycles"}, Requests: map[string]string{"cpu": "2", "memory": "4Gi"}})
	if err != nil {
		t.Fatal(err)
	}
	sc := p.Spec.SecurityContext
	if !*sc.RunAsNonRoot || *sc.RunAsUser != UID || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security %+v", sc)
	}
	c := p.Spec.Containers[0]
	if *c.SecurityContext.AllowPrivilegeEscalation || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("container security %+v", c.SecurityContext)
	}
	if c.Resources.Requests.Memory().String() != "4Gi" || p.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("resources %v restart %s", c.Resources.Requests, p.Spec.RestartPolicy)
	}
}

func TestPodRefusesQueueVariables(t *testing.T) {
	if _, err := Pod(Container{Image: "x", Env: map[string]string{"C4_FRAME_START": "1"}}); err == nil {
		t.Fatal("a C4_ variable was accepted")
	}
	if _, err := Pod(Container{}); err == nil {
		t.Fatal("no image was accepted")
	}
}

func TestRestrictKeepsWhatIsSet(t *testing.T) {
	in := corev1.PodTemplateSpec{Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)},
		Containers: []corev1.Container{{Name: "a", Image: "x"}}}}
	out := Restrict(in)
	if *out.Spec.SecurityContext.RunAsUser != 1000 || in.Spec.SecurityContext.RunAsNonRoot != nil {
		t.Fatalf("Restrict overrode or mutated its input")
	}
}
