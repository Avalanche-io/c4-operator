package syncer_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm/farmtest"
	_ "github.com/Avalanche-io/c4-operator/internal/tracker/flow"
	"github.com/Avalanche-io/c4-operator/internal/tracker/flow/flowtest"
	"github.com/Avalanche-io/c4-operator/internal/tracker/syncer"
)

var flowHook = []byte("flow-hook-token")

type flowEnv struct {
	t     *testing.T
	site  *flowtest.Server
	farm  *farmtest.Farm
	k8s   client.Client
	sync  *syncer.Syncer
	clock *clock
	hooks *httptest.Server
	dana  flowtest.Link
	task  flowtest.Link
	due   time.Time // the Task's due date, a calendar date
}

// setupFlow is a Flow site with one shot and its lighting Task, due four
// days out, and a Tracker that turns ready lighting Tasks into render
// jobs whose deadline is Thursday-style dailies: 14:00 local on the due
// date.
func setupFlow(t *testing.T, opts ...func(*fake.ClientBuilder)) *flowEnv {
	t.Helper()
	ck := &clock{t: time.Now().UTC().Truncate(time.Second)}
	site := flowtest.New()
	t.Cleanup(site.Close)
	site.SetClock(ck.now)
	prod := site.AddUser("pat", "Pat Producer", "pat@example.com")
	dana := site.AddUser("dana", "Dana Reyes", "dana@example.com")
	shot := site.AddShot("sh040")
	due := ck.now().AddDate(0, 0, 4)
	task := site.AddTask(shot, "Lighting", "LGT", "rdy", due.Format("2006-01-02"), prod)

	sch := runtime.NewScheme()
	if err := c4v1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	tr := &c4v1.Tracker{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "flow", Generation: 1},
		Spec: c4v1.TrackerSpec{
			Kind: "flow", Endpoint: site.URL,
			Scope: map[string]string{"project": strconv.Itoa(site.Project().ID), "script": "c4-sync",
				"eta_field": "sg_c4_eta", "link_field": "sg_c4_link"},
			Credential:    c4v1.TrackerCredential{Secret: c4v1.TrackerSecretKey{Name: "flow-script", Key: "key"}},
			WebhookSecret: &c4v1.TrackerSecretKey{Name: "flow-hook", Key: "token"},
			Events:        c4v1.TrackerEvents{Mode: "eventlog"},
			Rate:          c4v1.TrackerRate{PerSecond: "1000", Burst: 1000},
			Intake: []c4v1.TrackerIntake{{
				Match:    c4v1.TrackerMatch{Type: "Task", Fields: map[string]string{"sg_status_list": "rdy", "step.Step.short_name": "LGT"}},
				As:       "job",
				Job:      &c4v1.WorkJobSpec{Class: "render", Params: map[string]string{"frames": "1001-1240"}},
				Deadline: c4v1.TrackerDeadline{DateOnlyAt: "14:00", TimeZone: "America/Los_Angeles"},
				Review:   &c4v1.TrackerReview{Wait: "dailies"},
				Rework:   "nextVersion",
			}},
			StatusOut: c4v1.TrackerStatusOut{States: map[string]string{
				"running": "ip", "rework": "ip", "in-review": "rev", "needs-person": "hld"}},
			VerdictsIn: map[string]string{"status:apr": "approve", "status:fix": "rework", "status:cbb": "accept"},
			People:     map[string]string{"dana": "dana"},
		},
	}
	b := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&c4v1.Tracker{}, &c4v1.TrackerLink{}).
		WithObjects(tr,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "flow-script"}, Data: map[string][]byte{"key": []byte(site.Key)}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "flow-hook"}, Data: map[string][]byte{"token": flowHook}},
		)
	for _, o := range opts {
		o(b)
	}
	k8s := b.Build()
	fm := farmtest.New()
	fm.Now = ck.now
	s := &syncer.Syncer{Client: k8s, Farm: fm, Now: ck.now}
	hooks := httptest.NewServer(s.Webhooks())
	t.Cleanup(hooks.Close)
	return &flowEnv{t: t, site: site, farm: fm, k8s: k8s, sync: s, clock: ck, hooks: hooks, dana: dana, task: task, due: due}
}

func (e *flowEnv) pass() {
	e.t.Helper()
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
		e.t.Fatal(err)
	}
	if err := e.sync.Sync(context.Background(), &tr); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

func (e *flowEnv) link() *c4v1.TrackerLink {
	var l c4v1.TrackerLink
	name := "flow-task-" + strconv.Itoa(e.task.ID)
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &l); err != nil {
		e.t.Fatal(err)
	}
	return &l
}

func (e *flowEnv) job(name string, fn func(*api.Job)) { e.farm.Update(ns, name, fn) }

// versions are the Versions on the shot, oldest first.
func (e *flowEnv) versions() []*flowtest.Record { return e.site.All("Version") }

// scriptVersionWrites counts changes c4-sync's script made to Versions
// after creating them: there must be none (approval is Flow's).
func (e *flowEnv) scriptVersionWrites() int {
	n := 0
	for _, ev := range e.site.All("EventLogEntry") {
		u, _ := ev.Fields["user"].(flowtest.Link)
		ent, _ := ev.Fields["entity"].(flowtest.Link)
		if u.Type == "ApiUser" && ent.Type == "Version" && ev.Fields["event_type"] == "Shotgun_Version_Change" {
			n++
		}
	}
	return n
}

// The LAUNCH.md dailies demo, end to end against the fake site: a Task's
// due date becomes the render's deadline; work states go to the Task; the
// render publishes a Version with its C4 ID; the supervisor's note in Flow
// comes back as a rework that makes v002, which follows v001 with the note
// as its input; the supervisor's approval in Flow serves the wait; C4
// never sets an approval.
func TestFlowDailiesLoop(t *testing.T) {
	e := setupFlow(t)
	tid := strconv.Itoa(e.task.ID)
	first := "flow-" + tid
	// The work is the producer's, under the grant she gave the Tracker.
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
		t.Fatal(err)
	}
	tr.Spec.Intake[0].For = &c4v1.TrackerFor{Person: "pat"}
	if err := e.k8s.Update(context.Background(), &tr); err != nil {
		t.Fatal(err)
	}
	e.farm.Grants = map[string]bool{"pat": true}

	// 1. The ready lighting Task becomes a render job; its date becomes
	// 14:00 Los Angeles time on that date.
	e.pass()
	if len(e.farm.Submitted) != 1 {
		t.Fatalf("submitted %d jobs", len(e.farm.Submitted))
	}
	sub := e.farm.Submitted[0]
	loc, _ := time.LoadLocation("America/Los_Angeles")
	y, m, d := e.due.Date()
	want := time.Date(y, m, d, 14, 0, 0, 0, loc).UTC()
	if sub.Name != first || sub.Spec.DeadlineMode != "Tracking" || !sub.Spec.Deadline.Time.Equal(want) ||
		sub.TrackerRef != "flow/Task/"+tid || sub.Spec.Class != "render" {
		t.Fatalf("submitted: %+v (want deadline %s)", sub, want)
	}

	// 2. It runs: the Task goes in progress, with the ETA and the job link
	// in the site's own fields. A second pass with nothing new writes
	// nothing.
	eta := e.clock.now().Add(5 * time.Hour)
	e.job(first, func(j *api.Job) { j.Phase = "Running"; j.Estimate.EtaP80 = &eta })
	e.pass()
	tk := e.site.Get("Task", e.task.ID)
	if tk["sg_status_list"] != "ip" || tk["sg_c4_eta"] != eta.UTC().Format(time.RFC3339) || !strings.Contains(tk["sg_c4_link"].(string), "#/job/work/"+first) {
		t.Fatalf("task while running: %v", tk)
	}
	w := len(e.site.Writes())
	e.clock.add(time.Minute)
	e.pass()
	if got := len(e.site.Writes()); got != w {
		t.Fatalf("an idle pass wrote %d times", got-w)
	}

	// 3. A node fails some frames and a person is needed: one Note on the
	// Task with the cause, edited in place when the cause changes.
	e.job(first, func(j *api.Job) {
		j.NeedsPerson = &api.NeedsPerson{Reason: "failed", Message: "12 frames failed on node-3: GPU driver"}
	})
	e.pass()
	e.job(first, func(j *api.Job) {
		j.NeedsPerson = &api.NeedsPerson{Reason: "failed", Message: "14 frames failed on node-3: GPU driver"}
	})
	e.pass()
	var notes []*flowtest.Record
	for _, n := range e.site.All("Note") {
		if ls, _ := n.Fields["note_links"].([]flowtest.Link); len(ls) == 1 && ls[0].Type == "Task" {
			notes = append(notes, n)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0].Fields["content"].(string), "14 frames failed on node-3") {
		t.Fatalf("cause notes: %d %v", len(notes), notes)
	}
	if e.site.Get("Task", e.task.ID)["sg_status_list"] != "hld" {
		t.Fatal("a person needed is the site's hold status")
	}
	e.job(first, func(j *api.Job) { j.NeedsPerson = nil })

	// 4. The render finishes and its dailies wait opens: the output is
	// published as v001 in pending review, with its C4 ID.
	e.job(first, func(j *api.Job) { j.Output = &api.Output{View: "c4OutputV001"} })
	e.farm.OpenWait(ns, first, "dailies", "person:dana", "approve", "rework", "accept")
	e.pass()
	vs := e.versions()
	if len(vs) != 1 {
		t.Fatalf("versions: %d", len(vs))
	}
	v1 := vs[0]
	if v1.Fields["code"] != "sh040_lgt_v001" || v1.Fields["sg_status_list"] != "rev" || v1.Fields["sg_c4_id"] != "c4OutputV001" ||
		v1.Fields["sg_task"].(flowtest.Link).ID != e.task.ID || !strings.Contains(v1.Fields["description"].(string), "Provenance:") {
		t.Fatalf("v001: %v", v1.Fields)
	}
	if r := e.link().Status.Review; r == nil || r.ID != strconv.Itoa(v1.ID) {
		t.Fatalf("review: %+v", r)
	}
	if e.site.Get("Task", e.task.ID)["sg_status_list"] != "rev" {
		t.Fatal("the Task should read pending review")
	}
	e.pass()
	if len(e.versions()) != 1 {
		t.Fatal("published twice")
	}

	// 5. Dana, in Flow, sets v001 to needs-fix and writes a note with a
	// follow-up reply. The webhook is a hint; the event log carries it.
	e.clock.add(time.Minute)
	v1l := flowtest.Link{Type: "Version", ID: v1.ID}
	e.site.Set(v1l, map[string]any{"sg_status_list": "fix"}, e.dana)
	e.site.AddNote(v1l, "sh040 lighting", "Fill light too hot, 1040 to 1060.", e.dana, "Keep the rim as is.")
	ev := e.site.LastEvent()
	if c, err := flowtest.Deliver(e.hooks.URL+"/webhooks/"+ns+"/flow", e.site.Payload(ev), []byte("wrong"), "d1"); err != nil || c != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d %v", c, err)
	}
	if c, err := flowtest.Deliver(e.hooks.URL+"/webhooks/"+ns+"/flow", e.site.Payload(ev), flowHook, "d1"); err != nil || c != http.StatusAccepted {
		t.Fatalf("delivery: %d %v", c, err)
	}
	e.pass()
	if len(e.farm.Relays) != 1 {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
	r := e.farm.Relays[0]
	if r.Do != "rework" || r.Unit != "dailies" || r.Person != "dana" ||
		!strings.Contains(r.Note, "Fill light too hot, 1040 to 1060.") || !strings.Contains(r.Note, "Keep the rim as is.") || len(r.Evidence) == 0 {
		t.Fatalf("relay: %+v", r)
	}

	// 6. The rework makes v002: a new job that follows v001, reads the note
	// as an input view, and keeps the tracker's deadline. The link moves
	// to it.
	if len(e.farm.Submitted) != 2 {
		t.Fatalf("submitted: %d", len(e.farm.Submitted))
	}
	second := e.farm.Submitted[1]
	if second.Name != first+"-v002" || second.Follows != first || second.For != "pat" || len(second.Inputs) != 1 ||
		second.Inputs[0].MountPath != syncer.NoteMount || !second.Spec.Deadline.Time.Equal(want) || second.Spec.DeadlineMode != "Tracking" {
		t.Fatalf("v002 job: %+v", second)
	}
	if note := string(e.farm.Views[second.Inputs[0].View]["note.md"]); !strings.Contains(note, "Fill light too hot") {
		t.Fatalf("note view: %q", note)
	}
	l := e.link()
	if l.Spec.Target.WorkJob != second.Name || len(l.Status.Versions) != 1 || l.Status.Versions[0] != first || l.Status.Review != nil {
		t.Fatalf("link after rework: %+v %+v", l.Spec.Target, l.Status)
	}

	// 7. v002 renders and is published as v002 for review.
	e.job(second.Name, func(j *api.Job) { j.Phase = "Running" })
	e.pass()
	if e.site.Get("Task", e.task.ID)["sg_status_list"] != "ip" {
		t.Fatal("the Task should be back in progress")
	}
	e.job(second.Name, func(j *api.Job) { j.Output = &api.Output{View: "c4OutputV002"} })
	e.farm.OpenWait(ns, second.Name, "dailies", "person:dana", "approve", "rework", "accept")
	e.pass()
	vs = e.versions()
	if len(vs) != 2 || vs[1].Fields["code"] != "sh040_lgt_v002" || vs[1].Fields["sg_c4_id"] != "c4OutputV002" || vs[1].Fields["sg_status_list"] != "rev" {
		t.Fatalf("v002: %d %v", len(vs), vs[len(vs)-1].Fields)
	}
	if d, _ := vs[1].Fields["description"].(string); !strings.HasPrefix(d, "C4 output, version 2, follows "+ns+"/"+first+".") {
		t.Fatalf("v002's description says what it is and what it follows, no state: %q", d)
	}

	// 8. Dana approves v002 in Flow: the approval serves the wait. Nothing
	// C4 wrote to a Version after creating it: approval stays in Flow.
	e.clock.add(time.Minute)
	e.site.Set(flowtest.Link{Type: "Version", ID: vs[1].ID}, map[string]any{"sg_status_list": "apr"}, e.dana)
	e.pass()
	if len(e.farm.Relays) != 2 || e.farm.Relays[1].Do != "approve" {
		t.Fatalf("relays: %+v", e.farm.Relays)
	}
	e.job(second.Name, func(j *api.Job) { j.Phase = "Succeeded" })
	w = len(e.site.Writes())
	e.pass()
	e.pass()
	if got := len(e.site.Writes()); got != w {
		t.Fatalf("done wrote %d times: done is not C4's to write", got-w)
	}
	if n := e.scriptVersionWrites(); n != 0 {
		t.Fatalf("c4-sync changed a Version %d times after publishing it", n)
	}
	if e.site.Get("Version", v1.ID)["sg_status_list"] != "fix" || e.site.Get("Version", vs[1].ID)["sg_status_list"] != "apr" {
		t.Fatal("the supervisor's statuses must stand")
	}
}

// A Task's due date moved in Flow moves the job's tracking deadline, even
// after the Task left the status intake matches.
func TestFlowDueDateMoves(t *testing.T) {
	e := setupFlow(t)
	e.pass()
	e.job("flow-"+strconv.Itoa(e.task.ID), func(j *api.Job) { j.Phase = "Running" })
	e.pass() // the Task is now ip: intake no longer lists it
	e.clock.add(time.Minute)
	newDue := e.due.AddDate(0, 0, 2)
	e.site.Set(e.task, map[string]any{"due_date": newDue.Format("2006-01-02")}, e.dana)
	e.pass()
	if len(e.farm.Deadlines) != 1 {
		t.Fatalf("deadlines: %v", e.farm.Deadlines)
	}
	loc, _ := time.LoadLocation("America/Los_Angeles")
	y, m, d := newDue.Date()
	if want := time.Date(y, m, d, 14, 0, 0, 0, loc).UTC(); !e.farm.Deadlines[0].Equal(want) {
		t.Fatalf("deadline %s, want %s", e.farm.Deadlines[0], want)
	}
}

// Someone other than the wait's person, or a script, cannot serve it.
func TestFlowOnlyTheReviewerDecides(t *testing.T) {
	e := setupFlow(t)
	e.pass()
	job := "flow-" + strconv.Itoa(e.task.ID)
	e.job(job, func(j *api.Job) { j.Phase = "Running"; j.Output = &api.Output{View: "c4v"} })
	e.farm.OpenWait(ns, job, "dailies", "person:dana", "approve", "rework")
	e.pass()
	v := e.versions()[0]
	e.clock.add(time.Minute)
	pat := e.site.AddUser("lee", "Lee", "")
	e.site.Set(flowtest.Link{Type: "Version", ID: v.ID}, map[string]any{"sg_status_list": "apr"}, pat)
	e.pass()
	if len(e.farm.Relays) != 0 {
		t.Fatal("an unmapped user's approval was relayed")
	}
	if ds := e.link().Status.Decisions; len(ds) != 1 || !strings.Contains(ds[0].Error, "not mapped") {
		t.Fatalf("decisions: %+v", ds)
	}
}

// Access tokens expire: the adapter gets a fresh one and carries on.
func TestFlowTokenRenewal(t *testing.T) {
	e := setupFlow(t)
	e.pass()
	e.site.ExpireTokens()
	e.job("flow-"+strconv.Itoa(e.task.ID), func(j *api.Job) { j.Phase = "Running" })
	e.pass()
	if e.site.Get("Task", e.task.ID)["sg_status_list"] != "ip" {
		t.Fatal("the pass after expiry did not write")
	}
	n := 0
	for _, c := range e.site.Calls() {
		if strings.HasSuffix(c.Path, "/auth/access_token") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d token requests, want 2: one at the start, one after the expiry", n)
	}
}

// A pass that relays a rework and submits the next version, but cannot
// move the link (the API refused the patch: on the live cluster, RBAC),
// finishes the next version on the following pass instead of losing it.
func TestFlowNextVersionSurvivesAFailedLinkMove(t *testing.T) {
	refused := 0
	e := setupFlow(t, func(b *fake.ClientBuilder) {
		b.WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
			if _, ok := o.(*c4v1.TrackerLink); ok && refused == 0 {
				refused++
				return apierrors.NewForbidden(schema.GroupResource{Group: "c4.avalanche.io", Resource: "trackerlinks"}, o.GetName(), errors.New("no patch"))
			}
			return c.Patch(ctx, o, p, opts...)
		}})
	})
	first := "flow-" + strconv.Itoa(e.task.ID)
	e.pass()
	e.job(first, func(j *api.Job) { j.Phase = "Running"; j.Output = &api.Output{View: "c4v1"} })
	e.farm.OpenWait(ns, first, "dailies", "person:dana", "approve", "rework", "accept")
	e.pass()
	e.clock.add(time.Minute)
	v := e.versions()[0]
	e.site.Set(flowtest.Link{Type: "Version", ID: v.ID}, map[string]any{"sg_status_list": "fix"}, e.dana)
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
		t.Fatal(err)
	}
	if err := e.sync.Sync(context.Background(), &tr); err == nil {
		t.Fatal("the refused patch should fail the pass")
	}
	if l := e.link(); l.Spec.Target.WorkJob != first {
		t.Fatalf("the link moved despite the refusal: %s", l.Spec.Target.WorkJob)
	}
	e.pass()
	l := e.link()
	if l.Spec.Target.WorkJob != first+"-v002" || len(l.Status.Versions) != 1 || l.Status.Versions[0] != first {
		t.Fatalf("after the retry: target %s, versions %v", l.Spec.Target.WorkJob, l.Status.Versions)
	}
	if n := len(e.farm.Submitted); n != 2 || len(e.farm.Relays) != 1 {
		t.Fatalf("submitted %d jobs and relayed %d decisions; want 2 and 1", n, len(e.farm.Relays))
	}
}

// A Task taken as a milestone becomes a Milestone covering every job of
// its shot, whoever submits them, due at the project's due time on the
// Task's date; a moved date moves it. No job is submitted.
func TestFlowMilestoneCoversTheShot(t *testing.T) {
	proj := &c4v1.Project{ObjectMeta: metav1.ObjectMeta{Name: "show"}, Spec: c4v1.ProjectSpec{Namespaces: []string{ns},
		Work: &c4v1.ProjectWork{DueTime: &c4v1.DueTime{At: "18:00", TimeZone: "America/Los_Angeles"}}}}
	e := setupFlow(t, func(b *fake.ClientBuilder) { b.WithObjects(proj) })
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
		t.Fatal(err)
	}
	tr.Spec.Intake[0].As = "milestone"
	tr.Spec.Intake[0].Job = nil
	tr.Spec.Intake[0].Deadline = c4v1.TrackerDeadline{}
	tr.Spec.Intake[0].Covers = map[string]string{c4v1.LabelShot: "entity.name"}
	if err := e.k8s.Update(context.Background(), &tr); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if len(e.farm.Submitted) != 0 {
		t.Fatalf("milestone intake submitted %d jobs", len(e.farm.Submitted))
	}
	loc, _ := time.LoadLocation("America/Los_Angeles")
	at := func(d time.Time) time.Time {
		y, m, dd := d.Date()
		return time.Date(y, m, dd, 18, 0, 0, 0, loc)
	}
	name := "flow-task-" + strconv.Itoa(e.task.ID)
	m := func() *c4v1.Milestone {
		var m c4v1.Milestone
		if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}
	got := m()
	if !got.Spec.Due.Time.Equal(at(e.due)) || got.Spec.Selector.MatchLabels[c4v1.LabelShot] != "sh040" || len(got.Spec.Selector.MatchLabels) != 1 ||
		got.Spec.Source == nil || got.Spec.Source.Tracker != "flow" || got.Spec.Source.Ref != "Task/"+strconv.Itoa(e.task.ID) ||
		len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "flow" {
		t.Fatalf("milestone: %+v", got)
	}

	e.clock.add(time.Minute)
	newDue := e.due.AddDate(0, 0, 2)
	e.site.Set(e.task, map[string]any{"due_date": newDue.Format("2006-01-02")}, e.dana)
	e.pass()
	if got := m(); !got.Spec.Due.Time.Equal(at(newDue)) {
		t.Fatalf("due %s, want %s", got.Spec.Due, at(newDue))
	}
}

// A milestone rule that names no covers would cover nothing: the Tracker
// says so, and no Milestone is made.
func TestFlowMilestoneNeedsCovers(t *testing.T) {
	e := setupFlow(t)
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
		t.Fatal(err)
	}
	tr.Spec.Intake[0].As = "milestone"
	if err := e.k8s.Update(context.Background(), &tr); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(tr.Status.Conditions, syncer.CondIntake)
	if c == nil || c.Reason != "MilestoneCoversNothing" {
		t.Fatalf("condition: %+v", c)
	}
	var ms c4v1.MilestoneList
	if err := e.k8s.List(context.Background(), &ms); err != nil || len(ms.Items) != 0 {
		t.Fatalf("milestones: %d (%v)", len(ms.Items), err)
	}
}

// In Flow the assignment is a Note on the Task addressed to the person,
// so Flow notifies them.
func TestFlowAssignmentAddressesTheNote(t *testing.T) {
	e := setupFlow(t)
	e.pass()
	job := "flow-" + strconv.Itoa(e.task.ID)
	at := e.clock.now()
	e.job(job, func(j *api.Job) {
		j.Phase = "Running"
		j.Acks = []api.Ack{{Kind: "assign", Units: "1001-1004", Actor: "pat", Assignee: "dana", Note: "relight the key", At: at}}
	})
	e.pass()
	e.clock.add(time.Minute)
	e.pass()
	var found []*flowtest.Record
	for _, n := range e.site.All("Note") {
		if strings.Contains(fmt.Sprint(n.Fields["content"]), "units 1001-1004") {
			found = append(found, n)
		}
	}
	if len(found) != 1 {
		t.Fatalf("notes about the assignment: %d", len(found))
	}
	to := fmt.Sprint(found[0].Fields["addressings_to"])
	if !strings.Contains(to, strconv.Itoa(e.dana.ID)) || found[0].Fields["subject"] != "C4: units assigned to you" {
		t.Fatalf("note: %+v", found[0].Fields)
	}
}
