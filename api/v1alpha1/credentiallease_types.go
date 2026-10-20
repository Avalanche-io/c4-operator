package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A CredentialLease (proposals agent-jobs 09, "Credential leases") says who
// may use which model credential, where and until when, and keeps its
// Secret renewed or revokes it: to a model credential what a cert-manager
// Certificate is to a TLS key.
//
// This version carries the holder-transfer mode: the holder's agent
// (c4-agent lease, on the holder's own machine) takes the holder's current
// sign-in, short-lived token only, and writes it into the lease's Secret
// (LeaseSecretPrefix + the lease's name, key LeaseCredentialKey); the
// controller deletes the Secret when the lease lapses or the farm does not
// allow the mode; the work queue mounts it only in unit pods of jobs the
// holder owns (BoundTo), in the lease's pools, while it is active. It is
// the mode for a holder's own sign-in on the holder's own machines, used
// where the farm turns it on (the manager's --holder-transfer).

// Lease modes.
const (
	LeaseTransfer       = "transfer"
	LeaseResident       = "resident"
	LeaseHolderTransfer = "holder-transfer"
)

// Lease states.
const (
	LeaseActive           = "active"
	LeaseRenewing         = "renewing"
	LeaseWaitingForHolder = "waiting-for-holder"
	LeaseRevoked          = "revoked"
	LeaseExpired          = "expired"
)

// LeaseSecretPrefix names a lease's Secret: the prefix and the lease's name.
// Only the work queue creates pods that mount one (the c4-lease-mounts
// policy).
const LeaseSecretPrefix = "c4-lease-"

// LeaseCredentialKey is the Secret's key: Claude Code's credentials file.
const LeaseCredentialKey = ".credentials.json"

// LeaseBinding is whom a lease serves: only its holder's own work.
type LeaseBinding struct {
	// Owners are the identities (a WorkJob's c4.avalanche.io/owner) whose
	// jobs are the holder's own.
	Owners []string `json:"owners"`
}

// LeaseTarget is where a lease's Secret may be mounted.
type LeaseTarget struct {
	// Pools are the pools whose unit pods may mount it; in other pools a
	// pod runs without it.
	Pools []string `json:"pools"`
}

// CredentialLeaseSpec is the lease.
type CredentialLeaseSpec struct {
	// Account names the model account it serves (the ModelAccount, when
	// there is one), e.g. "anthropic-max".
	Account string `json:"account"`
	// Holder is the person the credential belongs to.
	Holder string `json:"holder"`
	// BoundTo is the work it may serve: only the holder's own.
	BoundTo LeaseBinding `json:"boundTo"`
	// Mode is how the credential reaches the farm.
	// +kubebuilder:validation:Enum=transfer;resident;holder-transfer
	Mode string `json:"mode"`
	// Target is where it may be mounted.
	Target LeaseTarget `json:"target"`
	// TTL is the lease's life from its creation.
	TTL metav1.Duration `json:"ttl"`
	// RenewBefore is how long before the credential's expiry the holder's
	// agent renews it.
	RenewBefore metav1.Duration `json:"renewBefore"`
}

// CredentialLeaseStatus is the lease as it stands.
type CredentialLeaseStatus struct {
	// +optional
	State string `json:"state,omitempty"`
	// ExpiresAt is when the credential or the lease ends, whichever is
	// first.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// RenewedAt is when the holder's agent last wrote the credential.
	// +optional
	RenewedAt *metav1.Time `json:"renewedAt,omitempty"`
	// Message says why, in plain words.
	// +optional
	Message string `json:"message,omitempty"`
}

// CredentialLease is a model credential's lease.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=lease
// +kubebuilder:printcolumn:name="Holder",type=string,JSONPath=`.spec.holder`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.status.expiresAt`
type CredentialLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CredentialLeaseSpec   `json:"spec,omitempty"`
	Status CredentialLeaseStatus `json:"status,omitempty"`
}

// CredentialLeaseList is a list of CredentialLeases.
// +kubebuilder:object:root=true
type CredentialLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CredentialLease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CredentialLease{}, &CredentialLeaseList{})
}

// SecretName is the lease's Secret.
func (l *CredentialLease) SecretName() string { return LeaseSecretPrefix + l.Name }

// LeaseDeadline is when lease l ends: its creation plus its TTL, or the
// credential's expiry, whichever is first.
func (l *CredentialLease) LeaseDeadline() metav1.Time {
	end := l.CreationTimestamp.Add(l.Spec.TTL.Duration)
	if l.Status.ExpiresAt != nil && l.Status.ExpiresAt.Time.Before(end) {
		end = l.Status.ExpiresAt.Time
	}
	return metav1.NewTime(end)
}
