// Package webhook injects materialization into annotated pods.
//
// A pod annotated
//
//	c4.avalanche.io/materialize: <Materialization name>
//	c4.avalanche.io/mount-path:  /assets
//
// gets an init container that asks the node agent for the view, and a
// read-only mount of the view in every other container. The workload's
// image, command, and spec are otherwise untouched.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=c4.avalanche.io,resources=materializations,verbs=get;list;watch
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;update;patch

// Names the webhook adds to a pod.
const (
	InitContainerName = "c4-materialize"
	AgentVolumeName   = "c4-agent"
	ViewVolumeName    = "c4-view"
	agentMountPath    = "/run/c4"
	DefaultMountPath  = "/c4"
)

// PodMutator is the admission handler.
type PodMutator struct {
	Client    client.Reader
	Decoder   admission.Decoder
	InitImage string
	HostRoot  string // node path of the agent's cache, e.g. /var/lib/c4
}

// Handle mutates one pod.
func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := m.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	name := pod.Annotations[c4v1.AnnotationMaterialize]
	if name == "" {
		return admission.Allowed("no materialization requested")
	}
	if pod.Annotations[c4v1.AnnotationView] != "" {
		return admission.Allowed("already injected")
	}
	ns := req.Namespace
	if ns == "" {
		ns = pod.Namespace
	}

	var mat c4v1.Materialization
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &mat); err != nil {
		return admission.Denied(fmt.Sprintf("materialization %q: %v", name, err))
	}
	st := mat.Status
	if st.ViewID == "" || st.ObservedGeneration != mat.Generation {
		return admission.Denied(fmt.Sprintf("materialization %q is not resolved yet (phase %q); retry when it is", name, st.Phase))
	}
	if err := Inject(pod, ns, name, st.ViewID, m.InitImage, m.HostRoot); err != nil {
		return admission.Denied(err.Error())
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}

// Inject adds the init container, volumes, and mounts to pod.
//
// A Materialization whose content is missing is still admitted: the init
// container's preflight fails, the workload never starts, and a pod that
// restarts on failure picks the content up once it is uploaded.
func Inject(pod *corev1.Pod, namespace, name, viewID, image, hostRoot string) error {
	mountPath := pod.Annotations[c4v1.AnnotationMountPath]
	if mountPath == "" {
		mountPath = DefaultMountPath
	}
	if !path.IsAbs(mountPath) || path.Clean(mountPath) != mountPath {
		return fmt.Errorf("%s must be a clean absolute path, got %q", c4v1.AnnotationMountPath, mountPath)
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == AgentVolumeName || v.Name == ViewVolumeName {
			return fmt.Errorf("pod already has a volume named %q", v.Name)
		}
	}

	pod.Annotations[c4v1.AnnotationView] = viewID
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		corev1.Volume{Name: AgentVolumeName, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: filepath.Join(hostRoot, "agent"), Type: ptr.To(corev1.HostPathDirectory),
		}}},
		// The view directory does not exist until the init container
		// has run, so no type check: kubelet bind-mounts it when each
		// regular container starts, after init has finished.
		corev1.Volume{Name: ViewVolumeName, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: filepath.Join(hostRoot, "views", viewID),
		}}},
	)

	viewMount := corev1.VolumeMount{Name: ViewVolumeName, MountPath: mountPath, ReadOnly: true}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		for _, vm := range c.VolumeMounts {
			if vm.MountPath == mountPath {
				return fmt.Errorf("container %q already mounts something at %s", c.Name, mountPath)
			}
		}
		c.VolumeMounts = append(c.VolumeMounts, viewMount)
	}
	// Later init containers see the tree too.
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		c.VolumeMounts = append(c.VolumeMounts, viewMount)
	}

	initC := corev1.Container{
		Name:  InitContainerName,
		Image: image,
		Args: []string{
			"ensure",
			"--socket=" + agentMountPath + "/agent.sock",
			"--namespace=" + namespace,
			"--name=" + name,
			"--view=" + viewID,
		},
		VolumeMounts: []corev1.VolumeMount{{Name: AgentVolumeName, MountPath: agentMountPath}},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To[int64](65532),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	pod.Spec.InitContainers = append([]corev1.Container{initC}, pod.Spec.InitContainers...)
	return nil
}
