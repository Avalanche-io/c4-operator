// Package webhook turns the MVP's pod annotations into a c4 CSI volume.
//
// A pod annotated
//
//	c4.avalanche.io/materialize: <Materialization name>
//	c4.avalanche.io/mount-path:  /assets
//
// gets an inline ephemeral csi volume and a mount of it in every
// container. Nothing else changes: no init container, no hostPath, so the
// pod stays valid under Pod Security restricted. Writing the csi volume
// into the pod spec directly does the same thing without the webhook.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Names the webhook adds to a pod.
const (
	VolumeName       = "c4-view"
	DefaultMountPath = "/c4"
)

// PodMutator is the admission handler.
type PodMutator struct {
	Decoder admission.Decoder
}

// Handle mutates one pod.
func (m *PodMutator) Handle(_ context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := m.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	name := pod.Annotations[c4v1.AnnotationMaterialize]
	if name == "" {
		return admission.Allowed("no materialization requested")
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == VolumeName {
			return admission.Allowed("already injected")
		}
	}
	if err := Inject(pod, name); err != nil {
		return admission.Denied(err.Error())
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}

// Inject adds the csi volume and its mounts.
func Inject(pod *corev1.Pod, materialization string) error {
	mountPath := pod.Annotations[c4v1.AnnotationMountPath]
	if mountPath == "" {
		mountPath = DefaultMountPath
	}
	if !path.IsAbs(mountPath) || path.Clean(mountPath) != mountPath {
		return fmt.Errorf("%s must be a clean absolute path, got %q", c4v1.AnnotationMountPath, mountPath)
	}
	readOnly := pod.Annotations[c4v1.AnnotationReadOnly] == "true"
	attrs := map[string]string{c4v1.VolumeAttrMaterialization: materialization}
	if pod.Annotations[c4v1.AnnotationCapture] == "false" {
		attrs[c4v1.VolumeAttrCapture] = "false"
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: VolumeName,
		VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
			Driver:           c4v1.DriverName,
			ReadOnly:         ptr.To(readOnly),
			VolumeAttributes: attrs,
		}},
	})
	mount := corev1.VolumeMount{Name: VolumeName, MountPath: mountPath, ReadOnly: readOnly}
	for _, list := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range list {
			for _, vm := range list[i].VolumeMounts {
				if vm.MountPath == mountPath {
					return fmt.Errorf("container %q already mounts something at %s", list[i].Name, mountPath)
				}
			}
			list[i].VolumeMounts = append(list[i].VolumeMounts, mount)
		}
	}
	return nil
}
