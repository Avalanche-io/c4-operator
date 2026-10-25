package queue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A unit whose pod the API server refuses, or whose image cannot be
// pulled, says so on its job, with the fix, instead of only in the
// manager's log; and the condition goes once the units start.
func TestSilentWaitsAreConditions(t *testing.T) {
	ns, q, now := farm(t, "cpu", 1, 2)
	ctx := context.Background()
	// The namespace enforces Pod Security restricted, as a local farm's
	// projects do; a template without a security context is refused.
	var n corev1.Namespace
	if err := k8s.Get(ctx, client.ObjectKey{Name: ns}, &n); err != nil {
		t.Fatal(err)
	}
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels["pod-security.kubernetes.io/enforce"] = "restricted"
	if err := k8s.Update(ctx, &n); err != nil {
		t.Fatal(err)
	}
	j := job(ns, "sh060", "render", "cpu-"+ns, map[string]string{"frames": "1-1", "estimateSeconds": "60"}, now.Add(time.Hour))
	j.Spec.Template.Spec.Containers[0].SecurityContext = nil
	if err := k8s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	tick(t, q)
	if pods := unitPods(t, ns, "sh060"); len(pods) != 0 {
		t.Skipf("this API server does not enforce Pod Security (%d pods made)", len(pods))
	}
	got := get(t, ns, "sh060")
	c := meta.FindStatusCondition(got.Status.Conditions, "Admitted")
	if c == nil || c.Reason != "PodSecurity" || !strings.Contains(c.Message, "Pod Security (restricted)") || !strings.Contains(c.Message, "runAsNonRoot") {
		t.Fatalf("a refused unit says nothing on its job: %+v", c)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson); c == nil || c.Reason != c4v1.NeedsPermission {
		t.Fatalf("a refused job does not need a person: %+v", c)
	}

	// Lifted: the unit starts, and the condition goes.
	delete(n.Labels, "pod-security.kubernetes.io/enforce")
	if err := k8s.Update(ctx, &n); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Second)
	tick(t, q)
	pods := unitPods(t, ns, "sh060")
	if len(pods) != 1 {
		t.Fatalf("%d pods once admitted", len(pods))
	}
	if c := meta.FindStatusCondition(get(t, ns, "sh060").Status.Conditions, "Admitted"); c != nil {
		t.Fatalf("an admitted job still reads refused: %+v", c)
	}

	// Its image cannot be pulled: condition Images, with the fix.
	p := &pods[0]
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "work", Image: "busybox",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: `Back-off pulling image "busybox"`}}}}
	if err := k8s.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Second)
	tick(t, q)
	got = get(t, ns, "sh060")
	c = meta.FindStatusCondition(got.Status.Conditions, "Images")
	if c == nil || !strings.Contains(c.Message, "unit 0 cannot pull its image busybox") || !strings.Contains(c.Message, "c4 farm image load busybox") {
		t.Fatalf("a unit that cannot pull its image says nothing: %+v", c)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, c4v1.ConditionNeedsPerson); c == nil || c.Reason != c4v1.NeedsStuck {
		t.Fatalf("an image that cannot be pulled does not need a person: %+v", c)
	}
}

// The refusals the API server gives, read in the farm's words.
func TestRefusalWords(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	for _, c := range []struct {
		err          error
		reason, want string
	}{
		{apierrors.NewForbidden(gr, "u", errors.New(`exceeded quota: compute, requested: cpu=2, used: cpu=8, limited: cpu=8`)),
			c4v1.NeedsQuota, "p's quota compute is used up (asked for cpu=2, using cpu=8 of cpu=8)"},
		{apierrors.NewForbidden(gr, "u", errors.New(`admission webhook "images.example.com" denied the request: unsigned image`)),
			"Webhook", "the admission webhook images.example.com refuses the job's units: unsigned image"},
		{apierrors.NewForbidden(gr, "u", errors.New(`ValidatingAdmissionPolicy 'c4-lease-pods' with binding 'c4-lease-pods' denied request: no lease`)),
			"Policy", "the farm's policy c4-lease-pods refuses the job's units: no lease"},
		{apierrors.NewConflict(gr, "u", errors.New("try again")), "", ""},
	} {
		reason, msg := refusal(c.err, "p")
		if reason != c.reason || !strings.Contains(msg, c.want) {
			t.Errorf("%v: %q %q, want %q containing %q", c.err, reason, msg, c.reason, c.want)
		}
	}
}

// A failed pull's cause reads in a few words, not the runtime's message.
func TestPullWhy(t *testing.T) {
	const img = "c4-local-registry:5000/c4-job-missing:dev"
	for _, c := range []struct{ msg, image, want string }{
		{`rpc error: code = NotFound desc = failed to pull and unpack image "` + img + `": failed to resolve reference "` + img + `": ` + img + `: not found`, img, "not found in c4-local-registry:5000"},
		{`failed to authorize: failed to fetch anonymous token: unexpected status: 401 Unauthorized`, "registry.studio.example/render:4", "registry.studio.example refused access to it"},
		{`dial tcp: lookup registry.studio.example: no such host`, "registry.studio.example/render:4", "registry.studio.example cannot be reached"},
		{`Back-off pulling image "busybox"`, "busybox", "the last pull failed; it is waiting to try again"},
		{`something odd happened: the disk is full`, "busybox", "the disk is full"},
		{"", "busybox", ""},
	} {
		if got := pullWhy(c.msg, c.image); got != c.want {
			t.Errorf("pullWhy(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}
