package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Isolation modes.
// +kubebuilder:validation:Enum=Strict;Shared
type Isolation string

const (
	// IsolationStrict gives the project its own caches and layers at every
	// tier. Nothing is shared with another project, even identical bytes.
	IsolationStrict Isolation = "Strict"
	// IsolationShared shares node caches and layers with the other Shared
	// projects of the same sharing group. A shared object is served only
	// after the project's own store confirms it holds the ID (proof of
	// possession). Cache timing still reveals that someone in the group
	// used the object.
	IsolationShared Isolation = "Shared"
)

// StoreAccess says which tier reads the project's store.
// +kubebuilder:validation:Enum=Tiered;Direct
type StoreAccess string

const (
	// AccessTiered: node daemons read through peers and the regional tier
	// and hold no store credentials. Only the regional tier and the
	// manager read or write the store.
	AccessTiered StoreAccess = "Tiered"
	// AccessDirect: node daemons read and write the store themselves, with
	// the project's credentials. For small installations, and the
	// baseline the herd test measures against.
	AccessDirect StoreAccess = "Direct"
)

// ProjectStore is where a project's content lives.
type ProjectStore struct {
	// Shards hold the project's objects. An object lives on the shard
	// whose name has the highest rendezvous score for its ID, so no one
	// endpoint carries every read, adding a shard moves about 1/(n+1) of
	// the objects, and moving a shard to a new URL moves none.
	// +kubebuilder:validation:MinItems=1
	Shards []StoreShard `json:"shards"`

	// CredentialsSecretRef names a Secret in the operator's namespace with
	// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. Users of the project's
	// namespaces can neither read nor choose it.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`

	// Access selects tiered or direct reads.
	// +kubebuilder:default=Tiered
	// +optional
	Access StoreAccess `json:"access,omitempty"`

	// VerifyFill rehashes objects as node caches fill. Off by default:
	// bytes are verified once, when they enter the store.
	// +optional
	VerifyFill bool `json:"verifyFill,omitempty"`
}

// StoreShard is one shard of a project's store.
type StoreShard struct {
	// Name is the shard's stable identity; placement depends on it, not on
	// the URL.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// URL in C4_STORE form: s3://bucket/prefix?region=..&endpoint=..,
	// c4d://host:port, or http(s)://host:port.
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`
}

// Scope says which trees a project may materialize.
// +kubebuilder:validation:Enum=Store;Grants
type Scope string

const (
	// ScopeStore allows any description that resolves from the project's
	// own store.
	ScopeStore Scope = "Store"
	// ScopeGrants allows only the trees, and the paths of them, listed in
	// Grants. With no grants it allows nothing, so revoking the last grant
	// never opens the whole store.
	ScopeGrants Scope = "Grants"
)

// Grant allows a project to use part of a tree in its store.
type Grant struct {
	// Tree is the ID of a view, root record, source c4m, or changeset base
	// the project may materialize.
	// +kubebuilder:validation:Pattern=`^c4[1-9A-HJ-NP-Za-km-z]{88}$`
	Tree string `json:"tree"`
	// Paths limits the grant to these path patterns (Materialization
	// include syntax). Empty means the whole tree.
	// +optional
	Paths []string `json:"paths,omitempty"`
}

// ProjectSpec defines a project.
type ProjectSpec struct {
	// Namespaces whose Materializations belong to this project. A
	// namespace belongs to at most one project.
	// +kubebuilder:validation:MinItems=1
	Namespaces []string `json:"namespaces"`

	// +kubebuilder:default=Strict
	// +optional
	Isolation Isolation `json:"isolation,omitempty"`

	// SharingGroup names the group a Shared project shares caches with.
	// +optional
	SharingGroup string `json:"sharingGroup,omitempty"`

	Store ProjectStore `json:"store"`

	// Scope is Store (anything in the project's own store) or Grants
	// (only what Grants lists). Listing grants requires scope Grants.
	// +kubebuilder:default=Store
	// +optional
	Scope Scope `json:"scope,omitempty"`

	// Grants are the trees a Grants-scoped project may materialize, and
	// which paths of them.
	// +optional
	Grants []Grant `json:"grants,omitempty"`

	// Work configures the project's share of the farm (P5).
	// +optional
	Work *ProjectWork `json:"work,omitempty"`
}

// ProjectWork is the project's entitlement and budgets in the work queue.
type ProjectWork struct {
	// Quotas per pool, with Kueue's semantics: nominal slots are
	// guaranteed when the pool is contended; unused nominal slots are lent
	// to others; up to BorrowingLimit slots may be borrowed.
	// +optional
	Quotas []Quota `json:"quotas,omitempty"`
	// BurstBudgetPerDay caps automatic burst spending, in the pools'
	// currency.
	// +optional
	BurstBudgetPerDay string `json:"burstBudgetPerDay,omitempty"`
	// LateCostPerHour prices an hour of lateness, so the scheduler can
	// weigh a burst against the lateness it prevents.
	// +optional
	LateCostPerHour string `json:"lateCostPerHour,omitempty"`
	// FairShareWeight scales the project's share of free capacity.
	// +optional
	FairShareWeight string `json:"fairShareWeight,omitempty"`
	// ShareHistory says whether the project's finished jobs may inform
	// other projects' estimates. "none" (the default) keeps the project's
	// history to itself. "aggregate" adds its calibration scores to a
	// farm-wide pool per class, which every opted-in project consults
	// behind its own, and only once at least three projects contribute.
	// Scores carry no names, inputs or C4 IDs.
	// +kubebuilder:validation:Enum=none;aggregate
	// +optional
	ShareHistory string `json:"shareHistory,omitempty"`
	// DeadlinePolicies override the class's deadline policy for this
	// project's jobs, one per class.
	// +optional
	DeadlinePolicies []DeadlinePolicy `json:"deadlinePolicies,omitempty"`
}

// DeadlinePolicy chooses the deadlines the farm suggests for a class's
// jobs that have none of their own.
type DeadlinePolicy struct {
	// Class is the work class the policy applies to.
	Class string `json:"class"`
	// Kind: achievable (the earliest time the job can finish at Quantile
	// without making another job late, rounded up), sessions (the next of
	// a calendar of review sessions; a job not expected to make it keeps
	// it and is offered the following one), or turnaround (submission
	// plus Turnaround).
	// +kubebuilder:validation:Enum=achievable;sessions;turnaround
	Kind string `json:"kind"`
	// Sessions are times of day, "HH:MM", in TimeZone, on Weekdays
	// ("Mon".."Sun"; every day when empty).
	// +optional
	Sessions []string `json:"sessions,omitempty"`
	// +optional
	Weekdays []string `json:"weekdays,omitempty"`
	// TimeZone is an IANA zone name (default UTC).
	// +optional
	TimeZone string `json:"timeZone,omitempty"`
	// Turnaround is a duration ("24h") for kind turnaround.
	// +optional
	Turnaround string `json:"turnaround,omitempty"`
	// Quantile is the finish percentile a suggestion must meet, "0.8" by
	// default ("0.9" suits deliveries).
	// +optional
	Quantile string `json:"quantile,omitempty"`
	// Round ("15m") and MinLead ("15m") shape achievable deadlines.
	// +optional
	Round string `json:"round,omitempty"`
	// +optional
	MinLead string `json:"minLead,omitempty"`
	// MaxRevisions caps how often a suggestion moves before it settles
	// (default 2).
	// +optional
	MaxRevisions *int32 `json:"maxRevisions,omitempty"`
}

// Quota is a project's entitlement in one pool.
type Quota struct {
	Pool string `json:"pool"`
	// +kubebuilder:validation:Minimum=0
	Nominal int32 `json:"nominal"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	BorrowingLimit *int32 `json:"borrowingLimit,omitempty"`
}

// ProjectStatus reports validation.
type ProjectStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Project is a unit of data isolation: its namespaces, its store and
// credentials, the trees it may use, and its share of the farm.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Isolation",type=string,JSONPath=`.spec.isolation`
// +kubebuilder:printcolumn:name="Access",type=string,JSONPath=`.spec.store.access`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProjectSpec   `json:"spec,omitempty"`
	Status ProjectStatus `json:"status,omitempty"`
}

// ProjectList is a list of Projects.
// +kubebuilder:object:root=true
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}

// Partition is the cache partition a project's objects live in on every
// tier: the project itself in Strict isolation, its sharing group in
// Shared.
func (p *Project) Partition() string {
	if p.Spec.Isolation == IsolationShared && p.Spec.SharingGroup != "" {
		return "g-" + p.Spec.SharingGroup
	}
	return "p-" + p.Name
}

// Restricted reports whether the project may materialize only granted
// trees. Grants listed under scope Store (which validation reports) are
// still enforced: the stricter reading wins.
func (p *Project) Restricted() bool {
	return p.Spec.Scope == ScopeGrants || len(p.Spec.Grants) > 0
}

// AccessMode returns the store access mode with its default.
func (p *Project) AccessMode() StoreAccess {
	if p.Spec.Store.Access == "" {
		return AccessTiered
	}
	return p.Spec.Store.Access
}
