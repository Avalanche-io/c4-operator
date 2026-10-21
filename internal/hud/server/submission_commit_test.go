package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/authoring"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

// commitFarm is the submission farm with a signer, a book, and show-a's
// store in dir, where a test reads the records signed.
func commitFarm(t *testing.T) (s *Server, dir string) {
	t.Helper()
	s = submissionFarm(t)
	ctx := context.Background()
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	kube := kfake.NewSimpleClientset()
	var mu sync.Mutex
	n := 0
	kube.PrependReactor("create", "events", func(a ktesting.Action) (bool, runtime.Object, error) {
		if ev := a.(ktesting.CreateAction).GetObject().(*corev1.Event); ev.Name == "" {
			mu.Lock()
			n++
			ev.Name = ev.GenerateName + strings.Repeat("x", n%7) + time.Now().Format("150405.000000000")
			mu.Unlock()
		}
		return false, nil, nil
	})
	s.Kube = kube
	// The records go to show-a's store, where the tools' trees are: a
	// file store, its shard named by its folder.
	st, err := s.Stores(ctx, "show-a")
	if err != nil || st == nil {
		t.Fatal(err)
	}
	dir = strings.TrimPrefix(st.Shards()[0].Name(), "file://")
	return s, dir
}

// signed counts the submission records signed in dir: submits and
// withdrawals.
func signed(t *testing.T, dir string) (submits, withdrawals int) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		text := string(b)
		var rec struct {
			Envelope struct {
				Payload string `json:"payload"`
			} `json:"dsseEnvelope"`
		}
		if json.Unmarshal(b, &rec) == nil && rec.Envelope.Payload != "" {
			if raw, err := base64.StdEncoding.DecodeString(rec.Envelope.Payload); err == nil {
				text += string(raw)
			}
		}
		switch {
		case strings.Contains(text, `"action":"withdraw"`):
			withdrawals++
		case strings.Contains(text, `"action":"submit"`) && strings.Contains(text, `submission:`):
			submits++
		}
		return nil
	})
	return
}

// submitCtx posts a submission on a request whose context the test ends
// (the client hanging up).
func submitCtx(ctx context.Context, t *testing.T, s *Server, body api.SubmissionRequest) (int, api.SubmissionResponse) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/submissions?as=artist", bytes.NewReader(raw)).WithContext(ctx)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var out api.SubmissionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func workJob(t *testing.T, s *Server, name string) *c4v1.WorkJob {
	t.Helper()
	var j c4v1.WorkJob
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: "show-a", Name: name}, &j); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &j
}

// committed checks a submission was made whole: both jobs, released,
// each with the one record.
func committed(t *testing.T, s *Server, dir string) {
	t.Helper()
	if jobs := listJobs(t, s); len(jobs) != 2 || jobs[0] != "sm0140-movie-v001" || jobs[1] != "sm0140-proxies-v001" {
		t.Fatalf("jobs: %v", jobs)
	}
	record := ""
	for _, n := range []string{"sm0140-proxies-v001", "sm0140-movie-v001"} {
		j := workJob(t, s, n)
		if j.Spec.Nudge.Hold || j.Annotations[AnnotationSubmissionPending] != "" || j.Annotations[authoring.AnnotationSubmissionRecord] == "" {
			t.Fatalf("%s not released: hold %v %v", n, j.Spec.Nudge.Hold, j.Annotations)
		}
		if record != "" && j.Annotations[authoring.AnnotationSubmissionRecord] != record {
			t.Fatalf("two records for one submission")
		}
		record = j.Annotations[authoring.AnnotationSubmissionRecord]
	}
	if subs, wds := signed(t, dir); subs != 1 || wds != 0 {
		t.Fatalf("signed %d submissions and %d withdrawals, want 1 and 0", subs, wds)
	}
}

// step is a point a submission passes: the Nth create, patch or delete of
// a WorkJob, or the signing (the record's store).
type step struct {
	verb string // create, patch, sign, delete
	n    int
}

// trap is a client and store that, at one step, hang the client up and
// optionally fail the step.
type trap struct {
	mu     sync.Mutex
	counts map[string]int
	at     step
	cancel context.CancelFunc
	fail   bool
	// failDeletes fails every delete of this job.
	failDeletes string
}

func (tr *trap) hit(verb string) (fail bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.counts == nil {
		tr.counts = map[string]int{}
	}
	tr.counts[verb]++
	if verb != tr.at.verb || tr.counts[verb] < tr.at.n {
		return false
	}
	if tr.counts[verb] == tr.at.n && tr.cancel != nil {
		tr.cancel()
	}
	// A failing step fails from then on: its retries fail too.
	return tr.fail
}

var errInjected = apierrors.NewServiceUnavailable("injected")

func (tr *trap) install(s *Server) {
	s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*c4v1.WorkJob); ok && tr.hit("create") {
				return errInjected
			}
			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*c4v1.WorkJob); ok && tr.hit("patch") {
				return errInjected
			}
			return c.Patch(ctx, obj, p, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*c4v1.WorkJob); ok {
				if tr.hit("delete") || obj.GetName() == tr.failDeletes {
					return errInjected
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	stores := s.Stores
	s.Stores = func(ctx context.Context, project string) (*storeurl.Sharded, error) {
		// Signing is the first store use after the creates; the trees
		// were stored before the submission.
		tr.mu.Lock()
		creates := tr.counts["create"]
		tr.mu.Unlock()
		if creates >= 2 && tr.hit("sign") {
			return nil, errors.New("injected: the store is down")
		}
		return stores(ctx, project)
	}
}

// A client that hangs up at any step of a submission, its creates, its
// signing, its release, gets it made whole; one that hangs up where a
// step fails gets nothing left behind and no stray record. Either way a
// retry with the same key duplicates nothing.
func TestSubmissionClientHangsUpAtEachStep(t *testing.T) {
	// patch 1 and 2 put the record on, 3 and 4 take the holds off.
	steps := []step{{"create", 1}, {"create", 2}, {"sign", 1}, {"patch", 1}, {"patch", 2}, {"patch", 3}, {"patch", 4}}
	for _, st := range steps {
		for _, fail := range []bool{false, true} {
			name := st.verb + "-" + string(rune('0'+st.n))
			if fail {
				name += "-fails"
			}
			t.Run(name, func(t *testing.T) {
				s, dir := commitFarm(t)
				tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				tr := &trap{at: st, cancel: cancel, fail: fail}
				tr.install(s)
				body := api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree), Key: "take-1"}
				code, out := submitCtx(ctx, t, s, body)
				if !fail {
					committed(t, s, dir)
					// The client never saw the answer; its retry is given it.
					code, again := submit(t, s, body)
					if code != http.StatusOK || len(again.Jobs) != 2 || again.Jobs[0].Name != "sm0140-proxies-v001" ||
						again.Jobs[1].Key != "movie" || again.Record == "" || !strings.Contains(again.Message, "submitted already") {
						t.Fatalf("retry: %d %+v", code, again)
					}
					committed(t, s, dir)
					return
				}
				// How a refusal reads (its status, its words) is farmWords'.
				if code < 400 || out.OK || len(out.Errors) != 1 {
					t.Fatalf("%d %+v", code, out)
				}
				if jobs := listJobs(t, s); len(jobs) != 0 {
					t.Fatalf("left behind: %v", jobs)
				}
				subs, wds := signed(t, dir)
				if st.verb == "patch" && (subs != 1 || wds != 1) || st.verb != "patch" && (subs != 0 || wds != 0) {
					t.Fatalf("signed %d submissions and %d withdrawals", subs, wds)
				}
				// The retry, with nothing left of the first, makes it once.
				tr.at = step{}
				if code, again := submit(t, s, body); code != http.StatusCreated || len(again.Jobs) != 2 {
					t.Fatalf("retry: %d %+v", code, again)
				}
				if jobs := listJobs(t, s); len(jobs) != 2 {
					t.Fatalf("after the retry: %v", jobs)
				}
			})
		}
	}
}

// A client that hangs up while its submission is being rolled back still
// leaves nothing behind.
func TestSubmissionRollbackOutlivesTheClient(t *testing.T) {
	s, dir := commitFarm(t)
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The second create fails; the client hangs up at the first delete,
	// which fails once and is tried again.
	tr := &trap{at: step{"delete", 1}, cancel: cancel}
	tr.install(s)
	failed := false
	s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == "sm0140-movie-v001" {
				return errInjected
			}
			return c.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if !failed {
				failed = true
				return errInjected
			}
			return c.Delete(ctx, obj, opts...)
		}})
	code, out := submitCtx(ctx, t, s, api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree)})
	if code < 400 || out.OK {
		t.Fatalf("%d %+v", code, out)
	}
	if jobs := listJobs(t, s); len(jobs) != 0 {
		t.Fatalf("left behind: %v", jobs)
	}
	if subs, wds := signed(t, dir); subs != 0 || wds != 0 {
		t.Fatalf("signed %d and %d for a submission refused before its jobs were made", subs, wds)
	}
}

// Every job is created held, marked pending, and released together once
// the last is made and the submission signed for; units cannot run
// before. A job the file asks to hold stays held.
func TestSubmissionHeldUntilAllMade(t *testing.T) {
	s, _ := commitFarm(t)
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	var mu sync.Mutex
	asMade := map[string]c4v1.WorkJob{}
	s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if j, ok := obj.(*c4v1.WorkJob); ok {
				mu.Lock()
				asMade[j.Name] = *j.DeepCopy()
				mu.Unlock()
			}
			return c.Create(ctx, obj, opts...)
		}})
	code, out := submit(t, s, api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree)})
	if code != http.StatusCreated || out.Record == "" {
		t.Fatalf("%d %+v", code, out)
	}
	for _, n := range []string{"sm0140-proxies-v001", "sm0140-movie-v001"} {
		j := asMade[n]
		if !j.Spec.Nudge.Hold || j.Annotations[AnnotationSubmissionPending] == "" || j.Annotations[AnnotationSubmissionAttempt] == "" ||
			j.Annotations[authoring.AnnotationSubmissionRecord] != "" {
			t.Fatalf("%s as made: hold %v %v", n, j.Spec.Nudge.Hold, j.Annotations)
		}
		now := workJob(t, s, n)
		if now.Spec.Nudge.Hold || now.Annotations[AnnotationSubmissionPending] != "" || now.Annotations[authoring.AnnotationSubmissionRecord] != out.Record {
			t.Fatalf("%s released: hold %v %v", n, now.Spec.Nudge.Hold, now.Annotations)
		}
	}

	// A job the request itself holds stays held after the release.
	held := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Name: "held", Namespace: "show-a", UID: "u1",
		Annotations: map[string]string{AnnotationSubmissionPending: "x", AnnotationSubmissionHoldAsked: "true"}},
		Spec: c4v1.WorkJobSpec{Class: "render", Nudge: c4v1.WorkNudge{Hold: true}}}
	if err := s.Client.Create(context.Background(), held); err != nil {
		t.Fatal(err)
	}
	made := workJob(t, s, "held")
	if err := s.release(context.Background(), made, "rec", true); err != nil {
		t.Fatal(err)
	}
	if j := workJob(t, s, "held"); !j.Spec.Nudge.Hold || j.Annotations[AnnotationSubmissionPending] != "" || j.Annotations[AnnotationSubmissionHoldAsked] != "" {
		t.Fatalf("an asked hold: %v %v", j.Spec.Nudge.Hold, j.Annotations)
	}
}

// A job the rollback cannot delete is named in the refusal, with what
// the farm does about it and what to do.
func TestSubmissionRollbackNamesWhatStays(t *testing.T) {
	s, _ := commitFarm(t)
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	tr := &trap{at: step{"create", 2}, fail: true, failDeletes: "sm0140-proxies-v001"}
	tr.install(s)
	code, out := submit(t, s, api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree)})
	if code < 400 || len(out.Errors) != 1 {
		t.Fatalf("%d %+v", code, out)
	}
	e := out.Errors[0]
	if !strings.Contains(e.Message, "sm0140-proxies-v001 could not be removed") || !strings.Contains(e.Fix, "delete it before submitting again") ||
		strings.Contains(e.Fix, "nothing of the submission was kept") {
		t.Fatalf("refusal: %+v", e)
	}
	if j := workJob(t, s, "sm0140-proxies-v001"); !j.Spec.Nudge.Hold || j.Annotations[AnnotationSubmissionPending] == "" {
		t.Fatalf("what stays is held and pending, for the sweep: %v %v", j.Spec.Nudge.Hold, j.Annotations)
	}
}

// A retry while the first is still being made is told to ask again; a
// key is the submitter's own; no key keeps today's two submissions.
func TestSubmissionKey(t *testing.T) {
	s, _ := commitFarm(t)
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	body := api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree), Key: "k1"}
	// Pending jobs under the key: a retry waits its turn.
	label := keyLabel("maya", "k1") // ?as=artist is maya
	pending := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Name: "sm0140-proxies-v001", Namespace: "show-a",
		Labels:      map[string]string{LabelSubmissionKey: label},
		Annotations: map[string]string{AnnotationSubmissionPending: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}},
		Spec: c4v1.WorkJobSpec{Class: "render", Nudge: c4v1.WorkNudge{Hold: true}}}
	if err := s.Client.Create(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if code, out := submit(t, s, body); code != http.StatusConflict || out.Errors[0].Path != "key" {
		t.Fatalf("while pending: %d %+v", code, out)
	}
	if err := s.Client.Delete(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if code, out := submit(t, s, body); code != http.StatusCreated || len(out.Jobs) != 2 {
		t.Fatalf("first: %d %+v", code, out)
	}
	// Another key, or none: new submissions, numbered past.
	if code, out := submit(t, s, api.SubmissionRequest{Submission: body.Submission, Key: "k2"}); code != http.StatusCreated || out.Jobs[0].Name != "sm0140-proxies-v002" {
		t.Fatalf("another key: %d %+v", code, out)
	}
	if code, out := submit(t, s, api.SubmissionRequest{Submission: body.Submission}); code != http.StatusCreated || out.Jobs[0].Name != "sm0140-proxies-v003" {
		t.Fatalf("no key: %d %+v", code, out)
	}
	if keyLabel("maya", "k1") == keyLabel("dana", "k1") {
		t.Fatal("a key is the submitter's own")
	}
}

// The sweep deletes the jobs of a submission that never finished: every
// job of an attempt one of whose jobs is pending past its time, released
// or not; nothing of an attempt still being made or finished.
func TestSweepUnfinishedSubmissions(t *testing.T) {
	s, _ := commitFarm(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	soon := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	mk := func(name, pending, attempt string) {
		j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "show-a",
			Annotations: map[string]string{AnnotationSubmissionAttempt: attempt}}, Spec: c4v1.WorkJobSpec{Class: "render"}}
		if pending != "" {
			j.Annotations[AnnotationSubmissionPending] = pending
			j.Spec.Nudge.Hold = true
		}
		if err := s.Client.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	mk("a-proxies-v001", "", "cut")   // released before the HUD stopped
	mk("a-movie-v001", past, "cut")   // still pending, past its time
	mk("b-proxies-v001", soon, "now") // being made
	mk("c-proxies-v001", "", "done")  // a finished submission
	modelOf(t, s)
	s.sweepSubmissions(ctx, time.Now())
	if jobs := listJobs(t, s); strings.Join(jobs, " ") != "b-proxies-v001 c-proxies-v001" {
		t.Fatalf("after the sweep: %v", jobs)
	}
	// Not again until submission.sweepEvery has passed.
	mk("d-movie-v001", past, "late")
	modelOf(t, s)
	s.sweepSubmissions(ctx, time.Now())
	if jobs := listJobs(t, s); len(jobs) != 3 {
		t.Fatalf("swept again too soon: %v", jobs)
	}
	s.sweepSubmissions(ctx, time.Now().Add(2*time.Minute))
	if jobs := listJobs(t, s); len(jobs) != 2 {
		t.Fatalf("not swept after the interval: %v", jobs)
	}
}

// modelOf gives s a model of the jobs its client holds, as a collection
// would.
func modelOf(t *testing.T, s *Server) {
	t.Helper()
	var list c4v1.WorkJobList
	if err := s.Client.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	m := &model{byKey: map[string]*jobRec{}}
	for _, j := range list.Items {
		rec := &jobRec{job: j, key: j.Namespace + "/" + j.Name, project: j.Namespace} // show-a is both
		m.jobs = append(m.jobs, rec)
		m.byKey[rec.key] = rec
	}
	s.mu.Lock()
	s.m = m
	s.mu.Unlock()
}

// stale is a client whose cache has not seen what was just written: Get
// and List of WorkJobs miss. The submission reads through Reader.
func stale(s *Server) {
	s.Reader = s.Client
	s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*c4v1.WorkJob); ok {
				return apierrors.NewNotFound(c4v1.GroupVersion.WithResource("workjobs").GroupResource(), key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*c4v1.WorkJobList); ok {
				return nil
			}
			return c.List(ctx, list, opts...)
		}})
}

// A submission's release and its key's answer read what was just written
// from the API server, not a cache that has not seen it yet.
func TestSubmissionReadsTheAPIServer(t *testing.T) {
	s, _ := commitFarm(t)
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	stale(s)
	body := api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree), Key: "k-stale"}
	if code, out := submit(t, s, body); code != http.StatusCreated || len(out.Jobs) != 2 {
		t.Fatalf("a stale cache rolled a good submission back: %d %+v", code, out)
	}
	if code, out := submit(t, s, body); code != http.StatusOK || len(out.Jobs) != 2 || out.Jobs[0].Name != "sm0140-proxies-v001" {
		t.Fatalf("the retry: %d %+v", code, out)
	}
	var list c4v1.WorkJobList
	if err := s.Reader.List(context.Background(), &list); err != nil || len(list.Items) != 2 {
		t.Fatalf("jobs: %d %v", len(list.Items), err)
	}
}

// A key names one submission: the same key with another file is refused;
// a key lapses after submission.keyFor.
func TestSubmissionKeyNamesOneSubmission(t *testing.T) {
	s, _ := commitFarm(t)
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	first := submission(t, "proxies-movie", tree)
	if code, _ := submit(t, s, api.SubmissionRequest{Submission: first, Key: "k1"}); code != http.StatusCreated {
		t.Fatalf("first: %d", code)
	}
	other := []byte(strings.Replace(string(first), `"jobs"`, `"note":"another take","jobs"`, 1))
	code, out := submit(t, s, api.SubmissionRequest{Submission: other, Key: "k1"})
	if code != http.StatusConflict || len(out.Errors) != 1 || out.Errors[0].Path != "key" || !strings.Contains(out.Errors[0].Message, "another submission") {
		t.Fatalf("the same key, another file: %d %+v", code, out)
	}
	// Past its lifetime the key is free: the jobs it named are old.
	ctx := context.Background()
	var list c4v1.WorkJobList
	if err := s.Client.List(ctx, &list); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		j := &list.Items[i]
		j.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour))
		if err := s.Client.Update(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := submit(t, s, api.SubmissionRequest{Submission: other, Key: "k1"}); code != http.StatusCreated || out.Jobs[0].Name != "sm0140-proxies-v002" {
		t.Fatalf("a lapsed key: %d %+v", code, out)
	}
}

// The rollback has a window of its own: a request whose time is gone
// still has its jobs deleted, and a signed submission's withdrawal names
// what was deleted, signed by the farm after the deletes.
func TestRollbackHasItsOwnWindow(t *testing.T) {
	s, dir := commitFarm(t)
	ctx := context.Background()
	var made []madeJob
	for _, n := range []string{"a-v001", "b-v001"} {
		j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "show-a", UID: types.UID("u-" + n)}, Spec: c4v1.WorkJobSpec{Class: "render"}}
		if err := s.Client.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		made = append(made, madeJob{job: workJob(t, s, n)})
	}
	done, cancel := context.WithTimeout(ctx, time.Nanosecond)
	defer cancel()
	<-done.Done()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/submissions", nil).WithContext(done)
	out := s.rollback(r, &api.Viewer{User: "maya"}, &authoring.Plan{Project: "show-a", Namespace: "show-a"}, made, "movie", "c4rec", "att1")
	if len(out.left) != 0 || out.unsigned {
		t.Fatalf("rolled back: %+v", out)
	}
	if jobs := listJobs(t, s); len(jobs) != 0 {
		t.Fatalf("left: %v", jobs)
	}
	if _, wds := signed(t, dir); wds != 1 {
		t.Fatalf("withdrawals: %d", wds)
	}
}

// Release is two passes: when the first hold comes off, every job already
// carries the record and is still held.
func TestReleaseInTwoPasses(t *testing.T) {
	s, _ := commitFarm(t)
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	var seen []string
	var mu sync.Mutex
	inner := s.Client.(client.WithWatch)
	s.Client = interceptor.NewClient(inner, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			if j, ok := obj.(*c4v1.WorkJob); ok && !j.Spec.Nudge.Hold && len(seen) == 0 {
				var list c4v1.WorkJobList
				_ = c.List(ctx, &list)
				mu.Lock()
				for _, x := range list.Items {
					seen = append(seen, fmt.Sprintf("%s held=%v record=%v", x.Name, x.Spec.Nudge.Hold, x.Annotations[authoring.AnnotationSubmissionRecord] != ""))
				}
				mu.Unlock()
			}
			return c.Patch(ctx, obj, p, opts...)
		}})
	if code, out := submit(t, s, api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree)}); code != http.StatusCreated {
		t.Fatalf("%d %+v", code, out)
	}
	if strings.Join(seen, "; ") != "sm0140-movie-v001 held=true record=true; sm0140-proxies-v001 held=true record=true" {
		t.Fatalf("at the first release: %v", seen)
	}
}

// A job being submitted cannot have its hold changed, and reads as being
// submitted, not held.
func TestPendingJobIsNotAPersonsHold(t *testing.T) {
	s, _ := commitFarm(t)
	ctx := context.Background()
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Name: "sm0140-proxies-v001", Namespace: "show-a",
		Annotations: map[string]string{AnnotationSubmissionPending: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}},
		Spec: c4v1.WorkJobSpec{Class: "render", Nudge: c4v1.WorkNudge{Hold: true}}}
	if err := s.Client.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	modelOf(t, s)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/sm0140-proxies-v001/nudge?as=wrangler", strings.NewReader(`{"hold": false}`)))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "being submitted") {
		t.Fatalf("nudge: %d %s", w.Code, w.Body)
	}
	if !workJob(t, s, "sm0140-proxies-v001").Spec.Nudge.Hold {
		t.Fatal("released by a nudge")
	}
	m := s.current()
	got := s.jobSummary(m, m.jobs[0])
	if got.Nudge.Hold || !strings.Contains(noETA(m, &m.jobs[0].job, false, 0), "being submitted") {
		t.Fatalf("shown as %+v / %q", got.Nudge, noETA(m, &m.jobs[0].job, false, 0))
	}
}

// The sweep withdraws, signed by the farm, an attempt that was signed for
// before the HUD stopped.
func TestSweepWithdrawsASignedAttempt(t *testing.T) {
	s, dir := commitFarm(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	j := &c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Name: "a-v001", Namespace: "show-a", Annotations: map[string]string{
		AnnotationSubmissionAttempt: "att", AnnotationSubmissionPending: past, authoring.AnnotationSubmissionRecord: "c4rec"}},
		Spec: c4v1.WorkJobSpec{Class: "render"}}
	if err := s.Client.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	modelOf(t, s)
	s.sweepSubmissions(ctx, time.Now())
	if jobs := listJobs(t, s); len(jobs) != 0 {
		t.Fatalf("left: %v", jobs)
	}
	if _, wds := signed(t, dir); wds != 1 {
		t.Fatalf("withdrawals: %d", wds)
	}
}

// A job a failed attempt could not delete is marked failed: a retry with
// the key does not wait for it, and keys' locks do not pile up.
func TestFailedLeftoverDoesNotBlockTheKey(t *testing.T) {
	s, _ := commitFarm(t)
	tree := putTree(t, s, "show-a", map[string]string{"plates/sm0140.1001.exr": "plate"}).String()
	tr := &trap{at: step{"create", 2}, fail: true, failDeletes: "sm0140-proxies-v001"}
	tr.install(s)
	body := api.SubmissionRequest{Submission: submission(t, "proxies-movie", tree), Key: "k-left"}
	if code, _ := submit(t, s, body); code < 400 {
		t.Fatalf("first: %d", code)
	}
	if a := workJob(t, s, "sm0140-proxies-v001").Annotations; a[AnnotationSubmissionFailed] != "true" {
		t.Fatalf("not marked failed: %v", a)
	}
	tr.mu.Lock()
	tr.at, tr.failDeletes = step{}, ""
	tr.mu.Unlock()
	code, out := submit(t, s, body)
	if code != http.StatusCreated || out.Jobs[0].Name != "sm0140-proxies-v002" {
		t.Fatalf("the retry waited for a leftover: %d %+v", code, out)
	}
	if n := s.submitKeys.size(); n != 0 {
		t.Fatalf("%d key locks kept", n)
	}
}
