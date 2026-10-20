package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// A held tracker write is listed for the people who see its project, as
// theirs when the job is; its owner says whether it happened, signed and
// booked, and c4-sync's record of it changes to match. No one edits a
// TrackerLink by hand.
func TestHeldWrites(t *testing.T) {
	s := grantFarm(t)
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := c4v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	at := metav1.NewTime(time.Date(2026, 10, 20, 9, 50, 0, 0, time.UTC))
	held := func(v string) c4v1.TrackerWritten {
		return c4v1.TrackerWritten{Value: v, Ref: "991", At: at, State: "unconfirmed"}
	}
	s.Client = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&c4v1.TrackerLink{}).WithObjects(
		&c4v1.Tracker{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "gh"}, Spec: c4v1.TrackerSpec{Kind: "github"}},
		&c4v1.TrackerLink{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "gh-issue-7"},
			Spec: c4v1.TrackerLinkSpec{Tracker: "gh", Ref: c4v1.TrackerRef{Type: "Issue", ID: "7", URL: "https://github.com/acme/widgets/issues/7"},
				Target: c4v1.TrackerTarget{WorkJob: "gh-7"}},
			Status: c4v1.TrackerLinkStatus{Written: map[string]c4v1.TrackerWritten{
				"comment": held("running#2026-10-20T12:00:00Z"), "check": held("running#"), "status": {Value: "running#", At: at}},
				Conditions: []metav1.Condition{{Type: condUnconfirmed, Status: metav1.ConditionTrue, Reason: "Held", LastTransitionTime: at}}}},
		&c4v1.TrackerLink{ObjectMeta: metav1.ObjectMeta{Namespace: "show-b", Name: "gh-milestone-2"},
			Spec:   c4v1.TrackerLinkSpec{Tracker: "gh", Ref: c4v1.TrackerRef{Type: "Milestone", ID: "2"}, Target: c4v1.TrackerTarget{Milestone: "m2"}},
			Status: c4v1.TrackerLinkStatus{Written: map[string]c4v1.TrackerWritten{"comment": held("")}}},
	).Build()
	kube := kfake.NewSimpleClientset()
	n := 0
	kube.PrependReactor("create", "events", func(a ktesting.Action) (bool, runtime.Object, error) {
		if ev := a.(ktesting.CreateAction).GetObject().(*corev1.Event); ev.Name == "" {
			n++
			ev.Name = fmt.Sprintf("%s%d", ev.GenerateName, n)
		}
		return false, nil, nil
	})
	s.Kube = kube
	m := s.current()
	job := c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "gh-7", Annotations: map[string]string{c4v1.AnnotationOwner: "maya"}}}
	rec := &jobRec{job: job, key: "show-a/gh-7", project: "show-a"}
	m.jobs = append(m.jobs, rec)
	m.byKey[rec.key] = rec

	list := func(as, query string) []api.HeldWrite {
		t.Helper()
		var out api.HeldList
		if code := call(t, s, http.MethodGet, "/api/v1/held?as="+as+query, nil, &out); code != http.StatusOK {
			t.Fatalf("held as %s%s: %d", as, query, code)
		}
		return out.Held
	}
	hs := list("artist", "")
	if len(hs) != 2 {
		t.Fatalf("maya sees show-a's two held writes, not the written one or show-b's: %+v", hs)
	}
	var c api.HeldWrite
	for _, h := range hs {
		if h.Field == "comment" {
			c = h
		}
	}
	if !c.Yours || c.Job != "show-a/gh-7" || c.Owner != "maya" || c.State != "running" || c.Kind != "github" ||
		c.Says != "the GitHub comment on Issue 7, saying running" || c.Entity.URL == "" || !c.Since.Equal(at.Time) {
		t.Fatalf("the held comment: %+v", c)
	}
	if hs := list("lee", ""); len(hs) != 2 || hs[0].Yours {
		t.Fatalf("lee sees them, not as his: %+v", hs)
	}
	if hs := list("lee", "&mine=1"); len(hs) != 0 {
		t.Fatalf("none is lee's: %+v", hs)
	}
	if hs := list("wrangler", ""); len(hs) != 3 {
		t.Fatalf("a farm-wide viewer sees show-b's too: %+v", hs)
	}
	if hs := list("wrangler", "&job=show-a/gh-7"); len(hs) != 2 {
		t.Fatalf("one job's: %+v", hs)
	}

	no, yes := false, true
	var out map[string]any
	if code := call(t, s, http.MethodPost, "/api/v1/held/show-a/gh-issue-7/comment?as=artist", api.ConfirmRequest{}, &out); code != http.StatusBadRequest {
		t.Fatalf("no answer: %d", code)
	}
	if code := call(t, s, http.MethodPost, "/api/v1/held/show-b/gh-milestone-2/comment?as=wrangler", api.ConfirmRequest{Happened: &yes}, &out); code != http.StatusForbidden {
		t.Fatalf("a write for no job, and no owner: %d", code)
	}
	if code := call(t, s, http.MethodPost, "/api/v1/held/show-a/gh-issue-7/comment?as=artist",
		api.ConfirmRequest{Happened: &no, Note: "not on the issue"}, &out); code != http.StatusOK || out["record"] == nil ||
		!strings.Contains(fmt.Sprint(out["message"]), "did not happen") {
		t.Fatalf("denied: %d %v", code, out)
	}
	if code := call(t, s, http.MethodPost, "/api/v1/held/show-a/gh-issue-7/comment?as=artist", api.ConfirmRequest{Happened: &no}, &out); code != http.StatusConflict {
		t.Fatalf("again: %d", code)
	}
	if code := call(t, s, http.MethodPost, "/api/v1/held/show-a/gh-issue-7/check?as=artist", api.ConfirmRequest{Happened: &yes}, &out); code != http.StatusOK {
		t.Fatalf("confirmed: %d %v", code, out)
	}
	var l c4v1.TrackerLink
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: "show-a", Name: "gh-issue-7"}, &l); err != nil {
		t.Fatal(err)
	}
	if w := l.Status.Written["comment"]; w.Value != "" || w.State != "" || w.Ref != "991" {
		t.Fatalf("a write that did not happen is written again, on its entity: %+v", w)
	}
	if w := l.Status.Written["check"]; w.Value != "running#" || w.State != "" {
		t.Fatalf("a write that happened stands: %+v", w)
	}
	if cond := meta.FindStatusCondition(l.Status.Conditions, condUnconfirmed); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("nothing held: %+v", l.Status.Conditions)
	}
	var book api.Book
	if code := call(t, s, http.MethodGet, "/api/v1/book?as=artist", nil, &book); code != http.StatusOK {
		t.Fatalf("book: %d", code)
	}
	kinds := map[string]int{}
	for _, e := range book.Entries {
		if e.Object.Job == "show-a/gh-7" && e.Record != "" {
			kinds[e.Kind]++
		}
	}
	if kinds["write-denied"] != 1 || kinds["write-confirmed"] != 1 {
		t.Fatalf("book: %v in %+v", kinds, book.Entries)
	}
}
