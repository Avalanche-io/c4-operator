package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// A runner's pods are the unit pods the farm bound to its Node: the queue
// creates a host pod already bound to a machine whose owners include its
// job's owner, so a runner binds nothing. An enrolled runner, with a HUD,
// reads them only through the HUD (GET /nodes/<node>/pods), each with what
// it needs of its job, so its own token reads nothing of anyone else's
// work. A runner without a HUD (a development farm, the tests) reads them
// from the API server.

// podJob is what a runner needs of a pod's job: whose work it is, its UID
// (a job made again under the same name is another), the grant it was
// submitted under, and its project.
type podJob struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	Owner   string `json:"owner"`
	Grant   string `json:"grant,omitempty"`
	Project string `json:"project,omitempty"`
}

// readsHUD reports whether the runner reads its pods through the HUD.
func (r *Runner) readsHUD() bool { return r.HUD != "" && r.HUDToken != nil && r.PodsThroughHUD }

// ownPods are the unit pods bound to the runner's Node.
func (r *Runner) ownPods(ctx context.Context) ([]corev1.Pod, error) {
	if r.jobs == nil {
		r.jobs = map[types.UID]podJob{}
	}
	if !r.readsHUD() {
		var pods corev1.PodList
		if err := r.Client.List(ctx, &pods, client.HasLabels{c4v1.LabelJob}); err != nil {
			return nil, err
		}
		var out []corev1.Pod
		for _, p := range pods.Items {
			if p.Spec.NodeName == r.Node {
				out = append(out, p)
			}
		}
		return out, nil
	}
	u := strings.TrimRight(r.HUD, "/") + "/api/v1/nodes/" + url.PathEscape(r.Node) + "/pods"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if t := r.HUDToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	hc := r.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("this machine's pods from the farm: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("this machine's pods from the farm: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var body struct {
		Pods []struct {
			Pod json.RawMessage `json:"pod"`
			Job *podJob         `json:"job"`
		} `json:"pods"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, fmt.Errorf("this machine's pods from the farm: %w", err)
	}
	out := make([]corev1.Pod, 0, len(body.Pods))
	for _, hp := range body.Pods {
		var p corev1.Pod
		if json.Unmarshal(hp.Pod, &p) != nil || p.Spec.NodeName != r.Node {
			continue
		}
		if hp.Job != nil {
			r.jobs[p.UID] = *hp.Job
			if hp.Job.Project != "" {
				if r.nsProject == nil {
					r.nsProject = map[string]string{}
				}
				r.nsProject[p.Namespace] = hp.Job.Project
			}
		} else {
			delete(r.jobs, p.UID)
		}
		out = append(out, p)
	}
	return out, nil
}

// jobOf is a pod's job, as the HUD gave it with the pod, or read from the
// API server by the pod's controller reference. ok is false, with why, for
// a pod of no job this runner can find.
func (r *Runner) jobOf(ctx context.Context, p *corev1.Pod) (j podJob, why string, err error) {
	ref := metav1.GetControllerOf(p)
	if ref == nil || ref.Kind != "WorkJob" || ref.APIVersion != c4v1.GroupVersion.String() {
		return podJob{}, "the pod is not a unit of a WorkJob", nil
	}
	if r.readsHUD() {
		j, ok := r.jobs[p.UID]
		switch {
		case !ok:
			return podJob{}, fmt.Sprintf("its job %s/%s does not exist", p.Namespace, ref.Name), nil
		case j.UID != string(ref.UID):
			return podJob{}, fmt.Sprintf("its controller reference names an earlier job %s/%s", p.Namespace, ref.Name), nil
		}
		return j, "", nil
	}
	var w c4v1.WorkJob
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: ref.Name}, &w)
	if apierrors.IsNotFound(err) {
		return podJob{}, fmt.Sprintf("its job %s/%s does not exist", p.Namespace, ref.Name), nil
	}
	if err != nil {
		return podJob{}, "", fmt.Errorf("its job %s/%s: %w", p.Namespace, ref.Name, err)
	}
	if w.UID != ref.UID {
		return podJob{}, fmt.Sprintf("its controller reference names an earlier job %s/%s", p.Namespace, ref.Name), nil
	}
	return podJob{Name: w.Name, UID: string(w.UID), Owner: w.Annotations[c4v1.AnnotationOwner], Grant: w.Annotations[c4v1.AnnotationGrant]}, "", nil
}

// freshPod is a pod as the farm has it now, with what the runner wrote to
// it since it was read: through the HUD, or from the API server.
func (r *Runner) freshPod(ctx context.Context, p *corev1.Pod) (*corev1.Pod, bool) {
	if !r.readsHUD() {
		var fresh corev1.Pod
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, &fresh); err != nil || fresh.UID != p.UID {
			return nil, false
		}
		return &fresh, true
	}
	pods, err := r.ownPods(ctx)
	if err != nil {
		return nil, false
	}
	for i := range pods {
		if pods[i].UID == p.UID {
			return &pods[i], true
		}
	}
	return nil, false
}
