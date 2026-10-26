package sandbox

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker/flow/flowtest"
)

// The stand-in Flow Production Tracking site, beside the stand-in GitHub
// (Config.FlowKey): the fake Flow REST API the tests use
// (internal/tracker/flow/flowtest), which c4-sync speaks to as it speaks to
// a site, at FlowAPI; and a page at FlowPrefix where a person does what
// they would do in Flow: set a Task ready, look at the Version C4
// published (its still, frames and C4 ID), approve it or ask for a fix
// with a note. It is built from Autodesk's published REST shapes, not
// Autodesk software.
const (
	// FlowPrefix is the page, on the sandbox and through the HUD.
	FlowPrefix = Prefix + "flow/"
	// FlowAPI is where c4-sync reaches the REST API on the sandbox:
	// {FlowAPI}/api/v1.1/..., so the Tracker's endpoint is the sandbox's
	// URL and FlowAPI.
	FlowAPI = "/flow"
	// FlowTracker is the Tracker's name, by default.
	FlowTracker = "sandbox-flow"
	// FlowProject is the stand-in site's one Project's id.
	FlowProject = 123
)

// The site's status codes the page sets, as a default site has them.
const (
	flowWaiting  = "wtg"
	flowReady    = "rdy"
	flowReview   = "rev"
	flowApproved = "apr"
	flowFix      = "fix"
)

type flowSite struct {
	cfg  Config
	g    *GitHub // for the HUD and the people
	site *flowtest.Server

	mu    sync.Mutex
	users map[string]flowtest.Link // login: HumanUser
}

func newFlow(cfg Config, g *GitHub) *flowSite {
	site := flowtest.Unstarted()
	site.Key = cfg.FlowKey
	f := &flowSite{cfg: cfg, g: g, site: site, users: map[string]flowtest.Link{}}
	for _, p := range cfg.People {
		f.users[p.Login] = site.AddUser(p.Login, p.Name, p.Login+"@studio.example")
	}
	return f
}

func (f *flowSite) routes(mux *http.ServeMux) {
	mux.Handle(FlowAPI+"/api/v1.1/", http.StripPrefix(FlowAPI, f.site.Handler()))
	mux.HandleFunc("GET "+FlowPrefix, f.page)
	mux.HandleFunc("GET "+FlowPrefix+"thumbnail/{id}", f.thumbnail)
	mux.HandleFunc("POST "+FlowPrefix+"shots", f.newShot)
	mux.HandleFunc("POST "+FlowPrefix+"status", f.setStatus)
}

func (f *flowSite) user(login string) (flowtest.Link, Person, bool) {
	p, ok := f.g.person(login)
	if !ok {
		return flowtest.Link{}, Person{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[login], p, true
}

// --- what people do ---

type shotIn struct {
	As       string `json:"as"`
	Shot     string `json:"shot"`
	Task     string `json:"task"`
	Step     string `json:"step"`
	Assignee string `json:"assignee"`
	// Due is the Task's due date (YYYY-MM-DD); Flow keeps no time.
	Due string `json:"due"`
}

// Shot adds a shot (or finds it) and a Task on it, waiting, assigned and
// due, as a coordinator does in Flow.
func (f *flowSite) Shot(in shotIn) (done, error) {
	by, _, ok := f.user(in.As)
	if !ok {
		return done{}, fmt.Errorf("no one here is %q", in.As)
	}
	who, assignee, ok := f.user(in.Assignee)
	if !ok {
		return done{}, fmt.Errorf("no one here is %q to assign", in.Assignee)
	}
	if strings.TrimSpace(in.Shot) == "" || strings.TrimSpace(in.Task) == "" || strings.TrimSpace(in.Step) == "" {
		return done{}, errors.New("a Task needs a shot, a name and a step")
	}
	if _, err := time.Parse("2006-01-02", in.Due); err != nil {
		return done{}, fmt.Errorf("due: %v", err)
	}
	var shot flowtest.Link
	for _, r := range f.site.All("Shot") {
		if r.Fields["code"] == in.Shot {
			shot = flowtest.Link{Type: "Shot", ID: r.ID, Name: in.Shot}
		}
	}
	if shot.ID == 0 {
		shot = f.site.AddShot(in.Shot)
	}
	t := f.site.AddTask(shot, in.Task, in.Step, flowWaiting, in.Due, by)
	f.site.Set(t, map[string]any{"task_assignees": []flowtest.Link{who}}, by)
	return done{Number: t.ID, Message: fmt.Sprintf("%s %s is waiting, assigned to %s, due %s. Set it ready and c4-sync makes it a job in %s for %s.",
		in.Shot, in.Task, assignee.Person, in.Due, f.cfg.Project, assignee.Person)}, nil
}

type statusIn struct {
	As     string `json:"as"`
	Type   string `json:"type"` // Task or Version
	ID     int    `json:"id"`
	Status string `json:"status"`
	// Note, with a fix, is the supervisor's note on the Version.
	Note string `json:"note"`
}

// Status changes a Task's or a Version's status as a person, and with a
// fix writes their note on the Version: the event log records the person,
// which is how c4-sync tells a person's decision from its own writes.
func (f *flowSite) Status(in statusIn) (done, error) {
	by, p, ok := f.user(in.As)
	if !ok {
		return done{}, fmt.Errorf("no one here is %q", in.As)
	}
	allowed := map[string][]string{"Task": {flowReady}, "Version": {flowApproved, flowFix}}[in.Type]
	if !has(allowed, in.Status) {
		return done{}, fmt.Errorf("the page sets a Task ready, or a Version approved or needing a fix, not %s %s", in.Type, in.Status)
	}
	rec := f.site.Get(in.Type, in.ID)
	if rec == nil {
		return done{}, fmt.Errorf("no %s %d", in.Type, in.ID)
	}
	if in.Status == flowFix && strings.TrimSpace(in.Note) == "" {
		return done{}, errors.New("say what to fix: the note is the next version's input")
	}
	l := flowtest.Link{Type: in.Type, ID: in.ID}
	f.site.Set(l, map[string]any{"sg_status_list": in.Status}, by)
	switch in.Status {
	case flowReady:
		return done{Number: in.ID, Message: fmt.Sprintf("%s set %s ready: c4-sync makes it a job on its next pass, a few seconds from now.", p.Person, rec["content"])}, nil
	case flowFix:
		f.site.AddNote(l, fmt.Sprint(rec["code"]), strings.TrimSpace(in.Note), by)
		return done{Number: in.ID, Message: fmt.Sprintf("%s asked for a fix on %s. c4-sync reads the status and the note from Flow's event log and makes the next version, with the note as its input.", p.Person, rec["code"])}, nil
	}
	return done{Number: in.ID, Message: fmt.Sprintf("%s approved %s. The job's review passes; C4 never sets an approval itself.", p.Person, rec["code"])}, nil
}

func (f *flowSite) newShot(w http.ResponseWriter, r *http.Request) {
	var in shotIn
	if decode(w, r, &in) {
		d, err := f.Shot(in)
		answer(w, d, err)
	}
}

func (f *flowSite) setStatus(w http.ResponseWriter, r *http.Request) {
	var in statusIn
	if decode(w, r, &in) {
		d, err := f.Status(in)
		answer(w, d, err)
	}
}

// thumbnail is a Version's still, as c4-sync uploaded it.
func (f *flowSite) thumbnail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	b := f.site.Upload("Version", id, "image")
	if b == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(b))
	_, _ = w.Write(b)
}

// --- the page ---

type flowVersion struct {
	ID                            int
	Code, Status, C4ID, Frames    string
	Artist, Created, Thumb, Focus string
	Says                          []line
	CanDecide                     bool
	Notes                         []flowNote
}

type flowNote struct{ By, Text string }

type flowTask struct {
	ID                                    int
	Shot, Name, Step, Status, Due, Assign string
	ETA, Link, Spend                      string
	Says                                  []line
	CanReady                              bool
	Versions                              []flowVersion
}

type flowView struct {
	As      Person
	People  []Person
	Project string
	Tracker string
	HUDYou  string
	Tasks   []flowTask
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func (f *flowSite) name(v any) string {
	l, ok := v.(flowtest.Link)
	if !ok {
		return ""
	}
	if l.Type == "ApiUser" {
		return "c4-sync"
	}
	for _, r := range f.site.All("HumanUser") {
		if r.ID == l.ID {
			if p, ok := f.g.person(str(r.Fields["login"])); ok {
				return p.Person
			}
			return str(r.Fields["name"])
		}
	}
	return l.Name
}

func (f *flowSite) view(ctx context.Context, as Person) flowView {
	v := flowView{As: as, People: f.cfg.People, Project: f.cfg.Project, Tracker: f.cfg.FlowTracker, HUDYou: hudLink(as.Person, "me")}
	notes := map[int][]flowNote{}
	for _, n := range f.site.All("Note") {
		for _, l := range linksOf(n.Fields["note_links"]) {
			if l.Type == "Version" {
				notes[l.ID] = append(notes[l.ID], flowNote{By: f.name(n.Fields["user"]), Text: str(n.Fields["content"])})
			}
		}
	}
	versions := map[int][]flowVersion{}
	for _, r := range f.site.All("Version") {
		task, _ := r.Fields["sg_task"].(flowtest.Link)
		fv := flowVersion{ID: r.ID, Code: str(r.Fields["code"]), Status: str(r.Fields["sg_status_list"]), C4ID: str(r.Fields["sg_c4_id"]),
			Frames: str(r.Fields["frame_range"]), Artist: f.name(r.Fields["user"]), Created: str(r.Fields["created_at"]), Notes: notes[r.ID]}
		if r.Fields["image"] != nil {
			fv.Thumb = FlowPrefix + "thumbnail/" + strconv.Itoa(r.ID)
		}
		switch fv.Status {
		case flowReview:
			fv.CanDecide = true
			fv.Says = []line{{Text: "Pending review. Approve it, or ask for a fix with a note: the note becomes the next version's input."}}
		case flowApproved:
			fv.Says = []line{{Text: "Approved in Flow by a person. C4 never sets an approval."}}
		case flowFix:
			fv.Says = []line{{Text: "Needs a fix: c4-sync made the next version from the note."}}
		}
		versions[task.ID] = append(versions[task.ID], fv)
	}
	tasks := f.site.All("Task")
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID > tasks[j].ID })
	for _, r := range tasks {
		shot, _ := r.Fields["entity"].(flowtest.Link)
		step, _ := r.Fields["step"].(flowtest.Link)
		ft := flowTask{ID: r.ID, Shot: shot.Name, Name: str(r.Fields["content"]), Step: step.Name, Status: str(r.Fields["sg_status_list"]),
			Due: str(r.Fields["due_date"]), ETA: str(r.Fields["sg_c4_eta"]), Link: str(r.Fields["sg_c4_link"]), Spend: str(f.site.Get("Shot", shot.ID)["sg_c4_spend"]),
			Versions: versions[r.ID]}
		var assignee Person
		for _, l := range linksOf(r.Fields["task_assignees"]) {
			ft.Assign = f.name(l)
			for _, p := range f.cfg.People {
				if p.Person == ft.Assign {
					assignee = p
				}
			}
		}
		ft.CanReady = ft.Status == flowWaiting
		ft.Says = f.taskSays(ctx, ft, assignee, as)
		v.Tasks = append(v.Tasks, ft)
	}
	return v
}

// taskSays is where a Task stands on the farm, in C4's words.
func (f *flowSite) taskSays(ctx context.Context, t flowTask, assignee, as Person) []line {
	if t.Link != "" {
		return nil // c4-sync has written the job's link and ETA on the Task
	}
	if assignee.Person == "" {
		return []line{{Text: "No one is assigned: c4 submits the work for the person it is assigned to.", Warn: true}}
	}
	ok, err := f.g.granted(ctx, f.cfg.FlowTracker, assignee.Person)
	switch {
	case err != nil:
		log.Printf("reading %s's grant: %v", assignee.Person, err)
		return []line{{Text: "The HUD did not answer; this page asks again in a few seconds.", Warn: true}}
	case !ok:
		s := line{Text: fmt.Sprintf("c4 hasn't been allowed to submit work for %s from Flow yet: allow it on", assignee.Person),
			Link: hudLink(assignee.Person, "me"), LinkText: "your page in the HUD", Warn: true}
		if assignee.Person != as.Person {
			s.Text = fmt.Sprintf("c4 hasn't been allowed to submit work for %s from Flow yet: %s allows it on", assignee.Person, assignee.Person)
			s.LinkText = "their page in the HUD"
		}
		return []line{s}
	case t.Status == flowReady:
		return []line{{Text: fmt.Sprintf("Ready: c4-sync makes it a render job in %s for %s on its next pass.", f.cfg.Project, assignee.Person),
			Link: hudLink(as.Person, "queue"), LinkText: "watch it in the HUD"}}
	}
	return []line{{Text: fmt.Sprintf("Waiting. Set it ready and c4-sync makes it a render job in %s for %s.", f.cfg.Project, assignee.Person)}}
}

func linksOf(v any) []flowtest.Link {
	switch x := v.(type) {
	case []flowtest.Link:
		return x
	case flowtest.Link:
		return []flowtest.Link{x}
	}
	return nil
}

var flowFocus = regexp.MustCompile(`^` + regexp.QuoteMeta(FlowPrefix) + `detail/(Task|Version)/(\d+)$`)

func (f *flowSite) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != FlowPrefix && !flowFocus.MatchString(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	as, ok := f.g.person(r.URL.Query().Get("as"))
	if !ok && len(f.cfg.People) > 0 {
		as = f.cfg.People[0]
	}
	v := f.view(r.Context(), as)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	t := "page"
	if r.URL.Query().Get("live") == "1" {
		t = "live"
	}
	if err := flowPageT.ExecuteTemplate(w, t, v); err != nil {
		log.Print(err)
	}
}

var flowPageT = template.Must(template.New("").Funcs(template.FuncMap{
	"prefix": func() string { return FlowPrefix },
	"short": func(s string) string {
		if len(s) > 22 {
			return s[:14] + "…" + s[len(s)-6:]
		}
		return s
	},
	"link": func(s string) bool {
		return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "/")
	},
}).Parse(`{{define "page"}}<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>{{.Project}} · sandbox Flow</title>
<style>
body{font:14px/1.5 -apple-system,Helvetica,sans-serif;margin:0;background:#f5f6f8;color:#1d2330}
main{max-width:980px;margin:0 auto;padding:20px 24px}
.top{background:#fff;border-bottom:1px solid #dfe3ea;padding:14px 24px}
.top p{margin:2px 0} .note{font-weight:600}
h1{font-size:19px;margin:0} .sub{color:#6b7280;font-size:12px}
.card{background:#fff;border:1px solid #dfe3ea;border-radius:8px;padding:12px 16px;margin-bottom:14px}
.focus{border-color:#2563eb;box-shadow:0 0 0 2px #dbeafe}
table{border-collapse:collapse;width:100%} td,th{text-align:left;padding:5px 8px;border-bottom:1px solid #eef0f4;font-size:13px;vertical-align:top}
th{color:#6b7280;font-weight:600;font-size:11px;text-transform:uppercase;letter-spacing:.04em}
.st{display:inline-block;padding:1px 8px;border-radius:10px;font-size:12px;font-weight:600;background:#e7eaf0}
.st-ip{background:#dbeafe;color:#1e40af}.st-rev{background:#fef3c7;color:#92400e}.st-fix{background:#fee2e2;color:#991b1b}
.st-apr{background:#dcfce7;color:#166534}.st-hld{background:#fde2e2;color:#991b1b}.st-rdy{background:#e0e7ff;color:#3730a3}
.says{margin:8px 0;padding:6px 10px;border-radius:6px;background:#f0f7ff} .says.warn{background:#fff8c5}
.v{display:flex;gap:14px;padding:10px 0;border-top:1px dashed #dfe3ea} .v img,.v .noimg{width:192px;height:108px;object-fit:cover;border-radius:4px;background:#1d2330}
.v .noimg{display:flex;align-items:center;justify-content:center;color:#9ca3af;font-size:12px}
.n{border-left:3px solid #c7cdd8;padding:2px 10px;margin:6px 0;white-space:pre-wrap}
button{font:inherit;padding:3px 12px;border:1px solid #d0d7de;border-radius:6px;background:#f6f8fa;cursor:pointer}
button.go{background:#1f883d;color:#fff;border-color:#1a7f37}
input[type=text]{font:inherit;padding:3px 8px;border:1px solid #d0d7de;border-radius:6px;width:20em}
code{font-size:12px;color:#374151} select{font:inherit} #flash{display:none} #flash.on{display:block}
</style>
<div class=top>
<p class=note>A stand-in for Flow Production Tracking on this machine, built from Autodesk's published REST shapes (not Autodesk software). Nothing here leaves your computer.</p>
<p>You are acting as <select id=as>{{range .People}}<option value="{{.Login}}"{{if eq .Login $.As.Login}} selected{{end}}>{{.Person}}</option>{{end}}</select>
<span class=sub>(in Flow, whoever is signed in; here, any of the local farm's people) · <a href="{{.HUDYou}}">your page in the HUD</a></span></p>
</div>
<main>
<h1>{{.Project}} · Shots</h1>
<div class=sub>Lighting Tasks set ready become render jobs in {{.Project}} for the person assigned (Tracker {{.Tracker}}). c4-sync writes the Task's status, ETA and job link, and the shot's total spend, and publishes each render as a Version in pending review. It never sets an approval.</div>
<div id=flash class=says></div>
<div id=live>{{template "live" .}}</div>
</main>
<script>
const as = () => document.getElementById('as').value;
document.getElementById('as').onchange = () => { const u = new URL(location); u.searchParams.set('as', as()); location = u; };
const flash = (t, warn) => { const f = document.getElementById('flash'); f.textContent = t; f.className = 'says on' + (warn ? ' warn' : ''); };
async function post(path, body) {
  const r = await fetch('{{prefix}}' + path, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(Object.assign({as: as()}, body))});
  const d = await r.json().catch(() => ({error: r.statusText}));
  if (d.error) flash(d.error, true); else flash(d.message);
  refresh();
  return d;
}
document.addEventListener('click', (e) => {
  const b = e.target.closest('button[data-status]');
  if (!b) return;
  const id = +b.dataset.id, note = document.getElementById('note' + id);
  post('status', {type: b.dataset.type, id, status: b.dataset.status, note: note && b.dataset.status === 'fix' ? note.value : ''});
});
function mark() {
  const m = location.pathname.match(/detail\/(Task|Version)\/(\d+)$/);
  document.querySelectorAll('.focus').forEach((x) => x.classList.remove('focus'));
  const el = m && document.getElementById(m[1] + m[2]);
  if (el) el.classList.add('focus');
  return el;
}
async function refresh() {
  const u = new URL(location); u.searchParams.set('as', as()); u.searchParams.set('live', '1');
  const r = await fetch(u).catch(() => null);
  if (r && r.ok) { document.getElementById('live').innerHTML = await r.text(); mark(); }
}
const first = mark(); if (first) first.scrollIntoView({block: 'center'});
setInterval(refresh, 4000);
</script>
{{end}}
{{define "live"}}{{range .Tasks}}<div class=card id="Task{{.ID}}">
<table><tr><th>Shot</th><th>Task</th><th>Status</th><th>Assigned</th><th>Due</th><th>C4 ETA</th><th>C4 spend (the shot's)</th><th>C4 job</th></tr>
<tr><td><b>{{.Shot}}</b></td><td>{{.Name}} <span class=sub>{{.Step}}</span></td><td><span class="st st-{{.Status}}">{{.Status}}</span></td><td>{{.Assign}}</td><td>{{.Due}}</td>
<td>{{.ETA}}</td><td>{{.Spend}}</td><td>{{if link .Link}}<a href="{{.Link}}">the job in the HUD</a>{{else}}{{.Link}}{{end}}</td></tr></table>
{{range .Says}}<div class="says{{if .Warn}} warn{{end}}">{{.Text}}{{if .Link}} <a href="{{.Link}}">{{.LinkText}}</a>{{end}}</div>{{end}}
{{if .CanReady}}<button class=go data-type=Task data-id="{{.ID}}" data-status=rdy>Set ready</button> <span class=sub>the coordinator's part: in Flow, the Task's status set to rdy</span>{{end}}
{{range .Versions}}<div class=v id="Version{{.ID}}">
{{if .Thumb}}<img src="{{.Thumb}}" alt="the still c4-sync uploaded">{{else}}<div class=noimg>no still</div>{{end}}
<div style="flex:1"><b>{{.Code}}</b> <span class="st st-{{.Status}}">{{.Status}}</span>
<div class=sub>frames {{or .Frames "—"}} · artist {{or .Artist "—"}} · created {{.Created}}</div>
<div class=sub>C4 ID of the output view (sg_c4_id) <code title="{{.C4ID}}">{{short .C4ID}}</code></div>
{{range .Says}}<div class="says{{if .Warn}} warn{{end}}">{{.Text}}</div>{{end}}
{{range .Notes}}<div class=n><b>{{.By}}</b>: {{.Text}}</div>{{end}}
{{if .CanDecide}}<button class=go data-type=Version data-id="{{.ID}}" data-status=apr>Approve</button> <button data-type=Version data-id="{{.ID}}" data-status=fix>Needs fix</button> <input type=text id="note{{.ID}}" placeholder="what to fix, e.g. fill light down a stop">{{end}}
</div></div>{{end}}
</div>{{else}}<div class=card>No Tasks yet. <code>c4 farm samples load flow</code> adds sh040's lighting Task.</div>{{end}}{{end}}`))
