package localfarm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// up waits on the farm's deployments and its HUD, and when one does not
// come it says what it last saw, in the farm's words: the pods that are
// not running and why (with the last line a crashing one logged), or what
// the HUD answered.

// logClient is a client that also reads a container's log, for the last
// line of one that crashed.
type logClient struct {
	client.Client
	cs kubernetes.Interface
}

// lastLog is the last line ns/pod's container logged, its previous run's
// when it has restarted; "" when there is none to read.
func (c logClient) lastLog(ctx context.Context, ns, pod, container string, previous bool) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tail := int64(20)
	rc, err := c.cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: previous, TailLines: &tail}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer rc.Close()
	last := ""
	sc := bufio.NewScanner(io.LimitReader(rc, 1<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = l
		}
	}
	return last
}

// rolledOut says a deployment runs its current template on every replica
// and nothing older, as kubectl rollout status does: the controller has
// seen the current generation, every replica is updated, no older one is
// left, and the updated ones are available. Under a rolling update the old
// pod stays available while the new one is made, so updated and available
// alone pass before the new pod runs.
func rolledOut(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation && s.UpdatedReplicas == want && s.Replicas == want &&
		s.AvailableReplicas == want && s.UnavailableReplicas == 0
}

func waitDeployment(ctx context.Context, c client.Client, ns, name string, limit time.Duration) error {
	var d appsv1.Deployment
	err := poll(ctx, limit, "deployment "+name, func() (bool, error) {
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d); err != nil {
			return false, nil
		}
		return rolledOut(&d), nil
	})
	if err == nil || d.Name == "" {
		return err
	}
	if why := podTrouble(ctx, c, ns, d.Spec.Selector.MatchLabels); why != "" {
		return fmt.Errorf("%w: %s", err, why)
	}
	return err
}

// podTrouble says, of the pods matching sel, each that is not running and
// ready and why: Pending (and its scheduling message), a container waiting
// (CrashLoopBackOff, ImagePullBackOff, with its message) or ended, and the
// last line a crashing container logged. "" when every pod is well.
func podTrouble(ctx context.Context, c client.Client, ns string, sel map[string]string) string {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(sel)}); err != nil {
		return ""
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	var out []string
	for _, p := range pods.Items {
		if why := podWhy(ctx, c, &p); why != "" {
			out = append(out, p.Name+" "+why)
		}
	}
	return strings.Join(out, "; ")
}

func podWhy(ctx context.Context, c client.Client, p *corev1.Pod) string {
	if p.Status.Phase == corev1.PodPending && len(p.Status.ContainerStatuses) == 0 {
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
				return "is Pending: " + cond.Message
			}
		}
		return "is Pending"
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			continue
		}
		var why string
		switch {
		case cs.State.Waiting != nil:
			why = cs.State.Waiting.Reason
			if m := cs.State.Waiting.Message; m != "" && cs.State.Waiting.Reason != "CrashLoopBackOff" {
				why += " (" + m + ")"
			}
		case cs.State.Terminated != nil:
			why = fmt.Sprintf("ended: %s, exit code %d", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		default:
			why = "is not ready"
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			why += fmt.Sprintf(", restarted %d times (last %s, exit code %d)", cs.RestartCount, t.Reason, t.ExitCode)
		}
		if lc, ok := c.(logClient); ok && (cs.RestartCount > 0 || cs.State.Terminated != nil) {
			if l := lc.lastLog(ctx, p.Namespace, p.Name, cs.Name, cs.State.Terminated == nil); l != "" {
				why += `, its last log line: "` + l + `"`
			}
		}
		return why
	}
	return ""
}

// waitHUD waits for the HUD to answer a development viewer, with the
// installer token it asks of one. When it does not, it says what the HUD
// last answered (or that nothing answered at its address) and what of the
// manager is not running.
func waitHUD(ctx context.Context, c client.Client, hud, installer string, limit time.Duration) error {
	u := hud + "/api/v1/snapshot?as=wrangler"
	last := ""
	err := poll(ctx, limit, "the HUD", func() (bool, error) {
		ok, said, err := askHUD(ctx, u, installer)
		if said != "" {
			last = said
		}
		return ok, err
	})
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	if last != "" {
		err = fmt.Errorf("%w: %s", err, last)
	}
	if c != nil {
		if why := podTrouble(ctx, c, "c4-system", map[string]string{"app.kubernetes.io/name": "c4-operator", "app.kubernetes.io/component": "manager"}); why != "" {
			err = fmt.Errorf("%w; the manager: %s", err, why)
		}
	}
	return err
}

// askHUD asks the HUD once: ok when it answers 200, else what it said in
// words, for the error that ends the wait.
func askHUD(ctx context.Context, u, installer string) (bool, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, "", err
	}
	if installer != "" {
		req.Header.Set(InstallerHeader, installer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return false, "nothing listens at " + req.URL.Host, nil
		}
		return false, "it did not answer: " + err.Error(), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, "", nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	msg := strings.TrimSpace(string(body))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	said := fmt.Sprintf("the HUD answered %d", resp.StatusCode)
	if msg != "" {
		said += ": " + msg
	}
	return false, said, nil
}
