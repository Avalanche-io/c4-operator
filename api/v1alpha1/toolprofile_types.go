package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelToolProfile on a unit pod names the tool profile it runs with; the
// profile's NetworkPolicy selects the pods by it.
const LabelToolProfile = "c4.avalanche.io/tool-profile"

// ToolProfile is what a unit works with beyond its image: a configuration
// (MCP server definitions, settings, skills, plugins) as a view, and what
// the unit may reach on the network (DESIGN 10.6, tool profiles). A unit
// names one in its reasoning.tools; the queue looks for it in the job's
// namespace, then in the operator's, where the farm's defaults live. The
// view is mounted read-only at $C4_TOOLS, and its C4 ID is in the unit's
// record, so a unit's record says exactly what it ran with. Egress is an
// allow-list: a unit with a profile reaches what the profile lists and
// nothing else, and with none it is sealed.
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="View",type=string,JSONPath=`.spec.view`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ToolProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ToolProfileSpec `json:"spec"`
}

// ToolProfileSpec is a profile's configuration and egress.
type ToolProfileSpec struct {
	// View is the configuration, a view's C4 ID, mounted read-only at
	// /etc/c4/tools ($C4_TOOLS).
	// +optional
	View string `json:"view,omitempty"`
	// Egress is what a unit with the profile may reach. With any rule,
	// the cluster's DNS is reachable too; with none, nothing is.
	// +optional
	Egress []ToolEgress `json:"egress,omitempty"`
}

// ToolEgress is one destination a profile allows: a host, an address
// block or a cluster service, optionally on some ports. Hosts are reached
// through the farm's egress proxy; the others directly.
type ToolEgress struct {
	// Host is a host name, or *.domain for the names under domain.
	// +optional
	Host string `json:"host,omitempty"`
	// CIDR is an address block, as 203.0.113.0/24.
	// +optional
	CIDR string `json:"cidr,omitempty"`
	// Service is a cluster service, <namespace>/<name>: the pods it
	// selects.
	// +optional
	Service string `json:"service,omitempty"`
	// Ports limits the rule to these TCP ports.
	// +optional
	Ports []int32 `json:"ports,omitempty"`
}

// +kubebuilder:object:root=true

// ToolProfileList contains a list of ToolProfile.
type ToolProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ToolProfile{}, &ToolProfileList{})
}
