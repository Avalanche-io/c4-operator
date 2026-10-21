package controller

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// CredentialLeaseReconciler keeps a lease's state and ends it: when the
// lease lapses (its TTL, or the credential's expiry) or the farm does not
// allow its mode, it deletes the lease's Secret. It never reads the Secret:
// the holder's agent writes it (c4-agent lease), and the work queue mounts
// it in the holder's own unit pods while the lease is active.
type CredentialLeaseReconciler struct {
	client.Client
	// HolderTransfer is the farm's switch for holder-transfer leases
	// (the manager's --holder-transfer: allow, or disallow by default).
	HolderTransfer bool
	// SinglePerson is a single-person farm's installer (the manager's
	// --single-person-farm), every person on it.
	SinglePerson string
	// Now is the clock (tests replace it).
	Now func() time.Time
}

func (r *CredentialLeaseReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile ends a lease that has lapsed or that the farm does not allow,
// and wakes again when the lease ends.
func (r *CredentialLeaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var l c4v1.CredentialLease
	if err := r.Get(ctx, req.NamespacedName, &l); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := r.now()
	state, msg := l.Status.State, l.Status.Message
	end := l.LeaseDeadline()
	switch {
	case l.Spec.Mode == c4v1.LeaseHolderTransfer && !r.HolderTransfer:
		state, msg = c4v1.LeaseRevoked, "this farm does not use holder-transfer leases (the manager's --holder-transfer)"
	case l.Spec.Mode == c4v1.LeaseResident:
		// a resident lease's credential never reaches the farm: nothing here
		// to end
	case !now.Before(end.Time):
		state, msg = c4v1.LeaseExpired, "the lease ended at "+end.UTC().Format(time.RFC3339)
	}
	if state == c4v1.LeaseRevoked || state == c4v1.LeaseExpired {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: l.Namespace, Name: l.SecretName()}}
		if err := r.Delete(ctx, sec); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
	}
	orig := l.DeepCopy()
	l.Status.State, l.Status.Message = state, msg
	holderCondition(&l, r.SinglePerson)
	if !equality.Semantic.DeepEqual(orig.Status, l.Status) {
		if err := r.Status().Patch(ctx, &l, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if state == c4v1.LeaseRevoked || state == c4v1.LeaseExpired || l.Spec.Mode == c4v1.LeaseResident {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: end.Sub(now) + time.Second}, nil
}

// holderCondition labels whether a lease was created by its holder: its
// creator (c4.avalanche.io/owner, stamped at creation by the owner webhook
// and fixed by the c4-workjob-owner policy) is spec.holder. Nothing is
// refused; the label is the record.
//
// On a single-person farm (a local farm), the installer is every person
// there: a lease its holder, the installer, made as the cluster's own user
// (kind's admin, say) was made by its holder. A party list will say so for
// any farm; until then this is the one case it would.
func holderCondition(l *c4v1.CredentialLease, singlePerson string) {
	creator := l.Annotations[c4v1.AnnotationOwner]
	c := metav1.Condition{Type: c4v1.ConditionHolderVerified, ObservedGeneration: l.Generation}
	switch {
	case creator == "":
		c.Status, c.Reason, c.Message = metav1.ConditionUnknown, "NoCreator", "the lease records no creator (made before leases were stamped)"
	case creator == l.Spec.Holder:
		c.Status, c.Reason, c.Message = metav1.ConditionTrue, "CreatedByHolder", "created by its holder, "+creator
	case singlePerson != "" && l.Spec.Holder == singlePerson && !strings.HasPrefix(creator, "system:serviceaccount:"):
		c.Status, c.Reason, c.Message = metav1.ConditionTrue, "CreatedByInstaller", "created by "+creator+", this single-person farm's installer "+singlePerson+", its holder"
	default:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "CreatedByAnother", "created by "+creator+", not its holder "+l.Spec.Holder
	}
	meta.SetStatusCondition(&l.Status.Conditions, c)
}

// SetupWithManager registers the controller.
func (r *CredentialLeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&c4v1.CredentialLease{}).Named("credentiallease").Complete(r)
}
