package queue

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Credential leases in unit pods. A job's template names a lease by
// mounting its Secret (c4v1.LeaseSecretPrefix + the lease's name). The
// queue releases a unit to a pool in the lease's target only while the
// lease is active and serves the job's owner, so a pod that mounts a lease
// is always the holder's own; in a pool outside the target (a host runner,
// with its own sign-in) the pod runs without it. Only the queue creates
// pods that mount a lease's Secret (the c4-lease-mounts policy).

// leaseVolumes are the names of the template's volumes that mount a
// lease's Secret, by lease name.
func leaseVolumes(spec *corev1.PodSpec) map[string]string {
	out := map[string]string{}
	for _, v := range spec.Volumes {
		if v.Secret == nil {
			continue
		}
		if name, ok := strings.CutPrefix(v.Secret.SecretName, c4v1.LeaseSecretPrefix); ok && name != "" {
			out[v.Name] = name
		}
	}
	return out
}

// leaseReady reports whether a unit of job j may be released to pool now:
// every lease its template mounts that targets the pool is active, not
// ended, of a mode the farm allows, and serves j's owner. why says what it
// waits for.
func (q *Queue) leaseReady(ctx context.Context, j *c4v1.WorkJob, pool string, now time.Time) (bool, string) {
	for _, name := range leaseVolumes(&j.Spec.Template.Spec) {
		var l c4v1.CredentialLease
		if err := q.Client.Get(ctx, types.NamespacedName{Namespace: j.Namespace, Name: name}, &l); err != nil {
			if apierrors.IsNotFound(err) {
				return false, fmt.Sprintf("credential lease %s does not exist", name)
			}
			return false, fmt.Sprintf("credential lease %s: %v", name, err)
		}
		if !slices.Contains(l.Spec.Target.Pools, pool) {
			continue // the pod runs without it there
		}
		owner := j.Annotations[c4v1.AnnotationOwner]
		switch {
		case l.Spec.Mode == c4v1.LeaseHolderTransfer && !q.HolderTransfer:
			return false, fmt.Sprintf("credential lease %s is holder-transfer, which this farm does not use", name)
		case owner == "" || !slices.Contains(l.Spec.BoundTo.Owners, owner):
			return false, fmt.Sprintf("credential lease %s does not serve %q's jobs", name, owner)
		case l.Status.State != c4v1.LeaseActive:
			return false, fmt.Sprintf("credential lease %s is %s", name, orNone(l.Status.State))
		case !now.Before(l.LeaseDeadline().Time):
			return false, fmt.Sprintf("credential lease %s has ended", name)
		}
	}
	return true, ""
}

func orNone(s string) string {
	if s == "" {
		return "not yet issued"
	}
	return s
}

// leaseOutside takes out of pod the lease volumes whose lease does not
// target its pool, and their mounts.
func (q *Queue) leaseOutside(ctx context.Context, pod *corev1.Pod, pool string) {
	drop := map[string]bool{}
	for vol, name := range leaseVolumes(&pod.Spec) {
		var l c4v1.CredentialLease
		if err := q.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: name}, &l); err != nil || !slices.Contains(l.Spec.Target.Pools, pool) {
			drop[vol] = true
		}
	}
	if len(drop) == 0 {
		return
	}
	pod.Spec.Volumes = slices.DeleteFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return drop[v.Name] })
	for _, cs := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range cs {
			cs[i].VolumeMounts = slices.DeleteFunc(cs[i].VolumeMounts, func(m corev1.VolumeMount) bool { return drop[m.Name] })
		}
	}
}
