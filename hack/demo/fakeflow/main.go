// Command fakeflow runs the fake Flow Production Tracking site
// (internal/tracker/flow/flowtest) as a service, for the dailies demo
// (hack/demo/dailies.sh). It is a test double built from Autodesk's
// published REST shapes, not Autodesk software.
//
//	/api/v1.1/...   the REST API c4-sync's Flow adapter speaks
//	/demo/...       what people do in the site: seed the show, change a
//	                status, write a note; and the site's state, as JSON
//	                (/demo/state) and as a page to capture (/demo/)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker/flow/flowtest"
)

type demo struct {
	site   *flowtest.Server
	mu     sync.Mutex
	users  map[string]flowtest.Link
	task   flowtest.Link
	seeded bool
}

func main() {
	addr := flag.String("listen", ":8080", "address to serve on")
	key := flag.String("script-key", "s3cretkey", "the API script c4-sync's key")
	flag.Parse()
	d := &demo{site: flowtest.Unstarted(), users: map[string]flowtest.Link{}}
	d.site.Key = *key
	mux := http.NewServeMux()
	mux.Handle("/api/v1.1/", d.site.Handler())
	mux.HandleFunc("POST /demo/seed", d.seed)
	mux.HandleFunc("POST /demo/set", d.set)
	mux.HandleFunc("POST /demo/note", d.note)
	mux.HandleFunc("GET /demo/state", d.state)
	mux.HandleFunc("GET /demo/", d.page)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fakeflow on %s (project %d)", *addr, d.site.Project().ID)
	log.Fatal(srv.ListenAndServe())
}

func fail(w http.ResponseWriter, code int, f string, a ...any) {
	http.Error(w, fmt.Sprintf(f, a...), code)
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// seed makes the show once: a producer and a supervisor, shot sh040, and
// its lighting Task, ready, due on the date given.
func (d *demo) seed(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Due string `json:"due"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Due == "" {
		fail(w, 400, "body: {due: YYYY-MM-DD}")
		return
	}
	if _, err := time.Parse("2006-01-02", in.Due); err != nil {
		fail(w, 400, "due: %v", err)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.seeded {
		d.users["pat"] = d.site.AddUser("pat", "Pat Producer", "pat@example.com")
		d.users["dana"] = d.site.AddUser("dana", "Dana Reyes", "dana@example.com")
		shot := d.site.AddShot("sh040")
		d.task = d.site.AddTask(shot, "Lighting", "LGT", "rdy", in.Due, d.users["pat"])
		d.seeded = true
	}
	reply(w, map[string]any{"project": d.site.Project().ID, "task": d.task.ID, "dana": d.users["dana"].ID})
}

func (d *demo) user(login string) (flowtest.Link, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u, ok := d.users[login]
	return u, ok
}

// set changes fields as a person: {type, id, fields, by}.
func (d *demo) set(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type   string         `json:"type"`
		ID     int            `json:"id"`
		Fields map[string]any `json:"fields"`
		By     string         `json:"by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "body: %v", err)
		return
	}
	u, ok := d.user(in.By)
	if !ok || d.site.Get(in.Type, in.ID) == nil {
		fail(w, 404, "no user %q or no %s %d", in.By, in.Type, in.ID)
		return
	}
	d.site.Set(flowtest.Link{Type: in.Type, ID: in.ID}, in.Fields, u)
	reply(w, map[string]any{"ok": true})
}

// note writes a Note as a person: {about: {type, id}, subject, content,
// by, replies}.
func (d *demo) note(w http.ResponseWriter, r *http.Request) {
	var in struct {
		About   flowtest.Link `json:"about"`
		Subject string        `json:"subject"`
		Content string        `json:"content"`
		By      string        `json:"by"`
		Replies []string      `json:"replies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, 400, "body: %v", err)
		return
	}
	u, ok := d.user(in.By)
	if !ok || d.site.Get(in.About.Type, in.About.ID) == nil {
		fail(w, 404, "no user %q or no %s %d", in.By, in.About.Type, in.About.ID)
		return
	}
	n := d.site.AddNote(in.About, in.Subject, in.Content, u, in.Replies...)
	reply(w, map[string]any{"note": n.ID})
}

type view struct {
	Task     map[string]any   `json:"task"`
	Versions []map[string]any `json:"versions"`
	Notes    []map[string]any `json:"notes"`
	Events   int              `json:"events"`
	// ScriptVersionChanges counts changes an API script (c4-sync) made to
	// a Version after creating it: approval is the supervisor's, so the
	// demo expects none.
	ScriptVersionChanges int `json:"scriptVersionChanges"`
}

func (d *demo) snapshot() view {
	d.mu.Lock()
	task := d.task
	d.mu.Unlock()
	v := view{Task: d.site.Get("Task", task.ID)}
	for _, e := range d.site.All("EventLogEntry") {
		v.Events++
		u, _ := e.Fields["user"].(flowtest.Link)
		ent, _ := e.Fields["entity"].(flowtest.Link)
		if u.Type == "ApiUser" && ent.Type == "Version" && e.Fields["event_type"] == "Shotgun_Version_Change" {
			v.ScriptVersionChanges++
		}
	}
	if v.Task != nil {
		v.Task["id"] = task.ID
	}
	for _, r := range d.site.All("Version") {
		m := map[string]any{"id": r.ID}
		for k, x := range r.Fields {
			m[k] = x
		}
		v.Versions = append(v.Versions, m)
	}
	replies := map[int][]string{}
	for _, r := range d.site.All("Reply") {
		if l, ok := r.Fields["entity"].(flowtest.Link); ok {
			replies[l.ID] = append(replies[l.ID], fmt.Sprint(r.Fields["content"]))
		}
	}
	for _, r := range d.site.All("Note") {
		m := map[string]any{"id": r.ID, "replies": replies[r.ID]}
		for k, x := range r.Fields {
			m[k] = x
		}
		v.Notes = append(v.Notes, m)
	}
	sort.Slice(v.Versions, func(i, j int) bool { return v.Versions[i]["id"].(int) < v.Versions[j]["id"].(int) })
	return v
}

func (d *demo) state(w http.ResponseWriter, _ *http.Request) { reply(w, d.snapshot()) }

var pageT = template.Must(template.New("p").Funcs(template.FuncMap{
	"by": func(v any) string {
		if l, ok := v.(flowtest.Link); ok {
			if l.Name != "" {
				return l.Name
			}
			return l.Type + " " + fmt.Sprint(l.ID)
		}
		return ""
	},
	"short": func(v any) string {
		s := fmt.Sprint(v)
		if len(s) > 22 {
			return s[:14] + "…" + s[len(s)-6:]
		}
		return s
	},
}).Parse(`<!doctype html><meta charset=utf-8><title>fake Flow: sh040</title>
<style>
body{font:15px/1.45 -apple-system,Helvetica,sans-serif;margin:28px;background:#f5f6f8;color:#1d2330}
h1{font-size:19px;margin:0 0 4px} .sub{color:#6b7280;font-size:12px;margin-bottom:18px}
.card{background:#fff;border:1px solid #dfe3ea;border-radius:8px;padding:14px 18px;margin-bottom:16px}
table{border-collapse:collapse;width:100%} td,th{text-align:left;padding:6px 8px;border-bottom:1px solid #eef0f4;font-size:14px}
th{color:#6b7280;font-weight:600;font-size:12px;text-transform:uppercase;letter-spacing:.04em}
.st{display:inline-block;padding:1px 8px;border-radius:10px;font-size:12px;font-weight:600;background:#e7eaf0}
.st-ip{background:#dbeafe;color:#1e40af}.st-rev{background:#fef3c7;color:#92400e}.st-fix{background:#fee2e2;color:#991b1b}
.st-apr{background:#dcfce7;color:#166534}.st-hld{background:#fde2e2;color:#991b1b}.st-rdy{background:#e0e7ff;color:#3730a3}
code{font-size:12px;color:#374151} .note{border-left:3px solid #c7cdd8;padding:2px 10px;margin:8px 0}
.reply{color:#4b5563;margin-left:14px}
</style>
<h1>sh040 · Lighting</h1>
<div class=sub>A fake Flow Production Tracking site (a test double from Autodesk's published REST shapes, not Autodesk software), for the C4 dailies demo.</div>
{{with .Task}}<div class=card><table>
<tr><th>Task</th><th>Status</th><th>Due</th><th>C4 ETA (sg_c4_eta)</th><th>C4 job (sg_c4_link)</th></tr>
<tr><td>{{.content}}</td><td><span class="st st-{{.sg_status_list}}">{{.sg_status_list}}</span></td><td>{{.due_date}}</td><td>{{.sg_c4_eta}}</td><td><code>{{.sg_c4_link}}</code></td></tr>
</table></div>{{end}}
<div class=card><table><tr><th>Version</th><th>Status</th><th>C4 ID of the output view (sg_c4_id)</th><th>Created</th></tr>
{{range .Versions}}<tr><td>{{.code}}</td><td><span class="st st-{{.sg_status_list}}">{{.sg_status_list}}</span></td><td><code>{{short .sg_c4_id}}</code></td><td>{{.created_at}}</td></tr>{{else}}<tr><td colspan=4>no versions yet</td></tr>{{end}}
</table></div>
<div class=card><b>Notes</b>
{{range .Notes}}<div class=note><b>{{.subject}}</b> <span class=sub>({{by .user}})</span><br>{{.content}}{{range .replies}}<div class=reply>↳ {{.}}</div>{{end}}</div>{{else}}<div class=sub>none</div>{{end}}
</div>
`))

func (d *demo) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageT.Execute(w, d.snapshot()); err != nil {
		log.Print(err)
	}
}
