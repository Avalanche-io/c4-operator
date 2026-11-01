package farmbook_test

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
)

// The ConfigMap pointer moves only from the version read: a writer behind
// another is told the State moved, and reads it again.
func TestConfigMapPointerMovesFromItsVersion(t *testing.T) {
	ctx := context.Background()
	p := &farmbook.ConfigMapPointer{Client: fake.NewClientBuilder().Build(), Namespace: "c4-system"}
	empty, err := p.Read(ctx)
	if err != nil || empty.Version != "" || empty.Head.Number != 0 {
		t.Fatalf("an unwritten book: %+v, %v", empty, err)
	}
	if err := p.Write(ctx, empty, farmbook.State{Pending: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(ctx, empty, farmbook.State{Pending: []string{"b"}}); !errors.Is(err, farmbook.ErrMoved) {
		t.Errorf("a second first write: %v", err)
	}
	one, _ := p.Read(ctx)
	if err := p.Write(ctx, one, farmbook.State{Head: farmbook.Head{Number: 1, Position: 1, Segment: "s", Entry: "e"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(ctx, one, farmbook.State{Pending: []string{"c"}}); !errors.Is(err, farmbook.ErrMoved) {
		t.Errorf("a write from a stale version: %v", err)
	}
	two, _ := p.Read(ctx)
	if two.Head.Number != 1 || two.Head.Entry != "e" || len(two.Pending) != 0 {
		t.Errorf("read back %+v", two)
	}
}
