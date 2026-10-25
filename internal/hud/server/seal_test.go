package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// sealFarm is submitFarm with the farm's networks: model-api only.
func sealFarm(t *testing.T) *Server {
	t.Helper()
	s := submitFarm(t)
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	s.Namespace = "c4-system"
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: c4v1.NetworksConfigMap},
		Data:       map[string]string{"model-api": "- {host: api.anthropic.com, ports: [443]}\n"}}).Build()
	return s
}

func toolSpec(t *testing.T, nets ...string) json.RawMessage {
	t.Helper()
	spec := c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1-2"},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "c4-job-hydra"}}}}}
	if nets != nil {
		spec.Tool = &c4v1.WorkTool{Name: "fetch", Network: nets}
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// submitAs submits a job as viewer as. A spec naming a tool is made as the
// HUD's lowering makes a tool's job (POST /jobs refuses spec.tool), so the
// checks are those a submission's jobs and a tool's test meet.
func submitAs(t *testing.T, s *Server, as, name string, spec json.RawMessage, dry bool) (int, string) {
	t.Helper()
	body := api.SubmitRequest{Project: "show-a", Name: name, Spec: spec, DryRun: dry}
	var probe c4v1.WorkJobSpec
	if err := json.Unmarshal(spec, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Tool != nil {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs?as="+as, nil)
		v, err := s.viewer(r)
		m := s.current()
		if err != nil || m == nil {
			t.Fatalf("viewer %v, model %v", err, m != nil)
		}
		_, code, err := s.createJob(r, v, m, body, createOpts{Lowered: true})
		if err != nil {
			return code, err.Error()
		}
		return code, ""
	}
	var out map[string]any
	code := call(t, s, http.MethodPost, "/api/v1/jobs?as="+as, body, &out)
	msg, _ := out["error"].(string)
	if msg == "" {
		b, _ := json.Marshal(out)
		msg = string(b)
	}
	return code, msg
}

// A network the farm does not define is refused at submit, a dry run
// included, in the farm's words; one it defines is taken.
func TestUndefinedNetworkRefusedAtSubmit(t *testing.T) {
	s := sealFarm(t)
	for _, dry := range []bool{true, false} {
		code, msg := submitAs(t, s, "wrangler", "fetch-pypi", toolSpec(t, "model-api", "pypi"), dry)
		if code != http.StatusUnprocessableEntity || !strings.Contains(msg, "fetch declares network pypi, which this farm does not define") ||
			!strings.Contains(msg, "c4-networks ConfigMap in c4-system") {
			t.Fatalf("dry run %v: an undefined network: %d %s", dry, code, msg)
		}
	}
	if code, msg := submitAs(t, s, "wrangler", "fetch-model", toolSpec(t, "model-api"), false); code != http.StatusCreated {
		t.Fatalf("a defined network: %d %s", code, msg)
	}
}

// A unit's job reaches no network its own job does not: the same or
// fewer networks are taken, a wider one is refused as the unit's own
// limit, and a unit whose job has no tool submits only sealed jobs.
func TestAUnitsJobIsNoWiderThanTheUnit(t *testing.T) {
	s := sealFarm(t)
	ctx := context.Background()
	for name, nets := range map[string][]string{"agent": {"model-api"}, "sealed": nil} {
		spec := c4v1.WorkJobSpec{Class: "agent", Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "unit", Image: "x"}}}}}
		if nets != nil {
			spec.Tool = &c4v1.WorkTool{Name: "agent", Network: nets}
		}
		parent := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name, UID: k8stypes.UID("uid-" + name),
			Annotations: map[string]string{c4v1.AnnotationOwner: "lee"}}, Spec: spec}
		if err := s.Client.Create(ctx, parent); err != nil {
			t.Fatal(err)
		}
		s.DevViewers[name] = &api.Viewer{User: "unit:show-a/" + name + "/0", Projects: []string{"show-a"},
			Unit: &api.UnitViewer{Namespace: "show-a", Job: name, JobUID: string(parent.UID), Unit: "0", Grant: "project"}}
	}
	if code, msg := submitAs(t, s, "agent", "same", toolSpec(t, "model-api"), false); code != http.StatusCreated {
		t.Fatalf("the unit's own network: %d %s", code, msg)
	}
	if code, msg := submitAs(t, s, "agent", "none", toolSpec(t), false); code != http.StatusCreated {
		t.Fatalf("a sealed job from a unit: %d %s", code, msg)
	}
	code, msg := submitAs(t, s, "agent", "wider", toolSpec(t, "model-api", "huggingface"), true)
	if code != http.StatusForbidden || !strings.Contains(msg, "show-a/agent reaches only model-api, and this job would reach huggingface") {
		t.Fatalf("a wider job from a unit: %d %s", code, msg)
	}
	code, msg = submitAs(t, s, "sealed", "out", toolSpec(t, "model-api"), false)
	if code != http.StatusForbidden || !strings.Contains(msg, "show-a/sealed reaches no network, and this job would reach model-api") {
		t.Fatalf("a networked job from a sealed unit: %d %s", code, msg)
	}
	// a person submits what a unit may not
	if code, msg := submitAs(t, s, "wrangler", "person", toolSpec(t, "model-api"), false); code != http.StatusCreated {
		t.Fatalf("a person's job: %d %s", code, msg)
	}
}

// A malformed c4-networks refuses a job naming a network with a 422 that
// says so, for a farm admin to fix, not a 503 to retry.
func TestMalformedNetworksRefusedAtSubmit(t *testing.T) {
	s := sealFarm(t)
	ctx := context.Background()
	var cm corev1.ConfigMap
	if err := s.Client.Get(ctx, k8stypes.NamespacedName{Namespace: "c4-system", Name: c4v1.NetworksConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["model-api"] = "- {cidr: anthropic}"
	if err := s.Client.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	code, msg := submitAs(t, s, "wrangler", "fetch-model", toolSpec(t, "model-api"), true)
	if code != http.StatusUnprocessableEntity || !strings.Contains(msg, "the c4-networks ConfigMap in c4-system is malformed") || !strings.Contains(msg, "farm admin") {
		t.Fatalf("%d %s", code, msg)
	}
}
