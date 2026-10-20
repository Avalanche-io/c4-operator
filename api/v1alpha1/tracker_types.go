package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Tracker and TrackerLink are experimental (proposals/integrations,
// ADAPTER.md section 3). c4-sync reconciles them; the operator does not
// read them. They are not in the default kustomization.

// SyncAccount is c4-sync's identity (config/sync), the only caller the HUD
// lets submit work for another person, under that person's grant
// (SubmitGrant); SyncDevViewer is the same in development (?as=c4-sync).
const (
	SyncAccount   = "system:serviceaccount:c4-system:c4-sync"
	SyncDevViewer = "c4-sync"
)

// LabelTracker on a WorkJob names the Tracker whose work it serves
// (integrations GAPS 2).
const LabelTracker = "c4.avalanche.io/tracker"

// AnnotationTrackerRef on a WorkJob is the tracker entity it serves:
// <tracker>/<type>/<id> (integrations GAPS 2).
const AnnotationTrackerRef = "c4.avalanche.io/tracker-ref"

// Tracker is one connection to one tracker site, scoped to what one
// project syncs with it.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Tracker struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TrackerSpec   `json:"spec"`
	Status TrackerStatus `json:"status,omitempty"`
}

// TrackerSpec says which tracker, how to reach it, and what crosses.
type TrackerSpec struct {
	// Kind names the adapter: github (built); flow, ftrack, kitsu, ayon,
	// gitlab, jira, linear, mlflow (designed).
	Kind string `json:"kind"`
	// Endpoint is the tracker's API base URL; empty means the adapter's
	// public default (https://api.github.com for github).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// Scope is the adapter's own filter: for github, repo: owner/name.
	// +optional
	Scope map[string]string `json:"scope,omitempty"`
	// Credential is where the tracker credential is. It is a Secret in
	// this namespace, materialized by a credential lease (integrations
	// GAPS 8); c4-sync rereads it on every use, so rotation needs no
	// restart.
	Credential TrackerCredential `json:"credential"`
	// WebhookSecret verifies deliveries the tracker pushes, when it does.
	// +optional
	WebhookSecret *TrackerSecretKey `json:"webhookSecret,omitempty"`
	// Events sets how changes are noticed.
	// +optional
	Events TrackerEvents `json:"events,omitempty"`
	// Rate bounds c4-sync's own requests to the tracker, well under the
	// tracker's published limit.
	// +optional
	Rate TrackerRate `json:"rate,omitempty"`
	// Owner is the party whose tracker this is (ownership MODEL 3.1).
	// +optional
	Owner string `json:"owner,omitempty"`
	// Intake says which tracker entities become C4 work.
	// +optional
	Intake []TrackerIntake `json:"intake,omitempty"`
	// StatusOut says how C4 states are written back.
	// +optional
	StatusOut TrackerStatusOut `json:"statusOut,omitempty"`
	// VerdictsIn maps the tracker's native decisions (for github:
	// APPROVED, CHANGES_REQUESTED, or a check-run action identifier) to
	// what C4 decides. Empty uses the adapter's defaults.
	// +optional
	VerdictsIn map[string]string `json:"verdictsIn,omitempty"`
	// People maps tracker users to C4 people. Roles still come from the
	// studio's groups, never from the tracker.
	// +optional
	People map[string]string `json:"people,omitempty"`
}

// TrackerCredential names the Secret key holding the tracker token.
type TrackerCredential struct {
	// Lease names the CredentialLease that maintains the Secret (designed).
	// +optional
	Lease string `json:"lease,omitempty"`
	// Secret holds the token today.
	Secret TrackerSecretKey `json:"secret"`
}

// TrackerSecretKey is one key of a Secret in the Tracker's namespace.
type TrackerSecretKey struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// TrackerEvents sets how c4-sync notices changes.
type TrackerEvents struct {
	// Mode is webhook (pushed deliveries are hints, with polling under
	// them), eventlog or poll.
	// +kubebuilder:validation:Enum=webhook;eventlog;poll
	// +optional
	Mode string `json:"mode,omitempty"`
	// PollSeconds between polls for changes. Default 60.
	// +optional
	PollSeconds int32 `json:"pollSeconds,omitempty"`
	// ResyncSeconds between full reconciles of every link. Default 900.
	// +optional
	ResyncSeconds int32 `json:"resyncSeconds,omitempty"`
}

// TrackerRate is a token bucket for requests to the tracker.
type TrackerRate struct {
	// PerSecond requests on average. Default 1.
	// +optional
	PerSecond string `json:"perSecond,omitempty"`
	// Burst requests at once. Default 10.
	// +optional
	Burst int32 `json:"burst,omitempty"`
}

// TrackerIntake is one rule for tracker entities that become C4 work.
type TrackerIntake struct {
	// Match selects entities.
	Match TrackerMatch `json:"match"`
	// As is job (the entity becomes a WorkJob) or milestone (it becomes a
	// Milestone, whose due date the jobs it covers take).
	// +kubebuilder:validation:Enum=job;milestone
	As string `json:"as"`
	// Job is the WorkJob spec a matched entity is submitted with. Its
	// deadline and deadline mode are set from Deadline.
	// +optional
	Job *WorkJobSpec `json:"job,omitempty"`
	// Deadline says where the deadline comes from.
	// +optional
	Deadline TrackerDeadline `json:"deadline,omitempty"`
	// Review opens a wait for a person once the work is up for review.
	// +optional
	Review *TrackerReview `json:"review,omitempty"`
	// Covers says which jobs a Milestone made by intake as milestone
	// covers: job labels, each matching a field of the entity (label:
	// field), so the date reaches every job submitted for it, not only
	// jobs c4-sync makes. Flow: {c4.avalanche.io/shot: entity.name}.
	// +optional
	Covers map[string]string `json:"covers,omitempty"`
	// For says whose work the jobs are: the person the farm records as
	// their owner, so what is bound to that person (a lease of their
	// sign-in) serves them. The person must have let this Tracker submit
	// for them (a grant, integrations GAPS 17); without one nothing is
	// submitted, never c4-sync's own job instead. Empty: the jobs are
	// c4-sync's.
	// +optional
	For *TrackerFor `json:"for,omitempty"`
	// Rework is what a rework decision does: inPlace (default: the job's
	// units run again, as in CI and agent work) or nextVersion (a new job
	// follows this one with the note as an input, as v008 follows v007 in
	// media; integrations GAPS 6).
	// +kubebuilder:validation:Enum=inPlace;nextVersion
	// +optional
	Rework string `json:"rework,omitempty"`
}

// TrackerFor names whose work an intake rule's jobs are: one person, or
// the entity's assignee.
type TrackerFor struct {
	// Person is a C4 person.
	// +optional
	Person string `json:"person,omitempty"`
	// Assignee takes the entity's first assignee that People maps to a C4
	// person.
	// +optional
	Assignee bool `json:"assignee,omitempty"`
}

// TrackerMatch selects tracker entities.
type TrackerMatch struct {
	// Type is the entity type (github: Issue).
	Type string `json:"type"`
	// Labels the entity must all carry.
	// +optional
	Labels []string `json:"labels,omitempty"`
	// State is open (default), closed or all.
	// +optional
	State string `json:"state,omitempty"`
	// Fields are equality filters in the tracker's own field names
	// (flow: {sg_status_list: rdy, step.Step.short_name: LGT}).
	// +optional
	Fields map[string]string `json:"fields,omitempty"`
}

// TrackerDeadline says how a tracker due date becomes a C4 deadline.
type TrackerDeadline struct {
	// Field names the due date (github: milestone.due_on). Empty uses the
	// adapter's default.
	// +optional
	Field string `json:"field,omitempty"`
	// DateOnlyAt is the local time ("18:00") a due date that carries no
	// time of day means, in TimeZone. Without it such a due date falls at
	// the project's due time (Project spec.work.dueTime; 18:00 UTC without
	// one), as the farm reads a date sent to it.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	// +optional
	DateOnlyAt string `json:"dateOnlyAt,omitempty"`
	// TimeZone is an IANA zone for DateOnlyAt (default UTC).
	// +optional
	TimeZone string `json:"timeZone,omitempty"`
}

// TrackerReview is the wait a person serves in the tracker.
type TrackerReview struct {
	// Wait is the name of the job's wait unit the tracker's decision
	// serves (a unit of the job's plan with wait.server set).
	Wait string `json:"wait"`
}

// TrackerStatusOut says how C4 states reach the tracker.
type TrackerStatusOut struct {
	// CheckName names the check run (github). Default "C4".
	// +optional
	CheckName string `json:"checkName,omitempty"`
	// CommitStatus also writes a commit status (github).
	// +optional
	CommitStatus bool `json:"commitStatus,omitempty"`
	// FieldSeconds is the least time between writes that only move a
	// continuous value (the ETA). Default 300.
	// +optional
	FieldSeconds int32 `json:"fieldSeconds,omitempty"`
	// States maps C4 summary states (queued, running, needs-person,
	// in-review, rework, held, failed) to the tracker's own status, for
	// trackers whose statuses differ per site (flow: running: ip). A state
	// not listed is not written. done is never written: approval is the
	// tracker's (integrations MAPPING section 2).
	// +optional
	States map[string]string `json:"states,omitempty"`
}

// TrackerStatus is what c4-sync last saw of the connection.
type TrackerStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Cursor is the adapter's own change cursor, opaque to the loop.
	// +optional
	Cursor string `json:"cursor,omitempty"`
	// FarmCursor is the last transition of the farm's log (GET /book?since,
	// integrations GAPS 5) this Tracker has acted on: after an outage it
	// reads what it missed from there.
	// +optional
	FarmCursor int64 `json:"farmCursor,omitempty"`
	// +optional
	LastResync *metav1.Time `json:"lastResync,omitempty"`
	// Links counts the TrackerLinks of this Tracker.
	// +optional
	Links int32 `json:"links,omitempty"`
}

// +kubebuilder:object:root=true

// TrackerList is a list of Trackers.
type TrackerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tracker `json:"items"`
}

// TrackerLink binds one tracker entity to one C4 object.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Tracker",type=string,JSONPath=`.spec.tracker`
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=`.spec.ref.url`
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.spec.target.workJob`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
type TrackerLink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TrackerLinkSpec   `json:"spec"`
	Status TrackerLinkStatus `json:"status,omitempty"`
}

// TrackerLinkSpec names the two ends.
type TrackerLinkSpec struct {
	// Tracker is the Tracker in this namespace.
	Tracker string `json:"tracker"`
	// Ref is the tracker entity.
	Ref TrackerRef `json:"ref"`
	// Target is the C4 object: exactly one of its fields.
	Target TrackerTarget `json:"target"`
	// Intake is the index of the Tracker intake rule that made the link.
	// +optional
	Intake int32 `json:"intake,omitempty"`
}

// TrackerRef names one entity in one tracker.
type TrackerRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// +optional
	URL string `json:"url,omitempty"`
}

// TrackerTarget is the C4 side of a link.
type TrackerTarget struct {
	// +optional
	WorkJob string `json:"workJob,omitempty"`
	// +optional
	Milestone string `json:"milestone,omitempty"`
}

// TrackerLinkStatus is what c4-sync last read and wrote.
type TrackerLinkStatus struct {
	// State is the job's summary state as last written
	// (integrations MAPPING section 2).
	// +optional
	State string `json:"state,omitempty"`
	// ExternalVersion is the entity's version (updatedAt, etag) last read.
	// +optional
	ExternalVersion string `json:"externalVersion,omitempty"`
	// Review is the entity under review for this link, once there is one
	// (github: the pull request).
	// +optional
	Review *TrackerRef `json:"review,omitempty"`
	// Versions are the jobs this link served before its current one,
	// oldest first (a rework that made a next version).
	// +optional
	Versions []string `json:"versions,omitempty"`
	// ReviewHead is the head commit of the entity under review as last
	// read; it is read again only when the tracker reports a change, or
	// at a resync.
	// +optional
	ReviewHead string `json:"reviewHead,omitempty"`
	// Written holds what c4-sync last wrote, by field, so it neither
	// repeats a write nor mistakes its own write for a person's.
	// +optional
	Written map[string]TrackerWritten `json:"written,omitempty"`
	// Decisions are the tracker decisions relayed to C4, newest last.
	// +optional
	Decisions []TrackerDecision `json:"decisions,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TrackerWritten is one field c4-sync wrote.
type TrackerWritten struct {
	// Value is a fingerprint of what was written.
	Value string `json:"value"`
	// Ref is the tracker entity the write made or changed, when it made
	// one (a check run, a comment).
	// +optional
	Ref string      `json:"ref,omitempty"`
	At  metav1.Time `json:"at"`
}

// TrackerDecision is one tracker decision relayed to C4.
type TrackerDecision struct {
	// Source is the tracker entity that carries it (a review, an action).
	Source string `json:"source"`
	// By is the tracker user who decided.
	By string `json:"by"`
	// Do is what C4 was asked to decide.
	Do string `json:"do"`
	// Unit is the wait it served.
	Unit string `json:"unit"`
	// Job is the job whose wait it served.
	// +optional
	Job string `json:"job,omitempty"`
	// Note is the decision's text (a reviewer's note), up to 4 KiB; the
	// whole of it is in the evidence C4 stored.
	// +optional
	Note string      `json:"note,omitempty"`
	At   metav1.Time `json:"at"`
	// Error says why it was not relayed.
	// +optional
	Error string `json:"error,omitempty"`
}

// +kubebuilder:object:root=true

// TrackerLinkList is a list of TrackerLinks.
type TrackerLinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrackerLink `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tracker{}, &TrackerList{}, &TrackerLink{}, &TrackerLinkList{})
}
