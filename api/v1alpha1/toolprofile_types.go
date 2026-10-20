package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelToolProfile on a unit pod names the tool profile it runs with; the
// profile's NetworkPolicy selects the pods by it.
const LabelToolProfile = "c4.avalanche.io/tool-profile"

// HUDAudience is the audience of the token a unit with a c4 grant holds:
// the HUD accepts it, the Kubernetes API does not.
const HUDAudience = "c4.avalanche.io/hud"

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
	// Sidecars are servers that run beside the unit for its whole run
	// (MCP servers, a headless browser), each told to the unit as
	// $C4_TOOL_<NAME>, http://localhost:<port>. They share the unit's
	// network, so its egress is theirs.
	// +optional
	Sidecars []ToolSidecar `json:"sidecars,omitempty"`
	// C4 is what the unit may do with c4 itself, through the HUD (and so
	// MCP): read (see its project), job (also act on its own job: decide
	// its waits, steer, nudge) or project (also submit jobs in its
	// project). Empty: nothing. The unit holds a token for its pod alone,
	// at $C4_HUD_TOKEN_FILE, and every control it makes is in the book
	// under its name.
	// +kubebuilder:validation:Enum=read;job;project
	// +optional
	C4 string `json:"c4,omitempty"`
}

// ToolSidecar is one server a profile runs beside its units.
type ToolSidecar struct {
	// Name names the container and the unit's $C4_TOOL_<NAME>.
	Name  string `json:"name"`
	Image string `json:"image"`
	// +optional
	Args []string `json:"args,omitempty"`
	// +optional
	Env map[string]string `json:"env,omitempty"`
	// Port is where it listens on localhost.
	Port int32 `json:"port"`
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
