package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelAgentMember names the bundle member a unit pod runs.
const LabelAgentMember = "c4.avalanche.io/agent-member"

// LabelSlotLimit on a Node lowers the slots it offers below its Pool's
// SlotsPerNode (never above): a host runner sets it from its power
// headroom, and 0 takes the node out of service without draining it. (It
// is not c4.avalanche.io/slots, which is the extended resource units
// request.)
const LabelSlotLimit = "c4.avalanche.io/slot-limit"

// AnnotationNodeOwners on a host runner's Node lists, comma-separated, the
// people whose work it runs: each enrolled it, accepting the machine's offer
// with its code, and the HUD wrote them. Its runner cannot set or change
// it; the queue places a person's host work only on a Node that names them.
const AnnotationNodeOwners = "c4.avalanche.io/node-owners"

// AnnotationCapacityReason on a Node says why its slots are lowered, in
// plain words ("power: 38 of 40 W, battery 48% on a 39 W adapter").
const AnnotationCapacityReason = "c4.avalanche.io/capacity-reason"

// AgentClock is a bundle's virtual clock (docs/agents/c4-agent.md,
// "Capacity under a power cap, and the bundle's clock"):
// it runs while the bundle runs and stops while it is suspended, so a
// suspension is invisible to the members' timers. Real deadlines stay real.
type AgentClock struct {
	// Start is when the bundle first ran.
	// +optional
	Start *metav1.Time `json:"start,omitempty"`
	// SuspendedSeconds is the total time the bundle has spent suspended,
	// not counting a suspension still in progress.
	// +optional
	SuspendedSeconds int64 `json:"suspendedSeconds,omitempty"`
	// SuspendedSince is set while the bundle is suspended or held, and
	// SuspendedBy says who stopped it: queue or person.
	// +optional
	SuspendedSince *metav1.Time `json:"suspendedSince,omitempty"`
	// +optional
	SuspendedBy string `json:"suspendedBy,omitempty"`
	// VirtualNow is the bundle's time as of the last status write: Start
	// plus the real time since, less every suspension.
	// +optional
	VirtualNow *metav1.Time `json:"virtualNow,omitempty"`
	// DriftSeconds is how far the bundle's time trails the real clock.
	// +optional
	DriftSeconds int64 `json:"driftSeconds,omitempty"`
	// Suspensions are the most recent suspended intervals, oldest first
	// (at most 16): where the HUD draws a gap in the bundle's time.
	// +optional
	Suspensions []AgentSuspension `json:"suspensions,omitempty"`
}

// AgentSuspension is one interval a bundle spent suspended.
type AgentSuspension struct {
	From metav1.Time `json:"from"`
	To   metav1.Time `json:"to"`
	// By is queue or person.
	By string `json:"by"`
}

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
// runs (docs/agents/c4-agent.md, "Bundles on the farm").
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
	// Phase is Running; Holding or Held (a person suspended the bundle,
	// spec.suspend, and a person resumes it); Suspended (the queue stopped
	// every member that ran at a turn boundary, and resumes them itself);
	// Done or Failed (its job ended so); or Pending.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Job is the WorkJob that holds the members' units.
	// +optional
	Job string `json:"job,omitempty"`
	// Members, in spec order.
	// +optional
	Members []AgentMemberStatus `json:"members,omitempty"`
	// Clock is the bundle's virtual clock.
	// +optional
	Clock AgentClock `json:"clock,omitempty"`
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
// +kubebuilder:printcolumn:name="Drift",type=integer,JSONPath=`.status.clock.driftSeconds`
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
