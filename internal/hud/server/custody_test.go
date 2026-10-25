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
	kfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// custodyFarm is a farm whose every entity signs as itself: an ingest by
// a workload, a render on two nodes, a denoise of the render's output.
type custodyFarm struct {
	t     *testing.T
	ctx   context.Context
	ca    *provenance.CA
	store *storeurl.Sharded
	srv   *Server
	k     client.Client
}

func (f *custodyFarm) sign(identity, predicateType string, subjects []provenance.Subject, pred any) string {
	f.t.Helper()
	sg, err := provenance.NewSigner(f.ctx, provenance.LocalIssuer(f.ca, identity, time.Hour))
	if err != nil {
		f.t.Fatal(err)
	}
	rec, err := provenance.SignStatement(f.ctx, sg, predicateType, subjects, pred, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return f.put(rec)
}

func (f *custodyFarm) put(data []byte) string {
	f.t.Helper()
	id, err := f.store.PutBytes(f.ctx, data)
	if err != nil {
		f.t.Fatal(err)
	}
	return id.String()
}

func idOf(s string) c4.ID { return c4.Identify(strings.NewReader(s)) }

func task(node, job, unit string, mat string, in c4.ID) provenance.TaskPredicate {
	return provenance.TaskPredicate{
		Invocation: provenance.Invocation{Project: "show-a", Namespace: "show-a", Pod: job + "-" + unit, Job: job, Unit: unit, Attempt: "1"},
		Inputs:     []provenance.Input{{Materialization: mat, View: in.String()}},
		Runtime:    provenance.Runtime{Node: node, StartedOn: time.Now().Add(-time.Minute), FinishedOn: time.Now()},
	}
}

func newCustodyFarm(t *testing.T) (*custodyFarm, string) {
	t.Helper()
	ctx := context.Background()
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	st, err := storeurl.OpenSharded([]storeurl.Spec{{Name: "s0", URL: t.TempDir()}}, storeurl.Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	f := &custodyFarm{t: t, ctx: ctx, ca: ca, store: st}
	plates, renderOut, denoiseOut := idOf("plates"), idOf("render output"), idOf("denoise output")

	ingest := f.sign(provenance.ServiceAccountIdentity("show-a", "ingest"), provenance.PredicateIngest,
		[]provenance.Subject{provenance.SubjectFor("view", plates)}, map[string]any{"source": "plates"})
	resolve := func(view c4.ID, source string) string {
		return f.sign(provenance.ComponentIdentity("controller"), provenance.PredicateResolution,
			[]provenance.Subject{provenance.SubjectFor("view", view)},
			map[string]any{"view": view.String(), "source": view.String(), "files": 8, "sourceRecord": map[string]any{"record": source}})
	}
	platesRes := resolve(plates, ingest)

	// The render: two units on two nodes, indexed, sealed by the queue.
	r0 := f.sign(provenance.NodeIdentity("w1"), provenance.PredicateTask,
		[]provenance.Subject{provenance.SubjectFor("renders/r.1001.exr", idOf("r1"))}, task("w1", "render", "0", "plates", plates))
	r1 := f.sign(provenance.NodeIdentity("w2"), provenance.PredicateTask,
		[]provenance.Subject{provenance.SubjectFor("renders/r.1002.exr", idOf("r2"))}, task("w2", "render", "1", "plates", plates))
	ix, _ := json.Marshal(queue.Index{Job: "show-a/render", Records: map[string]string{"renders/r.1001.exr": r0, "renders/r.1002.exr": r1},
		Units: map[int]string{0: r0, 1: r1}})
	renderIndex := f.put(ix)
	renderRec := f.sign(provenance.ComponentIdentity("queue"), provenance.PredicateJob,
		[]provenance.Subject{provenance.SubjectFor("output", renderOut)}, map[string]any{"job": "show-a/render", "phase": "Succeeded"})
	denoiseRes := resolve(renderOut, renderRec)

	render := c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "render"},
		Spec: c4v1.WorkJobSpec{Class: "render", Inputs: []c4v1.WorkInput{{Materialization: "plates", MountPath: "/show"}}},
		Status: c4v1.WorkJobStatus{Phase: "Succeeded", Finished: &metav1.Time{Time: time.Now()},
			Output: &c4v1.WorkOutput{View: renderOut.String(), Provenance: renderIndex, Record: renderRec}}}
	denoise := c4v1.WorkJob{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "denoise"},
		Spec: c4v1.WorkJobSpec{Class: "render", Nudge: c4v1.WorkNudge{Hold: true},
			Inputs: []c4v1.WorkInput{{FromJob: "render", MountPath: "/show"}}}}
	mats := []client.Object{
		&c4v1.Materialization{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: "plates"}, Status: c4v1.MaterializationStatus{Record: platesRes}},
		&c4v1.Materialization{ObjectMeta: metav1.ObjectMeta{Namespace: "show-a", Name: queue.InputMaterialization("denoise", denoise.Spec.Inputs[0])},
			Status: c4v1.MaterializationStatus{Record: denoiseRes}},
	}
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	c4v1.AddToScheme(scheme)
	f.k = fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(mats, denoise.DeepCopy())...).Build()
	certPEM, _, _ := ca.PEM()
	kube := kfake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "c4-system", Name: KeysConfigMap},
		Data: map[string]string{"ca.crt": string(certPEM)}})
	hudSigner, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.ComponentIdentity("hud"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f.srv = &Server{Client: f.k, Kube: kube, Namespace: "c4-system", Loc: time.UTC, Signer: hudSigner, Log: zap.New(zap.WriteTo(&bytes.Buffer{})),
		Stores:         func(context.Context, string) (*storeurl.Sharded, error) { return st, nil },
		DevViewersOpen: true,
		DevViewers:     map[string]*api.Viewer{"wrangler": {User: "wrangler", All: true}}}
	m := &model{byKey: map[string]*jobRec{}, projects: []c4v1.Project{{ObjectMeta: metav1.ObjectMeta{Name: "show-a"}}}}
	for _, j := range []c4v1.WorkJob{render, denoise} {
		rec := &jobRec{job: j, key: j.Namespace + "/" + j.Name, project: "show-a"}
		m.jobs = append(m.jobs, rec)
		m.byKey[rec.key] = rec
	}
	f.srv.m = m

	// An agent releases the held denoise through MCP: the HUD signs a
	// control record and writes it with the change.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/show-a/denoise/nudge?as=wrangler", strings.NewReader(`{"hold": false}`))
	req.Header.Set("X-C4-Client", "mcp")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	var out struct {
		Record string `json:"record"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK || out.Record == "" {
		t.Fatalf("release: %d %s", w.Code, w.Body)
	}
	var live c4v1.WorkJob
	if err := f.k.Get(ctx, client.ObjectKey{Namespace: "show-a", Name: "denoise"}, &live); err != nil {
		t.Fatal(err)
	}
	if live.Annotations[c4v1.AnnotationControlRecords] != out.Record || live.Spec.Nudge.Hold {
		t.Fatalf("the release and its record were not written together: %v hold %v", live.Annotations, live.Spec.Nudge.Hold)
	}

	// The denoise ran on w2; the queue sealed it with the control record.
	d0 := f.sign(provenance.NodeIdentity("w2"), provenance.PredicateTask,
		[]provenance.Subject{provenance.SubjectFor("renders/d.1001.exr", idOf("d1"))},
		task("w2", "denoise", "0", queue.InputMaterialization("denoise", denoise.Spec.Inputs[0]), renderOut))
	dix, _ := json.Marshal(queue.Index{Job: "show-a/denoise", Records: map[string]string{"renders/d.1001.exr": d0}, Units: map[int]string{0: d0}})
	denoiseRec := f.sign(provenance.ComponentIdentity("queue"), provenance.PredicateJob,
		[]provenance.Subject{provenance.SubjectFor("output", denoiseOut)},
		map[string]any{"job": "show-a/denoise", "phase": "Succeeded", "controls": []string{out.Record}})
	d := m.byKey["show-a/denoise"]
	d.job = live
	d.job.Status = c4v1.WorkJobStatus{Phase: "Succeeded", Finished: &metav1.Time{Time: time.Now()},
		Output: &c4v1.WorkOutput{View: denoiseOut.String(), Provenance: f.put(dix), Record: denoiseRec}}
	return f, out.Record
}

func (f *custodyFarm) custody() *custody {
	f.t.Helper()
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/custody/show-a/denoise?path=renders/d.1001.exr&as=wrangler", nil))
	if w.Code != http.StatusOK {
		f.t.Fatalf("custody: %d %s", w.Code, w.Body)
	}
	var c custody
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		f.t.Fatal(err)
	}
	return &c
}

func TestCustodyCrossesEverySigner(t *testing.T) {
	f, control := newCustodyFarm(t)
	c := f.custody()
	if !c.Verified || !c.Complete {
		for _, l := range c.Links {
			t.Logf("%+v", *l)
		}
		t.Fatalf("verified %v complete %v gaps %v", c.Verified, c.Complete, c.Gaps)
	}
	want := []string{
		provenance.NodeIdentity("w2"), provenance.ComponentIdentity("controller"), provenance.ComponentIdentity("queue"),
		provenance.NodeIdentity("w1"), provenance.ServiceAccountIdentity("show-a", "ingest"), provenance.ComponentIdentity("hud"),
	}
	for _, id := range want {
		found := false
		for _, s := range c.Signers {
			found = found || s == id
		}
		if !found {
			t.Errorf("no link signed by %s: %v", id, c.Signers)
		}
	}
	kinds := map[string]int{}
	for _, l := range c.Links {
		kinds[l.Kind]++
		if l.Kind == "control" && (l.Record != control || !strings.Contains(l.About, "by wrangler via mcp")) {
			t.Errorf("control link %+v", *l)
		}
	}
	for _, k := range []string{"task", "resolution", "job", "tasks", "ingest", "control"} {
		if kinds[k] == 0 {
			t.Errorf("no %s link: %v", k, kinds)
		}
	}
}

func TestCustodyFailsOnAForgedLink(t *testing.T) {
	f, _ := newCustodyFarm(t)
	// A node signs a record claiming to be the denoise's job record: the
	// signature verifies, but the policy says only the queue seals jobs.
	forged := f.sign(provenance.NodeIdentity("w1"), provenance.PredicateJob,
		[]provenance.Subject{provenance.SubjectFor("output", idOf("denoise output"))}, map[string]any{"job": "show-a/denoise", "phase": "Succeeded"})
	f.srv.m.byKey["show-a/denoise"].job.Status.Output.Record = forged
	c := f.custody()
	if c.Verified {
		t.Fatal("custody verified with a job record signed by a node")
	}
	// A Materialization that resolved with signing off leaves a gap.
	var m c4v1.Materialization
	if err := f.k.Get(f.ctx, client.ObjectKey{Namespace: "show-a", Name: "plates"}, &m); err != nil {
		t.Fatal(err)
	}
	m.Status.Record = ""
	if err := f.k.Update(f.ctx, &m); err != nil {
		t.Fatal(err)
	}
	if c = f.custody(); c.Complete {
		t.Fatal("custody complete without the plates' resolution record")
	}
}
