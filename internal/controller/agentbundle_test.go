package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A bundle's member pod meets Pod Security restricted, like every unit: a
// local farm's project namespace enforces it, and a member refused there
// would wait forever with nothing on the job to say why.
func TestBundleMemberPodIsRestricted(t *testing.T) {
	b := &c4v1.AgentBundle{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "research"}}
	spec := JobForBundle(b).Spec.Template.Spec
	sc := spec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.SeccompProfile == nil {
		t.Fatalf("the member pod has no restricted pod security context: %+v", sc)
	}
	c := spec.Containers[0].SecurityContext
	if c == nil || c.AllowPrivilegeEscalation == nil || *c.AllowPrivilegeEscalation ||
		c.Capabilities == nil || len(c.Capabilities.Drop) == 0 || c.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("the member container is not restricted: %+v", c)
	}
}
