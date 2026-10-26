package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/tracker/github/githubtest"
)

// --- the actions, as JSON over POST ---

// decode reads an action's body. Only JSON is taken, which a form on
// another site cannot send without the page's own script.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "send JSON", http.StatusUnsupportedMediaType)
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func answer(w http.ResponseWriter, d done, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(d)
}

func (g *GitHub) newIssue(w http.ResponseWriter, r *http.Request) {
	var in issueIn
	if decode(w, r, &in) {
		d, err := g.Issue(in)
		answer(w, d, err)
	}
}

func (g *GitHub) openPull(w http.ResponseWriter, r *http.Request) {
	var in pullIn
	if decode(w, r, &in) {
		d, err := g.Pull(r.Context(), in)
		answer(w, d, err)
	}
}

func (g *GitHub) review(w http.ResponseWriter, r *http.Request) {
	var in reviewIn
	if decode(w, r, &in) {
		d, err := g.Review(r.Context(), in)
		answer(w, d, err)
	}
}

// --- the page ---

// line is one sentence the page says, with a link at its end.
type line struct {
	Text, Link, LinkText string
	Warn                 bool
}

type issueView struct {
	githubtest.Issue
	Comments []githubtest.Comment
	Says     []line
	CanPull  bool
	Pull     *pullView
}

type pullView struct {
	githubtest.Pull
	Reviews   []githubtest.Review
	Checks    []githubtest.CheckRun
	Comments  []githubtest.Comment
	Says      []line
	CanReview bool
}

type pageView struct {
	As      Person
	People  []Person
	Repo    string
	Project string
	Label   string
	HUDYou  string
	Issues  []issueView
}

// hudLink is a page of the HUD, as person, from this page (the HUD serves
// this page, so its root is the HUD's).
func hudLink(person, hash string) string {
	return "/?as=" + url.QueryEscape(person) + "#/" + hash
}

func (g *GitHub) view(ctx context.Context, as Person) pageView {
	v := pageView{As: as, People: g.cfg.People, Repo: g.cfg.Owner + "/" + g.cfg.Repo, Project: g.cfg.Project,
		Label: Label, HUDYou: hudLink(as.Person, "me")}
	pullOf := map[int]int{}
	g.mu.Lock()
	for is, p := range g.pulls {
		pullOf[is] = p
	}
	g.mu.Unlock()
	all := g.gh.Issues()
	for i := len(all) - 1; i >= 0; i-- {
		is := all[i]
		if is.PullRequest != nil {
			continue
		}
		iv := issueView{Issue: is, Comments: g.gh.Comments(is.Number)}
		j := g.job(ctx, is.Number)
		iv.Says = g.issueSays(ctx, is, j, as)
		if n, ok := pullOf[is.Number]; ok {
			iv.Pull = g.pullView(n, j, as)
		} else {
			iv.CanPull = j != nil
		}
		v.Issues = append(v.Issues, iv)
	}
	return v
}

// issueSays is where an issue stands on the farm, in C4's words.
func (g *GitHub) issueSays(ctx context.Context, is githubtest.Issue, j *api.Job, as Person) []line {
	labeled := false
	for _, l := range is.Labels {
		labeled = labeled || l.Name == Label
	}
	if !labeled {
		return []line{{Text: fmt.Sprintf("Not labeled %s, so c4 leaves it alone.", Label)}}
	}
	if len(is.Assignees) == 0 {
		return []line{{Text: "No one is assigned: c4 submits the work for the person it is assigned to.", Warn: true}}
	}
	assignee, _ := g.person(is.Assignees[0].Login)
	if j != nil {
		return []line{{Text: fmt.Sprintf("Issue #%d is job %s in %s for %s: %s.", is.Number, j.Name, g.cfg.Project, assignee.Person, jobWords(j)),
			Link: hudLink(as.Person, "job/"+j.Namespace+"/"+j.Name), LinkText: "watch it in the HUD"}}
	}
	ok, err := g.granted(ctx, g.cfg.Tracker, assignee.Person)
	switch {
	case err != nil:
		log.Printf("reading %s's grant: %v", assignee.Person, err)
		return []line{{Text: "The HUD did not answer; this page asks again in a few seconds.", Warn: true}}
	case !ok:
		s := line{Text: fmt.Sprintf("c4 hasn't been allowed to submit work for %s yet: allow it on", assignee.Person),
			Link: hudLink(assignee.Person, "me"), LinkText: "your page in the HUD", Warn: true}
		if assignee.Person != as.Person {
			s.Text = fmt.Sprintf("c4 hasn't been allowed to submit work for %s yet: %s allows it on", assignee.Person, assignee.Person)
			s.LinkText = "their page in the HUD"
		}
		return []line{s}
	}
	return []line{{Text: fmt.Sprintf("Issue #%d will become a job in %s for %s;", is.Number, g.cfg.Project, assignee.Person),
		Link: hudLink(as.Person, "queue"), LinkText: "watch it in the HUD"}}
}

// jobWords is a job's state in a few words.
func jobWords(j *api.Job) string {
	if w := reviewWait(j); w != nil {
		return "its work is done, and up for review by " + who(w.WaitsFor)
	}
	switch j.Phase {
	case "Succeeded":
		return "done"
	case "Failed":
		return "failed"
	case "Running":
		return "running"
	}
	return "queued"
}

func (g *GitHub) pullView(n int, j *api.Job, as Person) *pullView {
	p, rs, ok := g.gh.Pull(n)
	if !ok {
		return nil
	}
	pv := &pullView{Pull: p, Reviews: rs, Checks: g.gh.CheckRuns(p.Head.SHA), Comments: g.gh.Comments(n)}
	w := reviewWait(j)
	switch {
	case j == nil:
	case w != nil:
		pv.CanReview = true
		s := line{Text: fmt.Sprintf("The job's review waits for %s: approving serves the review wait on %s; requesting changes makes its next version.", who(w.WaitsFor), j.Name)}
		pv.Says = append(pv.Says, s)
		if who(w.WaitsFor) != as.Person {
			pv.Says = append(pv.Says, line{Text: fmt.Sprintf("You are acting as %s, so your review would not serve it; act as %s.", as.Person, who(w.WaitsFor)), Warn: true})
		}
	case j.Phase == "Succeeded":
		pv.Says = append(pv.Says, line{Text: fmt.Sprintf("Approved: %s is done.", j.Name)})
	default:
		pv.Says = append(pv.Says, line{Text: fmt.Sprintf("The job's review opens once %s's work is done; review it then.", j.Name)})
	}
	return pv
}

var focus = regexp.MustCompile(`^` + regexp.QuoteMeta(Prefix) + `(issues|pull)/(\d+)$`)

func (g *GitHub) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != Prefix && !focus.MatchString(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	as, ok := g.person(r.URL.Query().Get("as"))
	if !ok && len(g.cfg.People) > 0 {
		as = g.cfg.People[0]
	}
	v := g.view(r.Context(), as)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	t := "page"
	if r.URL.Query().Get("live") == "1" {
		t = "live"
	}
	if err := pageT.ExecuteTemplate(w, t, v); err != nil {
		log.Print(err)
	}
}

// The little markdown c4-sync writes, shown as GitHub shows it: HTML
// comments (its idempotency keys) hidden; bold, code and links rendered.
var (
	mdComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	mdBold    = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdCode    = regexp.MustCompile("`([^`]+)`")
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\(((?:https?://|/)[^)\s]+)\)`)
)

func markdown(s string) template.HTML {
	s = template.HTMLEscapeString(strings.TrimSpace(mdComment.ReplaceAllString(s, "")))
	s = mdBold.ReplaceAllString(s, "<b>$1</b>")
	s = mdCode.ReplaceAllString(s, "<code>$1</code>")
	s = mdLink.ReplaceAllString(s, `<a href="$2">$1</a>`)
	return template.HTML(s)
}

var pageT = template.Must(template.New("").Funcs(template.FuncMap{
	"md":     markdown,
	"prefix": func() string { return Prefix },
	"concl": func(c *string) string {
		if c == nil {
			return ""
		}
		return *c
	},
	"short": func(s string) string {
		if len(s) > 7 {
			return s[:7]
		}
		return s
	},
}).Parse(`{{define "page"}}<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>{{.Repo}} · sandbox GitHub</title>
<style>
body{font:14px/1.5 -apple-system,Helvetica,sans-serif;margin:0;background:#f6f8fa;color:#1f2328}
main{max-width:860px;margin:0 auto;padding:20px 24px}
.top{background:#fff;border-bottom:1px solid #d0d7de;padding:14px 24px}
.top p{margin:2px 0} .note{font-weight:600}
h1{font-size:19px;margin:0} .sub{color:#656d76;font-size:12px}
.card{background:#fff;border:1px solid #d0d7de;border-radius:6px;padding:12px 16px;margin-bottom:14px}
.card.focus{border-color:#0969da;box-shadow:0 0 0 2px #ddf4ff}
.label{display:inline-block;padding:0 7px;border-radius:10px;font-size:12px;background:#ddf4ff;color:#0550ae}
.says{margin:8px 0;padding:6px 10px;border-radius:6px;background:#f0f7ff} .says.warn{background:#fff8c5}
.c{border-left:3px solid #d0d7de;padding:2px 10px;margin:8px 0;white-space:pre-wrap}
.who{font-weight:600} .ok{color:#1a7f37} .bad{color:#cf222e} .run{color:#9a6700}
.pr{margin-top:10px;padding-top:8px;border-top:1px dashed #d0d7de}
button{font:inherit;padding:3px 12px;border:1px solid #d0d7de;border-radius:6px;background:#f6f8fa;cursor:pointer}
button.go{background:#1f883d;color:#fff;border-color:#1a7f37}
input[type=text]{font:inherit;padding:3px 8px;border:1px solid #d0d7de;border-radius:6px;width:22em}
select{font:inherit} code{font-size:12px} table{border-collapse:collapse;width:100%}
td,th{text-align:left;padding:4px 8px;border-bottom:1px solid #eaeef2;font-size:13px}
#flash{display:none} #flash.on{display:block}
</style>
<div class=top>
<p class=note>A stand-in for GitHub on this machine. Nothing here leaves your computer.</p>
<p>You are acting as <select id=as>{{range .People}}<option value="{{.Login}}"{{if eq .Login $.As.Login}} selected{{end}}>{{.Person}}</option>{{end}}</select>
<span class=sub>(on GitHub, whoever is signed in; here, any of the local farm's people) · <a href="{{.HUDYou}}">your page in the HUD</a></span></p>
</div>
<main>
<h1>{{.Repo}}</h1>
<div class=sub>Issues labeled <span class=label>{{.Label}}</span> become jobs in {{.Project}}, for the person they are assigned to. c4 writes back here as GitHub's c4-sync app would, and never approves or merges.</div>
<div class=card style="margin-top:14px">
<b>New issue</b><br>
<input type=text id=title placeholder="What needs doing">
assigned to <select id=assignee>{{range .People}}<option value="{{.Login}}"{{if eq .Login $.As.Login}} selected{{end}}>{{.Person}}</option>{{end}}</select>
<label><input type=checkbox id=labeled checked> <span class=label>{{.Label}}</span></label>
<button class=go id=open>Submit new issue</button>
</div>
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
document.getElementById('open').onclick = async () => {
  const title = document.getElementById('title').value;
  const d = await post('issues', {title, assignee: document.getElementById('assignee').value, labels: document.getElementById('labeled').checked ? ['{{.Label}}'] : []});
  if (!d.error) document.getElementById('title').value = '';
};
document.addEventListener('click', (e) => {
  const b = e.target.closest('button[data-act]');
  if (!b) return;
  if (b.dataset.act === 'pull') post('pulls', {issue: +b.dataset.issue});
  else post('reviews', {pull: +b.dataset.pull, state: b.dataset.act, body: b.dataset.act === 'changes' ? document.getElementById('note' + b.dataset.pull).value : ''});
});
function mark() {
  const m = location.pathname.match(/(issues|pull)\/(\d+)$/);
  document.querySelectorAll('.focus').forEach((x) => x.classList.remove('focus'));
  const el = m && document.getElementById((m[1] === 'pull' ? 'pr' : 'is') + m[2]);
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
{{define "live"}}{{range .Issues}}<div class=card id="is{{.Number}}">
<b>#{{.Number}} {{.Title}}</b> {{range .Labels}}<span class=label>{{.Name}}</span> {{end}}
<div class=sub>{{.State}} · opened by {{.User.Login}}{{range .Assignees}} · assigned to {{.Login}}{{end}}</div>
{{range .Says}}<div class="says{{if .Warn}} warn{{end}}">{{.Text}}{{if .Link}} <a href="{{.Link}}">{{.LinkText}}</a>{{end}}</div>{{end}}
{{range .Comments}}<div class=c><span class=who>{{.User.Login}}</span>: {{md .Body}}</div>{{end}}
{{if .CanPull}}<button data-act=pull data-issue="{{.Number}}">Open the pull request</button> <span class=sub>the agent's part: on GitHub, the agent doing the work opens it</span>{{end}}
{{with .Pull}}<div class=pr id="pr{{.Number}}"><b>Pull request #{{.Number}} {{.Title}}</b> <span class=sub>by {{.User.Login}} · head <code>{{short .Head.SHA}}</code></span>
<div class=c>{{md .Body}}</div>
{{if .Checks}}<table><tr><th>check</th><th>status</th><th>conclusion</th><th>title</th></tr>
{{range .Checks}}<tr><td>{{.Name}}</td><td>{{.Status}}</td><td class="{{if eq (concl .Conclusion) "success"}}ok{{else if eq (concl .Conclusion) ""}}run{{else}}bad{{end}}">{{concl .Conclusion}}</td><td>{{if .DetailsURL}}<a href="{{.DetailsURL}}">{{.Output.Title}}</a>{{else}}{{.Output.Title}}{{end}}</td></tr>{{end}}
</table>{{end}}
{{range .Reviews}}<div class=c><span class=who>{{.User.Login}}</span> <b class="{{if eq .State "APPROVED"}}ok{{else}}bad{{end}}">{{.State}}</b>{{if .Body}}: {{md .Body}}{{end}}</div>{{end}}
{{range .Comments}}<div class=c><span class=who>{{.User.Login}}</span>: {{md .Body}}</div>{{end}}
{{range .Says}}<div class="says{{if .Warn}} warn{{end}}">{{.Text}}</div>{{end}}
{{if .CanReview}}<button class=go data-act=approve data-pull="{{.Number}}">Approve</button> <button data-act=changes data-pull="{{.Number}}">Request changes</button> <input type=text id="note{{.Number}}" placeholder="what should change">{{end}}
</div>{{end}}
</div>{{else}}<div class=card>No issues yet. Open one above: label it {{.Label}} and assign it to someone.</div>{{end}}{{end}}`))
