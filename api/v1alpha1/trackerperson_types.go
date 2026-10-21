package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A TrackerPerson is one entry of a Tracker's people map, confirmed: a
// person's own word that a tracker login is them, so a decision that login
// makes in the tracker is relayed as theirs (internal/hud/API.md, People).
// The Tracker's spec.people offers the mapping (the project's word);
// it counts once the person it names accepts it, as themselves, in the
// HUD, which signs the offer and the acceptance as one control record.
// For a party (party:<name>), a member of the party accepts.
//
// People never write one. The HUD is the only writer (the c4-tracker-people
// policy). A revoked or replaced entry is kept, marked, for the trail.
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Tracker",type=string,JSONPath=`.spec.tracker`
// +kubebuilder:printcolumn:name="Login",type=string,JSONPath=`.spec.login`
// +kubebuilder:printcolumn:name="Person",type=string,JSONPath=`.spec.person`
// +kubebuilder:printcolumn:name="Revoked",type=string,JSONPath=`.spec.revoked.at`
type TrackerPerson struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TrackerPersonSpec `json:"spec"`
}

// TrackerPersonSpec is which login of which Tracker is whom, who offered
// it, who accepted it, and whether it still stands.
type TrackerPersonSpec struct {
	// Tracker is the Tracker in this namespace.
	Tracker string `json:"tracker"`
	// Login is the tracker user, as the Tracker's people map keys it.
	Login string `json:"login"`
	// Person is the C4 person, or party:<name>, the login is.
	Person string `json:"person"`
	// Offer is where the mapping came from: the Tracker's spec.people, at
	// its generation.
	Offer TrackerPersonOffer `json:"offer"`
	// Accepted is the person's (or party member's) acceptance, with the
	// control record that signs offer and acceptance together.
	Accepted TrackerPersonAct `json:"accepted"`
	// Revoked is set when the person, a party member or a project admin
	// took it back, or a later acceptance for the same login replaced it.
	// +optional
	Revoked *TrackerPersonAct `json:"revoked,omitempty"`
}

// TrackerPersonOffer is the offer an entry accepted.
type TrackerPersonOffer struct {
	// From is what offered it: spec.people.
	From string `json:"from"`
	// Generation is the Tracker's generation the offer was read at.
	Generation int64 `json:"generation"`
}

// TrackerPersonAct is who did something to an entry, when, and its record.
type TrackerPersonAct struct {
	By string      `json:"by"`
	At metav1.Time `json:"at"`
	// +optional
	Record string `json:"record,omitempty"`
}

// +kubebuilder:object:root=true

// TrackerPersonList contains a list of TrackerPerson.
type TrackerPersonList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrackerPerson `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TrackerPerson{}, &TrackerPersonList{})
}

// TrackerPersonName is the one entry for a Tracker's login and person:
// accepting again renews it in place; another person for the login is
// another entry.
func TrackerPersonName(tracker, login, person string) string {
	h := sha256.Sum256([]byte(login + "\x00" + person))
	return tracker + "-" + hex.EncodeToString(h[:6])
}

// Standing says the entry counts: accepted (every entry is) and not
// revoked or replaced.
func (p *TrackerPerson) Standing() bool { return p.Spec.Revoked == nil }
