package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A host runner signs its requests with its own key (request/v1) and the
// HUD takes them as its machine's account, as it takes that account's
// token: it reads its own Node's pods. The signature holds the request to
// its method, target and body, and is taken once; another machine's
// runner is still refused its neighbour's pods, and a person's signed
// request is not taken here yet.
func TestHostRunnerSignsItsRequests(t *testing.T) {
	yes := true
	job := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "port", UID: "9c1d2e7f-port",
		Annotations: map[string]string{c4v1.AnnotationOwner: "dana"}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "port-0", Labels: map[string]string{c4v1.LabelJob: "port"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: c4v1.GroupVersion.String(), Kind: "WorkJob", Name: "port", UID: job.UID, Controller: &yes}}},
		Spec: corev1.PodSpec{NodeName: "laptop-1", Containers: []corev1.Container{{Name: "member", Image: "c4-agent"}}}}
	s := hostFarm(t, job, pod)
	f := provtest.NewFarm(t, time.Now())
	s.trusted, s.keysUntil = trustSet{roots: f.Roots, legacy: map[string]ed25519.PublicKey{}, farm: f.ID}, time.Now().Add(time.Hour)
	runner := f.Part(t, provenance.RunnerIdentity("laptop-1"))
	other := f.Part(t, provenance.RunnerIdentity("laptop-2"))

	send := func(method, target string, body []byte, sig string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, target, bytes.NewReader(body))
		r.Header.Set(schema.RequestHeader, sig)
		s.Handler().ServeHTTP(w, r)
		return w
	}
	const pods = "/api/v1/nodes/laptop-1/pods"

	sig := f.SignRequest(t, runner, http.MethodGet, pods, nil)
	w := send(http.MethodGet, pods, nil, sig)
	var got api.HostPods
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || len(got.Pods) != 1 || got.Pods[0].Job == nil || got.Pods[0].Job.Owner != "dana" {
		t.Fatalf("its own pods, signed: %d %s", w.Code, w.Body.String())
	}
	// a signed report: the handler reads the body the signature covers
	report := []byte(`{"owners":["dana"],"sessions":[]}`)
	if w := send(http.MethodPost, "/api/v1/nodes/laptop-1/observed", report,
		f.SignRequest(t, runner, http.MethodPost, "/api/v1/nodes/laptop-1/observed", report)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "shownTo") {
		t.Fatalf("its signed report: %d %s", w.Code, w.Body.String())
	}
	if w := send(http.MethodGet, pods, nil, sig); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "taken once") {
		t.Fatalf("the same signed request twice: %d %s", w.Code, w.Body.String())
	}

	refused := map[string]struct {
		method, target string
		body           []byte
		sig            string
		code           int
	}{
		"another machine's runner": {http.MethodGet, pods, nil, f.SignRequest(t, other, http.MethodGet, pods, nil), http.StatusForbidden},
		"signed for another target": {http.MethodGet, pods, nil,
			f.SignRequest(t, runner, http.MethodGet, "/api/v1/nodes/laptop-1/pods?all=1", nil), http.StatusUnauthorized},
		"signed for another body": {http.MethodPost, "/api/v1/nodes/laptop-1/observed", []byte(`{"pods":[]}`),
			f.SignRequest(t, runner, http.MethodPost, "/api/v1/nodes/laptop-1/observed", []byte(`{"pods":[{}]}`)), http.StatusUnauthorized},
		"signed as another method": {http.MethodPost, "/api/v1/nodes/laptop-1/observed", []byte(`{"pods":[]}`),
			f.SignRequest(t, runner, http.MethodGet, "/api/v1/nodes/laptop-1/observed", []byte(`{"pods":[]}`)), http.StatusUnauthorized},
		"a person's": {http.MethodGet, pods, nil, f.SignRequest(t, f.Person(t, "dana", false), http.MethodGet, pods, nil), http.StatusUnauthorized},
		"not base64": {http.MethodGet, pods, nil, "%%%", http.StatusUnauthorized},
	}
	for name, c := range refused {
		w := send(c.method, c.target, c.body, c.sig)
		if w.Code != c.code {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}
