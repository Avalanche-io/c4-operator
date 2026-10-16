package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Annotations a pod carries to request a Materialization.
const (
	// AnnotationMaterialize names the Materialization (same namespace).
	AnnotationMaterialize = "c4.avalanche.io/materialize"
	// AnnotationMountPath is where the tree appears in every container.
	AnnotationMountPath = "c4.avalanche.io/mount-path"
	// AnnotationView is set by the webhook: the view ID the pod mounts.
	AnnotationView = "c4.avalanche.io/view"
)

// ManifestSource says where the c4m file comes from. Exactly one field is set.
// +kubebuilder:validation:XValidation:rule="has(self.configMapKeyRef) != has(self.id)",message="set exactly one of configMapKeyRef or id"
type ManifestSource struct {
	// ConfigMapKeyRef selects a ConfigMap key holding c4m text.
	// +optional
	ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`

	// ID is the C4 ID of a c4m file (or a `c4 id -s` snapshot) in the store.
	// +kubebuilder:validation:Pattern=`^c4[1-9A-HJ-NP-Za-km-z]{88}$`
	// +optional
	ID string `json:"id,omitempty"`
}

// VerifyMode is how a node checks cached content before serving it.
// +kubebuilder:validation:Enum=Stat;Full
type VerifyMode string

const (
	// VerifyStat checks size, mode, and the sentinel mtime; any write to
	// a cached object fails it. Objects are always hashed when fetched.
	VerifyStat VerifyMode = "Stat"
	// VerifyFull rehashes every object each time a pod asks for the view.
	VerifyFull VerifyMode = "Full"
)

// MaterializationSpec declares the tree a pod needs.
type MaterializationSpec struct {
	// Source of the c4m file.
	Source ManifestSource `json:"source"`

	// Store holding the content, in C4_STORE form: s3://bucket/prefix?region=..&endpoint=..,
	// a c4d endpoint (http://, https://, c4d://), or an absolute path on the node.
	// +kubebuilder:validation:MinLength=1
	Store string `json:"store"`

	// StoreSecretRef names a Secret with S3 credentials, under the keys
	// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or access-key-id /
	// secret-access-key, as the c4 container examples use).
	// +optional
	StoreSecretRef *corev1.LocalObjectReference `json:"storeSecretRef,omitempty"`

	// Include limits the tree to these paths (sparse materialization).
	// Each pattern matches a path or any of its ancestors, component by
	// component, with path.Match. Empty means the whole tree.
	// +optional
	Include []string `json:"include,omitempty"`

	// Verify selects how cached content is checked before use.
	// +kubebuilder:default=Stat
	// +optional
	Verify VerifyMode `json:"verify,omitempty"`

	// RetainOnNode pins the materialized tree on a node for this long
	// after the last pod using it ends. Unpinned trees stay cached until
	// the node needs the space.
	// +optional
	RetainOnNode *metav1.Duration `json:"retainOnNode,omitempty"`
}

// Phase values.
const (
	PhasePending = "Pending"
	PhaseReady   = "Ready"
	PhaseFailed  = "Failed"
)

// Condition types.
const (
	ConditionResolved         = "Resolved"
	ConditionContentAvailable = "ContentAvailable"
	ConditionReady            = "Ready"
)

// NodeCacheStatus is one node's account of serving this Materialization.
type NodeCacheStatus struct {
	Node string `json:"node"`
	// ViewID last materialized on this node.
	ViewID string `json:"viewID,omitempty"`
	// Requests is how many pods asked this node for the tree.
	Requests int64 `json:"requests"`
	// Hits counts objects already cached and valid.
	Hits int64 `json:"hits"`
	// Misses counts objects fetched from the store.
	Misses int64 `json:"misses"`
	// Refetched counts cached objects that failed verification and were replaced.
	Refetched int64 `json:"refetched"`
	// BytesFetched is what this node read from the store.
	BytesFetched int64 `json:"bytesFetched"`
	// BytesMaterialized is what pods on this node were given.
	BytesMaterialized int64 `json:"bytesMaterialized"`
	// LastReadyMillis is how long the most recent request took.
	LastReadyMillis int64 `json:"lastReadyMillis"`
	// LastRequest is when the most recent request finished.
	LastRequest metav1.Time `json:"lastRequest,omitempty"`
	// LastError is the most recent failure on this node, cleared on success.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// MaterializationStatus reports the resolved tree and how nodes served it.
type MaterializationStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is Pending, Ready, or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// SourceID identifies the exact source resolved: the c4m text's ID for
	// a ConfigMap, or spec.source.id.
	// +optional
	SourceID string `json:"sourceID,omitempty"`
	// RootID is the C4 ID of the resolved description (the snapshot ID).
	// +optional
	RootID string `json:"rootID,omitempty"`
	// ViewID is the content-level C4 ID of the tree pods receive, after
	// the include filter. Scanning a pod's mount in c4 content mode gives
	// this ID.
	// +optional
	ViewID string `json:"viewID,omitempty"`
	// Files and Bytes in the tree; duplicates count each time.
	Files int64 `json:"files,omitempty"`
	Bytes int64 `json:"bytes,omitempty"`
	// UniqueObjects and UniqueBytes are what a cold node fetches.
	UniqueObjects int64 `json:"uniqueObjects,omitempty"`
	UniqueBytes   int64 `json:"uniqueBytes,omitempty"`
	// Missing counts unique objects the store does not have.
	Missing int64 `json:"missing,omitempty"`
	// MissingPaths samples paths whose content is missing.
	// +optional
	MissingPaths []string `json:"missingPaths,omitempty"`
	// Nodes reports per-node cache behavior.
	// +optional
	// +listType=map
	// +listMapKey=node
	Nodes []NodeCacheStatus `json:"nodes,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Materialization declares a content-addressed tree that pods mount.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mat
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Files",type=integer,JSONPath=`.status.files`
// +kubebuilder:printcolumn:name="Bytes",type=integer,JSONPath=`.status.bytes`
// +kubebuilder:printcolumn:name="Unique",type=integer,JSONPath=`.status.uniqueBytes`
// +kubebuilder:printcolumn:name="View",type=string,JSONPath=`.status.viewID`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Materialization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MaterializationSpec   `json:"spec,omitempty"`
	Status MaterializationStatus `json:"status,omitempty"`
}

// MaterializationList is a list of Materializations.
// +kubebuilder:object:root=true
type MaterializationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Materialization `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Materialization{}, &MaterializationList{})
}
