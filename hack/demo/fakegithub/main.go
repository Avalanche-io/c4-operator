// Command fakegithub runs the fake GitHub REST API
// (internal/tracker/github/githubtest) as a service, for the GitHub demo
// (hack/demo/github.sh). It is a test double built from GitHub's published
// REST shapes, not GitHub.
//
//	/repos/..., /users/...  the REST API c4-sync's GitHub adapter speaks
//	/demo/...               what people do: open the issue, open a pull
//	                        request and push to it (the agent's part, played
//	                        by the demo script), review it; and the state,
//	                        as JSON (/demo/state) and as a page (/demo/)
//
// A review is also delivered as a signed pull_request_review webhook to
// --hook, as GitHub would. A delivery is a hint: c4-sync polls under it,
// so one that does not arrive is recorded in the state, not fatal.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker/github/githubtest"
)

type demo struct {
	gh         *githubtest.Server
	hook       string
	secret     []byte
	mu         sync.Mutex
	seeded     bool
	deliveries []delivery
}

// delivery is one webhook the fake sent, and what the receiver said.
type delivery struct {
	Event  string `json:"event"`
	ID     string `json:"id"`
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
}

func main() {
	addr := flag.String("listen", ":8080", "address to serve on")
	token := flag.String("token", "demo-gh-token", "the only token the API accepts")
	url := flag.String("url", "http://fakegithub:8080", "the address clients reach this at, for links")
	hook := flag.String("hook", "", "where review webhooks are delivered (empty: none)")
	secret := flag.String("hook-secret", "", "the webhook secret deliveries are signed with")
	flag.Parse()
	http.DefaultClient.Timeout = 5 * time.Second
	gh := githubtest.Unstarted("acme", "widgets")
	gh.Token, gh.URL = *token, *url
	d := &demo{gh: gh, hook: *hook, secret: []byte(*secret)}
	mux := http.NewServeMux()
	mux.Handle("/repos/", gh.Handler())
	mux.Handle("/users/", gh.Handler())
	mux.HandleFunc("POST /demo/seed", d.seed)
	mux.HandleFunc("POST /demo/pull", d.pull)
	mux.HandleFunc("POST /demo/push", d.push)
	mux.HandleFunc("POST /demo/review", d.review)
	mux.HandleFunc("GET /demo/state", d.state)
	mux.HandleFunc("GET /demo/", d.page)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fakegithub on %s (acme/widgets)", *addr)
	log.Fatal(srv.ListenAndServe())
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "body: "+err.Error(), 400)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// seed opens the issue: #7, labeled c4:agent, in a milestone due at the
// given time.
func (d *demo) seed(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Due time.Time `json:"due"`
	}
	if !decode(w, r, &in) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.seeded {
		d.gh.AddUser("pat", "Pat Maintainer", "pat@example.com")
		d.gh.AddUser("dana", "Dana Reyes", "dana@example.com")
		d.gh.AddUser("c4-agent", "C4 agent bundle (a script in this demo)", "")
		d.gh.AddIssue(7, "Port the tier fan-in test", []string{"c4:agent"}, &in.Due, "pat")
		d.seeded = true
	}
	reply(w, map[string]any{"issue": 7})
}

func (d *demo) pull(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Number      int
		Title, Body string
		SHA         string `json:"sha"`
		By          string `json:"by"`
	}
	if !decode(w, r, &in) {
		return
	}
	d.gh.AddPull(in.Number, in.Title, in.Body, in.SHA, in.By)
	reply(w, map[string]any{"ok": true})
}

func (d *demo) push(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Number int
		SHA    string `json:"sha"`
	}
	if !decode(w, r, &in) {
		return
	}
	d.gh.Push(in.Number, in.SHA)
	reply(w, map[string]any{"ok": true})
}

func (d *demo) review(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Pull     int    `json:"pull"`
		By       string `json:"by"`
		State    string `json:"state"`
		Body     string `json:"body"`
		Comments []githubtest.ReviewComment
	}
	if !decode(w, r, &in) {
		return
	}
	rv := d.gh.AddReview(in.Pull, in.By, in.State, in.Body, in.Comments...)
	if d.hook != "" {
		id, code, err := githubtest.Deliver(d.hook, "pull_request_review", d.gh.ReviewPayload(in.Pull, rv), d.secret)
		dl := delivery{Event: "pull_request_review", ID: id, Status: code}
		if err != nil {
			dl.Error = err.Error()
		}
		d.mu.Lock()
		d.deliveries = append(d.deliveries, dl)
		d.mu.Unlock()
	}
	reply(w, map[string]any{"review": rv.ID})
}

type pullView struct {
	githubtest.Pull
	Reviews  []githubtest.Review
	Checks   []githubtest.CheckRun
	Statuses []githubtest.Status
	Comments []githubtest.Comment
}

type view struct {
	Issue      *githubtest.Issue
	Comments   []githubtest.Comment
	Pulls      []pullView
	Deliveries []delivery
	// Judgments counts what c4-sync wrote that would be a judgment: a
	// review or a merge. It must stay 0.
	Judgments int
}

func (d *demo) snapshot() view {
	var v view
	d.mu.Lock()
	v.Deliveries = append(v.Deliveries, d.deliveries...)
	d.mu.Unlock()
	for _, c := range d.gh.Writes() {
		if strings.Contains(c.Path, "/reviews") || strings.Contains(c.Path, "/merge") {
			v.Judgments++
		}
	}
	for _, is := range d.gh.Issues() {
		if is.PullRequest != nil {
			p, rs, ok := d.gh.Pull(is.Number)
			if !ok {
				continue
			}
			v.Pulls = append(v.Pulls, pullView{Pull: p, Reviews: rs, Checks: d.gh.CheckRuns(p.Head.SHA),
				Statuses: d.gh.Statuses(p.Head.SHA), Comments: d.gh.Comments(p.Number)})
			continue
		}
		c := is
		v.Issue = &c
		v.Comments = d.gh.Comments(is.Number)
	}
	return v
}

func (d *demo) state(w http.ResponseWriter, _ *http.Request) { reply(w, d.snapshot()) }

// The little markdown c4-sync writes, shown as GitHub shows it: HTML
// comments (its idempotency keys) hidden; bold, code and links rendered.
var (
	mdComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	mdBold    = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdCode    = regexp.MustCompile("`([^`]+)`")
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)\s]+)\)`)
)

func markdown(s string) template.HTML {
	s = template.HTMLEscapeString(strings.TrimSpace(mdComment.ReplaceAllString(s, "")))
	s = mdBold.ReplaceAllString(s, "<b>$1</b>")
	s = mdCode.ReplaceAllString(s, "<code>$1</code>")
	s = mdLink.ReplaceAllString(s, `<a href="$2">$1</a>`)
	return template.HTML(s)
}

var pageT = template.Must(template.New("p").Funcs(template.FuncMap{
	"md": markdown,
	"concl": func(c *string) string {
		if c == nil {
			return ""
		}
		return *c
	},
	"short": func(s string) string {
		if len(s) > 10 {
			return s[:10]
		}
		return s
	},
	"due": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("Mon 2 Jan 15:04 UTC")
	},
}).Parse(`<!doctype html><meta charset=utf-8><title>fake GitHub: acme/widgets</title>
<style>
body{font:14px/1.5 -apple-system,Helvetica,sans-serif;margin:28px;background:#f6f8fa;color:#1f2328}
h1{font-size:19px;margin:0 0 2px} .sub{color:#656d76;font-size:12px;margin-bottom:16px}
.card{background:#fff;border:1px solid #d0d7de;border-radius:6px;padding:12px 16px;margin-bottom:14px}
.label{display:inline-block;padding:0 7px;border-radius:10px;font-size:12px;background:#ddf4ff;color:#0550ae}
.c{border-left:3px solid #d0d7de;padding:2px 10px;margin:8px 0;white-space:pre-wrap}
.who{font-weight:600}.st{font-weight:600}.ok{color:#1a7f37}.bad{color:#cf222e}.run{color:#9a6700}
code{font-size:12px} table{border-collapse:collapse;width:100%} td,th{text-align:left;padding:4px 8px;border-bottom:1px solid #eaeef2;font-size:13px}
</style>
<h1>acme/widgets</h1>
<div class=sub>A fake GitHub (a test double from GitHub's published REST shapes, not GitHub), for the C4 agent-work demo. The agent here is a script.</div>
{{with .Issue}}<div class=card><b>#{{.Number}} {{.Title}}</b> <span class=label>{{range .Labels}}{{.Name}} {{end}}</span>
<div class=sub>{{.State}} · opened by {{.User.Login}}{{with .Milestone}} · milestone {{.Title}}, due {{due .DueOn}}{{end}}</div>
{{end}}{{range .Comments}}<div class=c><span class=who>{{.User.Login}}</span>: {{md .Body}}</div>{{end}}{{if .Issue}}</div>{{end}}
{{range .Pulls}}<div class=card><b>PR #{{.Number}} {{.Title}}</b> <span class=sub>head <code>{{short .Head.SHA}}</code> · by {{.User.Login}}</span>
<div class=c>{{md .Body}}</div>
<table><tr><th>check</th><th>status</th><th>conclusion</th><th>title</th></tr>
{{range .Checks}}<tr><td>{{.Name}}</td><td>{{.Status}}</td><td class="st {{if eq (concl .Conclusion) "success"}}ok{{else if eq (concl .Conclusion) ""}}run{{else}}bad{{end}}">{{concl .Conclusion}}</td><td>{{.Output.Title}}</td></tr>{{end}}
{{range .Statuses}}<tr><td>{{.Context}}</td><td>status</td><td>{{.State}}</td><td>{{.Description}}</td></tr>{{end}}
</table>
{{range .Reviews}}<div class=c><span class=who>{{.User.Login}}</span> <span class="st {{if eq .State "APPROVED"}}ok{{else}}bad{{end}}">{{.State}}</span>: {{md .Body}}</div>{{end}}
{{range .Comments}}<div class=c><span class=who>{{.User.Login}}</span>: {{md .Body}}</div>{{end}}
</div>{{end}}
`))

func (d *demo) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageT.Execute(w, d.snapshot()); err != nil {
		log.Print(err)
		fmt.Fprint(w, err)
	}
}
