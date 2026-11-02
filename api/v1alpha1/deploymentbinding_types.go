package v1alpha1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A DeploymentBinding says who owns deployments of the farm, and so whom
// the records they sign accrue to: a sign grant from the owner to the
// deployments (DESIGN.md section 9.2). The farm binding, named farm,
// covers every deployment of the farm (to: <farm>/*) and is made once,
// when the installer binds the farm to its owner. A binding for one
// deployment (to: <farm>/node/<name>, a node another party owns; a
// person's own Mac as a host runner) overrides it for that deployment,
// and only narrows what the signer policy lets it sign.
//
// People never write one. The HUD makes it for the installer
// (POST /api/v1/farm/binding) or a farm admin (POST /api/v1/bindings),
// signs it as a control record, and is the only writer (the
// c4-deployment-bindings policy). Its spec never changes: a new owner is
// a new binding that follows it. Ended is set once, when it ends early.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="To",type=string,JSONPath=`.spec.to`
// +kubebuilder:printcolumn:name="Run for",type=string,JSONPath=`.spec.runFor`
// +kubebuilder:printcolumn:name="Since",type=string,JSONPath=`.spec.start`
// +kubebuilder:printcolumn:name="Ended",type=string,JSONPath=`.ended.at`
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.ended) || has(self.ended)",message="a binding's end is never taken back"
type DeploymentBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a binding never changes: a new owner is a new binding that follows it"
	Spec DeploymentBindingSpec `json:"spec"`
	// Ended is the binding's early end, set once by the act that ended it
	// (end-binding, or a binding that follows it), and never changed.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a binding's end never changes"
	// +optional
	Ended *DeploymentBindingEnd `json:"ended,omitempty"`
}

// DeploymentBindingEnd is when and how a binding ended early.
type DeploymentBindingEnd struct {
	// At is when it ended.
	At metav1.Time `json:"at"`
	// By is who ended it, as the HUD recorded them.
	By string `json:"by"`
	// Record is the C4 ID of the signed control record that ended it: an
	// end-binding, or the bind of the binding that follows it.
	Record string `json:"record"`
	// FollowedBy is the record of the binding that follows it, when an
	// owner change ended it.
	// +optional
	FollowedBy string `json:"followedBy,omitempty"`
}

// DeploymentBindingSpec is who owns which deployments, since when, made by
// whom.
type DeploymentBindingSpec struct {
	// Owner is the party the deployments' records accrue to: a person
	// (their sign-in name) or an organization.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Owner string `json:"owner"`
	// To names the deployments bound: <farm>/* for the farm binding, or
	// exactly one deployment: <farm>/node/<name>, <farm>/runner/<name>,
	// <farm>/sync/<tracker>, <farm>/import/<project> or
	// <farm>/component/{hud,queue,controller} (BindingToPattern).
	// +kubebuilder:validation:MaxLength=330
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/(\*|(node|runner|sync|import)/[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?|component/(hud|queue|controller))$`
	To string `json:"to"`
	// RunFor, on a managed farm, is the party the owner runs the farm for
	// ("the provider runs this for the studio"). Empty on a farm its
	// owner runs.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	RunFor string `json:"runFor,omitempty"`
	// OnePerson says the farm is one person's: the installer said so when
	// they bound it (a local farm's bind request, and nowhere else), and
	// the owner then holds every role there. Being bound to someone is
	// not that: a shared farm's binding leaves it unset, and its people
	// keep their own roles and waits. It never goes with RunFor.
	// +optional
	OnePerson bool `json:"onePerson,omitempty"`
	// For lists the record kinds the deployments sign under it.
	// +kubebuilder:validation:MinItems=1
	For []string `json:"for"`
	// Start is when the binding came into force.
	Start metav1.Time `json:"start"`
	// End is when it stops by itself; none is a standing binding, shown
	// as standing until the farm has an annual review.
	// +optional
	End *metav1.Time `json:"end,omitempty"`
	// By is who made it, as the HUD recorded them.
	By string `json:"by"`
	// Record is the C4 ID of the signed control record that made it.
	// +optional
	Record string `json:"record,omitempty"`
	// Signed is that record itself, so the binding carries its own proof
	// on a farm with no project store yet.
	// +optional
	Signed []byte `json:"signed,omitempty"`
	// Follows names the binding this one replaces (an owner change), by
	// its record's C4 ID: the two meet at this one's start.
	// +optional
	Follows string `json:"follows,omitempty"`
}

// BindingToPattern is what a binding's To may be (the CRD's pattern):
// the farm binding's <farm>/*, or one deployment of the farm.
const BindingToPattern = `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/(\*|(node|runner|sync|import)/[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?|component/(hud|queue|controller))$`

// FarmOf is the farm a binding's To is in: what comes before its first
// slash.
func FarmOf(to string) string {
	farm, _, _ := strings.Cut(to, "/")
	return farm
}

// DeploymentOf is the deployment a binding's To names after its farm
// (node/laptop-1), or "*" for the farm binding.
func DeploymentOf(to string) string {
	_, dep, _ := strings.Cut(to, "/")
	return dep
}

// FarmWide reports whether a binding covers every deployment of its farm.
func (s *DeploymentBindingSpec) FarmWide() bool { return DeploymentOf(s.To) == "*" }

// FarmBindingName is the name of the farm binding.
const FarmBindingName = "farm"

// SingleOwner is the person a farm binding makes the farm's one person,
// who holds every role: its owner, where the installer bound the farm as
// one person's (OnePerson); "" on any other farm, a shared one bound to
// someone or a managed one run for someone else, where stand-in is the
// farm's policy.
func (s *DeploymentBindingSpec) SingleOwner() string {
	if !s.OnePerson || s.RunFor != "" {
		return ""
	}
	return s.Owner
}

// +kubebuilder:object:root=true

// DeploymentBindingList contains a list of DeploymentBinding.
type DeploymentBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DeploymentBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DeploymentBinding{}, &DeploymentBindingList{})
}
