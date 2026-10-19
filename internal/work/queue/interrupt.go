package queue

import (
	"context"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Interruptions (INTEGRATE 3.1). A unit stopped by the farm, not by its
// own fault, is interrupted: its attempt does not count against its
// retries or against its node, and it runs again. An attempt is
// interrupted when its pod carries the DisruptionTarget condition
// (preemption by the scheduler, an Eviction API call such as a drain, a
// NoExecute taint, the kubelet's node-pressure eviction or graceful
// shutdown, PodGC) or was deleted while it ran (a person, or the queue
// itself preempting it).
//
// Every unit pod carries the FinalizerUnit finalizer, so a pod deleted
// while it runs stays readable until the queue has recorded how it ended,
// with whatever its node daemon captured; the queue removes the finalizer
// as it deletes the recorded pod.

// interruptKept is how many interruptions status lists.
const interruptKept = 16

// deletingSlack is how long past its grace a deleting pod that never
// ended (its node is gone) is waited for before it is let go.
const deletingSlack = 5 * time.Minute

type interruptRec struct {
	Unit   int
	At     time.Time
	Reason string
	Node   string
}

// interruptedBy says why pod p was stopped by the farm, or "" if it was
// not: the DisruptionTarget condition's reason, or Deleted.
func interruptedBy(p *corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue {
			if c.Reason != "" {
				return c.Reason
			}
			return string(corev1.DisruptionTarget)
		}
	}
	if p.DeletionTimestamp != nil {
		return "Deleted"
	}
	return ""
}

// interrupted records that unit idx's attempt in pod p was interrupted:
// the attempt is given back, so it spends no retry.
func (m *jobMem) interrupted(idx int, p *corev1.Pod, reason string, now time.Time) {
	if attemptOf(p) == m.attempts[idx] && m.attempts[idx] > 0 {
		m.attempts[idx]--
	}
	m.interruptions = append(m.interruptions, interruptRec{Unit: idx, At: finishOf(p, now), Reason: reason, Node: p.Spec.NodeName})
	m.interruptCount++
}

// letGo reports whether a deleting pod that has not ended will never end
// on its own: it was never placed on a node, or its node has not ended it
// well past its grace.
func letGo(p *corev1.Pod, now time.Time) bool {
	if p.Spec.NodeName == "" {
		return true
	}
	grace := 30 * time.Second
	if g := p.DeletionGracePeriodSeconds; g != nil {
		grace = time.Duration(*g) * time.Second
	}
	return now.Sub(p.DeletionTimestamp.Time) > grace+deletingSlack
}

// release removes the queue's finalizer from a unit pod.
func (q *Queue) release(ctx context.Context, p *corev1.Pod) error {
	if !slices.Contains(p.Finalizers, c4v1.FinalizerUnit) {
		return nil
	}
	patch := client.MergeFrom(p.DeepCopy())
	p.Finalizers = slices.DeleteFunc(p.Finalizers, func(f string) bool { return f == c4v1.FinalizerUnit })
	if err := q.Client.Patch(ctx, p, patch); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func interruptStatus(m *jobMem) ([]c4v1.WorkInterruption, int32) {
	var out []c4v1.WorkInterruption
	for _, x := range m.interruptions[max(0, len(m.interruptions)-interruptKept):] {
		out = append(out, c4v1.WorkInterruption{Unit: int32(x.Unit), At: metav1.Time{Time: x.At.Truncate(time.Second)}, Reason: x.Reason, Node: x.Node})
	}
	return out, int32(m.interruptCount)
}
