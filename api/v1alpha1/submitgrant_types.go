package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A SubmitGrant is a person's leave for a Tracker to submit work for them
// (internal/hud/API.md, Grants): jobs the Tracker's sync makes "for"
// the person are the person's own, so what is bound to them (a lease of
// their own sign-in) serves those jobs and nothing else does.
//
// People never write one. They grant and revoke as themselves through the
// HUD (POST and DELETE /api/v1/grants, c4 farm grant), which signs each as
// a control record and is the only writer (the
// c4-submit-grants policy). A revoked grant is kept, marked, for the trail.
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Person",type=string,JSONPath=`.spec.person`
// +kubebuilder:printcolumn:name="Tracker",type=string,JSONPath=`.spec.tracker`
// +kubebuilder:printcolumn:name="Until",type=string,JSONPath=`.spec.until`
// +kubebuilder:printcolumn:name="Revoked",type=string,JSONPath=`.spec.revoked.at`
type SubmitGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SubmitGrantSpec `json:"spec"`
}

// SubmitGrantSpec is who lets which Tracker submit what, until when.
type SubmitGrantSpec struct {
	// Person is the C4 person who gave the grant, and whom the jobs are for.
	Person string `json:"person"`
	// Tracker is the Tracker in this namespace that may submit for them.
	Tracker string `json:"tracker"`
	// Pools limits the jobs to these pools; empty, any of the project's.
	// +optional
	Pools []string `json:"pools,omitempty"`
	// Classes limits the jobs to these job classes; empty, any.
	// +optional
	Classes []string `json:"classes,omitempty"`
	// Until is when the grant ends. Every grant ends.
	Until metav1.Time `json:"until"`
	// Record is the C4 ID of the signed control record of the grant.
	// +optional
	Record string `json:"record,omitempty"`
	// Revoked is set when the person (or a project admin) took it back.
	// +optional
	Revoked *GrantRevocation `json:"revoked,omitempty"`
}

// GrantRevocation is who took a grant back, when, and its record.
type GrantRevocation struct {
	By string      `json:"by"`
	At metav1.Time `json:"at"`
	// +optional
	Record string `json:"record,omitempty"`
}

// +kubebuilder:object:root=true

// SubmitGrantList contains a list of SubmitGrant.
type SubmitGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SubmitGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SubmitGrant{}, &SubmitGrantList{})
}

// AnnotationGrant on a WorkJob a Tracker submitted for a person is the C4
// ID of that person's grant record: the job is theirs under that grant
// Set at creation, never changed.
const AnnotationGrant = "c4.avalanche.io/grant"

// ConditionGrant, False with reason Waiting, on a job submitted under a
// person's grant (or a unit's job under it) says its units wait because
// the grant no longer serves them: the message is the grant's own
// sentence (revoked, ended, or not covering the pool or the class).
// Absent while the grant serves.
const ConditionGrant = "Grant"

// AnnotationGrantTracker on a job a unit submitted, whose own job a
// Tracker submitted under a person's grant, names that Tracker, so the
// queue finds the grant that bounds the child too. The child does not
// carry the Tracker's label: the job is not the Tracker's work, and its
// sync may not relay decisions to it. Set at creation, never changed.
const AnnotationGrantTracker = "c4.avalanche.io/grant-tracker"

// GrantTracker is the Tracker whose grant j was submitted under: its
// label on a job the Tracker made, else the grant-tracker annotation on a
// unit's job under it.
func GrantTracker(j *WorkJob) string {
	if t := j.Labels[LabelTracker]; t != "" {
		return t
	}
	return j.Annotations[AnnotationGrantTracker]
}

// SubmitGrantName is the one grant a person has for a Tracker, so granting
// again renews it in place.
func SubmitGrantName(tracker, person string) string {
	h := sha256.Sum256([]byte(person))
	return tracker + "-" + hex.EncodeToString(h[:5])
}

// Lapsed says why the grant no longer serves at now, or "" while it does.
func (g *SubmitGrant) Lapsed(now time.Time) string {
	switch {
	case g.Spec.Revoked != nil:
		return fmt.Sprintf("%s revoked the grant from %s to tracker %s", g.Spec.Revoked.By, g.Spec.Person, g.Spec.Tracker)
	case !now.Before(g.Spec.Until.Time):
		return fmt.Sprintf("the grant from %s to tracker %s ended at %s", g.Spec.Person, g.Spec.Tracker, g.Spec.Until.UTC().Format(time.RFC3339))
	}
	return ""
}

// Uncovered says what of a job in pools, of class, the grant does not
// cover, or "" when it covers it all.
func (g *SubmitGrant) Uncovered(pools []string, class string) string {
	if len(g.Spec.Classes) > 0 && !slices.Contains(g.Spec.Classes, class) {
		return fmt.Sprintf("the grant from %s does not cover class %s (only %s)", g.Spec.Person, class, strings.Join(g.Spec.Classes, ", "))
	}
	if len(g.Spec.Pools) == 0 {
		return ""
	}
	if len(pools) == 0 {
		return fmt.Sprintf("the grant from %s covers only pools %s, and the job names none", g.Spec.Person, strings.Join(g.Spec.Pools, ", "))
	}
	for _, p := range pools {
		if !slices.Contains(g.Spec.Pools, p) {
			return fmt.Sprintf("the grant from %s does not cover pool %s (only %s)", g.Spec.Person, p, strings.Join(g.Spec.Pools, ", "))
		}
	}
	return ""
}
