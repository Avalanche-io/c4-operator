// Package controller resolves Materializations: it reads the c4m source,
// applies the include filter, computes the view's content-level ID and
// totals, and checks the store has every object before any pod asks for
// them. Nodes do the fetching; the controller never moves content.
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
	"github.com/Avalanche-io/c4-operator/internal/manifest"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// MaterializationReconciler reconciles Materializations.
type MaterializationReconciler struct {
	client.Client

	// Retry is how soon a Materialization that failed for a reason that
	// may heal (missing content, unreachable store) is checked again.
	Retry time.Duration
	// PreflightWorkers bounds concurrent store existence checks.
	PreflightWorkers int
}

// +kubebuilder:rbac:groups=c4.avalanche.io,resources=materializations,verbs=get;list;watch
// +kubebuilder:rbac:groups=c4.avalanche.io,resources=materializations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile resolves one Materialization.
func (r *MaterializationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var mat c4v1.Materialization
	if err := r.Get(ctx, req.NamespacedName, &mat); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := mat.DeepCopy()
	st := &mat.Status

	data, sourceID, err := LoadSource(ctx, r.Client, &mat)
	if err != nil {
		st.Phase = c4v1.PhasePending
		setCond(st, mat.Generation, c4v1.ConditionResolved, false, "SourceUnavailable", err.Error())
		setCond(st, mat.Generation, c4v1.ConditionReady, false, "SourceUnavailable", err.Error())
		return r.finish(ctx, orig, &mat, r.Retry)
	}

	if st.ObservedGeneration == mat.Generation && st.SourceID == sourceID.String() && st.Phase == c4v1.PhaseReady {
		return ctrl.Result{}, nil
	}

	f, err := manifest.NewFilter(mat.Spec.Include)
	if err != nil {
		return r.fail(ctx, orig, &mat, "InvalidInclude", err, 0)
	}
	src, err := OpenStore(ctx, r.Client, &mat)
	if err != nil {
		return r.fail(ctx, orig, &mat, "StoreUnavailable", err, r.Retry)
	}

	start := time.Now()
	var res *manifest.Resolved
	if data != nil {
		res, err = manifest.FromBytes(data, src, f)
	} else {
		res, err = manifest.FromID(sourceID, src, f)
	}
	if err != nil {
		return r.fail(ctx, orig, &mat, "ResolveFailed", err, r.Retry)
	}

	t := res.View.Totals()
	st.SourceID = sourceID.String()
	st.RootID = res.RootID.String()
	st.ViewID = res.View.ID.String()
	st.Files, st.Bytes = t.Files, t.Bytes
	st.UniqueObjects, st.UniqueBytes = t.UniqueObjects, t.UniqueBytes
	setCond(st, mat.Generation, c4v1.ConditionResolved, true, "Resolved",
		fmt.Sprintf("%d files, %d unique objects", t.Files, t.UniqueObjects))

	missing := r.preflight(src, res.View)
	st.Missing = int64(len(missing))
	st.MissingPaths = nil
	logger.Info("resolved", "view", st.ViewID, "files", t.Files, "uniqueBytes", t.UniqueBytes,
		"missing", len(missing), "took", time.Since(start).String())
	if len(missing) > 0 {
		st.MissingPaths = samplePaths(res.View, missing, 10)
		msg := fmt.Sprintf("%d of %d objects are not in the store, e.g. %s", len(missing), t.UniqueObjects, st.MissingPaths[0])
		st.Phase = c4v1.PhaseFailed
		setCond(st, mat.Generation, c4v1.ConditionContentAvailable, false, "ContentMissing", msg)
		setCond(st, mat.Generation, c4v1.ConditionReady, false, "ContentMissing", msg)
		st.ObservedGeneration = mat.Generation
		return r.finish(ctx, orig, &mat, r.Retry)
	}

	st.Phase = c4v1.PhaseReady
	setCond(st, mat.Generation, c4v1.ConditionContentAvailable, true, "AllPresent", "every object is in the store")
	setCond(st, mat.Generation, c4v1.ConditionReady, true, "Ready", "pods may mount view "+st.ViewID)
	st.ObservedGeneration = mat.Generation
	return r.finish(ctx, orig, &mat, 0)
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

// preflight asks the store about every unique object.
func (r *MaterializationReconciler) preflight(src storeurl.Source, v *manifest.View) map[c4.ID]bool {
	objs := v.Objects()
	workers := r.PreflightWorkers
	if workers <= 0 {
		workers = 16
	}
	var mu sync.Mutex
	missing := map[c4.ID]bool{}
	ch := make(chan c4.ID)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range ch {
				if src.Has(id) {
					continue
				}
				mu.Lock()
				missing[id] = true
				mu.Unlock()
			}
		}()
	}
	for _, o := range objs {
		ch <- o.ID
	}
	close(ch)
	wg.Wait()
	return missing
}

func samplePaths(v *manifest.View, missing map[c4.ID]bool, n int) []string {
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

func setCond(st *c4v1.MaterializationStatus, gen int64, typ string, ok bool, reason, msg string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: typ, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen,
	})
}

// LoadSource returns the c4m bytes for a ConfigMap source (nil for an ID
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

// OpenStore opens the Materialization's store with its credentials.
// Pass an uncached reader: Secrets are read on demand, never cached.
func OpenStore(ctx context.Context, c client.Reader, mat *c4v1.Materialization) (storeurl.Source, error) {
	var creds storeurl.Credentials
	if ref := mat.Spec.StoreSecretRef; ref != nil {
		var sec corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: mat.Namespace, Name: ref.Name}, &sec); err != nil {
			return nil, fmt.Errorf("store secret %s: %w", ref.Name, err)
		}
		creds.AccessKeyID = firstKey(sec.Data, "AWS_ACCESS_KEY_ID", "access-key-id")
		creds.SecretAccessKey = firstKey(sec.Data, "AWS_SECRET_ACCESS_KEY", "secret-access-key")
		creds.Endpoint = firstKey(sec.Data, "C4_S3_ENDPOINT", "endpoint")
	}
	return storeurl.Open(mat.Spec.Store, creds)
}

func firstKey(data map[string][]byte, keys ...string) string {
	for _, k := range keys {
		if v, ok := data[k]; ok {
			return string(bytes.TrimSpace(v))
		}
	}
	return ""
}

// SetupWithManager registers the controller, re-resolving a
// Materialization when the ConfigMap it reads changes.
func (r *MaterializationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&c4v1.Materialization{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.forConfigMap)).
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
