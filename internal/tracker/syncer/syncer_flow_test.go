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
	"github.com/Avalanche-io/c4-operator/internal/tracker"
	"github.com/Avalanche-io/c4-operator/internal/tracker/farm"
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
		// the still's upload completes the publish: Flow sets a field
		// only once the Version exists
		if u.Type == "ApiUser" && ent.Type == "Version" && ev.Fields["event_type"] == "Shotgun_Version_Change" &&
			ev.Fields["attribute_name"] != "image" {
			n++
		}
	}
	return n
}

// The Flow dailies demo, end to end against the fake site: a Task's
// due date becomes the render's deadline; work states go to the Task; the
// render publishes a Version with its C4 ID; the supervisor's note in Flow
// comes back as a rework that makes v002, which follows v001 with the note
// as its input; the supervisor's approval in Flow serves the wait; C4
// never sets an approval.
func TestFlowDailiesLoop(t *testing.T) {
	e := setupFlow(t)
	tid := strconv.Itoa(e.task.ID)
	first := e.jobName()
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
	e.farm.Previews = map[string][]byte{ns + "/" + first: []byte("\x89PNG\r\n\x1a\nv1")}
	e.farm.OpenWait(ns, first, "dailies", "person:dana", "approve", "rework", "accept")
	e.pass()
	vs := e.versions()
	if len(vs) != 1 {
		t.Fatalf("versions: %d", len(vs))
	}
	v1 := vs[0]
	if got := e.site.Upload("Version", v1.ID, "image"); string(got) != "\x89PNG\r\n\x1a\nv1" {
		t.Fatalf("v001's still: %q", got)
	}
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
	e.clock.add(time.Hour)
	e.pass()
	e.pass()
	if got := len(e.site.Writes()); got != w {
		t.Fatalf("done wrote %d times: done is not C4's to write", got-w)
	}
	// and no stale ETA: a job with none clears the field, never leaves the
	// render's
	if got, ok := e.site.Get("Task", e.task.ID)["sg_c4_eta"]; !ok || got != nil {
		t.Fatalf("the finished Task's ETA: %v", got)
	}
	if n := e.scriptVersionWrites(); n != 0 {
		t.Fatalf("c4-sync changed a Version %d times after publishing it", n)
	}
	if e.farm.PreviewCalls != 2 {
		t.Fatalf("two publishes fetched %d stills, want one each", e.farm.PreviewCalls)
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
	e.job(e.jobName(), func(j *api.Job) { j.Phase = "Running" })
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
	job := e.jobName()
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
	e.job(e.jobName(), func(j *api.Job) { j.Phase = "Running" })
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
	first := e.jobName()
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
	job := e.jobName()
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

// In Flow a cause is a Note on the Task, its subject the cause.
func TestFlowCauseNote(t *testing.T) {
	e := setupFlow(t)
	e.pass()
	job := e.jobName()
	e.job(job, func(j *api.Job) {
		j.Phase = "Running"
		j.Causes = []api.Cause{{Kind: "verdict", Key: "verdict:escalate:tables changed", Title: "review: escalate: tables changed", Units: "1001",
			Count: 1, Did: []string{"escalated"}, To: "dana"}}
	})
	e.pass()
	var found int
	for _, n := range e.site.All("Note") {
		if n.Fields["subject"] == "C4: review: escalate: tables changed" && strings.Contains(fmt.Sprint(n.Fields["content"]), "Escalated to dana") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("cause notes: %d", found)
	}
}

// A Task not yet ready is a forecast job sized by its bid; nothing is
// written to Flow for it. When the Task is ready, the real job replaces
// the forecast and the link moves to it.
func TestFlowForecastThenReal(t *testing.T) {
	e := setupFlow(t)
	e.site.Set(e.task, map[string]any{"sg_status_list": "wtg", "est_in_mins": 600}, e.dana)
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
		t.Fatal(err)
	}
	real := tr.Spec.Intake[0]
	fc := *real.DeepCopy()
	fc.As, fc.Review, fc.Rework = "forecast", nil, ""
	fc.Match.Fields = map[string]string{"sg_status_list": "wtg", "step.Step.short_name": "LGT"}
	fc.Forecast = &c4v1.TrackerForecast{Field: "est_in_mins", PerUnit: "0.05", UnitHours: "1"}
	tr.Spec.Intake = []c4v1.TrackerIntake{fc, real}
	if err := e.k8s.Update(context.Background(), &tr); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if len(e.farm.Submitted) != 1 {
		t.Fatalf("submitted %d", len(e.farm.Submitted))
	}
	f := e.farm.Submitted[0]
	if f.Name != e.jobName()+"-forecast" || f.Spec.Forecast == nil || f.Spec.Forecast.SlotHours != "30" || f.Replaces != "" {
		t.Fatalf("the forecast: %+v", f)
	}
	if got := e.site.Get("Task", e.task.ID); got["sg_status_list"] != "wtg" || got["sg_c4_eta"] != nil {
		t.Fatalf("Flow was written for a forecast: %v", got)
	}
	if l := e.link(); l.Spec.Target.WorkJob != f.Name || l.Spec.Intake != 0 {
		t.Fatalf("link: %+v", l.Spec)
	}
	e.pass()
	if len(e.farm.Resized) != 0 {
		t.Fatalf("a forecast resized with no change to its bid: %+v", e.farm.Resized)
	}

	// The bid goes from ten hours to fifteen: the forecast follows it,
	// once.
	e.clock.add(time.Minute)
	e.site.Set(e.task, map[string]any{"est_in_mins": 900}, e.dana)
	e.pass()
	e.pass()
	if len(e.farm.Resized) != 1 || e.farm.Resized[0].SlotHours == nil || *e.farm.Resized[0].SlotHours != "45" || e.farm.Resized[0].UnitHours != nil {
		t.Fatalf("resized: %+v", e.farm.Resized)
	}
	if j, _ := e.farm.Job(context.Background(), farm.JobRef{Namespace: ns, Name: f.Name}); j.Forecast.SlotHours != "45" || j.Forecast.UnitHours != "1" {
		t.Fatalf("the forecast after the bid: %+v", j.Forecast)
	}

	e.clock.add(time.Minute)
	e.site.Set(e.task, map[string]any{"sg_status_list": "rdy"}, e.dana)
	e.pass()
	if len(e.farm.Submitted) != 2 {
		t.Fatalf("submitted %d", len(e.farm.Submitted))
	}
	r := e.farm.Submitted[1]
	if r.Name != e.jobName() || r.Replaces != f.Name || r.Spec.Forecast != nil {
		t.Fatalf("the real job: %+v", r)
	}
	if l := e.link(); l.Spec.Target.WorkJob != r.Name || l.Spec.Intake != 1 {
		t.Fatalf("link after: %+v", l.Spec)
	}
}

// A forecast whose Task leaves every rule (the shot is omitted) is
// withdrawn, and its link goes; if the Task comes back, it is a new
// forecast.
func TestFlowForecastWithdrawn(t *testing.T) {
	e := setupFlow(t)
	ctx := context.Background()
	e.site.Set(e.task, map[string]any{"sg_status_list": "wtg", "est_in_mins": 600}, e.dana)
	var tr c4v1.Tracker
	if err := e.k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
		t.Fatal(err)
	}
	real := tr.Spec.Intake[0]
	fc := *real.DeepCopy()
	fc.As, fc.Review, fc.Rework = "forecast", nil, ""
	fc.Match.Fields = map[string]string{"sg_status_list": "wtg", "step.Step.short_name": "LGT"}
	fc.Forecast = &c4v1.TrackerForecast{Field: "est_in_mins", PerUnit: "0.05", UnitHours: "1"}
	tr.Spec.Intake = []c4v1.TrackerIntake{fc, real}
	if err := e.k8s.Update(ctx, &tr); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if len(e.farm.Submitted) != 1 {
		t.Fatalf("submitted %d", len(e.farm.Submitted))
	}
	name := e.farm.Submitted[0].Name

	e.clock.add(time.Minute)
	e.site.Set(e.task, map[string]any{"sg_status_list": "omt"}, e.dana)
	e.pass()
	tid := strconv.Itoa(e.task.ID)
	if len(e.farm.Withdrawn) != 1 || e.farm.Withdrawn[0] != ns+"/"+name+": Task/"+tid+" no longer matches any of the tracker's intake rules" {
		t.Fatalf("withdrawn: %q", e.farm.Withdrawn)
	}
	if _, err := e.farm.Job(ctx, farm.JobRef{Namespace: ns, Name: name}); !errors.Is(err, farm.ErrNotFound) {
		t.Fatalf("the forecast is still there: %v", err)
	}
	var ls c4v1.TrackerLinkList
	if err := e.k8s.List(ctx, &ls, client.InNamespace(ns)); err != nil || len(ls.Items) != 0 {
		t.Fatalf("links after: %d %v", len(ls.Items), err)
	}
	e.pass()
	if len(e.farm.Withdrawn) != 1 || len(e.farm.Submitted) != 1 {
		t.Fatalf("a second pass did more: withdrawn %q, submitted %d", e.farm.Withdrawn, len(e.farm.Submitted))
	}

	// The shot is back in the cut: a new forecast.
	e.clock.add(time.Minute)
	e.site.Set(e.task, map[string]any{"sg_status_list": "wtg"}, e.dana)
	e.pass()
	if len(e.farm.Submitted) != 2 || e.farm.Submitted[1].Name != name {
		t.Fatalf("submitted after it came back: %d", len(e.farm.Submitted))
	}
	if l := e.link(); l.Spec.Target.WorkJob != name || l.Spec.Intake != 0 {
		t.Fatalf("link: %+v", l.Spec)
	}
}

// jobName is the lighting Task's job: the shot and the step, told apart
// by the Task's ref.
func (e *flowEnv) jobName() string {
	return syncer.JobName("flow", tracker.Item{Ref: tracker.Ref{Type: "Task", ID: strconv.Itoa(e.task.ID)}, Name: "sh040-LGT"})
}

// A job's name stays with its link: the shot renamed in Flow renames
// nothing, v002 builds on v001's name; and a link made when jobs were
// named by ID keeps that form for its next versions.
func TestFlowNamesStayWithTheLink(t *testing.T) {
	rework := func(e *flowEnv, first string) string {
		t.Helper()
		e.job(first, func(j *api.Job) { j.Phase = "Running"; j.Output = &api.Output{View: "c4v-" + first} })
		e.farm.OpenWait(ns, first, "dailies", "person:dana", "approve", "rework", "accept")
		e.pass()
		e.clock.add(time.Minute)
		v := e.versions()[len(e.versions())-1]
		e.site.Set(flowtest.Link{Type: "Version", ID: v.ID}, map[string]any{"sg_status_list": "fix"}, e.dana)
		e.pass()
		return e.link().Spec.Target.WorkJob
	}

	e := setupFlow(t)
	e.pass()
	first := e.jobName()
	if !strings.HasPrefix(first, "flow-sh040-lgt-") || len(e.farm.Submitted) != 1 || e.farm.Submitted[0].Name != first {
		t.Fatalf("the job: %s, submitted %+v", first, e.farm.Submitted)
	}
	shot := e.site.Get("Task", e.task.ID)["entity"].(flowtest.Link)
	e.site.Set(shot, map[string]any{"code": "sh041"}, e.dana)
	if got := rework(e, first); got != first+"-v002" {
		t.Fatalf("v002 is %s, want %s-v002", got, first)
	}

	// A link from before: its job is named by the Task's ID.
	old := setupFlow(t)
	tid := strconv.Itoa(old.task.ID)
	ctx := context.Background()
	if _, err := old.farm.Submit(ctx, farm.JobRequest{Namespace: ns, Name: "flow-" + tid, TrackerRef: "flow/Task/" + tid}); err != nil {
		t.Fatal(err)
	}
	l := &c4v1.TrackerLink{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "flow-task-" + tid},
		Spec: c4v1.TrackerLinkSpec{Tracker: "flow", Ref: c4v1.TrackerRef{Type: "Task", ID: tid}, Target: c4v1.TrackerTarget{WorkJob: "flow-" + tid}}}
	if err := old.k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	old.pass()
	if len(old.farm.Submitted) != 1 {
		t.Fatalf("intake submitted again for a linked Task: %+v", old.farm.Submitted)
	}
	if got := rework(old, "flow-"+tid); got != "flow-"+tid+"-v002" {
		t.Fatalf("an old link's v002 is %s", got)
	}
}

// The Version names the artist the work is for, by the people map run in
// reverse, only once they have confirmed the login in the HUD; it carries
// the job's frames; and the Task carries the job's spend, as one number.
func TestFlowArtistFramesSpend(t *testing.T) {
	publish := func(t *testing.T, confirmed bool, spendOn string) (*flowEnv, *flowtest.Record) {
		t.Helper()
		var opts []func(*fake.ClientBuilder)
		if confirmed {
			opts = append(opts, func(b *fake.ClientBuilder) {
				b.WithObjects(&c4v1.TrackerPerson{
					ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: c4v1.TrackerPersonName("flow", "dana", "dana")},
					Spec: c4v1.TrackerPersonSpec{Tracker: "flow", Login: "dana", Person: "dana",
						Accepted: c4v1.TrackerPersonAct{By: "dana", At: metav1.Now()}}})
			})
		}
		e := setupFlow(t, opts...)
		var tr c4v1.Tracker
		if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
			t.Fatal(err)
		}
		tr.Spec.Scope["spend_field"] = "sg_c4_spend"
		if spendOn != "" {
			tr.Spec.Scope["spend_on"] = spendOn
		}
		if err := e.k8s.Update(context.Background(), &tr); err != nil {
			t.Fatal(err)
		}
		e.pass()
		name := e.jobName()
		e.job(name, func(j *api.Job) {
			j.Phase, j.Owner = "Running", "dana"
			j.Spend = &api.Spend{ModelSlotCost: 1.25, ProceduralSlotCost: 10, TokenCost: 1.5}
			j.Output = &api.Output{View: "c4OutputV001"}
		})
		e.farm.OpenWait(ns, name, "dailies", "person:dana", "approve", "rework", "accept")
		e.pass()
		vs := e.versions()
		if len(vs) != 1 {
			t.Fatalf("versions: %d", len(vs))
		}
		return e, vs[0]
	}
	e, v := publish(t, true, "")
	if u, _ := v.Fields["user"].(flowtest.Link); u.ID != e.dana.ID {
		t.Fatalf("the Version's user is %v, want the artist", v.Fields["user"])
	}
	if v.Fields["frame_range"] != "1001-1240" {
		t.Fatalf("frames: %v", v.Fields["frame_range"])
	}
	// spend goes on the Shot, a total naming no one; a Task is one
	// artist's, so it carries spend only when the Tracker says so
	shot := e.site.Get("Task", e.task.ID)["entity"].(flowtest.Link)
	if got, task := e.site.Get("Shot", shot.ID)["sg_c4_spend"], e.site.Get("Task", e.task.ID)["sg_c4_spend"]; got != 12.75 || task != nil {
		t.Fatalf("the Shot's spend %v, the Task's %v", got, task)
	}
	// spend_on: task is refused for now (no per-person spend setting, no
	// signed record of the write): the spend stays on the Shot, and the
	// Tracker says why
	e, _ = publish(t, true, "task")
	if got, sh := e.site.Get("Task", e.task.ID)["sg_c4_spend"], e.site.Get("Shot", shot.ID)["sg_c4_spend"]; got != nil || sh != 12.75 {
		t.Fatalf("spend_on task: the Task's spend %v, the Shot's %v", got, sh)
	}
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(tr.Status.Conditions, syncer.CondSpend); c == nil || c.Reason != "PerPersonSpendOff" ||
		!strings.Contains(c.Message, "spend on a Task names one person") {
		t.Fatalf("the Tracker's spend condition: %+v", c)
	}
	// the map's offer alone names nobody
	if _, v := publish(t, false, ""); v.Fields["user"] != nil {
		t.Fatalf("an unconfirmed login named the artist: %v", v.Fields["user"])
	}
}

// A Shot's spend is every version's: what v001 spent stays in the total
// after a note makes v002, which adds its own.
func TestFlowShotSpendKeepsEarlierVersions(t *testing.T) {
	e := setupFlow(t)
	var tr c4v1.Tracker
	if err := e.k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "flow"}, &tr); err != nil {
		t.Fatal(err)
	}
	tr.Spec.Scope["spend_field"] = "sg_c4_spend"
	if err := e.k8s.Update(context.Background(), &tr); err != nil {
		t.Fatal(err)
	}
	e.pass()
	first := e.jobName()
	e.job(first, func(j *api.Job) {
		j.Phase, j.Spend, j.Output = "Running", &api.Spend{ProceduralSlotCost: 2}, &api.Output{View: "c4OutputV001"}
	})
	e.farm.OpenWait(ns, first, "dailies", "person:dana", "approve", "rework", "accept")
	e.pass()
	shot := e.site.Get("Task", e.task.ID)["entity"].(flowtest.Link)
	if got := e.site.Get("Shot", shot.ID)["sg_c4_spend"]; got != 2.0 {
		t.Fatalf("v001's spend on the Shot: %v", got)
	}
	e.clock.add(time.Minute)
	e.site.Set(flowtest.Link{Type: "Version", ID: e.versions()[0].ID}, map[string]any{"sg_status_list": "fix"}, e.dana)
	e.pass()
	if len(e.farm.Submitted) != 2 {
		t.Fatalf("submitted %d", len(e.farm.Submitted))
	}
	if l := e.link(); l.Status.SpentBefore == "" || l.Status.Parent == nil || l.Status.Parent.ID != strconv.Itoa(shot.ID) {
		t.Fatalf("link status: spentBefore %q, parent %+v", l.Status.SpentBefore, l.Status.Parent)
	}
	e.job(e.farm.Submitted[1].Name, func(j *api.Job) { j.Phase, j.Spend = "Running", &api.Spend{ProceduralSlotCost: 3} })
	e.clock.add(time.Hour)
	e.pass()
	if got := e.site.Get("Shot", shot.ID)["sg_c4_spend"]; got != 5.0 {
		t.Fatalf("v001 and v002 on the Shot: %v", got)
	}
	// a link made before links kept their parent reads it from Flow once
	l := e.link()
	l.Status.Parent = nil
	if err := e.k8s.Status().Update(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if p := e.link().Status.Parent; p == nil || p.Type != "Shot" || p.ID != strconv.Itoa(shot.ID) {
		t.Fatalf("parent read back: %+v", p)
	}
}
