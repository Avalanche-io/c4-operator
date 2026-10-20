package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelTool on a unit pod names the tool it runs with; the
// tool's NetworkPolicy selects the pods by it.
const LabelTool = "c4.avalanche.io/tool"

// HUDAudience is the audience of the token a unit with a c4 grant holds:
// the HUD accepts it, the Kubernetes API does not.
const HUDAudience = "c4.avalanche.io/hud"

// Tool is what a unit works with beyond its image: a configuration
// (MCP server definitions, settings, skills, plugins) as a view, and what
// the unit may reach on the network (DESIGN 10.6, tools). A unit
// names one in its reasoning.tools; the queue looks for it in the job's
// namespace, then in the operator's, where the farm's defaults live. The
// view is mounted read-only at $C4_TOOLS, and its C4 ID is in the unit's
// record, so a unit's record says exactly what it ran with. Egress is an
// allow-list: a unit with a tool reaches what the tool lists and
// nothing else, and with none it is sealed.
//
// A Tool whose tree has a tool.yaml at its root is a published tool
// (c4: tool/1, the HUD's POST /tools): its status records each version
// published and its test on the farm, and spec.view is the latest version
// whose test passed. One without is an agent's configuration.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.status.kind`
// +kubebuilder:printcolumn:name="View",type=string,JSONPath=`.spec.view`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Tool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ToolSpec `json:"spec"`
	// +optional
	Status ToolStatus `json:"status,omitempty"`
}

// ToolStatus is what publishing a tool recorded: the version in use, what
// its tool.yaml says of itself, and every version published, newest last.
type ToolStatus struct {
	// Version is the published version in use (v<N>): the latest whose
	// test passed, whose tree is spec.view. Empty until one passes.
	// +optional
	Version string `json:"version,omitempty"`
	// Title, Summary and Kind are the tool.yaml's, at Version.
	// +optional
	Title string `json:"title,omitempty"`
	// +optional
	Summary string `json:"summary,omitempty"`
	// +optional
	Kind string `json:"kind,omitempty"`
	// Image is the unit image at Version, pinned by digest (tool.lock).
	// +optional
	Image string `json:"image,omitempty"`
	// Versions are the versions published, newest last (at most
	// MaxToolVersions), whether their tests passed or not.
	// +optional
	Versions []ToolVersion `json:"versions,omitempty"`
}

// MaxToolVersions bounds ToolStatus.Versions; the oldest go first.
const MaxToolVersions = 32

// ToolVersion is one publish of a tool.
type ToolVersion struct {
	// Version is v<N>, counted per tool in its namespace.
	Version string `json:"version"`
	// View is the published tree: the folder with tool.lock.
	View string `json:"view"`
	// Image is the unit image, pinned by digest.
	// +optional
	Image string `json:"image,omitempty"`
	// Title, Summary and Kind are the version's tool.yaml's.
	// +optional
	Title string `json:"title,omitempty"`
	// +optional
	Summary string `json:"summary,omitempty"`
	// +optional
	Kind string `json:"kind,omitempty"`
	// By is who published it, and At when.
	By string      `json:"by"`
	At metav1.Time `json:"at"`
	// Test is the version's test on the farm.
	Test ToolTest `json:"test"`
}

// ToolTest is a version's test run on the farm: the job running the
// tool's test, and how it went.
type ToolTest struct {
	// Job is the test job, <namespace>/<name>; empty for a tool with no
	// test, which passes at once.
	// +optional
	Job string `json:"job,omitempty"`
	// State is running, ok or failed.
	// +kubebuilder:validation:Enum=running;ok;failed
	State string `json:"state"`
	// Seconds is how long the test took, once it ended.
	// +optional
	Seconds int64 `json:"seconds,omitempty"`
	// Reason says why it failed.
	// +optional
	Reason string `json:"reason,omitempty"`
	// TestedIn is the release whose own tests ran the version, for the
	// tools a release ships (published by c4 farm up from the folders it
	// embeds): the farm ran no test of its own, and State is ok.
	// +optional
	TestedIn string `json:"testedIn,omitempty"`
}

// Tool test states (ToolTest.State).
const (
	ToolTestRunning = "running"
	ToolTestOK      = "ok"
	ToolTestFailed  = "failed"
)

// ToolSpec is a tool's configuration and egress.
type ToolSpec struct {
	// View is the configuration, a view's C4 ID, mounted read-only at
	// /etc/c4/tools ($C4_TOOLS).
	// +optional
	View string `json:"view,omitempty"`
	// Egress is what a unit with the tool may reach. With any rule,
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

// ToolSidecar is one server a tool runs beside its units.
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

// ToolEgress is one destination a tool allows: a host, an address
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

// ToolList contains a list of Tool.
type ToolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tool{}, &ToolList{})
}
