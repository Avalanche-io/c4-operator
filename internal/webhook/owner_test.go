package webhook

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

const managerSA = "system:serviceaccount:c4-system:c4-manager"

func ownerRequest(t *testing.T, user string, op admissionv1.Operation, ann map[string]string) admission.Request {
	t.Helper()
	job := &c4v1.WorkJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob"},
		ObjectMeta: metav1.ObjectMeta{Name: "sm0140-light-v001", Namespace: "saltmarsh", Annotations: ann},
		Spec:       c4v1.WorkJobSpec{Class: "render"},
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op,
		UserInfo:  authnv1.UserInfo{Username: user},
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func owner(t *testing.T, resp admission.Response) (string, bool) {
	t.Helper()
	if !resp.Allowed {
		t.Fatalf("denied: %v", resp.Result)
	}
	for _, p := range resp.Patches {
		if p.Path == "/metadata/annotations/c4.avalanche.io~1owner" {
			return p.Value.(string), true
		}
		if p.Path == "/metadata/annotations" {
			m := p.Value.(map[string]any)
			return m[c4v1.AnnotationOwner].(string), true
		}
	}
	return "", false
}

func newOwner(t *testing.T) *Owner {
	return &Owner{Delegates: []string{managerSA}}
}

func TestOwnerFromRequester(t *testing.T) {
	m := newOwner(t)
	resp := m.Handle(context.Background(), ownerRequest(t, "noor", admissionv1.Create, nil))
	if got, ok := owner(t, resp); !ok || got != "noor" {
		t.Fatalf("owner %q (patched %v), want noor; patches %v", got, ok, resp.Patches)
	}
}

func TestOwnerCannotBeClaimed(t *testing.T) {
	m := newOwner(t)
	resp := m.Handle(context.Background(), ownerRequest(t, "ivo", admissionv1.Create,
		map[string]string{c4v1.AnnotationOwner: "noor"}))
	if got, ok := owner(t, resp); !ok || got != "ivo" {
		t.Fatalf("a user claimed another's ownership: owner %q, patches %v", got, resp.Patches)
	}
}

// Only the farm says a unit submitted a job: anyone else's claim is
// dropped, even with the owner already the requester.
func TestOwnerDropsAUnitClaim(t *testing.T) {
	m := newOwner(t)
	resp := m.Handle(context.Background(), ownerRequest(t, "ivo", admissionv1.Create, map[string]string{
		c4v1.AnnotationOwner: "ivo", c4v1.AnnotationSubmittedVia: "api", c4v1.AnnotationSubmittedByUnit: "saltmarsh/lead/0"}))
	dropped := false
	for _, p := range resp.Patches {
		dropped = dropped || (p.Operation == "remove" && p.Path == "/metadata/annotations/c4.avalanche.io~1submitted-by-unit")
	}
	if !resp.Allowed || !dropped {
		t.Fatalf("a unit claim stood: %v %v", resp.Allowed, resp.Patches)
	}
	// the farm's own, made for a unit, stands
	resp = m.Handle(context.Background(), ownerRequest(t, managerSA, admissionv1.Create, map[string]string{
		c4v1.AnnotationOwner: "noor", c4v1.AnnotationSubmittedVia: "hud", c4v1.AnnotationSubmittedByUnit: "saltmarsh/lead/0"}))
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatalf("the farm's own was changed: %v", resp.Patches)
	}
}

func TestOwnerDelegate(t *testing.T) {
	m := newOwner(t)
	resp := m.Handle(context.Background(), ownerRequest(t, managerSA, admissionv1.Create,
		map[string]string{c4v1.AnnotationOwner: "noor", c4v1.AnnotationSubmittedVia: "hud"}))
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatalf("a delegate's owner was changed: %v %v", resp.Allowed, resp.Patches)
	}
	// a delegate that names no owner is stamped like anyone else
	resp = m.Handle(context.Background(), ownerRequest(t, managerSA, admissionv1.Create, nil))
	if got, _ := owner(t, resp); got != managerSA {
		t.Fatalf("owner %q, want the service account", got)
	}
}

func TestOwnerIgnoresUpdates(t *testing.T) {
	m := newOwner(t)
	resp := m.Handle(context.Background(), ownerRequest(t, "ivo", admissionv1.Update, nil))
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatalf("an update was patched: %v", resp.Patches)
	}
}

// A bundle is stamped as a job is: its member jobs carry its owner.
func TestOwnerOfABundle(t *testing.T) {
	b := &c4v1.AgentBundle{TypeMeta: metav1.TypeMeta{APIVersion: c4v1.GroupVersion.String(), Kind: "AgentBundle"},
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "agents", Annotations: map[string]string{c4v1.AnnotationOwner: "ivo"}},
		Spec:       c4v1.AgentBundleSpec{Pools: []string{"team"}}}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	resp := newOwner(t).Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create, UserInfo: authnv1.UserInfo{Username: "lee"}, Object: runtime.RawExtension{Raw: raw}}})
	if got, ok := owner(t, resp); !ok || got != "lee" {
		t.Fatalf("owner %q, patches %v", got, resp.Patches)
	}
	for _, p := range resp.Patches {
		if p.Path != "/metadata/annotations/c4.avalanche.io~1owner" && p.Path != "/metadata/annotations/c4.avalanche.io~1submitted-via" {
			t.Errorf("patched %s", p.Path)
		}
	}
}
