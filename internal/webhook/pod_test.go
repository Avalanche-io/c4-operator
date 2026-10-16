package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	jsonpatchapply "github.com/evanphx/json-patch/v5"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

const viewID = "c4XGqpTTFUafKixXmYrjGcU7mvkbaTvYxSqfC6mmD6UnyGvTLK3bLMa9uBWRHC5dLxkU5BkVZjaatjgM4jpWoyWHuWK"

func handler(t *testing.T, objs ...runtime.Object) *PodMutator {
	t.Helper()
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	c4v1.AddToScheme(s)
	return &PodMutator{
		Client:    fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...).Build(),
		Decoder:   admission.NewDecoder(s),
		InitImage: "c4-operator:test",
		HostRoot:  "/var/lib/c4",
	}
}

func mat(name, view, phase string) *c4v1.Materialization {
	return &c4v1.Materialization{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "render", Generation: 1},
		Status:     c4v1.MaterializationStatus{ViewID: view, Phase: phase, ObservedGeneration: 1},
	}
}

func request(t *testing.T, pod *corev1.Pod) admission.Request {
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Namespace: "render",
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func renderPod(ann map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "render-0", Namespace: "render", Annotations: ann},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "render", Image: "renderer:1"},
			{Name: "sidecar", Image: "logger:1"},
		}},
	}
}

// apply applies the handler's JSON patches to pod.
func apply(t *testing.T, pod *corev1.Pod, patches []jsonpatch.Operation) *corev1.Pod {
	t.Helper()
	raw, _ := json.Marshal(pod)
	ops, _ := json.Marshal(patches)
	p, err := jsonpatchapply.DecodePatch(ops)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Apply(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got corev1.Pod
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func TestInjects(t *testing.T) {
	h := handler(t, mat("plates", viewID, c4v1.PhaseReady))
	pod := renderPod(map[string]string{
		c4v1.AnnotationMaterialize: "plates",
		c4v1.AnnotationMountPath:   "/assets",
	})
	resp := h.Handle(context.Background(), request(t, pod))
	if !resp.Allowed || len(resp.Patches) == 0 {
		t.Fatalf("not mutated: %+v", resp.Result)
	}

	want := apply(t, pod, resp.Patches)
	if want.Annotations[c4v1.AnnotationView] != viewID {
		t.Fatal("view annotation missing")
	}
	ic := want.Spec.InitContainers[0]
	if ic.Name != InitContainerName || !strings.Contains(strings.Join(ic.Args, " "), "--view="+viewID) {
		t.Fatalf("init container %+v", ic)
	}
	for _, c := range want.Spec.Containers {
		m := c.VolumeMounts[len(c.VolumeMounts)-1]
		if m.MountPath != "/assets" || !m.ReadOnly || m.Name != ViewVolumeName {
			t.Fatalf("container %s mount %+v", c.Name, m)
		}
	}
	var viewVol *corev1.Volume
	for i := range want.Spec.Volumes {
		if want.Spec.Volumes[i].Name == ViewVolumeName {
			viewVol = &want.Spec.Volumes[i]
		}
	}
	if viewVol == nil || viewVol.HostPath.Path != "/var/lib/c4/views/"+viewID {
		t.Fatalf("view volume %+v", viewVol)
	}
	// The workload's image and command are untouched.
	if want.Spec.Containers[0].Image != "renderer:1" || want.Spec.Containers[0].Command != nil {
		t.Fatal("workload container modified")
	}
}

func TestIgnoresUnannotatedPods(t *testing.T) {
	h := handler(t)
	resp := h.Handle(context.Background(), request(t, renderPod(nil)))
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatal("touched a pod that asked for nothing")
	}
}

func TestDenies(t *testing.T) {
	h := handler(t, mat("pending", "", c4v1.PhasePending), mat("ok", viewID, c4v1.PhaseReady))
	cases := map[string]map[string]string{
		"not found":    {c4v1.AnnotationMaterialize: "nope"},
		"not resolved": {c4v1.AnnotationMaterialize: "pending"},
		"bad path":     {c4v1.AnnotationMaterialize: "ok", c4v1.AnnotationMountPath: "assets/../x"},
	}
	for name, ann := range cases {
		resp := h.Handle(context.Background(), request(t, renderPod(ann)))
		if resp.Allowed {
			t.Errorf("%s: allowed", name)
		}
	}
}

func TestAdmitsFailedSoInitContainerGuards(t *testing.T) {
	h := handler(t, mat("missing", viewID, c4v1.PhaseFailed))
	resp := h.Handle(context.Background(), request(t, renderPod(map[string]string{c4v1.AnnotationMaterialize: "missing"})))
	if !resp.Allowed || len(resp.Patches) == 0 {
		t.Fatal("a Materialization with missing content should still be injected; its init container fails first")
	}
}
