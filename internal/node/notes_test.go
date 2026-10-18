package node

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

func notePod(t *testing.T) (client.Client, *time.Time) {
	t.Helper()
	s := runtime.NewScheme()
	corev1.AddToScheme(s)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "saltmarsh", Name: "sm0140-light-v001-u3-x", UID: "u3"}}
	now := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(pod).Build(), &now
}

func annotation(t *testing.T, c client.Client, key string) string {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "saltmarsh", Name: "sm0140-light-v001-u3-x"}, &p); err != nil {
		t.Fatal(err)
	}
	return p.Annotations[key]
}

func TestPreflightNoteIsThrottled(t *testing.T) {
	c, now := notePod(t)
	d := &Daemon{Kube: c}
	ctx, done := d.preflightNote(context.Background(), "saltmarsh", "sm0140-light-v001-u3-x", "u3")
	// a note of its own, to drive its clock
	note := d.note("saltmarsh", "sm0140-light-v001-u3-x", "u3", c4v1.AnnotationPreflight)
	note.now = func() time.Time { return *now }
	note.set(ctx, Preflight{Step: "fetch", Bytes: 0, Of: 100}, false)
	note.set(ctx, Preflight{Step: "fetch", Bytes: 40, Of: 100}, false) // too soon: skipped
	var pf Preflight
	json.Unmarshal([]byte(annotation(t, c, c4v1.AnnotationPreflight)), &pf)
	if pf.Bytes != 0 || pf.Of != 100 {
		t.Fatalf("a throttled write went through: %+v", pf)
	}
	*now = now.Add(NoteEvery)
	note.set(ctx, Preflight{Step: "fetch", Bytes: 60, Of: 100}, false)
	json.Unmarshal([]byte(annotation(t, c, c4v1.AnnotationPreflight)), &pf)
	if pf.Bytes != 60 {
		t.Fatalf("after the interval: %+v", pf)
	}
	note.set(ctx, Preflight{Step: "fetch", Bytes: 100, Of: 100}, true) // final: always written
	json.Unmarshal([]byte(annotation(t, c, c4v1.AnnotationPreflight)), &pf)
	if pf.Bytes != 100 {
		t.Fatalf("the final write was dropped: %+v", pf)
	}
	done()
	json.Unmarshal([]byte(annotation(t, c, c4v1.AnnotationPreflight)), &pf)
	if pf.Step != "done" {
		t.Fatalf("preflight not ended: %+v", pf)
	}
}

func TestProgressReachesTheNote(t *testing.T) {
	c, _ := notePod(t)
	d := &Daemon{Kube: c}
	ctx, _ := d.preflightNote(context.Background(), "saltmarsh", "sm0140-light-v001-u3-x", "u3")
	progressOf(ctx)(100, 100) // done: final, written at once
	var pf Preflight
	json.Unmarshal([]byte(annotation(t, c, c4v1.AnnotationPreflight)), &pf)
	if pf.Step != "fetch" || pf.Bytes != 100 || pf.Of != 100 {
		t.Fatalf("preflight %+v", pf)
	}
	// without a note, progress is a no-op
	progressOf(context.Background())(1, 2)
}

func TestNoteSkipsAReplacedPod(t *testing.T) {
	c, _ := notePod(t)
	d := &Daemon{Kube: c}
	n := d.note("saltmarsh", "sm0140-light-v001-u3-x", "another-uid", c4v1.AnnotationEgress)
	n.set(context.Background(), Egress{Bytes: 1, Of: 2}, true)
	if got := annotation(t, c, c4v1.AnnotationEgress); got != "" {
		t.Fatalf("wrote to a pod with another UID: %s", got)
	}
	var none *podNote
	none.set(context.Background(), Egress{}, true) // a nil note does nothing
}
