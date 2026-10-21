package queue

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// NoneTool is the farm's tool with no configuration and no egress: a unit
// with it is sealed (DESIGN 10.6, tools).
const NoneTool = "none"

// ensureFarmTools creates the farm's own tools in the operator's namespace
// if they are absent, and never changes one that exists. The manager makes
// them rather than the install manifest, because a manifest that creates a
// Tool in the same apply as the Tool CRD fails on a clean cluster. It
// reports whether they exist; until the CRD is established it is retried.
func (q *Queue) ensureFarmTools(ctx context.Context) bool {
	if q.Namespace == "" {
		return true
	}
	t := &c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: q.Namespace, Name: NoneTool}}
	err := q.Client.Create(ctx, t)
	if err == nil || apierrors.IsAlreadyExists(err) {
		return true
	}
	q.Log.V(1).Info("farm tools not made yet", "error", err.Error())
	return false
}
