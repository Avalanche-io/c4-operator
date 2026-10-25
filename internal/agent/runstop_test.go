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

// An enrolled runner reads its pods only through the HUD: it lists no pod
// and reads no job from the API server, and judges each pod by the job the
// HUD names with it, refusing another owner's.
func TestRunnerReadsItsPodsThroughTheHUD(t *testing.T) {
	e := newEnv(t)
	yes := true
	pod := func(name, job, uid string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name, UID: types.UID("uid-" + name), Labels: map[string]string{c4v1.LabelJob: job},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: job, UID: types.UID(uid), Controller: &yes}}},
			Spec: corev1.PodSpec{NodeName: "laptop-1", Containers: []corev1.Container{{Name: "member", Image: "c4-agent",
				Env: []corev1.EnvVar{{Name: "C4_AGENT_MEMBER", Value: "lead"}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodPending}}
	}
	theirs := pod("theirs-0", "theirs", "u-theirs")
	hud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := json.Marshal(theirs)
		fmt.Fprintf(w, `{"node":"laptop-1","pods":[{"pod":%s,"job":{"name":"theirs","uid":"u-theirs","owner":"ivo","project":"show-a"}}]}`, raw)
	}))
	defer hud.Close()
	reads := 0
	c := fake.NewClientBuilder().WithScheme(runnerScheme(t)).WithObjects(theirs.DeepCopy()).WithStatusSubresource(&corev1.Pod{}).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				reads++
			}
			return c.List(ctx, list, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			switch obj.(type) {
			case *c4v1.WorkJob, *corev1.Pod:
				reads++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := &Runner{Client: c, Harness: e.h, Node: "laptop-1", Labels: map[string]string{LabelHost: "laptop-1"}, Owners: []string{"noor"}, Slots: 2,
		PodsThroughHUD: true, HUD: hud.URL, HUDToken: func() string { return "tok" }, Logf: t.Logf}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Errorf("%d pod or job reads from the API server", reads)
	}
	var got corev1.Pod
	c.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: "theirs-0"}, &got)
	if got.Status.Phase != corev1.PodFailed || got.Status.Reason != "NotTheOwner" || !strings.Contains(got.Status.Message, "is ivo's") {
		t.Fatalf("theirs: %s %s %q", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
	if p, err := r.projectOf(context.Background(), "show-a"); err != nil || p != "show-a" {
		t.Errorf("project %q %v", p, err)
	}
}
