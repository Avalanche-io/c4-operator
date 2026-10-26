package localfarm

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

// The token's file is one the manager can read: its mode and the pod's
// fsGroup against the user the manager runs as (config/manager), never
// the mode alone. v0.2.0 mounted it 0400 with no fsGroup, root's alone, and
// the manager, user 65532, exited at start.
func TestTheManagerCanReadItsToken(t *testing.T) {
	var p struct {
		Spec struct {
			Template struct {
				Spec struct {
					SecurityContext struct {
						FSGroup *int64 `json:"fsGroup"`
					} `json:"securityContext"`
					Volumes []struct {
						Name   string `json:"name"`
						Secret *struct {
							DefaultMode *int32 `json:"defaultMode"`
						} `json:"secret"`
					} `json:"volumes"`
				}
			}
		}
	}
	if err := json.Unmarshal(ManagerPatch(Options{Owner: "noor"}, []string{"manager"}), &p); err != nil {
		t.Fatal(err)
	}
	var mode int32 = 0o644 // a Secret volume's default
	for _, v := range p.Spec.Template.Spec.Volumes {
		if v.Name == "installer" && v.Secret != nil && v.Secret.DefaultMode != nil {
			mode = *v.Secret.DefaultMode
		}
	}
	b, err := os.ReadFile("../../config/manager/manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	if err := yaml.Unmarshal(b, &dep); err != nil {
		t.Fatal(err)
	}
	sc := dep.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser == 0 {
		t.Fatalf("the manager's user: %+v", sc)
	}
	fsGroup := p.Spec.Template.Spec.SecurityContext.FSGroup
	// without fsGroup the file is root:root; with it, the group is fsGroup,
	// which the pod's user is in
	readable := mode&0o004 != 0 || (fsGroup != nil && mode&0o040 != 0)
	if !readable {
		t.Fatalf("user %d cannot read a %#o file (fsGroup %v)", *sc.RunAsUser, mode, fsGroup)
	}
	if fsGroup == nil || *fsGroup != *sc.RunAsUser || mode&0o007 != 0 {
		t.Errorf("the token is %#o, fsGroup %v: it should be the manager's group's alone", mode, fsGroup)
	}
}

// A deployment has rolled out only when its current template runs on every
// replica and no older pod is left: a rolling update keeps the old pod
// available while the new one crashes, and that is not done.
func TestRolledOut(t *testing.T) {
	one := int32(1)
	d := func(gen, observed int64, replicas, updated, available, unavailable int32) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: gen}, Spec: appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{ObservedGeneration: observed, Replicas: replicas, UpdatedReplicas: updated, AvailableReplicas: available, UnavailableReplicas: unavailable}}
	}
	for name, tc := range map[string]struct {
		d    *appsv1.Deployment
		want bool
	}{
		"the new pod runs":                   {d(2, 2, 1, 1, 1, 0), true},
		"the old pod answers, the new is up": {d(2, 2, 2, 1, 1, 1), false},
		"the new pod crashes":                {d(2, 2, 2, 1, 1, 1), false},
		"the controller has not seen it":     {d(3, 2, 1, 1, 1, 0), false},
		"nothing available":                  {d(2, 2, 1, 1, 0, 1), false},
	} {
		if got := rolledOut(tc.d); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// A pod that does not run says why, in Kubernetes' words, and a crashing
// one its last log line.
func TestPodTrouble(t *testing.T) {
	sel := map[string]string{"app.kubernetes.io/component": "manager"}
	pod := func(name string, st corev1.PodStatus) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "c4-system", Labels: sel}, Status: st}
	}
	crash := pod("c4-manager-new", corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "manager", RestartCount: 5,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 2m40s"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}},
	}}})
	pull := pod("c4-manager-pull", corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "manager", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: `Back-off pulling image "c4-operator:dev"`}},
	}}})
	pending := pod("c4-manager-unscheduled", corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/5 nodes are available: 5 Insufficient memory.",
	}}})
	well := pod("c4-manager-old", corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "manager", Ready: true}}})

	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(crash, pull, pending, well).Build()
	got := podTrouble(context.Background(), logClient{Client: c, cs: k8sfake.NewSimpleClientset()}, "c4-system", sel)
	for _, want := range []string{
		`c4-manager-new CrashLoopBackOff, restarted 5 times (last Error, exit code 1), its last log line: "fake logs"`,
		`c4-manager-pull ImagePullBackOff (Back-off pulling image "c4-operator:dev")`,
		"c4-manager-unscheduled is Pending: 0/5 nodes are available: 5 Insufficient memory.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "c4-manager-old") {
		t.Errorf("a running pod is named: %s", got)
	}
}

// The HUD's answer is said as the farm said it: a status and its error, or
// that nothing listens at its address; the wait's error ends on the last.
func TestWaitHUDSaysWhatItLastSaw(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get(InstallerHeader) {
		case "ok":
			w.Write([]byte("{}"))
		case "":
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"development viewers need this machine's installer token: run c4 farm open"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"the HUD is still reading the farm"}`))
		}
	}))
	defer srv.Close()
	u := srv.URL + "/api/v1/snapshot?as=wrangler"
	if ok, said, err := askHUD(ctx, u, "ok"); !ok || said != "" || err != nil {
		t.Errorf("200: %v %q %v", ok, said, err)
	}
	if _, said, _ := askHUD(ctx, u, "reading"); said != "the HUD answered 503: the HUD is still reading the farm" {
		t.Errorf("503: %q", said)
	}
	if _, said, _ := askHUD(ctx, u, ""); !strings.HasPrefix(said, "the HUD answered 401: development viewers need") {
		t.Errorf("401: %q", said)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().String()
	l.Close()
	if _, said, _ := askHUD(ctx, "http://"+closed+"/api/v1/snapshot", ""); said != "nothing listens at "+closed {
		t.Errorf("closed: %q", said)
	}

	err := waitHUD(ctx, nil, srv.URL, "reading", time.Millisecond)
	if err == nil || err.Error() != "the HUD was not ready after 1ms: the HUD answered 503: the HUD is still reading the farm" {
		t.Errorf("wait: %v", err)
	}
}
