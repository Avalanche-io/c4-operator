package main

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/auth"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// The issuer certifies a workload only if a project names it as an
// importer: any other service account (a unit's, a project's default) is
// refused, so nothing but a project's importers can sign a record that
// verifies as a source of its content.
func TestIssuerCertifiesOnlyImporters(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	show := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-a"}, Spec: c4v1.ProjectSpec{
		Namespaces: []string{"show-a", "show-a-plates"},
		Importers:  []c4v1.Importer{{Namespace: "show-a", ServiceAccount: "import-git"}, {Namespace: "show-a-plates", ServiceAccount: "ingest"}},
	}}
	other := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-b"}, Spec: c4v1.ProjectSpec{Namespaces: []string{"show-b"}}}
	// A Tracker names the account that relays its decisions; one that
	// names none certifies no one.
	flow := &c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "flow"}, Spec: c4v1.TrackerSpec{Kind: "flow",
		Sync: &c4v1.TrackerSync{Namespace: "c4-system", ServiceAccount: "c4-sync"}}}
	plain := &c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-b", Name: "gh"}, Spec: c4v1.TrackerSpec{Kind: "github"}}
	r := fake.NewClientBuilder().WithScheme(scheme).WithObjects(show, other, flow, plain).Build()
	sa := func(ns, name string) *auth.Identity {
		return &auth.Identity{User: "system:serviceaccount:" + ns + ":" + name, Namespace: ns, Account: name}
	}
	ctx := context.Background()
	for _, c := range []struct {
		id   *auth.Identity
		want string
		life time.Duration
		err  string
	}{
		{id: sa("show-a", "import-git"), want: provenance.ServiceAccountIdentity("show-a", "import-git"), life: time.Hour},
		{id: sa("show-a-plates", "ingest"), want: provenance.ServiceAccountIdentity("show-a-plates", "ingest"), life: time.Hour},
		{id: sa("show-a", "default"), err: "is not an importer of a project"},
		{id: sa("show-a", "ingest"), err: "is not an importer of a project"},
		{id: sa("show-b", "import-git"), err: "is not an importer of a project"},
		{id: sa("elsewhere", "import-git"), err: "is not an importer of a project"},
		{id: &auth.Identity{User: "system:serviceaccount:c4-system:c4-node", Namespace: "c4-system", Account: "c4-node", NodeName: "w1"},
			want: provenance.NodeIdentity("w1"), life: 24 * time.Hour},
		{id: sa("c4-system", "c4-node"), err: "bound to its pod"},
		{id: sa("c4-system", "c4-manager"), err: "does not sign records"},
		{id: &auth.Identity{User: "alice"}, err: "not a service account"},
		// A host runner is its own machine's service account in c4-hosts.
		{id: sa("c4-hosts", "laptop-1"), want: provenance.RunnerIdentity("laptop-1"), life: time.Hour},
		{id: sa("show-a", "laptop-1"), err: "is not an importer of a project"},
		// c4-sync, while a Tracker names it as its sync; another account
		// in its namespace is not
		{id: sa("c4-system", "c4-sync"), want: provenance.ServiceAccountIdentity("c4-system", "c4-sync"), life: time.Hour},
		{id: sa("c4-system", "c4-sync-other"), err: "or a Tracker's sync"},
	} {
		got, life, err := signerIdentity(ctx, r, c.id, "c4-system")
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: certified as %q (err %v), want refused with %q", c.id.User, got, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want || life != c.life {
			t.Errorf("%s: %q for %v (err %v), want %q for %v", c.id.User, got, life, err, c.want, c.life)
		}
	}
}

// A HUD is loopback-only when its listen address says so, never when a
// request does: an empty host or a routable address listens to the network.
func TestLoopbackOnly(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8090": true, "[::1]:8090": true, "localhost:8090": true, "127.0.0.2:8090": true,
		":8090": false, "0.0.0.0:8090": false, "[::]:8090": false, "10.0.0.5:8090": false, "hud.example.com:8090": false, "": false,
	} {
		if got := loopbackOnly(addr); got != want {
			t.Errorf("%q: %v, want %v", addr, got, want)
		}
	}
}

// A unit is certified on its own pod's token as that unit's attempt,
// unit/<ns>/<job>/<unit>@<job UID>/<attempt>, for an hour, and only while
// its pod is that pod and running: never as its service account (even one
// a project names among its importers), never for another pod's token,
// a finished attempt, or a pod that names no job, unit or attempt.
func TestIssuerCertifiesAUnitAsItself(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	yes := true
	unitPod := func(name, uid string, edit func(p *corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name, UID: types.UID(uid),
			Labels:      map[string]string{c4v1.LabelJob: "sh040-comp", c4v1.LabelUnit: "3"},
			Annotations: map[string]string{c4v1.AnnotationAttempt: "2"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: "sh040-comp",
				UID: "9c1d2e7f-0000-4000-8000-000000000001", Controller: &yes}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		if edit != nil {
			edit(p)
		}
		return p
	}
	show := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-a"}, Spec: c4v1.ProjectSpec{
		Namespaces: []string{"show-a"}, Importers: []c4v1.Importer{{Namespace: "show-a", ServiceAccount: "default"}}}}
	r := fake.NewClientBuilder().WithScheme(scheme).WithObjects(show,
		unitPod("u3", "pod-u3", nil),
		unitPod("done", "pod-done", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		unitPod("noattempt", "pod-na", func(p *corev1.Pod) { delete(p.Annotations, c4v1.AnnotationAttempt) }),
		unitPod("loose", "pod-loose", func(p *corev1.Pod) { p.OwnerReferences = nil }),
		unitPod("plain", "pod-plain", func(p *corev1.Pod) { p.Labels = nil }),
		unitPod("placed", "pod-placed", func(p *corev1.Pod) { p.Spec.NodeName = "w1" }),
		unitPod("onhost", "pod-onhost", func(p *corev1.Pod) { p.Spec.NodeName = "laptop-1" }),
		unitPod("tainted", "pod-tainted", func(p *corev1.Pod) { p.Spec.NodeName = "laptop-2" }),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "laptop-1", Labels: map[string]string{c4v1.LabelHost: "laptop-1"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "laptop-2"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: c4v1.TaintHostRunner, Value: "true", Effect: corev1.TaintEffectNoSchedule}}}},
	).Build()
	pod := func(name, uid string) *auth.Identity {
		return &auth.Identity{User: "system:serviceaccount:show-a:default", Namespace: "show-a", Account: "default", PodName: name, PodUID: uid}
	}
	ctx := context.Background()
	got, life, err := signerIdentity(ctx, r, pod("u3", "pod-u3"), "c4-system")
	if want := provenance.UnitIdentity("show-a", "sh040-comp", "3", "9c1d2e7f-0000-4000-8000-000000000001", 2); err != nil || got != want || life != time.Hour {
		t.Fatalf("a unit: %q for %v (err %v), want %q", got, life, err, want)
	}
	for name, c := range map[string]struct {
		id  *auth.Identity
		err string
	}{
		"another pod's token": {pod("u3", "pod-old"), "now another pod"},
		"a finished attempt":  {pod("done", "pod-done"), "has ended"},
		"no attempt":          {pod("noattempt", "pod-na"), "names no attempt"},
		"no controlling job":  {pod("loose", "pod-loose"), "is not a unit of job"},
		"a pod that is gone":  {pod("gone", "pod-gone"), "is gone"},
		// a host runner's unit gets its certificate through its runner
		"a host runner's unit, by label": {pod("onhost", "pod-onhost"), "runs on host runner laptop-1"},
		"a host runner's unit, by taint": {pod("tainted", "pod-tainted"), "runs on host runner laptop-2"},
	} {
		if got, _, err := signerIdentity(ctx, r, c.id, "c4-system"); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: certified as %q (err %v), want refused with %q", name, got, err, c.err)
		}
	}
	if got, _, err := signerIdentity(ctx, r, pod("placed", "pod-placed"), "c4-system"); err != nil || got != provenance.UnitIdentity("show-a", "sh040-comp", "3", "9c1d2e7f-0000-4000-8000-000000000001", 2) {
		t.Errorf("a unit placed on a farm Node: %q (err %v)", got, err)
	}
	// a pod that is no unit's is judged as its account: here an importer
	if got, _, err := signerIdentity(ctx, r, pod("plain", "pod-plain"), "c4-system"); err != nil || got != provenance.ServiceAccountIdentity("show-a", "default") {
		t.Errorf("a pod that is no unit's: %q (err %v)", got, err)
	}
}

// The issuer takes a unit's whole allowance of signed controls from one
// pod at once, then one every podEvery; another pod has its own.
func TestIssuerLimitsEachPod(t *testing.T) {
	var q podLimits
	now := time.Now()
	for i := range podBurst {
		if ok, _ := q.allow("pod-a", now); !ok {
			t.Fatalf("certificate %d of a unit's allowance refused", i+1)
		}
	}
	if ok, wait := q.allow("pod-a", now); ok || wait <= 0 || wait > podEvery {
		t.Fatalf("past the allowance: %v, wait %v", ok, wait)
	}
	if ok, _ := q.allow("pod-b", now); !ok {
		t.Fatal("another pod shares the first's allowance")
	}
	if ok, _ := q.allow("pod-a", now.Add(podEvery)); !ok {
		t.Fatal("refused after waiting")
	}
}

// A sync's leaf names each Tracker that names it as its sync, and only
// those (RelayURN): the only Trackers its relayed acts may be for. Any
// other identity's names none.
func TestSyncLeafNamesItsTrackers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sync := &c4v1.TrackerSync{Namespace: "c4-system", ServiceAccount: "c4-sync"}
	r := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "flow"}, Spec: c4v1.TrackerSpec{Kind: "flow", Sync: sync}},
		&c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-b", Name: "kitsu"}, Spec: c4v1.TrackerSpec{Kind: "kitsu", Sync: sync}},
		&c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-b", Name: "gh"}, Spec: c4v1.TrackerSpec{Kind: "github"}},
	).Build()
	ctx := context.Background()
	got, err := trackerRelays(ctx, r, provenance.ServiceAccountIdentity("c4-system", "c4-sync"))
	if err != nil || strings.Join(got, ",") != "show-a/flow,show-b/kitsu" {
		t.Fatalf("c4-sync's Trackers: %v %v", got, err)
	}
	for _, id := range []string{provenance.ServiceAccountIdentity("show-a", "import-git"), provenance.NodeIdentity("w1")} {
		if got, err := trackerRelays(ctx, r, id); err != nil || len(got) != 0 {
			t.Errorf("%s names %v (%v)", id, got, err)
		}
	}
}

// One account, one role: an account a project names as an importer and a
// Tracker names as its sync is certified as neither, so one certificate
// never signs both sources and relayed decisions.
func TestIssuerRefusesAnImporterThatIsASync(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show-a"}, Spec: c4v1.ProjectSpec{Namespaces: []string{"show-a"},
			Importers: []c4v1.Importer{{Namespace: "show-a", ServiceAccount: "import-git"}}}},
		&c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "flow"}, Spec: c4v1.TrackerSpec{Kind: "flow",
			Sync: &c4v1.TrackerSync{Namespace: "show-a", ServiceAccount: "import-git"}}},
	).Build()
	id := &auth.Identity{User: "system:serviceaccount:show-a:import-git", Namespace: "show-a", Account: "import-git"}
	if got, _, err := signerIdentity(context.Background(), r, id, "c4-system"); err == nil || !strings.Contains(err.Error(), "one account takes one role") {
		t.Fatalf("an importer that is a sync: certified as %q (%v)", got, err)
	}
}
