package webhook

import (
	"context"
	"encoding/json"
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
// bundles) may set the owner themselves. With the owner it records the
// requesting user's groups (c4.avalanche.io/owner-groups), so a unit's
// submission for the owner is checked against the owner's own rights. The
// c4-workjob-owner admission policy keeps both from changing on either.
// Only a delegate says a job is a lead's (c4.avalanche.io/lead-session):
// anyone else's claim is dropped, and a job that says its lead answers
// its questions (spec.answers.by lead) without one is refused.
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
	delegate := slices.Contains(m.Delegates, user) && ann[c4v1.AnnotationOwner] != ""
	_, lead := ann[c4v1.AnnotationLeadSession]
	if by, _, _ := unstructured.NestedString(obj.Object, "spec", "answers", "by"); by == c4v1.AnswersByLead && (!delegate || !lead) {
		// Only the farm makes a lead's job, so no one else's job has a
		// lead to answer it.
		return admission.Denied(c4v1.NoLeadMessage)
	}
	if delegate {
		return admission.Allowed("owner set by a delegate")
	}
	if ann == nil {
		ann = map[string]string{}
	}
	_, byUnit := ann[c4v1.AnnotationSubmittedByUnit]
	_, asRole := ann[c4v1.AnnotationSubmittedAs]
	groups := ownerGroups(req.UserInfo.Groups)
	if ann[c4v1.AnnotationOwner] == user && ann[c4v1.AnnotationSubmittedVia] == "api" && !byUnit && !asRole && !lead && ann[c4v1.AnnotationOwnerGroups] == groups {
		return admission.Allowed("owner already the requester")
	}
	ann[c4v1.AnnotationOwner] = user
	ann[c4v1.AnnotationOwnerGroups] = groups
	ann[c4v1.AnnotationSubmittedVia] = "api"
	// only the farm says a unit submitted it, under whose grant, as
	// which role, and for which lead
	delete(ann, c4v1.AnnotationSubmittedByUnit)
	delete(ann, c4v1.AnnotationGrantTracker)
	delete(ann, c4v1.AnnotationSubmittedAs)
	delete(ann, c4v1.AnnotationLeadSession)
	obj.SetAnnotations(ann)
	raw, err := obj.MarshalJSON()
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}

// ownerGroups is the annotation value for groups: a JSON list, "[]" for
// none.
func ownerGroups(groups []string) string {
	if groups == nil {
		groups = []string{}
	}
	b, _ := json.Marshal(groups)
	return string(b)
}
