package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Milestone is a due time owned by whoever plans the work (a producer, a
// tracker of intent), and the jobs it covers: a shot's delivery, a
// release. A job with no deadline of its own takes the earliest due of
// the Milestones in its namespace whose selector matches it (DESIGN
// 10.7); its deadline source reads "milestone". The queue writes which
// jobs it covers and the worst risk among them, so a tracker can write
// one risk back to its task instead of reading every job.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Due",type=string,JSONPath=`.spec.due`
// +kubebuilder:printcolumn:name="Jobs",type=integer,JSONPath=`.status.count`
// +kubebuilder:printcolumn:name="Risk",type=string,JSONPath=`.status.risk`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Milestone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MilestoneSpec   `json:"spec"`
	Status MilestoneStatus `json:"status,omitempty"`
}

// MilestoneSpec is the due time, the jobs it covers, and where it came
// from.
type MilestoneSpec struct {
	// Due is when the covered jobs must be done.
	Due metav1.Time `json:"due"`
	// Selector picks the jobs in the namespace it covers, by label (the
	// shot, the tracker task). An empty selector covers none.
	Selector metav1.LabelSelector `json:"selector"`
	// Source names the tracker entity that owns the date, when one does.
	// +optional
	Source *MilestoneSource `json:"source,omitempty"`
}

// MilestoneSource is the tracker entity a Milestone mirrors.
type MilestoneSource struct {
	// Tracker names the Tracker in the namespace.
	Tracker string `json:"tracker"`
	// Ref is the entity, <type>/<id> in the tracker's own terms.
	Ref string `json:"ref"`
}

// MilestoneStatus is what the queue last saw of the covered jobs.
type MilestoneStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Jobs are the covered real jobs' names (at most 200, sorted), and
	// Count how many it covers; forecast jobs are counted in Forecast.
	// +optional
	Jobs []string `json:"jobs,omitempty"`
	// +optional
	Count int32 `json:"count,omitempty"`
	// Risk is the worst of the covered unfinished jobs' risks: none,
	// atRisk, willMiss or missed (WorkEstimate.Risk).
	// +optional
	Risk string `json:"risk,omitempty"`
	// Done counts the covered jobs that succeeded.
	// +optional
	Done int32 `json:"done,omitempty"`
	// Forecast counts the covered forecast jobs (DESIGN.md 10.12).
	// ForecastRisk is the worst risk once their work comes: theirs, and
	// the covered real jobs' as the farm plans them with it. Risk is the
	// real work's alone, as the farm plans it now.
	// +optional
	Forecast int32 `json:"forecast,omitempty"`
	// +optional
	ForecastRisk string `json:"forecastRisk,omitempty"`
}

// +kubebuilder:object:root=true

// MilestoneList contains a list of Milestone.
type MilestoneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Milestone `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Milestone{}, &MilestoneList{})
}
