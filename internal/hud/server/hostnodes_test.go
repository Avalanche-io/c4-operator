package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

func hostFarm(t *testing.T, objs ...runtime.Object) *Server {
	t.Helper()
	sch := runtime.NewScheme()
	clientgoscheme.AddToScheme(sch)
	c4v1.AddToScheme(sch)
	host := func(n string) *api.Viewer { return &api.Viewer{User: HostAccountPrefix + n} }
	return &Server{Loc: time.UTC, Log: zap.New(zap.WriteTo(&bytes.Buffer{})), m: &model{at: time.Now(), byKey: map[string]*jobRec{}, nsProject: map[string]string{}},
		Client: fake.NewClientBuilder().WithScheme(sch).WithRuntimeObjects(objs...).Build(),
		DevViewers: map[string]*api.Viewer{"host1": host("laptop-1"), "host2": host("laptop-2"),
			"dana": {User: "dana", Projects: []string{}}, "lee": {User: "lee", Projects: []string{}}}, DevViewersOpen: true}
}

func hostCall(t *testing.T, s *Server, method, path, as string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, path+"?as="+as, rd))
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func hostNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{hostLabel: name}},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: hostTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}}}}
}

// A host runner reads only the pods bound to its own Node, through the HUD,
// each with its job's owner, UID and grant; no one else reads them there.
func TestHostRunnerReadsOnlyItsOwnPods(t *testing.T) {
	yes := true
	job := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "port", UID: "9c1d2e7f-port",
		Annotations: map[string]string{c4v1.AnnotationOwner: "dana", c4v1.AnnotationGrant: "c4grant"}}}
	pod := func(name, node string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name, Labels: map[string]string{c4v1.LabelJob: "port"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: "port", UID: job.UID, Controller: &yes}}},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "member", Image: "c4-agent",
				Env: []corev1.EnvVar{{Name: "C4_BLOCK_PROMPT", Value: "port the notes"}}}}}}
	}
	s := hostFarm(t, job, pod("port-0", "laptop-1"), pod("port-1", "laptop-2"), pod("port-2", ""))

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/nodes/laptop-1/pods?as=host1", nil))
	var got api.HostPods
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || len(got.Pods) != 1 {
		t.Fatalf("its own pods: %d %s", w.Code, w.Body.String())
	}
	var p corev1.Pod
	json.Unmarshal(got.Pods[0].Pod, &p)
	if p.Name != "port-0" || got.Pods[0].Job == nil || got.Pods[0].Job.Owner != "dana" || got.Pods[0].Job.UID != "9c1d2e7f-port" || got.Pods[0].Job.Grant != "c4grant" {
		t.Fatalf("pod %s job %+v", p.Name, got.Pods[0].Job)
	}
	for _, as := range []string{"host2", "dana"} {
		if code, _ := hostCall(t, s, http.MethodGet, "/api/v1/nodes/laptop-1/pods", as, nil); code != http.StatusForbidden {
			t.Errorf("%s read laptop-1's pods: %d", as, code)
		}
	}
}

// A machine is enrolled by offer and acceptance: its runner offers it with
// its own token and gets a code, and its holder (whose sign-in the runner
// uses) accepts with that code, once. The HUD writes the Node's owners as a
// JSON list. Anyone but the holder is refused with how to be added; the
// holder adds them (for). A wrong code, a second use, five wrong codes and
// anyone but the machine's own runner offering are refused.
func TestEnrollIsOfferAndAcceptance(t *testing.T) {
	s := hostFarm(t, hostNode("laptop-1"))
	offerAs := func(as, holder string) (int, string) {
		code, out := hostCall(t, s, http.MethodPost, "/api/v1/nodes/laptop-1/enroll-offer", as, map[string]string{"holder": holder})
		c, _ := out["code"].(string)
		return code, c
	}
	if code, _ := offerAs("dana", "dana"); code != http.StatusForbidden {
		t.Fatalf("a person offered a machine: %d", code)
	}
	if code, _ := offerAs("host2", "dana"); code != http.StatusForbidden {
		t.Fatalf("another machine's runner offered it: %d", code)
	}
	code, c := offerAs("host1", "dana")
	if code != http.StatusOK || len(c) != 9 || c[4] != '-' || strings.ContainsAny(c, "01OIL") {
		t.Fatalf("offer: %d %q", code, c)
	}
	if code, _ := hostCall(t, s, http.MethodPost, "/api/v1/nodes/laptop-1/enroll", "dana", map[string]string{"code": "AAAA-AAAA"}); code != http.StatusForbidden {
		t.Fatalf("a wrong code was taken: %d", code)
	}
	// lee holds the code but not the machine's sign-in
	code, out := hostCall(t, s, http.MethodPost, "/api/v1/nodes/laptop-1/enroll", "lee", map[string]string{"code": c})
	if e, _ := out["error"].(string); code != http.StatusForbidden || !strings.Contains(e, "c4 farm enroll laptop-1 --for lee") {
		t.Fatalf("not the holder: %d %v", code, out)
	}
	typed := strings.ToLower(c[:2] + " " + c[2:4] + c[5:]) // no dash, a space, lower case
	if code, out := hostCall(t, s, http.MethodPost, "/api/v1/nodes/laptop-1/enroll", "dana", map[string]string{"code": typed}); code != http.StatusOK {
		t.Fatalf("enroll: %d %v", code, out)
	}
	owners := func() string {
		var n corev1.Node
		s.Client.Get(context.Background(), types.NamespacedName{Name: "laptop-1"}, &n)
		return n.Annotations[c4v1.AnnotationNodeOwners]
	}
	if got := owners(); got != `["dana"]` {
		t.Fatalf("owners %s", got)
	}
	if code, _ := hostCall(t, s, http.MethodPost, "/api/v1/nodes/laptop-1/enroll", "dana", map[string]string{"code": c}); code != http.StatusForbidden {
		t.Fatalf("a code was used twice: %d", code)
	}
	// the holder adds lee
	_, c = offerAs("host1", "dana")
	if code, out := hostCall(t, s, http.MethodPost, "/api/v1/nodes/laptop-1/enroll", "dana", map[string]string{"code": c, "for": "lee"}); code != http.StatusOK ||
		!strings.Contains(out["message"].(string), "lee's jobs spend dana's sign-in") {
		t.Fatalf("for lee: %d %v", code, out)
	}
	if got := owners(); got != `["dana","lee"]` {
		t.Fatalf("owners %s", got)
	}
	// five wrong codes end an offer
	_, c = offerAs("host1", "dana")
	for i := 0; i < EnrollWrongCodes; i++ {
		hostCall(t, s, http.MethodPost, "/api/v1/nodes/laptop-1/enroll", "dana", map[string]string{"code": "AAAA-AAAA"})
	}
	if code, _ := hostCall(t, s, http.MethodPost, "/api/v1/nodes/laptop-1/enroll", "dana", map[string]string{"code": c}); code != http.StatusForbidden {
		t.Fatalf("an offer stood after %d wrong codes: %d", EnrollWrongCodes, code)
	}
}
