package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// WorkJobOwner records who submitted a WorkJob. On create it sets
// c4.avalanche.io/owner to the requesting user, so a job applied with
// kubectl is owned by whoever applied it. Delegates (the manager's own
// service account, which creates jobs for signed-in HUD and MCP users
// after checking their permission) may set the owner themselves.
// The c4-workjob-owner admission policy keeps the owner from changing.
type WorkJobOwner struct {
	Decoder   admission.Decoder
	Delegates []string
}

// Handle sets the owner of one WorkJob being created.
func (m *WorkJobOwner) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("only creates are stamped")
	}
	job := &c4v1.WorkJob{}
	if err := m.Decoder.Decode(req, job); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	user := req.UserInfo.Username
	if slices.Contains(m.Delegates, user) && job.Annotations[c4v1.AnnotationOwner] != "" {
		return admission.Allowed("owner set by a delegate")
	}
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	if job.Annotations[c4v1.AnnotationOwner] == user && job.Annotations[c4v1.AnnotationSubmittedVia] == "api" {
		return admission.Allowed("owner already the requester")
	}
	job.Annotations[c4v1.AnnotationOwner] = user
	job.Annotations[c4v1.AnnotationSubmittedVia] = "api"
	raw, err := json.Marshal(job)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}
