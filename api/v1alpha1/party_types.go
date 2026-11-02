package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A Party is one of the farm's parties (accountability ORGANIZATION 1): a
// person or an organisation, by the bare name records use (for: {party:
// kestrel}). Nothing else is a party: an agent, a unit, a tracker and a
// deployment never are.
//
// People never write one. The HUD makes and changes it (POST and PATCH
// /api/v1/parties), each change a signed control record booked in the
// farm book, and is its only writer (the c4-parties policy). A party is
// never deleted, so its name is never reused. Who may act for an
// organisation (its members, by name or by the studio's own groups, and
// the purposes each holds) is not kept here: it is the farm book's, by its
// one rule of what is in force (farmbook.InForceAt, ForceMembership).
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Display name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Made",type=string,JSONPath=`.spec.made.at`
type Party struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self.kind == oldSelf.kind",message="a party's kind never changes"
	// +kubebuilder:validation:XValidation:rule="self.made == oldSelf.made",message="who made a party never changes"
	// +kubebuilder:validation:XValidation:rule="!has(self.signIns) || self.kind == 'person'",message="only a person has sign-ins"
	// +kubebuilder:validation:XValidation:rule="!has(self.legalName) || self.kind == 'organisation'",message="only an organisation has a legal name"
	// +kubebuilder:validation:XValidation:rule="!has(self.perPersonViews) || !self.perPersonViews || self.kind == 'organisation'",message="only an organisation sets per-person views"
	Spec PartySpec `json:"spec"`
}

// The kinds of party.
const (
	PartyPerson       = "person"
	PartyOrganisation = "organisation"
)

// PartySpec is who the party is.
type PartySpec struct {
	// Kind is person or organisation, and never changes.
	// +kubebuilder:validation:Enum=person;organisation
	Kind string `json:"kind"`
	// DisplayName is how the HUD and the record reader show the party
	// ("Kestrel Productions").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	DisplayName string `json:"displayName"`
	// LegalName, for an organisation, is the name a contract or an
	// invoice carries, which the farm CA names (ORGANIZATION 4, O5).
	// +kubebuilder:validation:MaxLength=200
	// +optional
	LegalName string `json:"legalName,omitempty"`
	// SignIns, for a person, are the sign-in subjects that are this
	// person: a provider's subject ID, which the people's issuer
	// certifies their device keys on, and the names the HUD signs them in
	// as (a token username, an SSO header's value, a development name).
	// One subject is one person's at most, across every Party.
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	SignIns []PartySignIn `json:"signIns,omitempty"`
	// PerPersonViews, for an organisation, is its policy on per-person
	// spend views (SPEND 5): off, the default, leaves the view-people
	// purpose held but not in effect.
	// +optional
	PerPersonViews bool `json:"perPersonViews,omitempty"`
	// Document is the C4 ID of the party's founding document (ownership
	// MODEL 3.1), once it has made one (ORGANIZATION 7). Not set yet.
	// +optional
	Document string `json:"document,omitempty"`
	// Made is who made the party, when, and the make-party record.
	Made PartyAct `json:"made"`
	// Changed is the party's last change, if any.
	// +optional
	Changed *PartyAct `json:"changed,omitempty"`
}

// PartySignIn is one sign-in subject that is a person.
type PartySignIn struct {
	// Issuer is the sign-in provider's issuer URL, for a provider's
	// subject ID: the people's issuer matches an ID token's iss and sub
	// against it (PEOPLE-KEYS-2 3.1), never the email. Empty for a name
	// the HUD signs the person in as.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Issuer string `json:"issuer,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Subject string `json:"subject"`
}

// PartyAct is one signed change to a party.
type PartyAct struct {
	By string      `json:"by"`
	At metav1.Time `json:"at"`
	// Record is the C4 ID of its signed control record.
	// +optional
	Record string `json:"record,omitempty"`
}

// +kubebuilder:object:root=true

// PartyList contains a list of Party.
type PartyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Party `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Party{}, &PartyList{})
}
