package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A bundle whose members cannot start says why, and is not Ready: the
// refusal its job carries is the bundle's too.
func TestBundleSaysWhyMembersCannotStart(t *testing.T) {
	b := &c4v1.AgentBundle{ObjectMeta: metav1.ObjectMeta{Namespace: "p", Name: "research"},
		Spec: c4v1.AgentBundleSpec{Members: []c4v1.AgentMemberSpec{{Name: "lead"}}}}
	j := JobForBundle(b)
	j.Status.Conditions = []metav1.Condition{{Type: "Admitted", Status: metav1.ConditionFalse, Reason: "PodSecurity",
		Message: "Pod Security (restricted) in p refuses the job's units"}}
	st := bundleStatus(b, j, nil)
	ready := meta.FindStatusCondition(st.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "Admitted" || !strings.Contains(ready.Message, "Pod Security") {
		t.Fatalf("a bundle whose members are refused reads ready: %+v", ready)
	}
	if c := meta.FindStatusCondition(st.Conditions, "Admitted"); c == nil || c.Reason != "PodSecurity" {
		t.Fatalf("the bundle does not carry its job's refusal: %+v", c)
	}
	// Admitted again: Ready, and the refusal gone.
	j.Status.Conditions = nil
	b.Status = st
	st = bundleStatus(b, j, nil)
	if ready := meta.FindStatusCondition(st.Conditions, "Ready"); ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("an admitted bundle is not ready: %+v", ready)
	}
	if c := meta.FindStatusCondition(st.Conditions, "Admitted"); c != nil {
		t.Fatalf("an admitted bundle still carries the refusal: %+v", c)
	}
}
