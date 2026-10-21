package farmkey

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
)

// The key is made once, read back the same, and a rotation starts the next
// epoch with a different key: pseudonyms before and after are unrelated.
func TestKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	kube := kfake.NewSimpleClientset()
	a, err := LoadOrCreate(ctx, kube, "c4-system")
	if err != nil {
		t.Fatal(err)
	}
	if a.Epoch != 1 {
		t.Fatalf("first epoch %d", a.Epoch)
	}
	again, err := LoadOrCreate(ctx, kube, "c4-system")
	if err != nil || again != a {
		t.Fatalf("reading the key back gave another: %v", err)
	}
	sec, err := kube.CoreV1().Secrets("c4-system").Get(ctx, Secret, metav1.GetOptions{})
	if err != nil || len(sec.Data["key"]) != telemetry.KeySize {
		t.Fatalf("the secret: %v %d bytes", err, len(sec.Data["key"]))
	}
	b, err := Rotate(ctx, kube, "c4-system")
	if err != nil {
		t.Fatal(err)
	}
	if b.Epoch != 2 || b == a {
		t.Fatalf("rotation: epoch %d, same key %v", b.Epoch, b == a)
	}
	na, nb := telemetry.NewNamer(a), telemetry.NewNamer(b)
	na.Add(telemetry.KindJob, "saltmarsh/sh0420")
	nb.Add(telemetry.KindJob, "saltmarsh/sh0420")
	na.Freeze()
	nb.Freeze()
	if na.Of(telemetry.KindJob, "saltmarsh/sh0420").Text() == nb.Of(telemetry.KindJob, "saltmarsh/sh0420").Text() {
		t.Fatal("a pseudonym survived the rotation")
	}
}

// The tier is none until saved, then what was saved, with its record.
func TestTierSetting(t *testing.T) {
	ctx := context.Background()
	kube := kfake.NewSimpleClientset()
	if _, _, ok, err := LoadTier(ctx, kube, "c4-system"); ok || err != nil {
		t.Fatalf("a new farm has a tier: %v %v", ok, err)
	}
	for _, tier := range []string{"b", "a"} {
		if err := SaveTier(ctx, kube, "c4-system", tier, "c4rec", []byte("{}")); err != nil {
			t.Fatal(err)
		}
		got, rec, ok, err := LoadTier(ctx, kube, "c4-system")
		if err != nil || !ok || got != tier || rec != "c4rec" {
			t.Fatalf("saved %s, read %q %v %v", tier, got, ok, err)
		}
	}
}
