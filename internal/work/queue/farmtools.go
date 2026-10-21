package queue

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// NoneTool is the farm's sealed tool (c4v1.NoneTool).
const NoneTool = c4v1.NoneTool

// ensureFarmTools creates the farm's own tools in the operator's namespace
// if they are absent, and never changes one that exists. The manager makes
// them rather than the install manifest, because a manifest that creates a
// Tool in the same apply as the Tool CRD fails on a clean cluster. It
// reports whether they exist; until the CRD is established it is retried.
func (q *Queue) ensureFarmTools(ctx context.Context) bool {
	if q.Namespace == "" {
		return true
	}
	for _, name := range c4v1.FarmTools {
		t := &c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: q.Namespace, Name: name}}
		if err := q.Client.Create(ctx, t); err != nil && !apierrors.IsAlreadyExists(err) {
			q.Log.V(1).Info("farm tools not made yet", "error", err.Error())
			return false
		}
	}
	return true
}
