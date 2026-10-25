package agent

import (
	"context"
	"encoding/json"
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

// An enrolled machine no one owns offers itself, as its own runner, naming
// its holder, and says the code; it offers again before the code runs out,
// and not at all once someone has enrolled it.
func TestRunnerOffersItsMachine(t *testing.T) {
	e := newEnv(t)
	var offers []map[string]string
	hud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		b["path"], b["auth"] = r.URL.Path, r.Header.Get("Authorization")
		offers = append(offers, b)
		w.Write([]byte(`{"node":"laptop-1","code":"ABCD-EFGH","message":"to enroll laptop-1 as yours, give this code: ABCD-EFGH"}`))
	}))
	defer hud.Close()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "laptop-1"}}
	c := fake.NewClientBuilder().WithScheme(runnerScheme(t)).WithObjects(node).Build()
	now := time.Now()
	var logs []string
	r := &Runner{Client: c, Harness: e.h, Node: "laptop-1", Owners: []string{"noor"}, Offer: true, HUD: hud.URL,
		HUDToken: func() string { return "tok" }, Now: func() time.Time { return now }, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	r.offerTick(context.Background())
	r.offerTick(context.Background()) // within OfferEvery: nothing more
	if len(offers) != 1 || offers[0]["holder"] != "noor" || offers[0]["path"] != "/api/v1/nodes/laptop-1/enroll-offer" || offers[0]["auth"] != "Bearer tok" {
		t.Fatalf("offers %v", offers)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "c4 farm enroll laptop-1 ABCD-EFGH") {
		t.Fatalf("logs %q", logs)
	}
	now = now.Add(OfferEvery)
	r.offerTick(context.Background())
	if len(offers) != 2 {
		t.Fatalf("no new offer after %s: %d", OfferEvery, len(offers))
	}
	node.Annotations = map[string]string{c4v1.AnnotationNodeOwners: `["noor"]`}
	c.Update(context.Background(), node)
	now = now.Add(OfferEvery)
	r.offerTick(context.Background())
	r.offerTick(context.Background())
	if len(offers) != 2 || !r.offerDone {
		t.Fatalf("an enrolled machine offered itself: %d offers", len(offers))
	}
}
