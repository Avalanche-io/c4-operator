package node

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// NoteEvery is the least time between two writes of a pod's preflight or
// egress annotation; the last one of a step is always written.
const NoteEvery = 2 * time.Second

// Preflight is the c4.avalanche.io/preflight annotation's value.
type Preflight struct {
	Step  string `json:"step"`
	Bytes int64  `json:"bytes,omitempty"`
	Of    int64  `json:"of,omitempty"`
}

// Egress is the c4.avalanche.io/egress annotation's value.
type Egress struct {
	Bytes int64 `json:"bytes"`
	Of    int64 `json:"of"`
}

// podNote writes one annotation on one pod, throttled, so the HUD can show
// a unit preparing (fetching its inputs) or done with its output still
// moving. The node daemon's admission policy allows exactly these
// annotations besides output-*.
type podNote struct {
	c        client.Client
	ns, name string
	uid      string
	key      string
	every    time.Duration
	now      func() time.Time

	mu   sync.Mutex
	last time.Time
	sent string
}

func (d *Daemon) note(ns, name, uid, key string) *podNote {
	if d.Kube == nil || name == "" {
		return nil
	}
	return &podNote{c: d.Kube, ns: ns, name: name, uid: uid, key: key, every: NoteEvery, now: time.Now}
}

// set writes value unless the last write was too recent (final always
// writes) or it is unchanged. Errors are dropped: the note is a courtesy
// to the HUD, never a reason to fail a mount or a capture.
func (n *podNote) set(ctx context.Context, value any, final bool) {
	if n == nil {
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	v := string(data)
	n.mu.Lock()
	if v == n.sent || (!final && n.now().Sub(n.last) < n.every) {
		n.mu.Unlock()
		return
	}
	n.last, n.sent = n.now(), v
	n.mu.Unlock()
	var pod corev1.Pod
	if err := n.c.Get(ctx, types.NamespacedName{Namespace: n.ns, Name: n.name}, &pod); err != nil || (n.uid != "" && string(pod.UID) != n.uid) {
		return
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{n.key: v}}})
	if err := n.c.Patch(ctx, &pod, client.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) {
		return
	}
}

type progressKey struct{}

// progress is told how many of a fetch's bytes have arrived.
type progress func(done, of int64)

func withProgress(ctx context.Context, p progress) context.Context {
	return context.WithValue(ctx, progressKey{}, p)
}

func progressOf(ctx context.Context) progress {
	if p, ok := ctx.Value(progressKey{}).(progress); ok {
		return p
	}
	return func(int64, int64) {}
}

// preflightNote reports a pod's inputs arriving, as step fetch.
func (d *Daemon) preflightNote(ctx context.Context, ns, pod, uid string) (context.Context, func()) {
	n := d.note(ns, pod, uid, c4v1.AnnotationPreflight)
	if n == nil {
		return ctx, func() {}
	}
	ctx = withProgress(ctx, func(done, of int64) {
		n.set(ctx, Preflight{Step: "fetch", Bytes: done, Of: of}, done >= of)
	})
	return ctx, func() { n.set(context.WithoutCancel(ctx), Preflight{Step: "done"}, true) }
}
