package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	c4view "github.com/Avalanche-io/c4-operator/internal/view"
)

// A decision on a wait that asks for a reply carries one: a text, which
// the HUD stores as a view of one file, reply.md, or a view already
// stored. A wait asking for one refuses a decision without it, and a
// wait asking for none refuses one.
func TestDecideWithAReply(t *testing.T) {
	s := farm(t)
	ctx := context.Background()
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	s.Stores = func(context.Context, string) (*storeurl.Sharded, error) { return st, nil }
	rec := s.m.byKey["show-a/sh040-light-v007"]
	since := metav1.NewTime(time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC))
	rec.job.Status.UnitStates = map[string]string{"succeeded": "0-6", "waits": "7-8", "blocked": "9"}
	rec.job.Status.Waits = []c4v1.WorkWait{
		{Unit: 8, Name: "brief", Server: "person:dana", Ask: "what should the draft say?", Reply: true, Since: &since, Blocking: true},
		{Unit: 7, Name: "ruling", Server: "person:dana", Since: &since, Blocking: true},
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec.job.DeepCopy()).Build()
	post := func(body string) (int, string) {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sh040-light-v007/decide?as=wrangler", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	stored := c4.Identify(strings.NewReader("a view already stored")).String()
	for body, want := range map[string]int{
		`{"unit": "brief", "do": "approve"}`:                                                   http.StatusBadRequest, // asks for a reply
		`{"unit": "brief", "do": "approve", "reply": {}}`:                                      http.StatusBadRequest,
		`{"unit": "brief", "do": "approve", "reply": {"view": "nope"}}`:                        http.StatusBadRequest,
		`{"unit": "brief", "do": "approve", "reply": {"view": "` + stored + `", "text": "x"}}`: http.StatusBadRequest,
		`{"unit": "ruling", "do": "approve", "reply": {"text": "x"}}`:                          http.StatusBadRequest, // asks for none
	} {
		if code, out := post(body); code != want {
			t.Errorf("%s: %d %s, want %d", body, code, out, want)
		}
	}
	if code, out := post(`{"unit": "brief", "do": "approve", "reply": {"text": "Keep the chapter whole."}}`); code != http.StatusOK {
		t.Fatalf("decide with a text: %d %s", code, out)
	}
	if code, out := post(`{"unit": "brief", "do": "approve", "reply": {"view": "` + stored + `"}}`); code != http.StatusOK {
		t.Fatalf("decide with a view: %d %s", code, out)
	}
	if code, out := post(`{"unit": "brief", "do": "fail"}`); code != http.StatusOK {
		t.Fatalf("fail without a reply: %d %s", code, out)
	}

	var live c4v1.WorkJob
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "sh040-light-v007"}, &live); err != nil {
		t.Fatal(err)
	}
	var reqs []c4v1.WorkDecideRequest
	if err := json.Unmarshal([]byte(live.Annotations[c4v1.AnnotationDecide]), &reqs); err != nil || len(reqs) != 3 {
		t.Fatalf("requests: %v %s", err, live.Annotations[c4v1.AnnotationDecide])
	}
	if reqs[1].View != stored || reqs[2].View != "" {
		t.Fatalf("requests: %+v", reqs)
	}
	// The text is a view of one file, reply.md, in the project's store.
	id, err := c4.Parse(reqs[0].View)
	if err != nil {
		t.Fatalf("the text's view %q: %v", reqs[0].View, err)
	}
	v, err := c4view.Load(ctx, id, c4view.StoreRecords(st), 1)
	if err != nil {
		t.Fatal(err)
	}
	e := v.Root.Files["reply.md"]
	if e == nil || len(v.Root.Files) != 1 {
		t.Fatalf("the reply view's files: %v", v.Root.Files)
	}
	r, err := st.Open(e.C4ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if b, _ := io.ReadAll(r); string(b) != "Keep the chapter whole." {
		t.Fatalf("reply.md is %q", b)
	}
}
