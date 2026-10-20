package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// A client that was away reads the transitions after its cursor, in order,
// from sealed segments and the tail alike, only in projects it sees, a
// page at a time; a cursor the log no longer reaches is told so.
func TestBookSince(t *testing.T) {
	s := submitFarm(t)
	s.Namespace = "c4-system"
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	s.Client = fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	tr := func(seq int64, project, kind string) api.Transition {
		return api.Transition{Seq: seq, At: at, Project: project, Job: project + "/sh040", Kind: kind, Message: kind, Key: kind}
	}
	seg := queue.BookSegmentObject{Kind: "c4.avalanche.io/work-book-segment", Project: "show-a", First: 1, Last: 3,
		Entries: []api.Transition{tr(1, "show-a", "phase"), tr(2, "show-a", "wait-opened"), tr(3, "show-a", "wait-decided")}}
	raw, _ := json.Marshal(seg)
	st, _ := s.Stores(ctx, "show-a")
	id, err := st.PutBytes(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	project := func(p string, data map[string]string) *corev1.ConfigMap {
		data["project"] = p
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: queue.BookProjectName(p),
			Labels: map[string]string{queue.BookLabel: p}}, Data: data}
	}
	cm := project("show-a", map[string]string{
		"segments": enc([]queue.BookSegment{{First: 1, Last: 3, ID: id.String()}}),
		"tail":     enc([]api.Transition{tr(4, "show-a", "cause"), tr(6, "show-a", "finished")}),
	})
	for _, o := range []*corev1.ConfigMap{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: queue.BookConfigMap}, Data: map[string]string{"next": "7"}},
		cm,
		project("show-b", map[string]string{"tail": enc([]api.Transition{tr(5, "show-b", "phase")})}),
	} {
		if err := s.Client.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	read := func(q string) (int, api.Transitions) {
		var out api.Transitions
		code := call(t, s, http.MethodGet, "/api/v1/book?as=artist&"+q, nil, &out)
		return code, out
	}
	code, all := read("since=0")
	if code != http.StatusOK || len(all.Entries) != 5 || all.Next != 6 || all.More {
		t.Fatalf("since 0: %d %+v", code, all)
	}
	for i, want := range []int64{1, 2, 3, 4, 6} {
		if all.Entries[i].Seq != want {
			t.Fatalf("order: %+v", all.Entries)
		}
	}
	if _, page := read("since=1&limit=2"); len(page.Entries) != 2 || page.Entries[0].Seq != 2 || page.Next != 3 || !page.More {
		t.Fatalf("a page: %+v", page)
	}
	if _, rest := read("since=4"); len(rest.Entries) != 1 || rest.Entries[0].Seq != 6 || rest.Next != 6 {
		t.Fatalf("after 4: %+v", rest)
	}
	// The wrangler sees show-b's too.
	var wr api.Transitions
	call(t, s, http.MethodGet, "/api/v1/book?as=wrangler&since=4", nil, &wr)
	if len(wr.Entries) != 2 || wr.Entries[0].Project != "show-b" {
		t.Fatalf("the wrangler: %+v", wr)
	}

	// Older segments dropped: a cursor before what is kept is told where
	// the log begins.
	// (Later: the kept segments begin at 11, those before were dropped.)
	cm.Data["dropped"] = "true"
	cm.Data["segments"] = enc([]queue.BookSegment{{First: 11, Last: 13, ID: id.String()}})
	if err := s.Client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	var gone map[string]any
	if code := call(t, s, http.MethodGet, "/api/v1/book?as=artist&since=4", nil, &gone); code != http.StatusGone || gone["oldest"] != float64(11) {
		t.Fatalf("a cursor the log no longer reaches: %d %v", code, gone)
	}
	if code, _ := read("since=10"); code != http.StatusOK {
		t.Fatalf("since 10 is still reached: %d", code)
	}
	if code := call(t, s, http.MethodGet, "/api/v1/book?as=artist&since=soon", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("a bad cursor: %d", code)
	}
}
