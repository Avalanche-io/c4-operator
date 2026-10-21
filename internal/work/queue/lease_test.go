package queue

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A unit whose template mounts a credential lease is released only while
// the lease is active, of a mode the farm uses, and serves the job's
// owner; its pod carries the owner. In a pool outside the lease's target
// it runs without the lease.
func TestLeaseGatesUnitPods(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	pool := "cpu-" + ns
	leased := func(name, owner string) *c4v1.WorkJob {
		j := job(ns, name, "render", pool, map[string]string{"frames": "1-1"}, now.Add(time.Hour))
		j.Annotations = map[string]string{c4v1.AnnotationOwner: owner}
		spec := &j.Spec.Template.Spec
		spec.Volumes = []corev1.Volume{{Name: "lease", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: c4v1.LeaseSecretPrefix + "mine"}}}}
		spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "lease", MountPath: "/etc/c4/lease", ReadOnly: true}}
		if err := k8s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	leased("mine", "noor")
	tick(t, q)
	if n := len(unitPods(t, ns, "mine")); n != 0 {
		t.Fatalf("released %d units with no lease", n)
	}
	l := &c4v1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "mine"}, Spec: c4v1.CredentialLeaseSpec{
		Account: "anthropic-max", Holder: "noor", BoundTo: c4v1.LeaseBinding{Owners: []string{"noor"}},
		Mode: c4v1.LeaseHolderTransfer, Target: c4v1.LeaseTarget{Pools: []string{pool}},
		TTL: metav1.Duration{Duration: 24 * time.Hour}, RenewBefore: metav1.Duration{Duration: 10 * time.Minute}}}
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if n := len(unitPods(t, ns, "mine")); n != 0 {
		t.Fatalf("released %d units on a lease not yet issued", n)
	}
	exp := metav1.NewTime(now.Add(30 * time.Minute))
	l.Status = c4v1.CredentialLeaseStatus{State: c4v1.LeaseActive, ExpiresAt: &exp}
	if err := k8s.Status().Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if n := len(unitPods(t, ns, "mine")); n != 0 {
		t.Fatalf("released %d units on a holder-transfer lease the farm does not use", n)
	}
	q.HolderTransfer = true
	tick(t, q)
	pods := unitPods(t, ns, "mine")
	if len(pods) != 1 || pods[0].Annotations[c4v1.AnnotationOwner] != "noor" || len(pods[0].Spec.Volumes) == 0 {
		t.Fatalf("pods %+v", pods)
	}

	// another owner's job does not get the lease
	leased("theirs", "ivo")
	tick(t, q)
	if n := len(unitPods(t, ns, "theirs")); n != 0 {
		t.Fatalf("released %d units of another owner's job on the lease", n)
	}

	// outside the lease's target, a unit runs without it
	l.Spec.Target.Pools = []string{"elsewhere"}
	if err := k8s.Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	pods = unitPods(t, ns, "theirs")
	if len(pods) != 1 {
		t.Fatalf("outside the lease's target: %d pods", len(pods))
	}
	for _, v := range pods[0].Spec.Volumes {
		if v.Name == "lease" {
			t.Fatal("a pod outside the lease's target mounts it")
		}
	}
	for _, m := range pods[0].Spec.Containers[0].VolumeMounts {
		if m.Name == "lease" {
			t.Fatal("a container outside the lease's target mounts it")
		}
	}
}

// A job a tracker submitted for a person, under the person's grant, gets
// their lease only while that grant serves: not once it is revoked, ended
// or does not cover the pool. Units are not released in the meantime.
func TestLeaseNeedsTheGrantTheJobWasSubmittedUnder(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	q.HolderTransfer = true
	ctx := context.Background()
	pool := "cpu-" + ns
	exp := metav1.NewTime(now.Add(30 * time.Minute))
	l := &c4v1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "mine"}, Spec: c4v1.CredentialLeaseSpec{
		Account: "anthropic-max", Holder: "noor", BoundTo: c4v1.LeaseBinding{Owners: []string{"noor"}},
		Mode: c4v1.LeaseHolderTransfer, Target: c4v1.LeaseTarget{Pools: []string{pool}},
		TTL: metav1.Duration{Duration: 24 * time.Hour}, RenewBefore: metav1.Duration{Duration: 10 * time.Minute}}}
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	l.Status = c4v1.CredentialLeaseStatus{State: c4v1.LeaseActive, ExpiresAt: &exp}
	if err := k8s.Status().Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	j := job(ns, "github-7", "render", pool, map[string]string{"frames": "1-1"}, now.Add(time.Hour))
	j.Annotations = map[string]string{c4v1.AnnotationOwner: "noor", c4v1.AnnotationGrant: "c4grantrecord"}
	j.Labels = map[string]string{c4v1.LabelTracker: "github"}
	spec := &j.Spec.Template.Spec
	spec.Volumes = []corev1.Volume{{Name: "lease", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: c4v1.LeaseSecretPrefix + "mine"}}}}
	spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "lease", MountPath: "/etc/c4/lease", ReadOnly: true}}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	held := func(why string) {
		t.Helper()
		tick(t, q)
		if n := len(unitPods(t, ns, "github-7")); n != 0 {
			t.Fatalf("released %d units %s", n, why)
		}
		if reason := q.grantServes(ctx, j, pool, *now); reason == "" {
			t.Fatalf("%s: the grant serves", why)
		}
	}
	held("with no grant")

	g := &c4v1.SubmitGrant{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: c4v1.SubmitGrantName("github", "noor")},
		Spec: c4v1.SubmitGrantSpec{Person: "noor", Tracker: "github", Until: metav1.NewTime(now.Add(time.Hour)),
			Revoked: &c4v1.GrantRevocation{By: "noor", At: metav1.NewTime(*now)}}}
	if err := k8s.Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	held("under a revoked grant")
	g.Spec.Revoked, g.Spec.Until = nil, metav1.NewTime(now.Add(-time.Minute))
	if err := k8s.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	held("under an ended grant")
	g.Spec.Until, g.Spec.Pools = metav1.NewTime(now.Add(time.Hour)), []string{"gpu"}
	if err := k8s.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	held("in a pool the grant does not cover")
	g.Spec.Pools = []string{pool}
	if err := k8s.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if pods := unitPods(t, ns, "github-7"); len(pods) != 1 || pods[0].Annotations[c4v1.AnnotationOwner] != "noor" {
		t.Fatalf("under the grant: %+v", pods)
	}
}

// A job submitted under a person's grant releases no unit once the grant
// is revoked, though it holds no credential lease: the grant bounds the
// job, not only the lease.
func TestGrantHoldsALeaselessJob(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 4)
	ctx := context.Background()
	pool := "cpu-" + ns
	g := &c4v1.SubmitGrant{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: c4v1.SubmitGrantName("github", "noor")},
		Spec: c4v1.SubmitGrantSpec{Person: "noor", Tracker: "github", Until: metav1.NewTime(now.Add(time.Hour)),
			Revoked: &c4v1.GrantRevocation{By: "noor", At: metav1.NewTime(*now)}}}
	if err := k8s.Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	j := job(ns, "github-8", "render", pool, map[string]string{"frames": "1-1"}, now.Add(time.Hour))
	j.Annotations = map[string]string{c4v1.AnnotationOwner: "noor", c4v1.AnnotationGrant: "c4grantrecord"}
	j.Labels = map[string]string{c4v1.LabelTracker: "github"}
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if n := len(unitPods(t, ns, "github-8")); n != 0 {
		t.Fatalf("released %d units under a revoked grant", n)
	}
	grantCond := func() *metav1.Condition {
		var got c4v1.WorkJob
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "github-8"}, &got); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionGrant)
	}
	if c := grantCond(); c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "noor revoked the grant") {
		t.Errorf("the job says nothing of its revoked grant: %+v", c)
	}
	g.Spec.Revoked = nil
	if err := k8s.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if n := len(unitPods(t, ns, "github-8")); n != 1 {
		t.Fatalf("released %d units under the grant, want 1", n)
	}
	tick(t, q)
	if c := grantCond(); c != nil {
		t.Errorf("the grant serves again, but the job still says %q", c.Message)
	}
}
