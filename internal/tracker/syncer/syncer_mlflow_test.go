package syncer_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm/farmtest"
	"github.com/Avalanche-io/c4-operator/internal/tracker/mlflow"
	"github.com/Avalanche-io/c4-operator/internal/tracker/mlflow/mlflowtest"
	"github.com/Avalanche-io/c4-operator/internal/tracker/syncer"
)

var mlHook = []byte("ml-hook")

type mlEnv struct {
	t     *testing.T
	ml    *mlflowtest.Server
	farm  *farmtest.Farm
	k8s   client.Client
	sync  *syncer.Syncer
	clock *clock
	hooks *httptest.Server
	run   string
	due   time.Time
}

// setupML is an MLflow server with a registered model "denoiser" and one
// run a client created SCHEDULED for C4, due in two days; a Tracker turns
// such runs into ML jobs whose output a party of ML leads reviews.
func setupML(t *testing.T, rework string) *mlEnv {
	t.Helper()
	ck := &clock{t: time.Now().UTC().Truncate(time.Second)}
	ml := mlflowtest.New("denoiser")
	t.Cleanup(ml.Close)
	ml.SetClock(ck.now)
	due := ck.now().Add(48 * time.Hour)
	run := ml.AddRun("7", "denoiser-sweep", "SCHEDULED", map[string]string{"c4.intake": "true", "c4.deadline": due.Format(time.RFC3339)})
	ml.AddRun("7", "a person's own run", "RUNNING", nil)

	sch := runtime.NewScheme()
	if err := c4v1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	tr := &c4v1.Tracker{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ml", Generation: 1},
		Spec: c4v1.TrackerSpec{
			Kind: "mlflow", Endpoint: ml.URL,
			Scope:         map[string]string{"experiment": "7", "model": "denoiser", "user": ml.User},
			Credential:    c4v1.TrackerCredential{Secret: c4v1.TrackerSecretKey{Name: "ml-pw", Key: "password"}},
			WebhookSecret: &c4v1.TrackerSecretKey{Name: "ml-hook", Key: "secret"},
			Events:        c4v1.TrackerEvents{Mode: "poll"},
			Rate:          c4v1.TrackerRate{PerSecond: "1000", Burst: 1000},
			Intake: []c4v1.TrackerIntake{{
				Match:  c4v1.TrackerMatch{Type: "Run", Fields: map[string]string{"status": "SCHEDULED", "c4.intake": "true"}},
				As:     "job",
				Job:    &c4v1.WorkJobSpec{Class: "ml", Params: map[string]string{"steps": "20000"}},
				Review: &c4v1.TrackerReview{Wait: "promote"},
				Rework: rework,
			}},
			VerdictsIn: map[string]string{"alias:production": "approve", "tag:rework": "rework", "tag:accept": "accept"},
			People:     map[string]string{mlflow.Registry: "ml-leads"},
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&c4v1.Tracker{}, &c4v1.TrackerLink{}).
		WithObjects(tr,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ml-pw"}, Data: map[string][]byte{"password": []byte(ml.Password)}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ml-hook"}, Data: map[string][]byte{"secret": mlHook}},
		).Build()
	fm := farmtest.New()
	fm.Now = ck.now
	s := &syncer.Syncer{Client: k8s, Farm: fm, Now: ck.now}
	hooks := httptest.NewServer(s.Webhooks())
	t.Cleanup(hooks.Close)
	return &mlEnv{t: t, ml: ml, farm: fm, k8s: k8s, sync: s, clock: ck, hooks: hooks, run: run, due: due}
}

func (e *mlEnv) pass() {
	e.t.Helper()
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "ml"}, &tr); err != nil {
		e.t.Fatal(err)
	}
	if err := e.sync.Sync(context.Background(), &tr); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

func (e *mlEnv) jobName() string { return syncer.JobName("ml", trackerRef("Run", e.run)) }

func (e *mlEnv) job(fn func(*api.Job)) { e.farm.Update(ns, e.jobName(), fn) }

func (e *mlEnv) link() *c4v1.TrackerLink {
	var l c4v1.TrackerLink
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: syncer.LinkName("ml", trackerRef("Run", e.run))}, &l); err != nil {
		e.t.Fatal(err)
	}
	return &l
}

// LAUNCH.md 3.3 against the fake: a scheduled run becomes an ML job with
// its tag's deadline; the run shows the work's states; the output is
// registered as a model version from the run with its C4 ID; an approver
// setting the production alias serves the promote wait. C4 never sets the
// alias.
func TestMLflowPromoteLoop(t *testing.T) {
	e := setupML(t, "")

	// 1. The scheduled run becomes a job; the person's own run is left.
	e.pass()
	if len(e.farm.Submitted) != 1 {
		t.Fatalf("submitted: %d", len(e.farm.Submitted))
	}
	sub := e.farm.Submitted[0]
	if sub.Spec.Class != "ml" || sub.Spec.DeadlineMode != "Tracking" || !sub.Spec.Deadline.Time.Equal(e.due) || sub.TrackerRef != "ml/Run/"+e.run {
		t.Fatalf("submitted: %+v", sub)
	}

	// 2. Running: the run says so, with the job link and ETA as tags.
	eta := e.clock.now().Add(6 * time.Hour)
	e.job(func(j *api.Job) { j.Phase = "Running"; j.Estimate.EtaP80 = &eta })
	e.pass()
	r := e.ml.GetRun(e.run)
	if r.Status != "RUNNING" || r.Tags["c4.state"] != "running" || r.Tags["c4.eta"] != eta.UTC().Format(time.RFC3339) ||
		r.Tags["c4.job"] != ns+"/"+e.jobName() || !strings.Contains(r.Tags["c4.job_url"], "#/job/work/"+e.jobName()) {
		t.Fatalf("run: %+v", r)
	}
	w := len(e.ml.Writes())
	e.pass()
	if len(e.ml.Writes()) != w {
		t.Fatal("an idle pass wrote")
	}

	// 3. Training finishes; the promote wait opens: the output is
	// registered as version 1, from the run, with its C4 ID.
	e.job(func(j *api.Job) { j.Output = &api.Output{View: "c4Checkpoint1"} })
	e.farm.OpenWait(ns, e.jobName(), "promote", "party:ml-leads", "approve", "rework")
	e.pass()
	vs := e.ml.Versions("denoiser")
	if len(vs) != 1 || vs[0].Source != "c4:///c4Checkpoint1" || vs[0].RunID != e.run || vs[0].Tags["c4.id"] != "c4Checkpoint1" ||
		vs[0].Tags["c4.custody"] == "" {
		t.Fatalf("versions: %+v", vs)
	}
	if rv := e.link().Status.Review; rv == nil || rv.ID != "denoiser/1" {
		t.Fatalf("review: %+v", rv)
	}
	if e.ml.GetRun(e.run).Status != "FINISHED" {
		t.Fatal("the run's work finished while it waits for review")
	}
	e.pass()
	if len(e.ml.Versions("denoiser")) != 1 {
		t.Fatal("registered twice")
	}

	// 4. An approver sets the production alias (the webhook is a hint).
	e.clock.add(time.Minute)
	e.ml.SetAlias("denoiser", "production", "1")
	body := mlflowtest.Payload("model_version_alias", "created", map[string]any{"name": "denoiser", "version": "1", "alias": "production"}, e.clock.now())
	if c, _ := mlflowtest.Deliver(e.hooks.URL+"/webhooks/"+ns+"/ml", body, []byte("nope"), "m1", time.Now()); c != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", c)
	}
	if c, _ := mlflowtest.Deliver(e.hooks.URL+"/webhooks/"+ns+"/ml", body, mlHook, "m2", time.Now().Add(-time.Hour)); c != http.StatusUnauthorized {
		t.Fatalf("stale timestamp: %d", c)
	}
	if c, _ := mlflowtest.Deliver(e.hooks.URL+"/webhooks/"+ns+"/ml", body, mlHook, "m3", time.Now()); c != http.StatusAccepted {
		t.Fatalf("delivery: %d", c)
	}
	e.pass()
	if len(e.farm.Relays) != 1 {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
	rl := e.farm.Relays[0]
	if rl.Do != "approve" || rl.Unit != "promote" || rl.By != "@"+mlflow.Registry || rl.Person != "ml-leads" || !strings.Contains(rl.Note, "alias production") {
		t.Fatalf("relay: %+v", rl)
	}
	e.pass()
	if len(e.farm.Relays) != 1 {
		t.Fatal("relayed twice")
	}

	// 5. Done, and approval stayed in the registry.
	e.job(func(j *api.Job) { j.Phase = "Succeeded" })
	e.pass()
	w = len(e.ml.Writes())
	e.pass()
	if len(e.ml.Writes()) != w {
		t.Fatal("a pass after done wrote")
	}
	for _, c := range e.ml.Writes() {
		if strings.Contains(c.Path, "registered-models/alias") {
			t.Fatalf("c4-sync set an alias: %s", c.Body)
		}
	}
}

// A reviewer's rework tag reworks the job in place (ML retrains in the
// same job), and the run shows the rework.
func TestMLflowReworkTag(t *testing.T) {
	e := setupML(t, "")
	e.pass()
	e.job(func(j *api.Job) { j.Phase = "Running"; j.Output = &api.Output{View: "c4C1"} })
	e.farm.OpenWait(ns, e.jobName(), "promote", "party:ml-leads", "approve", "rework")
	e.pass()
	e.clock.add(time.Minute)
	e.ml.SetVersionTag("denoiser", "1", "c4.note", "val loss regressed on night plates; add the night set")
	e.ml.SetVersionTag("denoiser", "1", "c4.review", "rework")
	e.pass()
	if len(e.farm.Relays) != 1 || e.farm.Relays[0].Do != "rework" || !strings.Contains(e.farm.Relays[0].Note, "night plates") {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
	if len(e.farm.Submitted) != 1 {
		t.Fatal("an in-place rework must not make a new job")
	}
	r := e.ml.GetRun(e.run)
	if r.Status != "RUNNING" || r.Tags["c4.state"] != "rework" {
		t.Fatalf("run during rework: %+v", r)
	}
}

// A decision for a person's wait cannot come from the registry, which
// names no person.
func TestMLflowRegistryCannotServeAPersonsWait(t *testing.T) {
	e := setupML(t, "")
	e.pass()
	e.job(func(j *api.Job) { j.Phase = "Running"; j.Output = &api.Output{View: "c4C1"} })
	e.farm.OpenWait(ns, e.jobName(), "promote", "person:dana", "approve", "rework")
	e.pass()
	e.clock.add(time.Minute)
	e.ml.SetAlias("denoiser", "production", "1")
	e.pass()
	if len(e.farm.Relays) != 0 {
		t.Fatal("the registry served a person's wait")
	}
	if ds := e.link().Status.Decisions; len(ds) != 1 || !strings.Contains(ds[0].Error, "wait is for dana") {
		t.Fatalf("decisions: %+v", ds)
	}
}

func trackerRef(typ, id string) tracker.Ref { return tracker.Ref{Type: typ, ID: id} }

// A run that names its own job (mlflow run --backend c4 submitted it and
// tagged c4.job) is adopted: linked to that job, nothing submitted.
func TestMLflowAdoptsTheJobARunNames(t *testing.T) {
	e := setupML(t, "")
	ref, err := e.farm.Submit(context.Background(), farmJobRequest(ns, "main-v001"))
	if err != nil {
		t.Fatal(err)
	}
	run := e.ml.AddRun("7", "from the plugin", "SCHEDULED", map[string]string{"c4.intake": "true", "c4.job": ns + "/" + ref.Name})
	foreign := e.ml.AddRun("7", "other namespace", "SCHEDULED", map[string]string{"c4.intake": "true", "c4.job": "elsewhere/x"})
	e.pass()
	var l c4v1.TrackerLink
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: syncer.LinkName("ml", trackerRef("Run", run))}, &l); err != nil {
		t.Fatal(err)
	}
	if l.Spec.Target.WorkJob != "main-v001" {
		t.Fatalf("adopted: %+v", l.Spec.Target)
	}
	for _, s := range e.farm.Submitted {
		if strings.Contains(s.TrackerRef, run) || strings.Contains(s.TrackerRef, foreign) {
			t.Fatalf("a run that names its job was submitted again: %+v", s)
		}
	}
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: syncer.LinkName("ml", trackerRef("Run", foreign))}, &l); err == nil {
		t.Fatal("a run naming another namespace's job was linked")
	}
}

func farmJobRequest(namespace, name string) farm.JobRequest {
	return farm.JobRequest{Namespace: namespace, Name: name, Spec: c4v1.WorkJobSpec{Class: "ml"}}
}
