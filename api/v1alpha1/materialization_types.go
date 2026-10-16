package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Annotations.
const (
	// AnnotationMaterialize names the Materialization a pod wants. The
	// webhook turns it into a CSI volume; it is a convenience only.
	AnnotationMaterialize = "c4.avalanche.io/materialize"
	// AnnotationMountPath is where the webhook mounts the volume.
	AnnotationMountPath = "c4.avalanche.io/mount-path"
	// AnnotationCapture turns changeset capture off ("false") for a
	// webhook-injected volume.
	AnnotationCapture = "c4.avalanche.io/capture"
	// AnnotationReadOnly makes a webhook-injected volume read-only ("true").
	AnnotationReadOnly = "c4.avalanche.io/read-only"

	// AnnotationOutputTree is written at capture: the output view's ID.
	AnnotationOutputTree = "c4.avalanche.io/output-tree"
	// AnnotationOutputChangeset is the ID of the changeset c4m (parent
	// view ID on the first line, then the changed entries).
	AnnotationOutputChangeset = "c4.avalanche.io/output-changeset"
	// AnnotationOutputChanges summarizes the changeset:
	// "added=N modified=N deleted=N bytes=N".
	AnnotationOutputChanges = "c4.avalanche.io/output-changes"
	// AnnotationOutputProvenance is the C4 ID of the signed task record
	// (a DSSE envelope) the node daemon made for the capture.
	AnnotationOutputProvenance = "c4.avalanche.io/output-provenance"
	// AnnotationAIUsage on a WorkJob (copied to its unit pods) declares the
	// job's use of generative models, as JSON: {"generated": "none" |
	// "assisted" | "generated", "model": ..., "oversight": ...,
	// "consent": [...]}. Task records carry it (DESIGN.md section 9.4).
	AnnotationAIUsage = "c4.avalanche.io/ai-usage"
	// AnnotationOutputPrefix is what the node daemon's admission policy
	// allows it to write on pods.
	AnnotationOutputPrefix = "c4.avalanche.io/output-"
)

// CSI driver name and volume attributes.
const (
	DriverName = "c4.avalanche.io"
	// VolumeAttrMaterialization names the Materialization, in the pod's
	// own namespace.
	VolumeAttrMaterialization = "materialization"
	// VolumeAttrCapture is "true" or "false" (default "true" for writable
	// volumes).
	VolumeAttrCapture = "capture"
	// VolumeAttrClass labels publish metrics with a work class.
	VolumeAttrClass = "class"
)

// Labels.
const (
	// LabelLocality is the default node label that groups nodes for peer
	// fetch.
	LabelLocality = "topology.kubernetes.io/zone"
	// LabelCacheWarm marks a node whose cache holds the views its pool's
	// capacity requests named.
	LabelCacheWarm = "c4.avalanche.io/cache-warm"
)

// ManifestSource says where the description comes from. Exactly one field
// is set.
// +kubebuilder:validation:XValidation:rule="has(self.configMapKeyRef) != has(self.id)",message="set exactly one of configMapKeyRef or id"
type ManifestSource struct {
	// ConfigMapKeyRef selects a ConfigMap key holding c4m text, in the
	// Materialization's namespace.
	// +optional
	ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`

	// ID is the C4 ID of a description in the project's store: a c4m file,
	// a view or snapshot root record, or a changeset (a c4m whose first
	// line names its parent). A changeset resolves to a chain of views that
	// nodes can mount as stacked layers.
	// +kubebuilder:validation:Pattern=`^c4[1-9A-HJ-NP-Za-km-z]{88}$`
	// +optional
	ID string `json:"id,omitempty"`
}

// MaterializationSpec declares the tree a pod needs. The store and its
// credentials come from the Project that owns the namespace.
type MaterializationSpec struct {
	// Source of the description.
	Source ManifestSource `json:"source"`

	// Include limits the tree to these paths (sparse materialization).
	// Each pattern matches a path or any of its ancestors, component by
	// component, with path.Match. Empty means the whole tree. A project's
	// grant paths further limit it.
	// +optional
	Include []string `json:"include,omitempty"`

	// RetainOnNode keeps the view's layers on a node for this long after
	// the last volume using them is unmounted.
	// +optional
	RetainOnNode *metav1.Duration `json:"retainOnNode,omitempty"`
}

// Phases.
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

// MaterializationStatus is written by the controller only, once per
// generation. Per-node behavior is exported as metrics, not here.
type MaterializationStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
	// Project that owns this Materialization's namespace.
	// +optional
	Project string `json:"project,omitempty"`
	// ProjectGeneration is the generation of the Project the status was
	// resolved under; a change to the Project's store or grants resolves
	// again.
	// +optional
	ProjectGeneration int64 `json:"projectGeneration,omitempty"`
	// SourceID identifies the exact description resolved.
	// +optional
	SourceID string `json:"sourceID,omitempty"`
	// RootID is the ID of the top description's root listing as recorded
	// (the snapshot ID).
	// +optional
	RootID string `json:"rootID,omitempty"`
	// ViewID is the ID of the tree pods see.
	// +optional
	ViewID string `json:"viewID,omitempty"`
	// Layers are the view IDs of the source's changeset chain, base first.
	// The last is ViewID. Nodes use them as candidates to stack on.
	// +optional
	Layers []string `json:"layers,omitempty"`
	// +optional
	Files int64 `json:"files,omitempty"`
	// +optional
	Dirs int64 `json:"dirs,omitempty"`
	// +optional
	Bytes int64 `json:"bytes,omitempty"`
	// UniqueObjects and UniqueBytes are what a cold node fetches.
	// +optional
	UniqueObjects int64 `json:"uniqueObjects,omitempty"`
	// +optional
	UniqueBytes int64 `json:"uniqueBytes,omitempty"`
	// Missing counts unique objects the store does not have.
	// +optional
	Missing int64 `json:"missing,omitempty"`
	// +optional
	MissingPaths []string `json:"missingPaths,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Materialization is a tree, by C4 ID, that pods mount through the c4 CSI
// driver.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mat
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.status.project`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Files",type=integer,JSONPath=`.status.files`
// +kubebuilder:printcolumn:name="Bytes",type=integer,JSONPath=`.status.bytes`
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
