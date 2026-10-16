// Package controller resolves Materializations and validates Projects.
//
// The controller is the only component that reads descriptions: it
// resolves a Materialization's source through its project's store and
// grants into a chain of views, publishes each view's records to the
// project's store (so nodes can fetch a view top-down from its ID alone),
// checks that the store holds every object of the view, and writes one
// status. Nodes and caches never call the API server for content.
package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/project"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// MaterializationReconciler resolves Materializations.
type MaterializationReconciler struct {
	client.Client
	Projects *project.Registry
	// Retry is how soon a Materialization that failed for a reason that
	// may heal (missing content, unreachable store) is checked again.
	Retry time.Duration
	// PreflightWorkers bounds concurrent existence checks.
	PreflightWorkers int
}

// Reconcile resolves one Materialization.
func (r *MaterializationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var mat c4v1.Materialization
	if err := r.Get(ctx, req.NamespacedName, &mat); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := mat.DeepCopy()
	st := &mat.Status

	p, err := r.Projects.ForNamespace(ctx, mat.Namespace)
	if err != nil {
		st.Project = ""
		return r.fail(ctx, orig, &mat, "NoProject", err, r.Retry)
	}
	st.Project = p.Name

	data, sourceID, err := LoadSource(ctx, r.Client, &mat)
	if err != nil {
		st.Phase = c4v1.PhasePending
		setCond(st, mat.Generation, c4v1.ConditionResolved, false, "SourceUnavailable", err.Error())
		setCond(st, mat.Generation, c4v1.ConditionReady, false, "SourceUnavailable", err.Error())
		return r.finish(ctx, orig, &mat, r.Retry)
	}
	if st.ObservedGeneration == mat.Generation && st.ProjectGeneration == p.Generation &&
		st.SourceID == sourceID.String() && st.Phase == c4v1.PhaseReady {
		return ctrl.Result{}, nil
	}
	st.ProjectGeneration = p.Generation

	include, err := view.NewFilter(mat.Spec.Include)
	if err != nil {
		return r.fail(ctx, orig, &mat, "InvalidInclude", err, 0)
	}
	store, err := r.Projects.Store(ctx, p)
	if err != nil {
		return r.fail(ctx, orig, &mat, "StoreUnavailable", err, r.Retry)
	}
	resolve := func(f view.Filter) (*view.Resolved, error) {
		if data != nil {
			return view.FromBytes(data, store, f)
		}
		return view.FromID(sourceID, store, f)
	}

	start := time.Now()
	// With grants, the grant covering the source (or, for a changeset, its
	// base) limits the paths; a source no grant covers is refused.
	g := grantFor(p, sourceID)
	f, err := intersect(include, g)
	if err != nil {
		return r.fail(ctx, orig, &mat, "NotGranted", err, 0)
	}
	res, err := resolve(f)
	if err != nil {
		return r.fail(ctx, orig, &mat, "ResolveFailed", err, r.Retry)
	}
	if len(p.Spec.Grants) > 0 && g == nil {
		g = grantFor(p, res.Chain[0].ID, res.RootID)
		if g == nil {
			return r.fail(ctx, orig, &mat, "NotGranted",
				fmt.Errorf("project %s has grants, and neither source %s nor its base %s is one of them", p.Name, sourceID, res.Chain[0].ID), 0)
		}
		if f, err = intersect(include, g); err != nil {
			return r.fail(ctx, orig, &mat, "NotGranted", err, 0)
		}
		if res, err = resolve(f); err != nil {
			return r.fail(ctx, orig, &mat, "ResolveFailed", err, r.Retry)
		}
	}

	if err := publishRecords(ctx, store, res.Chain); err != nil {
		return r.fail(ctx, orig, &mat, "StoreUnavailable", fmt.Errorf("publish records: %w", err), r.Retry)
	}

	v := res.View()
	t := v.Totals()
	st.SourceID = sourceID.String()
	st.RootID = res.RootID.String()
	st.ViewID = v.ID.String()
	st.Layers = st.Layers[:0]
	for _, lv := range res.Chain {
		st.Layers = append(st.Layers, lv.ID.String())
	}
	st.Files, st.Dirs, st.Bytes = t.Files, t.Dirs, t.Bytes
	st.UniqueObjects, st.UniqueBytes = t.UniqueObjects, t.UniqueBytes
	setCond(st, mat.Generation, c4v1.ConditionResolved, true, "Resolved",
		fmt.Sprintf("%d files, %d unique objects, %d layers", t.Files, t.UniqueObjects, len(res.Chain)))

	missing, err := r.preflight(ctx, store, v)
	if err != nil {
		return r.fail(ctx, orig, &mat, "StoreUnavailable", err, r.Retry)
	}
	st.Missing = int64(len(missing))
	st.MissingPaths = nil
	logger.Info("resolved", "project", p.Name, "view", st.ViewID, "files", t.Files, "uniqueBytes", t.UniqueBytes,
		"layers", len(res.Chain), "missing", len(missing), "took", time.Since(start).String())
	st.ObservedGeneration = mat.Generation
	if len(missing) > 0 {
		st.MissingPaths = samplePaths(v, missing, 10)
		msg := fmt.Sprintf("%d of %d objects are not in the store, e.g. %s", len(missing), t.UniqueObjects, st.MissingPaths[0])
		st.Phase = c4v1.PhaseFailed
		setCond(st, mat.Generation, c4v1.ConditionContentAvailable, false, "ContentMissing", msg)
		setCond(st, mat.Generation, c4v1.ConditionReady, false, "ContentMissing", msg)
		return r.finish(ctx, orig, &mat, r.Retry)
	}
	st.Phase = c4v1.PhaseReady
	setCond(st, mat.Generation, c4v1.ConditionContentAvailable, true, "AllPresent", "every object is in the store")
	setCond(st, mat.Generation, c4v1.ConditionReady, true, "Ready", "pods may mount view "+st.ViewID)
	return r.finish(ctx, orig, &mat, 0)
}

// grantFor returns the project's grant for any of ids, or nil.
func grantFor(p *c4v1.Project, ids ...c4.ID) *c4v1.Grant {
	for i := range p.Spec.Grants {
		for _, id := range ids {
			if p.Spec.Grants[i].Tree == id.String() {
				return &p.Spec.Grants[i]
			}
		}
	}
	return nil
}

// intersect limits an include filter to a grant's paths. A grant can
// narrow what a Materialization reaches, never widen it.
func intersect(include view.Filter, g *c4v1.Grant) (view.Filter, error) {
	if g == nil || len(g.Paths) == 0 {
		return include, nil
	}
	gf, err := view.NewFilter(g.Paths)
	if err != nil {
		return include, fmt.Errorf("grant %s: %w", g.Tree, err)
	}
	return view.Intersect(include, gf), nil
}

// publishRecords writes every view's records to the store, deepest first,
// root last, skipping a view whose root record is already there (its
// records were written before it, so they are too).
func publishRecords(ctx context.Context, st *storeurl.Sharded, chain []*view.View) error {
	for _, v := range chain {
		if _, err := st.Head(ctx, v.ID); err == nil {
			continue
		} else if !errors.Is(err, storeurl.ErrNotFound) {
			return err
		}
		for _, rec := range v.Records() {
			if _, err := st.PutBytes(ctx, rec.Data); err != nil {
				return err
			}
		}
	}
	return nil
}

// preflight asks the store about every unique object of the view.
func (r *MaterializationReconciler) preflight(ctx context.Context, st *storeurl.Sharded, v *view.View) (map[c4.ID]bool, error) {
	objs := v.Objects()
	workers := r.PreflightWorkers
	if workers <= 0 {
		workers = 32
	}
	var (
		mu      sync.Mutex
		missing = map[c4.ID]bool{}
		first   error
		wg      sync.WaitGroup
		ch      = make(chan c4.ID)
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range ch {
				_, err := st.Head(ctx, id)
				if err == nil {
					continue
				}
				mu.Lock()
				if errors.Is(err, storeurl.ErrNotFound) {
					missing[id] = true
				} else if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}()
	}
	for _, o := range objs {
		ch <- o.ID
	}
	close(ch)
	wg.Wait()
	return missing, first
}

func samplePaths(v *view.View, missing map[c4.ID]bool, n int) []string {
	var out []string
	for _, f := range v.Files {
		if !missing[f.ID] {
			continue
		}
		out = append(out, f.Path+" ("+f.ID.String()+")")
		if len(out) == n {
			break
		}
	}
	sort.Strings(out)
	return out
}

func (r *MaterializationReconciler) fail(ctx context.Context, orig, mat *c4v1.Materialization, reason string, err error, retry time.Duration) (ctrl.Result, error) {
	st := &mat.Status
	st.Phase = c4v1.PhaseFailed
	st.ObservedGeneration = mat.Generation
	setCond(st, mat.Generation, c4v1.ConditionResolved, false, reason, err.Error())
	setCond(st, mat.Generation, c4v1.ConditionReady, false, reason, err.Error())
	return r.finish(ctx, orig, mat, retry)
}

func (r *MaterializationReconciler) finish(ctx context.Context, orig, mat *c4v1.Materialization, retry time.Duration) (ctrl.Result, error) {
	if err := r.Status().Patch(ctx, mat, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: retry}, nil
}

func setCond(st *c4v1.MaterializationStatus, gen int64, typ string, ok bool, reason, msg string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typ, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen})
}

// LoadSource returns the c4m bytes of a ConfigMap source (nil for an ID
// source) and the source's ID.
func LoadSource(ctx context.Context, c client.Reader, mat *c4v1.Materialization) ([]byte, c4.ID, error) {
	s := mat.Spec.Source
	if s.ID != "" {
		id, err := c4.Parse(s.ID)
		return nil, id, err
	}
	ref := s.ConfigMapKeyRef
	if ref == nil {
		return nil, c4.ID{}, errors.New("spec.source sets neither configMapKeyRef nor id")
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: mat.Namespace, Name: ref.Name}, &cm); err != nil {
		return nil, c4.ID{}, fmt.Errorf("configmap %s: %w", ref.Name, err)
	}
	text, ok := cm.Data[ref.Key]
	data := []byte(text)
	if !ok {
		bin, ok := cm.BinaryData[ref.Key]
		if !ok {
			return nil, c4.ID{}, fmt.Errorf("configmap %s has no key %q", ref.Name, ref.Key)
		}
		data = bin
	}
	return data, c4.Identify(bytes.NewReader(data)), nil
}

// SetupWithManager registers the controller. It re-resolves a
// Materialization when its ConfigMap changes, and every Materialization of
// a project's namespaces when the Project changes.
func (r *MaterializationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&c4v1.Materialization{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.forConfigMap)).
		Watches(&c4v1.Project{}, handler.EnqueueRequestsFromMapFunc(r.forProject)).
		Complete(r)
}

func (r *MaterializationReconciler) forConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	var list c4v1.MaterializationList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, m := range list.Items {
		ref := m.Spec.Source.ConfigMapKeyRef
		if ref == nil || ref.Name != obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: m.Namespace, Name: m.Name}})
	}
	return out
}

func (r *MaterializationReconciler) forProject(ctx context.Context, obj client.Object) []reconcile.Request {
	p, ok := obj.(*c4v1.Project)
	if !ok {
		return nil
	}
	var out []reconcile.Request
	for _, ns := range p.Spec.Namespaces {
		var list c4v1.MaterializationList
		if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
			continue
		}
		for _, m := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: m.Namespace, Name: m.Name}})
		}
	}
	return out
}
