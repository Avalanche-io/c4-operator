package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// ProjectReconciler validates Projects: a namespace belongs to at most
// one project, the credentials Secret exists, the shards parse, and the
// grants are well formed. It reports the result as the Ready condition.
type ProjectReconciler struct {
	client.Client
	Projects *project.Registry
}

// Reconcile validates one Project.
func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p c4v1.Project
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := p.DeepCopy()
	problems := r.validate(ctx, &p)
	cond := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Valid", Message: "project is valid", ObservedGeneration: p.Generation}
	if len(problems) > 0 {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "Invalid", strings.Join(problems, "; ")
	}
	meta.SetStatusCondition(&p.Status.Conditions, cond)
	p.Status.ObservedGeneration = p.Generation
	if err := r.Status().Patch(ctx, &p, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	if len(problems) > 0 {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	return ctrl.Result{}, nil
}

func (r *ProjectReconciler) validate(ctx context.Context, p *c4v1.Project) []string {
	var problems []string
	all, err := r.Projects.List(ctx)
	if err != nil {
		return []string{err.Error()}
	}
	owner := map[string]string{}
	for _, other := range all {
		if other.Name == p.Name {
			continue
		}
		for _, ns := range other.Spec.Namespaces {
			owner[ns] = other.Name
		}
	}
	for _, ns := range p.Spec.Namespaces {
		if o, taken := owner[ns]; taken {
			problems = append(problems, fmt.Sprintf("namespace %s already belongs to project %s", ns, o))
		}
	}
	if p.Spec.Isolation == c4v1.IsolationShared && p.Spec.SharingGroup == "" {
		problems = append(problems, "Shared isolation needs a sharingGroup")
	}
	if _, err := r.Projects.Store(ctx, p); err != nil {
		problems = append(problems, err.Error())
	}
	for _, s := range p.Spec.Store.Shards {
		if strings.Contains(s.URL, "@") {
			problems = append(problems, "shard "+s.Name+" ("+storeurl.Redact(s.URL)+") carries credentials in its URL; use credentialsSecretRef")
		}
	}
	for _, g := range p.Spec.Grants {
		if _, err := view.NewFilter(g.Paths); err != nil {
			problems = append(problems, fmt.Sprintf("grant %s: %v", g.Tree, err))
		}
	}
	return problems
}

// SetupWithManager registers the controller.
func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&c4v1.Project{}).Complete(r)
}
