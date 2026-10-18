package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelAgentMember names the bundle member a unit pod runs.
const LabelAgentMember = "c4.avalanche.io/agent-member"

// AgentMemberSpec is one member of an agent bundle: a Claude Code session
// that a runner resumes by its session ID.
type AgentMemberSpec struct {
	// Name is the member's name in the bundle and in the runner's
	// registry (c4-agent register --name).
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
	Name string `json:"name"`
	// Kind is resident (lives as long as the bundle) or task (has one job).
	// +kubebuilder:validation:Enum=resident;task
	// +kubebuilder:default=resident
	// +optional
	Kind string `json:"kind,omitempty"`
	// Prompt is given to the member as its next turn when a runner resumes
	// it after a yield. Empty resumes it without a new turn.
	// +optional
	Prompt string `json:"prompt,omitempty"`
}

// AgentBundleSpec declares a bundle of agents that the farm schedules as
// one: its members, how many may run at once, its deadline and where it
// runs (proposals/agent-jobs, section 2).
type AgentBundleSpec struct {
	// Members, in the order the bundle's lead wants them run.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Members []AgentMemberSpec `json:"members"`
	// MaxRunning caps the members running at once (0: no cap).
	// +optional
	MaxRunning int32 `json:"maxRunning,omitempty"`
	// Deadline is the bundle's real-time deadline.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
	// Pools the members may run in. Empty means the agent class's default,
	// pool agent.
	// +optional
	Pools []string `json:"pools,omitempty"`
	// Suspend asks every running member to yield at its next turn boundary
	// and releases none until it is cleared.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// AgentMemberStatus is what the farm knows of one member.
type AgentMemberStatus struct {
	Name string `json:"name"`
	// State is running, yielding (asked to stop at its next turn
	// boundary), parked (saved, not running), queued or failed.
	State string `json:"state"`
	// Node is where the member runs or last ran.
	// +optional
	Node string `json:"node,omitempty"`
	// Epoch is the member's lease number as its runner last resumed it.
	// +optional
	Epoch string `json:"epoch,omitempty"`
	// Checkpoint is where the member resumes next
	// ("<session>#<seq>:<head>").
	// +optional
	Checkpoint string `json:"checkpoint,omitempty"`
	// Attempts counts the times the member was started or resumed.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
}

// AgentBundleStatus reports the bundle.
type AgentBundleStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is Running, Suspending, Suspended or Pending.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Job is the WorkJob that holds the members' units.
	// +optional
	Job string `json:"job,omitempty"`
	// Members, in spec order.
	// +optional
	Members []AgentMemberStatus `json:"members,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AgentBundle is a team of agents the farm schedules, suspends and resumes
// as one.
// +kubebuilder:object:root=true
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 50",message="an AgentBundle name is at most 50 characters (it names its WorkJob)"
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ab
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.maxRunning`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
type AgentBundle struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentBundleSpec   `json:"spec,omitempty"`
	Status AgentBundleStatus `json:"status,omitempty"`
}

// AgentBundleList is a list of AgentBundles.
// +kubebuilder:object:root=true
type AgentBundleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentBundle `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentBundle{}, &AgentBundleList{})
}
