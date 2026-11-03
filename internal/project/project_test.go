package project

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

func proj(name string, verify bool, group string) *c4v1.Project {
	p := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}}
	p.Spec.Store.VerifyFill = verify
	if group != "" {
		p.Spec.Isolation = c4v1.IsolationShared
		p.Spec.SharingGroup = group
	}
	return p
}

type failingList struct{ client.Reader }

func (failingList) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("cache unavailable")
}

// A project's fills verify when it asks, or when a project it shares a
// partition with asks, since they share fills; a strict project's never
// depend on another's. Unknown is yes.
func TestVerifiesFills(t *testing.T) {
	s := runtime.NewScheme()
	c4v1.AddToScheme(s)
	strict, asks := proj("strict", false, ""), proj("asks", true, "")
	lax, careful, alone := proj("lax", false, "vfx"), proj("careful", true, "vfx"), proj("alone", false, "edit")
	other := proj("other-strict", true, "")
	r := &Registry{Cache: fake.NewClientBuilder().WithScheme(s).WithObjects(strict, asks, lax, careful, alone, other).Build()}
	ctx := context.Background()
	for p, want := range map[*c4v1.Project]bool{strict: false, asks: true, lax: true, careful: true, alone: false} {
		if got := r.VerifiesFills(ctx, p); got != want {
			t.Errorf("%s: %v, want %v", p.Name, got, want)
		}
	}
	r = &Registry{Cache: failingList{}}
	if !r.VerifiesFills(ctx, lax) {
		t.Error("a shared project whose partners can't be listed doesn't verify")
	}
}
