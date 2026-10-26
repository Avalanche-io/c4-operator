package localfarm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
)

var (
	projectKind = schema.GroupVersionKind{Group: "c4.avalanche.io", Version: "v1alpha1", Kind: "Project"}
	workJobKind = schema.GroupVersionKind{Group: "c4.avalanche.io", Version: "v1alpha1", Kind: "WorkJob"}
)

// Prepared is what Prepare did to the local farm.
type Prepared struct {
	Projects []string // every project on the local farm a scenario knows
	Added    []string // the ones it added
	Removed  int      // jobs left from an earlier load, deleted
}

// OnFarm is sc with each node its jobs fail on named as the local farm
// has it: a scenario names a worker as kind does after the cluster's name
// (worker2), and the farm's node has the cluster's name before it
// (c4-local-worker2).
func OnFarm(sc farmcli.Scenario) farmcli.Scenario {
	jobs := make([]farmcli.ScenarioJob, len(sc.Jobs))
	for i, j := range sc.Jobs {
		if j.Fail != nil {
			f := *j.Fail
			f.Node = Name + "-" + f.Node
			j.Fail = &f
		}
		jobs[i] = j
	}
	sc.Jobs = jobs
	return sc
}

// Prepare readies the local farm for a scenario: the projects it adds,
// each project's burst budget as the scenario sets it (the local farm's
// otherwise), and none of any scenario's jobs from an earlier load, so
// scenarios never stack. all is every scenario.
func Prepare(ctx context.Context, sc farmcli.Scenario, all []farmcli.Scenario) (Prepared, error) {
	var out Prepared
	c, err := kubeClient(Name)
	if err != nil {
		return out, err
	}
	for _, j := range sc.Jobs {
		if j.Fail == nil {
			continue
		}
		var n corev1.Node
		if err := c.Get(ctx, types.NamespacedName{Name: j.Fail.Node}, &n); err != nil {
			return out, fmt.Errorf("%s fails on node %s, which the local farm %s does not have: %w", j.Name, j.Fail.Node, Name, err)
		}
	}

	// the projects any scenario adds, with the local farm's own first
	specs := []ProjectSpec{{Name: Project}}
	known := map[string]ProjectSpec{Project: specs[0]}
	for _, s := range all {
		for _, p := range s.Projects {
			if _, ok := known[p.Name]; !ok {
				ps := ProjectSpec{Name: p.Name, Title: p.Title, BurstBudgetPerDay: p.BurstBudgetPerDay, LateCostPerHour: p.LateCostPerHour}
				known[p.Name] = ps
				specs = append(specs, ps)
			}
		}
	}
	for _, p := range sc.Projects {
		have, err := exists(ctx, c, projectKind, "", p.Name)
		if err != nil {
			return out, err
		}
		if have {
			continue
		}
		if err := applyAll(ctx, c, YAML(ProjectObjects(known[p.Name]))); err != nil {
			return out, fmt.Errorf("project %s: %w", p.Name, err)
		}
		out.Added = append(out.Added, p.Name)
	}

	for _, ps := range specs {
		have, err := exists(ctx, c, projectKind, "", ps.Name)
		if err != nil {
			return out, err
		}
		if !have {
			continue
		}
		out.Projects = append(out.Projects, ps.Name)
		budget := sc.Budgets[ps.Name]
		if budget == "" {
			budget = ps.BurstBudgetPerDay
		}
		if budget == "" {
			budget = "20"
		}
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"work": map[string]any{"burstBudgetPerDay": budget}}})
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(projectKind)
		u.SetName(ps.Name)
		if err := c.Patch(ctx, u, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return out, fmt.Errorf("project %s's burst budget: %w", ps.Name, err)
		}
	}

	// every scenario's jobs, gone before the new ones are submitted
	var gone []*unstructured.Unstructured
	for _, s := range all {
		for _, j := range s.Jobs {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(workJobKind)
			u.SetNamespace(j.Project)
			u.SetName(j.Name)
			err := c.Delete(ctx, u, client.PropagationPolicy("Background"))
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return out, fmt.Errorf("removing %s/%s: %w", j.Project, j.Name, err)
			}
			gone = append(gone, u)
		}
	}
	out.Removed = len(gone)
	if err := poll(ctx, 2*time.Minute, "the earlier scenario's jobs' removal", func() (bool, error) {
		for _, u := range gone {
			have, err := exists(ctx, c, workJobKind, u.GetNamespace(), u.GetName())
			if err != nil || have {
				return false, err
			}
		}
		return true, nil
	}); err != nil {
		return out, err
	}

	for _, p := range out.Added {
		if err := poll(ctx, 2*time.Minute, "project "+p, func() (bool, error) { return projectReady(ctx, c, p) }); err != nil {
			return out, err
		}
	}
	return out, nil
}

func exists(ctx context.Context, c client.Client, kind schema.GroupVersionKind, ns, name string) (bool, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(kind)
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, u)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func projectReady(ctx context.Context, c client.Client, name string) (bool, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(projectKind)
	if err := c.Get(ctx, types.NamespacedName{Name: name}, u); err != nil {
		return false, nil
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, x := range conds {
		m, _ := x.(map[string]any)
		if m["type"] == "Ready" {
			return m["status"] == "True", nil
		}
	}
	return false, nil
}

// FlowPlate makes view the plate the stand-in Flow's jobs render into
// (SandboxFlowPlate, in Saltmarsh); farmcli.ErrNoSandbox on a farm
// without the sandbox.
func FlowPlate(ctx context.Context, view string) error {
	c, err := kubeClient(Name)
	if err != nil {
		return err
	}
	if !HasTracker(ctx) {
		return farmcli.ErrNoSandbox
	}
	return applyAll(ctx, c, YAML([]obj{{"apiVersion": "c4.avalanche.io/v1alpha1", "kind": "Materialization",
		"metadata": meta(SandboxFlowPlate, "namespace", Project), "spec": obj{"source": obj{"id": view}}}}))
}
