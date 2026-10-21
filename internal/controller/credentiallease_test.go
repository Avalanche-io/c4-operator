package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A lease keeps its Secret while it lasts and wakes when it ends; at its
// end, or when the farm does not use its mode, the controller deletes the
// Secret (without reading it) and says why.
func TestCredentialLeaseEnds(t *testing.T) {
	ctx := context.Background()
	ns := fmt.Sprintf("lease-%d", time.Now().UnixNano()%1e9)
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lease := func(name string, expires time.Time) *c4v1.CredentialLease {
		l := &c4v1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: c4v1.CredentialLeaseSpec{
			Account: "anthropic-max", Holder: "noor", BoundTo: c4v1.LeaseBinding{Owners: []string{"noor"}},
			Mode: c4v1.LeaseHolderTransfer, Target: c4v1.LeaseTarget{Pools: []string{"blocks"}},
			TTL: metav1.Duration{Duration: 24 * time.Hour}, RenewBefore: metav1.Duration{Duration: 10 * time.Minute}}}
		if err := k8s.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
		exp := metav1.NewTime(expires)
		l.Status = c4v1.CredentialLeaseStatus{State: c4v1.LeaseActive, ExpiresAt: &exp}
		if err := k8s.Status().Update(ctx, l); err != nil {
			t.Fatal(err)
		}
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: l.SecretName()},
			Data: map[string][]byte{c4v1.LeaseCredentialKey: []byte(`{"claudeAiOauth":{}}`)}}
		if err := k8s.Create(ctx, sec); err != nil {
			t.Fatal(err)
		}
		return l
	}
	// reconcile until the cache has the lease as written, then report
	reconcile := func(r *CredentialLeaseReconciler, l *c4v1.CredentialLease, want string) ctrl.Result {
		t.Helper()
		var res ctrl.Result
		for end := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			var err error
			res, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: l.Name}})
			if err != nil {
				t.Fatal(err)
			}
			var got c4v1.CredentialLease
			if k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: l.Name}, &got) == nil && got.Status.State == want {
				return res
			}
			if time.Now().After(end) {
				t.Fatalf("lease %s: state %q, want %q", l.Name, got.Status.State, want)
			}
		}
	}
	secretGone := func(l *c4v1.CredentialLease) bool {
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: l.SecretName()}, &corev1.Secret{})
			if apierrors.IsNotFound(err) {
				return true
			}
		}
		return false
	}
	r := &CredentialLeaseReconciler{Client: k8s, HolderTransfer: true, Now: func() time.Time { return now }}

	live := lease("live", now.Add(time.Hour))
	if res := reconcile(r, live, c4v1.LeaseActive); res.RequeueAfter < 59*time.Minute || res.RequeueAfter > 61*time.Minute {
		t.Fatalf("a live lease wakes after %s, not at its end", res.RequeueAfter)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: live.SecretName()}, &corev1.Secret{}); err != nil {
		t.Fatalf("a live lease lost its Secret: %v", err)
	}

	lapsed := lease("lapsed", now.Add(-time.Minute))
	reconcile(r, lapsed, c4v1.LeaseExpired)
	if !secretGone(lapsed) {
		t.Fatal("a lapsed lease kept its Secret")
	}

	off := &CredentialLeaseReconciler{Client: k8s, Now: r.Now}
	reconcile(off, live, c4v1.LeaseRevoked)
	if !secretGone(live) {
		t.Fatal("a holder-transfer lease kept its Secret on a farm that does not use the mode")
	}
}

// A bundle's job is its owner's: pools with owners and host runners read
// it there.
func TestJobForBundleCarriesItsOwner(t *testing.T) {
	b := &c4v1.AgentBundle{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "agents",
		Annotations: map[string]string{c4v1.AnnotationOwner: "noor", c4v1.AnnotationSubmittedVia: "api"}},
		Spec: c4v1.AgentBundleSpec{Pools: []string{"team"}, Members: []c4v1.AgentMemberSpec{{Name: "lead"}}}}
	j := JobForBundle(b)
	if j.Annotations[c4v1.AnnotationOwner] != "noor" || j.Annotations[c4v1.AnnotationSubmittedVia] != "api" {
		t.Fatalf("annotations %v", j.Annotations)
	}
	b.Annotations = nil
	if j := JobForBundle(b); j.Annotations[c4v1.AnnotationOwner] != "" {
		t.Fatalf("an owner from nowhere: %v", j.Annotations)
	}
}

// A lease says whether its holder created it: its creator (the owner the
// webhook stamped) against spec.holder. It labels; it refuses nothing.
func TestLeaseHolderVerified(t *testing.T) {
	for _, c := range []struct {
		creator, holder, single string
		status                  metav1.ConditionStatus
		msg                     string
	}{
		{"noor", "noor", "", metav1.ConditionTrue, "created by its holder, noor"},
		{"kubernetes-admin", "noor", "", metav1.ConditionFalse, "created by kubernetes-admin, not its holder noor"},
		{"", "noor", "", metav1.ConditionUnknown, "the lease records no creator (made before leases were stamped)"},
		// a single-person farm's installer is every person on it
		{"kubernetes-admin", "noor", "noor", metav1.ConditionTrue, "created by kubernetes-admin, this single-person farm's installer noor, its holder"},
		{"system:serviceaccount:show-a:importer", "noor", "noor", metav1.ConditionFalse, "created by system:serviceaccount:show-a:importer, not its holder noor"},
		{"kubernetes-admin", "ivo", "noor", metav1.ConditionFalse, "created by kubernetes-admin, not its holder ivo"},
	} {
		l := &c4v1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: "mine", Annotations: map[string]string{}},
			Spec: c4v1.CredentialLeaseSpec{Holder: c.holder}}
		if c.creator != "" {
			l.Annotations[c4v1.AnnotationOwner] = c.creator
		}
		holderCondition(l, c.single)
		got := meta.FindStatusCondition(l.Status.Conditions, c4v1.ConditionHolderVerified)
		if got == nil || got.Status != c.status || got.Message != c.msg {
			t.Errorf("%q made %q's lease: %+v", c.creator, c.holder, got)
		}
	}
}

// A bundle's phase follows its job once the job has ended: every member
// done, the bundle is Done, not Pending.
func TestBundlePhaseFollowsItsJob(t *testing.T) {
	b := &c4v1.AgentBundle{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "agents"}, Spec: c4v1.AgentBundleSpec{Members: []c4v1.AgentMemberSpec{{Name: "lead"}}}}
	for phase, want := range map[string]string{"Succeeded": "Done", "Failed": "Failed", "": "Pending"} {
		j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Name: "team"}}
		j.Status.Phase = phase
		if got := bundleStatus(b, j, nil).Phase; got != want {
			t.Errorf("job %q: bundle %q, want %q", phase, got, want)
		}
	}
}
