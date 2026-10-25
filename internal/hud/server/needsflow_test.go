package server

import (
	"bytes"
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

// Supply and Withdraw work end to end, with signing on: the HUD's button
// (its POST) writes a signed request, the queue checks the record against
// the farm's roots and carries it out, and the unit's next pod has the
// answer ($C4_SUPPLIED) or the withdrawal ($C4_WITHDRAWN). The needs-me
// list offers both while the questions are open, and drops each once it
// is settled.
func TestSupplyAndWithdrawReachTheUnit(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	stores := func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
	ca, err := provenance.NewCA(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	signer := func(as string) *provenance.Signer {
		s, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity(as), time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)

	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1", Labels: map[string]string{"pool": "cpu"}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pool := &c4v1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "cpu"}, Spec: c4v1.PoolSpec{NodeSelector: map[string]string{"pool": "cpu"},
		SlotsPerNode: 4, CostPerSlotHour: "0.40"}}
	project := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show"}, Spec: c4v1.ProjectSpec{Namespaces: []string{"show"}}}
	retries := int32(1)
	job := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show", Name: "sh010-light", UID: "uid-sh010",
		Annotations: map[string]string{c4v1.AnnotationOwner: "dana"}},
		Spec: c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-2", "chunk": "1"}, Pools: []string{"cpu"},
			Retries:  &retries,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "work", Image: "busybox"}}}}}}
	kc := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&c4v1.WorkJob{}, &c4v1.Pool{}, &c4v1.Project{}, &corev1.Pod{}, &corev1.Node{}).
		WithObjects(node, pool, project, job).Build()

	q := &queue.Queue{Client: kc, Policy: sched.Decide, Cfg: sched.DefaultConfig(), Log: zap.New(zap.WriteTo(&bytes.Buffer{})),
		Now: func() time.Time { return now }, CaptureWait: time.Minute, Signer: signer("queue"), Roots: roots, Stores: stores}
	tick := func() {
		t.Helper()
		if err := q.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pods := func() map[string]corev1.Pod {
		t.Helper()
		var l corev1.PodList
		if err := kc.List(ctx, &l, client.InNamespace("show"), client.MatchingLabels{c4v1.LabelJob: "sh010-light"}); err != nil {
			t.Fatal(err)
		}
		out := map[string]corev1.Pod{}
		for _, p := range l.Items {
			if p.Status.Phase == "" || p.Status.Phase == corev1.PodPending {
				out[p.Labels[c4v1.LabelUnit]] = p
			}
		}
		return out
	}
	stop := func(p corev1.Pod, msg string) {
		t.Helper()
		p.Status.Phase = corev1.PodFailed
		p.Status.StartTime = &metav1.Time{Time: now.Add(-time.Minute)}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "work", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: c4v1.NeedsExitCode, Message: msg, StartedAt: metav1.Time{Time: now.Add(-time.Minute)}, FinishedAt: metav1.Time{Time: now}}}}}
		if err := kc.Status().Update(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	env := func(p corev1.Pod, name string) string {
		for _, e := range p.Spec.Containers[0].Env {
			if e.Name == name {
				return e.Value
			}
		}
		return ""
	}

	// Both units run, and both stop for information.
	tick()
	running := pods()
	if len(running) != 2 {
		t.Fatalf("released %d units, want 2", len(running))
	}
	stop(running["0"], "the delivery frame rate\nwhy: the brief gives 24 and 25")
	stop(running["1"], "which grade to match")
	tick()
	tick()

	s := &Server{Client: kc, Kube: kfake.NewSimpleClientset(), Namespace: "c4-system", Loc: time.UTC,
		Log: zap.New(zap.WriteTo(&bytes.Buffer{})), Signer: signer("hud"), Stores: stores,
		DevViewersOpen: true,
		DevViewers: map[string]*api.Viewer{
			"wrangler": {User: "dana", Projects: []string{"show"}, All: true},
			"artist":   {User: "maya", Projects: []string{"show"}}}}
	if err := s.collect(ctx); err != nil {
		t.Fatal(err)
	}

	// The owner's list has both questions, each with its two buttons;
	// someone else's has neither.
	var mine api.NeedsMe
	if code := get(t, s, "/api/v1/needs-me?as=wrangler", &mine); code != http.StatusOK {
		t.Fatalf("needs-me: %d", code)
	}
	if len(mine.Items) != 2 {
		t.Fatalf("dana's list: %+v", mine.Items)
	}
	for _, it := range mine.Items {
		if it.Kind != "question" || it.Job != "show/sh010-light" || len(it.Actions) != 2 ||
			it.Actions[0].Action != "supply" || it.Actions[1].Action != "withdraw-needs" {
			t.Fatalf("item %+v", it)
		}
	}
	var hers api.NeedsMe
	get(t, s, "/api/v1/needs-me?as=artist", &hers)
	if len(hers.Items) != 0 {
		t.Fatalf("maya, who does not own the job, is asked: %+v", hers.Items)
	}

	post := func(path, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path+"?as=wrangler", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	// The buttons, as the list gives them.
	post(mine.Items[0].Actions[0].Path, `{"unit": 0, "text": "24 fps"}`)
	post("/api/v1/jobs/show/sh010-light/needs/1/withdraw", `{"why": "the grade comes from the plate now"}`)

	now = now.Add(time.Minute)
	tick()
	next := pods()
	p0, ok0 := next["0"]
	p1, ok1 := next["1"]
	if !ok0 || !ok1 {
		var l corev1.EventList
		kc.List(ctx, &l, client.InNamespace("show"))
		var why []string
		for _, e := range l.Items {
			why = append(why, e.Reason+": "+e.Message)
		}
		t.Fatalf("no next attempt (unit 0 %v, unit 1 %v); events %v", ok0, ok1, why)
	}
	if env(p0, "C4_SUPPLIED") != "24 fps" || env(p0, "C4_NEEDED") != "the delivery frame rate" {
		t.Fatalf("unit 0's next attempt: %v", p0.Spec.Containers[0].Env)
	}
	if env(p1, "C4_WITHDRAWN") != "the grade comes from the plate now" {
		t.Fatalf("unit 1's next attempt: %v", p1.Spec.Containers[0].Env)
	}

	// Settled, both leave the list.
	if err := s.collect(ctx); err != nil {
		t.Fatal(err)
	}
	var after api.NeedsMe
	get(t, s, "/api/v1/needs-me?as=wrangler", &after)
	if len(after.Items) != 0 {
		t.Fatalf("settled questions still listed: %+v", after.Items)
	}
	if after.Viewer == nil || after.Viewer.Solo {
		t.Fatalf("a shared farm's viewer reads as solo: %+v", after.Viewer)
	}

	// On a single-person farm its installer is told so, and the HUD opens
	// on what needs them.
	s.SinglePerson = "dana"
	var solo api.NeedsMe
	get(t, s, "/api/v1/needs-me?as=wrangler", &solo)
	var snap api.Snapshot
	get(t, s, "/api/v1/snapshot?as=wrangler", &snap)
	if solo.Viewer == nil || !solo.Viewer.Solo || snap.Viewer == nil || !snap.Viewer.Solo || s.DevViewers["wrangler"].Solo {
		t.Fatalf("the installer is not solo (needs-me %+v, snapshot %+v), or the shared viewer changed", solo.Viewer, snap.Viewer)
	}
}
