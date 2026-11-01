package main

import (
	"context"
	"errors"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

// versioned makes the fake clientset keep a Secret's resourceVersion as
// the API server does: each write bumps it, and an update from another
// version conflicts.
func versioned(kube *kfake.Clientset) {
	n := 0
	gvr := corev1.SchemeGroupVersion.WithResource("secrets")
	kube.PrependReactor("*", "secrets", func(a ktesting.Action) (bool, runtime.Object, error) {
		w, ok := a.(ktesting.CreateAction) // an update carries its object the same way
		if !ok {
			return false, nil, nil
		}
		switch a.GetVerb() {
		case "create":
			sec := w.GetObject().(*corev1.Secret).DeepCopy()
			if _, err := kube.Tracker().Get(gvr, sec.Namespace, sec.Name); err == nil {
				return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, sec.Name)
			}
			n++
			sec.ResourceVersion = strconv.Itoa(n)
			return true, sec, kube.Tracker().Create(gvr, sec, sec.Namespace)
		case "update":
			sec := w.GetObject().(*corev1.Secret).DeepCopy()
			cur, err := kube.Tracker().Get(gvr, sec.Namespace, sec.Name)
			if err != nil {
				return true, nil, err
			}
			if cur.(*corev1.Secret).ResourceVersion != sec.ResourceVersion {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, sec.Name, errors.New("the object has been modified"))
			}
			n++
			sec.ResourceVersion = strconv.Itoa(n)
			if err := kube.Tracker().Delete(gvr, sec.Namespace, sec.Name); err != nil {
				return true, nil, err
			}
			return true, sec, kube.Tracker().Create(gvr, sec, sec.Namespace)
		}
		return false, nil, nil
	})
}

// The index's head is one Secret, written by resourceVersion: of two
// replicas that read the same head, the first to write moves it and the
// second gets ErrMoved, so it reads the index again before it books.
func TestPeopleHeadMovesOnce(t *testing.T) {
	ctx := context.Background()
	kube := kfake.NewSimpleClientset()
	versioned(kube)
	h := &secretHead{kube: kube, ns: "c4-signing"}
	empty, err := h.Read(ctx)
	if err != nil || empty.Position != 0 || empty.Version != "" {
		t.Fatalf("an empty head: %+v %v", empty, err)
	}
	if err := h.Write(ctx, empty, people.HeadState{ID: "c4first", Position: 1}); err != nil {
		t.Fatal(err)
	}
	if err := h.Write(ctx, empty, people.HeadState{ID: "c4other", Position: 1}); !errors.Is(err, people.ErrMoved) {
		t.Errorf("a second first entry: %v", err)
	}
	one, _ := h.Read(ctx)
	if one.ID != "c4first" || one.Position != 1 {
		t.Fatalf("head %+v", one)
	}
	if err := h.Write(ctx, one, people.HeadState{ID: "c4second", Position: 2}); err != nil {
		t.Fatal(err)
	}
	if err := h.Write(ctx, one, people.HeadState{ID: "c4stale", Position: 2}); !errors.Is(err, people.ErrMoved) {
		t.Errorf("a write from a stale head: %v", err)
	}
	if two, _ := h.Read(ctx); two.ID != "c4second" || two.Position != 2 {
		t.Errorf("head %+v", two)
	}
}

// The sign-ins Secret names each person's provider subjects, one per
// line; a line without a subject names none.
func TestSignInsSecret(t *testing.T) {
	ins := parseSignIns(map[string][]byte{
		"dana": []byte("https://sso.studio.example 00u-dana-7f3a\nhttps://login.studio.example dana-2\n"),
		"noor": []byte("https://sso.studio.example\n"),
	})
	ctx := context.Background()
	if p, _ := ins.Person(ctx, "https://login.studio.example", "dana-2"); p != "dana" {
		t.Errorf("dana's second sign-in is %q's", p)
	}
	if p, _ := ins.Person(ctx, "https://sso.studio.example", "00u-dana-7f3a"); p != "dana" {
		t.Errorf("dana's sign-in is %q's", p)
	}
	if len(ins["noor"]) != 0 {
		t.Errorf("noor's sign-ins %v", ins["noor"])
	}
}
