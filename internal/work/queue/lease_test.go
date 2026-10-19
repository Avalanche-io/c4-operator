package queue

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	leased("mine", "joshua")
	tick(t, q)
	if n := len(unitPods(t, ns, "mine")); n != 0 {
		t.Fatalf("released %d units with no lease", n)
	}
	l := &c4v1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "mine"}, Spec: c4v1.CredentialLeaseSpec{
		Account: "anthropic-max", Holder: "joshua", BoundTo: c4v1.LeaseBinding{Owners: []string{"joshua"}},
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
	if len(pods) != 1 || pods[0].Annotations[c4v1.AnnotationOwner] != "joshua" || len(pods[0].Spec.Volumes) == 0 {
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
