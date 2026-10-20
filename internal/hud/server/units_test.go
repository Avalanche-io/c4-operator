package server

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// unitToken is a token whose claims say it is for the HUD, as a pod's
// projected token is; the fake review decides who it is.
func unitToken(pod string) string {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"aud":["` + c4v1.HUDAudience + `"],"sub":"` + pod + `"}`))
	return "eyJhbGciOiJSUzI1NiJ9." + claims + ".sig"
}

// A unit holding its pod's HUD token acts as its tool grants: job
// lets it steer its own job and no other, read lets it only look. A token
// the review rejects, or for a pod that is no unit, is refused.
func TestUnitTokens(t *testing.T) {
	s := farm(t)
	s.DevViewers = nil
	s.Namespace = "c4-system"
	rec := s.m.byKey["show-a/sh040-light-v007"]
	other := s.m.byKey["show-a/sh050-comp-v001"]
	if other == nil {
		t.Fatal("the farm has no second job in show-a")
	}
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-6", "running": "7-9"}
	pod := func(name, uid, tool string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: name, UID: k8stypes.UID("uid-" + uid),
			Labels: map[string]string{c4v1.LabelJob: rec.job.Name, c4v1.LabelUnit: "8", c4v1.LabelTool: tool}}}
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	objs := []client.Object{rec.job.DeepCopy(), pod("lead-u8", "lead", "lead"), pod("reader-u8", "reader", "reader"),
		&c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "lead"}, Spec: c4v1.ToolSpec{C4: "job"}},
		&c4v1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: "reader"}, Spec: c4v1.ToolSpec{C4: "read"}}}
	objs = append(objs, other.job.DeepCopy())
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	kube := kfake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		tr := a.(ktesting.CreateAction).GetObject().(*authnv1.TokenReview)
		claims, _ := base64.RawURLEncoding.DecodeString(strings.Split(tr.Spec.Token, ".")[1])
		name := strings.TrimSuffix(strings.SplitAfter(string(claims), `"sub":"`)[1], `"}`)
		if name == "forged-u8" || len(tr.Spec.Audiences) != 1 || tr.Spec.Audiences[0] != c4v1.HUDAudience {
			return true, &authnv1.TokenReview{}, nil
		}
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{Authenticated: true, Audiences: tr.Spec.Audiences,
			User: authnv1.UserInfo{Username: "system:serviceaccount:show-a:default", Extra: map[string]authnv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {name}, "authentication.kubernetes.io/pod-uid": {"uid-" + strings.TrimSuffix(name, "-u8")}}}}}, nil
	})
	s.Kube = kube
	do := func(method, path, tok, body string) (int, string) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		s.Handler().ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	steer := fmt.Sprintf("/api/v1/jobs/show-a/%s/steer", rec.job.Name)
	if code, out := do(http.MethodPost, steer, unitToken("lead-u8"), `{"unit": 9, "text": "the API is frozen"}`); code != http.StatusOK {
		t.Fatalf("a job grant steering its own job: %d %s", code, out)
	}
	var live c4v1.WorkJob
	s.Client.Get(t.Context(), client.ObjectKeyFromObject(&rec.job), &live)
	if !strings.Contains(live.Annotations[c4v1.AnnotationSteer], `"actor":"unit:show-a/`+rec.job.Name+`/8"`) {
		t.Fatalf("the request's actor: %s", live.Annotations[c4v1.AnnotationSteer])
	}
	if code, _ := do(http.MethodPost, fmt.Sprintf("/api/v1/jobs/show-a/%s/steer", other.job.Name), unitToken("lead-u8"), `{"unit": 0, "text": "hi"}`); code != http.StatusForbidden {
		t.Fatalf("a job grant steering another job: %d", code)
	}
	if code, _ := do(http.MethodPost, steer, unitToken("reader-u8"), `{"unit": 9, "text": "hi"}`); code != http.StatusForbidden {
		t.Fatalf("a read grant steering: %d", code)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	r.Header.Set("Authorization", "Bearer "+unitToken("reader-u8"))
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a read grant reading: %d %s", w.Code, w.Body.String())
	}
	if code, _ := do(http.MethodPost, steer, unitToken("forged-u8"), `{"unit": 9, "text": "hi"}`); code != http.StatusUnauthorized {
		t.Fatalf("a token the review rejects: %d", code)
	}
}
