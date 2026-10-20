package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

func TestSafepointRelay(t *testing.T) {
	c, _ := notePod(t)
	root, pods := t.TempDir(), t.TempDir()
	d := &Daemon{Kube: c, Cfg: Config{Root: root, PodsDir: pods}}
	v := &Volume{ID: "vol-u3", Partition: "p0", Namespace: "saltmarsh", Pod: "sm0140-light-v001-u3-x", PodUID: "u3", Capture: true}
	os.MkdirAll(d.volDir(), 0o755)
	if err := d.saveVolume(v); err != nil {
		t.Fatal(err)
	}
	upper, _ := d.volumeDirs(v.Partition, v.ID)
	os.MkdirAll(filepath.Join(upper, ".c4"), 0o755)
	os.WriteFile(filepath.Join(upper, ".c4", "safepoint"), []byte("seq 3\nat 2026-10-22T14:00:00Z\ncheckpoint turn-3\n"), 0o644)
	control := filepath.Join(pods, "u3", "volumes", "kubernetes.io~empty-dir", ControlVolume)
	os.MkdirAll(control, 0o755)

	relays := map[string]*spRelay{}
	ctx := context.Background()
	d.safepointsOnce(ctx, relays)
	var note SafepointNote
	if err := json.Unmarshal([]byte(annotation(t, c, c4v1.AnnotationSafepoint)), &note); err != nil || note.Seq != 3 || note.Checkpoint != "turn-3" {
		t.Fatalf("safe point not reported: %+v %v", note, err)
	}
	decision := filepath.Join(control, "safepoint.decision")
	if _, err := os.Stat(decision); err == nil {
		t.Fatal("a decision before the queue answered")
	}

	// An answer to an older safe point is not passed on.
	answer := func(seq int, do string) {
		var p corev1.Pod
		c.Get(ctx, types.NamespacedName{Namespace: "saltmarsh", Name: "sm0140-light-v001-u3-x"}, &p)
		data, _ := json.Marshal(SafepointDecision{Seq: seq, Do: do, Why: "capacity returned"})
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{c4v1.AnnotationSafepointDecision: string(data)}}})
		if err := c.Patch(ctx, &p, client.RawPatch(types.MergePatchType, patch)); err != nil {
			t.Fatal(err)
		}
	}
	answer(2, "stop")
	d.safepointsOnce(ctx, relays)
	if _, err := os.Stat(decision); err == nil {
		t.Fatal("a stale answer was passed on")
	}
	answer(3, "continue")
	d.safepointsOnce(ctx, relays)
	var got SafepointDecision
	b, _ := os.ReadFile(decision)
	if json.Unmarshal(b, &got) != nil || got.Seq != 3 || got.Do != "continue" || got.Why != "capacity returned" {
		t.Fatalf("decision file %q", b)
	}

	// The unit's pod is gone: its relay is forgotten.
	os.Remove(d.volumePath(v.ID))
	d.safepointsOnce(ctx, relays)
	if len(relays) != 0 {
		t.Fatalf("relays kept: %v", relays)
	}
}

// A unit's unread messages go to its control directory, and how far it
// read them back to its pod.
func TestSteerRelay(t *testing.T) {
	c, _ := notePod(t)
	root, pods := t.TempDir(), t.TempDir()
	d := &Daemon{Kube: c, Cfg: Config{Root: root, PodsDir: pods}}
	v := &Volume{ID: "vol-u3", Partition: "p0", Namespace: "saltmarsh", Pod: "sm0140-light-v001-u3-x", PodUID: "u3", Capture: true}
	os.MkdirAll(d.volDir(), 0o755)
	if err := d.saveVolume(v); err != nil {
		t.Fatal(err)
	}
	upper, _ := d.volumeDirs(v.Partition, v.ID)
	os.MkdirAll(filepath.Join(upper, ".c4"), 0o755)
	control := filepath.Join(pods, "u3", "volumes", "kubernetes.io~empty-dir", ControlVolume)
	os.MkdirAll(control, 0o755)
	ctx := context.Background()
	msgs := `[{"seq":1,"at":"2026-10-22T14:00:00Z","actor":"dana","text":"use the v3 plates"}]`
	var p corev1.Pod
	c.Get(ctx, types.NamespacedName{Namespace: "saltmarsh", Name: "sm0140-light-v001-u3-x"}, &p)
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{c4v1.AnnotationUnitSteer: msgs}}})
	if err := c.Patch(ctx, &p, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatal(err)
	}
	relays := map[string]*spRelay{}
	d.safepointsOnce(ctx, relays)
	if b, err := os.ReadFile(filepath.Join(control, "steer")); err != nil || string(b) != msgs {
		t.Fatalf("steer file %q %v", b, err)
	}
	os.WriteFile(filepath.Join(upper, ".c4", "steer-seen"), []byte("1\n"), 0o644)
	relays["u3"].steerAt = time.Time{}
	d.safepointsOnce(ctx, relays)
	if got := annotation(t, c, c4v1.AnnotationSteerSeen); got != "1" {
		t.Fatalf("steer-seen %q", got)
	}
}
