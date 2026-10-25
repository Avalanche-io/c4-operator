package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

func runnerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := c4v1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A runner that stops (its service's SIGTERM, or an interrupt) leaves its
// Node offering no slots, with why, so the farm binds nothing more to it.
func TestStoppedRunnerOffersNoSlots(t *testing.T) {
	e := newEnv(t)
	c := fake.NewClientBuilder().WithScheme(runnerScheme(t)).Build()
	r := &Runner{Client: c, Harness: e.h, Node: "laptop-1", Labels: map[string]string{LabelHost: "laptop-1"}, Slots: 4, Logf: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Run(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var n corev1.Node
	if err := c.Get(context.Background(), types.NamespacedName{Name: "laptop-1"}, &n); err != nil {
		t.Fatal(err)
	}
	if n.Labels[c4v1.LabelSlotLimit] != "0" || n.Annotations[c4v1.AnnotationCapacityReason] != "its runner stopped" {
		t.Fatalf("labels %v annotations %v", n.Labels, n.Annotations)
	}
}

// A runner whose token the farm keeps refusing ends with ErrEnrollAgain,
// after RefusedTicks rounds in a row, not on the first refusal.
func TestRefusedRunnerEnds(t *testing.T) {
	e := newEnv(t)
	calls := 0
	c := fake.NewClientBuilder().WithScheme(runnerScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			calls++
			return apierrors.NewUnauthorized("token refused")
		},
	}).Build()
	r := &Runner{Client: c, Harness: e.h, Node: "laptop-1", Slots: 4, Logf: t.Logf}
	err := r.Run(context.Background(), time.Millisecond)
	if !errors.Is(err, ErrEnrollAgain) {
		t.Fatalf("Run: %v", err)
	}
	if calls != RefusedTicks {
		t.Errorf("gave up after %d refusals, want %d", calls, RefusedTicks)
	}
}

// An enrolled machine no one owns says so, and how to enroll it, again
// before a code would run out; it makes no offer and writes no code (a
// code in a log could leave the machine), and says nothing more once
// someone has enrolled it.
func TestRunnerSaysItIsNotEnrolled(t *testing.T) {
	e := newEnv(t)
	posts := 0
	hud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts++ }))
	defer hud.Close()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "laptop-1"}}
	c := fake.NewClientBuilder().WithScheme(runnerScheme(t)).WithObjects(node).Build()
	now := time.Now()
	var logs []string
	r := &Runner{Client: c, Harness: e.h, Node: "laptop-1", Owners: []string{"noor"}, Offer: true, HUD: hud.URL,
		HUDToken: func() string { return "tok" }, Now: func() time.Time { return now }, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	r.offerTick(context.Background())
	r.offerTick(context.Background()) // within OfferEvery: nothing more
	if posts != 0 || len(logs) != 1 || !strings.Contains(logs[0], "c4-agent enroll-offer") || !strings.Contains(logs[0], "c4 farm enroll laptop-1 <code>") {
		t.Fatalf("posts %d, logs %q", posts, logs)
	}
	now = now.Add(OfferEvery)
	r.offerTick(context.Background())
	if len(logs) != 2 {
		t.Fatalf("not said again after %s: %q", OfferEvery, logs)
	}
	node.Annotations = map[string]string{c4v1.AnnotationNodeOwners: `["noor"]`}
	c.Update(context.Background(), node)
	now = now.Add(OfferEvery)
	r.offerTick(context.Background())
	now = now.Add(OfferEvery)
	r.offerTick(context.Background())
	if len(logs) != 3 || !strings.Contains(logs[2], "is enrolled") || !r.offerDone || posts != 0 {
		t.Fatalf("after enrolment: %q, %d posts", logs, posts)
	}
}
