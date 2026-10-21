package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A refusal names what the person tried in the farm's words, the project,
// and the role that allows it with how to get it, never only the
// Kubernetes verb; and publishing reads the right to create tools from
// RBAC, so an admin whose sign-in carries no groups is not refused.
func TestRefusalsInFarmWords(t *testing.T) {
	s := farm(t)
	allow := map[string]bool{} // "ns verb resource" the API server allows
	k := kfake.NewSimpleClientset()
	k.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sar := a.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		ra := sar.Spec.ResourceAttributes
		sar.Status.Allowed = allow[ra.Namespace+" "+ra.Verb+" "+ra.Resource]
		return true, sar, nil
	})
	s.Kube, s.DevViewers, s.Namespace = k, nil, "c4-system"
	ns := ""
	for n := range s.m.nsProject {
		ns = n
		break
	}
	project := s.m.nsProject[ns]
	v := &api.Viewer{User: "joe"}
	r := httptest.NewRequest("POST", "/", nil)
	err := s.authorize(context.Background(), r, v, ns, "patch", "workjobs", "sh010-comp-v001")
	want := "joe may not change sh010-comp-v001 (hold, boost, steer, requeue or decide) in " + project +
		": that takes the c4-artist role there (c4 farm invite joe --project " + project + " --role artist"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("refusal: %v\nwant: %s", err, want)
	}
	if err := s.authorize(context.Background(), r, v, "", "patch", "nodes", "w1"); err == nil || !strings.Contains(err.Error(), "disable or enable the node w1: that is a farm admin's") {
		t.Fatalf("node refusal: %v", err)
	}

	// Publishing: the role from RBAC.
	if got := s.publishRole(r, v, ns); got.Role != "" || mayPublish(got, false, false) == "" {
		t.Fatalf("joe publishes with no right: %+v", got)
	}
	if !strings.Contains(mayPublish(v, false, false), "c4-project-admin in the project") {
		t.Fatalf("the publish refusal names no way to the role: %s", mayPublish(v, false, false))
	}
	allow[ns+" create tools"] = true
	if got := s.publishRole(r, v, ns); got.Role != "pipeline-td" || mayPublish(got, false, false) != "" || mayPublish(got, true, false) == "" {
		t.Fatalf("one who may create tools in the project is not a TD for it: %+v", got)
	}
	allow["c4-system create tools"] = true
	if got := s.publishRole(r, v, ns); got.Role != "farm-admin" || mayPublish(got, true, false) != "" {
		t.Fatalf("one who may create tools in the operator's namespace is not a farm admin for it: %+v", got)
	}
	if v.Role != "" {
		t.Fatalf("publishRole changed the viewer: %+v", v)
	}
}
