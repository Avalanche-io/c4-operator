package webhook

import (
	"context"
	"net/http"
	"slices"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Owner records who created a WorkJob or an AgentBundle. On create it sets
// c4.avalanche.io/owner to the requesting user, so a job applied with
// kubectl is owned by whoever applied it, and a bundle's member jobs (which
// the manager makes from it) are owned by whoever applied the bundle.
// Delegates (the manager's own service account, which creates jobs for
// signed-in HUD and MCP users after checking their permission, and for
// bundles) may set the owner themselves. The c4-workjob-owner admission
// policy keeps the owner from changing on either.
type Owner struct {
	Delegates []string
}

// Handle sets the owner of one object being created.
func (m *Owner) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("only creates are stamped")
	}
	var obj unstructured.Unstructured
	if err := obj.UnmarshalJSON(req.Object.Raw); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	ann := obj.GetAnnotations()
	user := req.UserInfo.Username
	if slices.Contains(m.Delegates, user) && ann[c4v1.AnnotationOwner] != "" {
		return admission.Allowed("owner set by a delegate")
	}
	if ann == nil {
		ann = map[string]string{}
	}
	_, byUnit := ann[c4v1.AnnotationSubmittedByUnit]
	if ann[c4v1.AnnotationOwner] == user && ann[c4v1.AnnotationSubmittedVia] == "api" && !byUnit {
		return admission.Allowed("owner already the requester")
	}
	ann[c4v1.AnnotationOwner] = user
	ann[c4v1.AnnotationSubmittedVia] = "api"
	// only the farm says a unit submitted it
	delete(ann, c4v1.AnnotationSubmittedByUnit)
	obj.SetAnnotations(ann)
	raw, err := obj.MarshalJSON()
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}
