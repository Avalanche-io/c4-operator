// Package kitsutest is a fake Kitsu site for testing c4-sync offline. It
// serves the subset of Zou's REST API that c4-sync's Kitsu adapter uses,
// in the shapes Zou's source gives them (github.com/cgwire/zou, main,
// read 2026-10-20; checked against a local cgwire/cgwire server):
//
//   - GET /api/auth/authenticated, and POST /api/auth/login for people
//     (bots log in with their token only, and never by password);
//   - GET /api/data/{projects,task-status,task-types,entity-types,
//     entities,tasks,persons} with column filters (a JSON array is an IN
//     filter), plain arrays without page; GET .../{id}; PUT
//     /api/data/tasks/{id} (data is merged; an empty data object clears
//     it, as Zou does);
//   - GET /api/data/tasks/{id}/full, with the entity, its type, sequence
//     and episode nested;
//   - GET /api/data/tasks/{id}/comments, newest first, with the author,
//     status, previews and replies embedded;
//   - POST /api/actions/tasks/{id}/comment, the only way a status
//     changes: mentions are read from the text ("@First Last" followed by
//     a space or the end), and "#all" or "#<task type>" posts the comment
//     again on the entity's other tasks;
//   - PUT /api/data/comments/{id}, by the author or a manager;
//   - POST /api/actions/tasks/{id}/comments/{cid}/add-preview, a preview
//     that is "processing" until a file is uploaded to
//     POST /api/pictures/preview-files/{id} (png, jpg, mp4...; anything
//     else deletes the preview and is refused);
//   - GET /api/data/events/last: newest first, a thousand at most, times
//     written to the second and compared finer, for managers only.
//
// Errors are Zou's three shapes: {"error": true, "message"}, the JWT
// library's {"msg"} (401 revoked, 422 not a token), and CRUD's
// {"message"}.
package kitsutest

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const layout = "2006-01-02T15:04:05"

// Person is a Kitsu user or bot.
type Person struct {
	ID, FirstName, LastName, Email, Role, Password, Token string
	Bot                                                   bool
}

// FullName is how Kitsu names a person, and how a mention must.
func (p *Person) FullName() string { return strings.TrimSpace(p.FirstName + " " + p.LastName) }

// Status is a task status.
type Status struct {
	ID, Name, ShortName                                   string
	IsDone, IsRetake, IsFeedbackRequest, IsWIP, IsDefault bool
	ForConcept                                            bool
}

// Entity is a shot, sequence, episode or asset.
type Entity struct {
	ID, Name, ParentID, TypeID, ProjectID string
}

// Task is one task.
type Task struct {
	ID, ProjectID, EntityID, TypeID, StatusID string
	Due                                       string // naive, Zou's layout; "" for none
	Estimation                                float64
	Priority                                  int
	Assignees                                 []string
	Data                                      map[string]any
	Updated                                   time.Time
	RetakeCount                               int
}

// Comment is a comment on a task.
type Comment struct {
	ID, TaskID, StatusID, PersonID, Text string
	Created                              time.Time
	Mentions                             []string
	Previews                             []string
	Replies                              []Reply
}

// Reply is a reply to a comment.
type Reply struct {
	ID, PersonID, Text string
	Created            time.Time
}

// Preview is a preview file.
type Preview struct {
	ID, TaskID, CommentID, PersonID, Status, Extension, OriginalName string
	Revision                                                         int
	Size                                                             int
}

// Event is one row of the event log.
type Event struct {
	ID, Name, UserID, ProjectID string
	Created                     time.Time
	Data                        map[string]any
}

// Call is one request served.
type Call struct{ Method, Path, Body string }

// Server is the fake site.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	now      func() time.Time
	n        int
	persons  map[string]*Person
	projects map[string]string   // id: name
	team     map[string][]string // project: person ids
	statuses []*Status
	types    map[string]string // task type id: name
	etypes   map[string]string // entity type id: name
	entities map[string]*Entity
	tasks    map[string]*Task
	comments map[string]*Comment
	previews map[string]*Preview
	events   []*Event
	calls    []Call
	// drop answers the next n matching writes with 502 after making them:
	// the answer is lost, the write is not.
	drop     int
	dropPath string
	// Project is the production the site starts with, Saltmarsh.
	Project string
}

// New starts a fake site, on a random local port, with Zou's seeded
// statuses and entity types and one production.
func New() *Server {
	s := newServer()
	s.Server.Start()
	return s
}

// Unstarted makes a fake site that does not listen: serve Handler()
// yourself, beside other routes.
func Unstarted() *Server { return newServer() }

// Handler is the site's API, to mount at /api/ beside other routes.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

func newServer() *Server {
	s := &Server{now: time.Now, persons: map[string]*Person{}, projects: map[string]string{}, team: map[string][]string{},
		types: map[string]string{}, etypes: map[string]string{}, entities: map[string]*Entity{}, tasks: map[string]*Task{},
		comments: map[string]*Comment{}, previews: map[string]*Preview{}}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range []Status{
		{Name: "Todo", ShortName: "todo", IsDefault: true},
		{Name: "Work In Progress", ShortName: "wip", IsWIP: true},
		{Name: "Waiting For Approval", ShortName: "wfa", IsFeedbackRequest: true},
		{Name: "Retake", ShortName: "retake", IsRetake: true},
		{Name: "Done", ShortName: "done", IsDone: true},
		{Name: "Ready To Start", ShortName: "ready"},
		{Name: "Neutral", ShortName: "neutral", IsDefault: true, ForConcept: true},
		{Name: "Approved", ShortName: "approved", ForConcept: true},
		{Name: "Rejected", ShortName: "rejected", ForConcept: true},
	} {
		st.ID = s.id("status")
		c := st
		s.statuses = append(s.statuses, &c)
	}
	for _, n := range []string{"Episode", "Sequence", "Shot", "Edit", "Character", "Prop", "Environment"} {
		s.etypes[s.id("etype")] = n
	}
	for _, n := range []string{"Animation", "Lighting", "Compositing", "Modeling"} {
		s.types[s.id("ttype")] = n
	}
	s.Project = s.id("project")
	s.projects[s.Project] = "Saltmarsh"
	return s
}

// id makes a UUID-shaped id, distinct per kind.
func (s *Server) id(kind string) string {
	s.n++
	k := 0
	for _, r := range kind {
		k = k*31 + int(r)
	}
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", uint32(k), s.n)
}

// SetClock replaces time.Now.
func (s *Server) SetClock(now func() time.Time) { s.mu.Lock(); s.now = now; s.mu.Unlock() }

// --- what people do in the site, for tests and demos ---

// AddPerson adds a person to the production's team: role user (an
// artist), supervisor, manager or admin. A bot gets a token; a person a
// password, "pw-" and their first name.
func (s *Server) AddPerson(first, last, email, role string, bot bool) *Person {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &Person{ID: s.id("person"), FirstName: first, LastName: last, Email: email, Role: role, Bot: bot}
	if bot {
		p.Token = "bot-" + p.ID
	} else {
		p.Password = "pw-" + strings.ToLower(first)
	}
	s.persons[p.ID] = p
	s.team[s.Project] = append(s.team[s.Project], p.ID)
	return p
}

// Regenerate gives a bot a new token; the old one is revoked.
func (s *Server) Regenerate(p *Person) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	p.Token = fmt.Sprintf("bot-%s-%d", p.ID, s.n)
	return p.Token
}

// AddEntity adds a sequence, shot or asset by its type name, under parent
// ("" for none).
func (s *Server) AddEntity(kind, name, parent string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := &Entity{ID: s.id("entity"), Name: name, ParentID: parent, TypeID: s.etype(kind), ProjectID: s.Project}
	s.entities[e.ID] = e
	return e.ID
}

func (s *Server) etype(name string) string {
	for id, n := range s.etypes {
		if n == name {
			return id
		}
	}
	id := s.id("etype")
	s.etypes[id] = name
	return id
}

// AddTask adds a task of a type on an entity, in a status (short name),
// due on a date (YYYY-MM-DD, "" for none), with an estimation in minutes.
func (s *Server) AddTask(entity, taskType, status, due string, minutes float64, assignees ...string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &Task{ID: s.id("task"), ProjectID: s.Project, EntityID: entity, TypeID: s.ttype(taskType),
		StatusID: s.status(status).ID, Estimation: minutes, Assignees: assignees, Updated: s.now()}
	if due != "" {
		t.Due = due + "T00:00:00"
	}
	s.tasks[t.ID] = t
	return t.ID
}

func (s *Server) ttype(name string) string {
	for id, n := range s.types {
		if n == name {
			return id
		}
	}
	panic("kitsutest: no task type " + name)
}

func (s *Server) status(short string) *Status {
	for _, st := range s.statuses {
		if st.ShortName == short {
			return st
		}
	}
	panic("kitsutest: no status " + short)
}

// AddStatus adds a studio status, as an admin does in the global library.
func (s *Server) AddStatus(st Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.ID = s.id("status")
	s.statuses = append(s.statuses, &st)
}

// Comment posts a comment as a person, as the comment action does,
// moving the task to a status (short name; "" keeps it).
func (s *Server) Comment(as *Person, task, status, text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.tasks[task].StatusID
	if status != "" {
		st = s.status(status).ID
	}
	c, e := s.comment(as, task, st, text)
	if e != nil {
		panic(e.msg)
	}
	return c.ID
}

// ReplyTo replies to a comment as a person.
func (s *Server) ReplyTo(as *Person, comment, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.comments[comment]
	c.Replies = append(c.Replies, Reply{ID: s.id("reply"), PersonID: as.ID, Text: text, Created: s.now()})
}

// SetTask changes a task as a person in the site would, from a field
// map: due (YYYY-MM-DD), estimation (minutes), assignees.
func (s *Server) SetTask(as *Person, task string, f map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tasks[task]
	for k, v := range f {
		switch k {
		case "due":
			t.Due = v.(string) + "T00:00:00"
		case "estimation":
			t.Estimation = v.(float64)
		case "assignees":
			t.Assignees = v.([]string)
		default:
			panic("kitsutest: SetTask " + k)
		}
	}
	t.Updated = s.now()
	s.event("task:update", as.ID, map[string]any{"task_id": t.ID})
}

// Task returns a copy of a task.
func (s *Server) Task(id string) Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := *s.tasks[id]
	t.Data = map[string]any{}
	for k, v := range s.tasks[id].Data {
		t.Data[k] = v
	}
	return t
}

// StatusOf is a task's status short name.
func (s *Server) StatusOf(task string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.statuses {
		if st.ID == s.tasks[task].StatusID {
			return st.ShortName
		}
	}
	return ""
}

// Comments returns a task's comments, oldest first.
func (s *Server) Comments(task string) []Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Comment
	for _, c := range s.comments {
		if c.TaskID == task {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Created.Before(out[j].Created) || out[i].Created.Equal(out[j].Created) && out[i].ID < out[j].ID
	})
	return out
}

// ShortOf is a status id's short name.
func (s *Server) ShortOf(statusID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.statuses {
		if st.ID == statusID {
			return st.ShortName
		}
	}
	return ""
}

// Previews returns a task's preview files by revision.
func (s *Server) Previews(task string) []Preview {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Preview
	for _, p := range s.previews {
		if p.TaskID == task {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Revision < out[j].Revision })
	return out
}

// Calls returns the requests served.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Writes returns the requests that changed something.
func (s *Server) Writes() []Call {
	var out []Call
	for _, c := range s.Calls() {
		if c.Method != http.MethodGet && !strings.HasPrefix(c.Path, "/api/auth/") {
			out = append(out, c)
		}
	}
	return out
}

// LoseAnswers makes the next n writes whose path contains path happen,
// then answer 502: the write was made and its answer lost.
func (s *Server) LoseAnswers(n int, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop, s.dropPath = n, path
}

// --- the event log ---

func (s *Server) event(name, user string, data map[string]any) {
	data["project_id"] = s.Project
	s.events = append(s.events, &Event{ID: s.id("event"), Name: name, UserID: user, ProjectID: s.Project, Created: s.now(), Data: data})
}

// --- the API ---

type apiErr struct {
	status int
	body   map[string]any
	msg    string
}

func zouErr(status int, msg string) *apiErr {
	return &apiErr{status: status, msg: msg, body: map[string]any{"error": true, "message": msg}}
}

func notFound() *apiErr {
	return zouErr(404, "404 Not Found: The requested URL was not found on the server. If you entered the URL manually please check your spelling and try again.")
}

func forbidden() *apiErr {
	return zouErr(403, "You don't have the permission to access the requested resource. It is either read-protected or not readable by the server.")
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api")
	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.RequestURI(), Body: string(body)})
	w.Header().Set("Content-Type", "application/json")
	if path == "/auth/login" && r.Method == http.MethodPost {
		s.login(w, body)
		return
	}
	who, e := s.auth(r)
	if e == nil {
		var out any
		var code int
		out, code, e = s.route(r, path, body, who)
		if e == nil {
			if s.drop > 0 && r.Method != http.MethodGet && strings.Contains(path, s.dropPath) {
				s.drop--
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
				return
			}
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(out)
			return
		}
	}
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(e.body)
}

func (s *Server) login(w http.ResponseWriter, body []byte) {
	var in struct{ Email, Password string }
	_ = json.Unmarshal(body, &in)
	for _, p := range s.persons {
		if !p.Bot && p.Email == in.Email && p.Password == in.Password {
			if p.Token == "" {
				p.Token = "person-" + p.ID
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"user": s.personFull(p), "login": true,
				"access_token": p.Token, "refresh_token": "refresh-" + p.Token})
			return
		}
	}
	w.WriteHeader(400)
	_ = json.NewEncoder(w).Encode(map[string]any{"login": false, "message": "Wrong email or password."})
}

func (s *Server) auth(r *http.Request) (*Person, *apiErr) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return nil, &apiErr{401, map[string]any{"msg": "Missing JWT in headers or cookies (Missing Authorization Header; Missing cookie \"access_token_cookie\")"}, "missing"}
	}
	for _, p := range s.persons {
		if p.Token == tok {
			return p, nil
		}
	}
	if strings.HasPrefix(tok, "bot-") || strings.HasPrefix(tok, "person-") {
		return nil, &apiErr{401, map[string]any{"msg": "Token has been revoked"}, "revoked"}
	}
	return nil, &apiErr{422, map[string]any{"msg": "Not enough segments"}, "not a token"}
}

func (s *Server) inTeam(p *Person, project string) bool {
	if p.Role == "admin" {
		return true
	}
	for _, id := range s.team[project] {
		if id == p.ID {
			return true
		}
	}
	return false
}

func manager(p *Person) bool { return p.Role == "manager" || p.Role == "admin" }

func (s *Server) route(r *http.Request, path string, body []byte, who *Person) (any, int, *apiErr) {
	p := strings.Split(strings.Trim(path, "/"), "/")
	q := r.URL.Query()
	get, put, post := r.Method == http.MethodGet, r.Method == http.MethodPut, r.Method == http.MethodPost
	switch {
	case get && path == "/auth/authenticated":
		return map[string]any{"authenticated": true, "user": s.personFull(who), "organisation": map[string]any{"name": "Kitsu"}}, 200, nil
	case get && path == "/data/events/last":
		out, e := s.lastEvents(q, who)
		return out, 200, e
	case len(p) >= 2 && p[0] == "data" && p[1] == "tasks":
		return s.tasksRoute(r, p[2:], q, body, who)
	case put && len(p) == 3 && p[0] == "data" && p[1] == "comments":
		out, e := s.editComment(p[2], body, who)
		return out, 200, e
	case post && len(p) == 4 && p[0] == "actions" && p[1] == "tasks" && p[3] == "comment":
		out, e := s.postComment(p[2], body, who)
		return out, 201, e
	case post && len(p) == 6 && p[0] == "actions" && p[1] == "tasks" && p[3] == "comments" && p[5] == "add-preview":
		out, e := s.addPreview(p[2], p[4], who)
		return out, 201, e
	case post && len(p) == 3 && p[0] == "pictures" && p[1] == "preview-files":
		out, e := s.upload(r, p[2], body, who)
		return out, 201, e
	case get && len(p) >= 2 && p[0] == "data":
		out, e := s.list(p[1:], q, who)
		return out, 200, e
	}
	return nil, 0, notFound()
}

// list serves the generic and named collections and instances.
func (s *Server) list(p []string, q url.Values, who *Person) (any, *apiErr) {
	var rows []map[string]any
	switch p[0] {
	case "projects":
		for id, n := range s.projects {
			if s.inTeam(who, id) {
				rows = append(rows, map[string]any{"id": id, "name": n, "type": "Project"})
			}
		}
	case "task-status":
		for _, st := range s.statuses {
			rows = append(rows, statusJSON(st))
		}
	case "task-types":
		for id, n := range s.types {
			rows = append(rows, map[string]any{"id": id, "name": n, "type": "TaskType", "for_entity": "Shot"})
		}
	case "entity-types":
		for id, n := range s.etypes {
			rows = append(rows, map[string]any{"id": id, "name": n, "type": "EntityType"})
		}
	case "entities":
		for _, e := range s.entities {
			if s.inTeam(who, e.ProjectID) {
				rows = append(rows, s.entityJSON(e))
			}
		}
	case "persons":
		for _, pp := range s.persons {
			switch {
			case who.Role == "admin" || len(p) == 2 && manager(who):
				rows = append(rows, s.personFull(pp))
			default:
				rows = append(rows, s.personMinimal(pp))
			}
		}
		// Anyone may filter persons by email, though in a list only
		// admins see it (one person read by a manager shows it).
		if v := q.Get("email"); v != "" && who.Role != "admin" {
			var keep []map[string]any
			for _, row := range rows {
				if s.persons[row["id"].(string)].Email == v {
					keep = append(keep, row)
				}
			}
			rows = keep
			q.Del("email")
		}
	default:
		return nil, notFound()
	}
	if len(p) == 2 {
		for _, row := range rows {
			if row["id"] == p[1] {
				return row, nil
			}
		}
		return nil, notFound()
	}
	rows, e := filter(rows, q)
	if e != nil {
		return nil, e
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	return rows, nil
}

// filter applies Zou's column filters: each key that is a column; a JSON
// array is IN; name is case-insensitive; anything else is ignored.
func filter(rows []map[string]any, q url.Values) ([]map[string]any, *apiErr) {
	var out []map[string]any
	for _, row := range rows {
		keep := true
		for k := range q {
			if k == "page" || k == "relations" || k == "fields" {
				continue
			}
			got, isCol := row[k]
			if !isCol {
				continue
			}
			v := q.Get(k)
			want := []string{v}
			if strings.HasPrefix(v, "[") {
				if err := json.Unmarshal([]byte(v), &want); err != nil {
					return nil, zouErr(400, "Wrong filter format.")
				}
			}
			hit := false
			for _, w := range want {
				switch g := got.(type) {
				case []string:
					for _, x := range g {
						hit = hit || x == w
					}
				default:
					if k == "name" {
						hit = hit || strings.EqualFold(fmt.Sprint(g), w)
					} else {
						hit = hit || fmt.Sprint(g) == w
					}
				}
			}
			keep = keep && hit
		}
		if keep {
			out = append(out, row)
		}
	}
	return out, nil
}

func statusJSON(st *Status) map[string]any {
	return map[string]any{"id": st.ID, "name": st.Name, "short_name": st.ShortName, "color": "#3273dc", "priority": 1,
		"is_done": st.IsDone, "is_retake": st.IsRetake, "is_feedback_request": st.IsFeedbackRequest, "is_wip": st.IsWIP,
		"is_default": st.IsDefault, "is_artist_allowed": true, "is_client_allowed": true, "for_concept": st.ForConcept,
		"archived": false, "type": "TaskStatus"}
}

func (s *Server) entityJSON(e *Entity) map[string]any {
	var parent any
	if e.ParentID != "" {
		parent = e.ParentID
	}
	return map[string]any{"id": e.ID, "name": e.Name, "parent_id": parent, "entity_type_id": e.TypeID,
		"project_id": e.ProjectID, "status": "running", "data": map[string]any{}, "canceled": false, "type": "Entity"}
}

func (s *Server) personFull(p *Person) map[string]any {
	return map[string]any{"id": p.ID, "first_name": p.FirstName, "last_name": p.LastName, "full_name": p.FullName(),
		"email": p.Email, "role": p.Role, "is_bot": p.Bot, "active": true, "desktop_login": "", "type": "Person"}
}

func (s *Server) personMinimal(p *Person) map[string]any {
	return map[string]any{"id": p.ID, "first_name": p.FirstName, "last_name": p.LastName, "full_name": p.FullName(),
		"role": p.Role, "is_bot": p.Bot, "active": true, "has_avatar": false, "desktop_login": "", "type": "Person"}
}

func (s *Server) taskJSON(t *Task, relations bool) map[string]any {
	var due any
	if t.Due != "" {
		due = t.Due
	}
	var data any
	if t.Data != nil {
		data = t.Data
	}
	m := map[string]any{"id": t.ID, "project_id": t.ProjectID, "entity_id": t.EntityID, "task_type_id": t.TypeID,
		"task_status_id": t.StatusID, "due_date": due, "estimation": t.Estimation, "priority": t.Priority,
		"duration": 0.0, "retake_count": t.RetakeCount, "data": data, "name": "main",
		"updated_at": t.Updated.UTC().Format(layout), "type": "Task"}
	if relations {
		a := append([]string{}, t.Assignees...)
		m["assignees"] = a
	}
	return m
}

func (s *Server) tasksRoute(r *http.Request, p []string, q url.Values, body []byte, who *Person) (any, int, *apiErr) {
	if len(p) == 0 {
		if r.Method != http.MethodGet {
			return nil, 0, notFound()
		}
		var rows []map[string]any
		ids := make([]string, 0, len(s.tasks))
		for id := range s.tasks {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			t := s.tasks[id]
			if s.inTeam(who, t.ProjectID) {
				rows = append(rows, s.taskJSON(t, q.Get("relations") == "true"))
			}
		}
		// assignees is a relation: filtered as "any", whatever relations says.
		if a := q.Get("assignees"); a != "" {
			var keep []map[string]any
			for _, row := range rows {
				for _, x := range s.tasks[row["id"].(string)].Assignees {
					if x == a {
						keep = append(keep, row)
						break
					}
				}
			}
			rows = keep
			q.Del("assignees")
		}
		rows, e := filter(rows, q)
		if rows == nil {
			rows = []map[string]any{}
		}
		return rows, 200, e
	}
	t := s.tasks[p[0]]
	if t == nil {
		return nil, 0, notFound()
	}
	if !s.inTeam(who, t.ProjectID) {
		return nil, 0, forbidden()
	}
	switch {
	case len(p) == 1 && r.Method == http.MethodGet:
		return s.taskJSON(t, true), 200, nil
	case len(p) == 1 && r.Method == http.MethodPut:
		out, e := s.putTask(t, body, who)
		return out, 200, e
	case len(p) == 2 && p[1] == "full" && r.Method == http.MethodGet:
		return s.fullTask(t), 200, nil
	case len(p) == 2 && p[1] == "comments" && r.Method == http.MethodGet:
		return s.taskComments(t), 200, nil
	}
	return nil, 0, notFound()
}

func (s *Server) fullTask(t *Task) map[string]any {
	m := s.taskJSON(t, true)
	e := s.entities[t.EntityID]
	m["entity"] = s.entityJSON(e)
	m["entity_type"] = map[string]any{"id": e.TypeID, "name": s.etypes[e.TypeID]}
	m["task_type"] = map[string]any{"id": t.TypeID, "name": s.types[t.TypeID]}
	for _, st := range s.statuses {
		if st.ID == t.StatusID {
			m["task_status"] = statusJSON(st)
		}
	}
	m["project"] = map[string]any{"id": t.ProjectID, "name": s.projects[t.ProjectID]}
	persons := []map[string]any{}
	for _, id := range t.Assignees {
		persons = append(persons, s.personFull(s.persons[id]))
	}
	m["persons"] = persons
	if par := s.entities[e.ParentID]; par != nil && s.etypes[par.TypeID] == "Sequence" {
		m["sequence"] = s.entityJSON(par)
		if ep := s.entities[par.ParentID]; ep != nil {
			m["episode"] = s.entityJSON(ep)
		}
	}
	return m
}

func (s *Server) putTask(t *Task, body []byte, who *Person) (any, *apiErr) {
	if !manager(who) && who.Role != "supervisor" {
		return nil, forbidden()
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, zouErr(400, "Wrong parameters.")
	}
	for k, v := range in {
		switch k {
		case "data":
			var d map[string]any
			_ = json.Unmarshal(v, &d)
			if len(d) == 0 {
				t.Data = d // Zou: an empty object replaces the whole bag
				continue
			}
			if t.Data == nil {
				t.Data = map[string]any{}
			}
			for dk, dv := range d {
				t.Data[dk] = dv
			}
		case "due_date":
			var d string
			_ = json.Unmarshal(v, &d)
			x, err := time.Parse(layout, strings.TrimSuffix(d, "Z"))
			if err != nil {
				if x, err = time.Parse("2006-01-02", d); err != nil {
					return nil, zouErr(400, "Wrong date format for due_date")
				}
			}
			t.Due = x.Format(layout)
		case "estimation":
			_ = json.Unmarshal(v, &t.Estimation)
		case "task_status_id":
			// Zou writes it as a column: no comment, no status event.
			_ = json.Unmarshal(v, &t.StatusID)
		}
	}
	t.Updated = s.now()
	s.event("task:update", who.ID, map[string]any{"task_id": t.ID})
	return s.taskJSON(t, false), nil
}

func (s *Server) commentJSON(c *Comment) map[string]any {
	au := s.persons[c.PersonID]
	var st *Status
	for _, x := range s.statuses {
		if x.ID == c.StatusID {
			st = x
		}
	}
	previews := []map[string]any{}
	for _, id := range c.Previews {
		if p := s.previews[id]; p != nil {
			var on any
			if p.OriginalName != "" {
				on = p.OriginalName
			}
			previews = append(previews, map[string]any{"id": p.ID, "task_id": p.TaskID, "revision": p.Revision,
				"extension": p.Extension, "status": p.Status, "original_name": on, "position": 1,
				"validation_status": "neutral", "annotations": []any{}})
		}
	}
	replies := []map[string]any{}
	for _, r := range c.Replies {
		replies = append(replies, map[string]any{"id": r.ID, "text": r.Text, "person_id": r.PersonID,
			"created_at": r.Created.UTC().Format(layout), "mentions": []any{}, "department_mentions": []any{}})
	}
	return map[string]any{"id": c.ID, "object_id": c.TaskID, "object_type": "Task", "text": c.Text,
		"created_at": c.Created.UTC().Format(layout), "updated_at": c.Created.UTC().Format(layout),
		"task_status_id": c.StatusID, "person_id": c.PersonID, "preview_file_id": nil, "data": nil, "checklist": []any{},
		"pinned": nil, "for_client": false, "links": []any{}, "mentions": append([]string{}, c.Mentions...),
		"department_mentions": []any{}, "acknowledgements": []any{}, "attachment_files": []any{},
		"replies": replies, "previews": previews,
		"person": map[string]any{"id": au.ID, "first_name": au.FirstName, "last_name": au.LastName,
			"full_name": au.FullName(), "has_avatar": false, "role": au.Role},
		"task_status": map[string]any{"id": st.ID, "name": st.Name, "short_name": st.ShortName, "color": "#3273dc"},
		"type":        "Comment"}
}

func (s *Server) taskComments(t *Task) []map[string]any {
	var cs []*Comment
	for _, c := range s.comments {
		if c.TaskID == t.ID {
			cs = append(cs, c)
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if !cs[i].Created.Equal(cs[j].Created) {
			return cs[i].Created.After(cs[j].Created)
		}
		return cs[i].ID > cs[j].ID
	})
	out := []map[string]any{}
	for _, c := range cs {
		out = append(out, s.commentJSON(c))
	}
	return out
}

var hashtag = regexp.MustCompile(`#([a-zA-Z]+)`)

// comment is the comment action: permissions, mentions, the status move
// and its events, and hashtags.
func (s *Server) comment(who *Person, taskID, statusID, text string) (*Comment, *apiErr) {
	t := s.tasks[taskID]
	if t == nil {
		return nil, notFound()
	}
	var st *Status
	for _, x := range s.statuses {
		if x.ID == statusID {
			st = x
		}
	}
	if st == nil {
		return nil, notFound()
	}
	if !s.inTeam(who, t.ProjectID) {
		return nil, forbidden()
	}
	assigned := false
	for _, a := range t.Assignees {
		assigned = assigned || a == who.ID
	}
	if !manager(who) && who.Role != "supervisor" && !assigned {
		return nil, forbidden()
	}
	c := &Comment{ID: s.id("comment"), TaskID: taskID, StatusID: statusID, PersonID: who.ID, Text: text, Created: s.now()}
	c.Mentions = s.mentions(t.ProjectID, text)
	s.comments[c.ID] = c
	prev := t.StatusID
	t.StatusID, t.Updated = statusID, s.now()
	if st.IsRetake && prev != statusID {
		t.RetakeCount++
	}
	s.event("comment:new", who.ID, map[string]any{"comment_id": c.ID, "task_id": taskID, "task_status_id": statusID, "person_id": who.ID})
	s.event("task:update", who.ID, map[string]any{"task_id": taskID})
	if prev != statusID {
		s.event("task:status-changed", who.ID, map[string]any{"task_id": taskID, "new_task_status_id": statusID,
			"previous_task_status_id": prev, "person_id": who.ID})
		if st.IsFeedbackRequest {
			s.event("task:to-review", who.ID, map[string]any{"task_id": taskID, "comment_id": c.ID, "person_id": who.ID})
		}
	}
	// Hashtags post the comment again on the entity's other tasks.
	for _, m := range hashtag.FindAllStringSubmatch(text, -1) {
		tag := strings.ToLower(m[1])
		for _, o := range s.tasks {
			if o.ID == t.ID || o.EntityID != t.EntityID || (tag != "all" && strings.ToLower(s.types[o.TypeID]) != tag) {
				continue
			}
			cp := &Comment{ID: s.id("comment"), TaskID: o.ID, StatusID: o.StatusID, PersonID: who.ID,
				Text: text + "\n\n____\nFrom " + s.types[t.TypeID] + " task", Created: s.now()}
			s.comments[cp.ID] = cp
		}
	}
	return c, nil
}

// mentions are the team members whose full name follows an @, then a
// space or the end of the text.
func (s *Server) mentions(project, text string) []string {
	var out []string
	for _, id := range s.team[project] {
		p := s.persons[id]
		if regexp.MustCompile("@" + regexp.QuoteMeta(p.FullName()) + "( |$)").MatchString(text) {
			out = append(out, p.ID)
		}
	}
	return out
}

func (s *Server) postComment(taskID string, body []byte, who *Person) (any, *apiErr) {
	var in struct {
		TaskStatusID string `json:"task_status_id"`
		Comment      string `json:"comment"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.TaskStatusID == "" {
		return nil, &apiErr{400, map[string]any{"error": true, "message": "Validation error.",
			"data": map[string]any{"errors": []map[string]any{{"field": "task_status_id", "message": "Field required"}}}}, "validation"}
	}
	c, e := s.comment(who, taskID, in.TaskStatusID, in.Comment)
	if e != nil {
		return nil, e
	}
	out := s.commentJSON(c)
	out["person"] = s.personFull(who)
	return out, nil
}

func (s *Server) editComment(id string, body []byte, who *Person) (any, *apiErr) {
	c := s.comments[id]
	if c == nil {
		return nil, notFound()
	}
	if !manager(who) && who.ID != c.PersonID {
		return nil, forbidden()
	}
	var in struct {
		Text *string `json:"text"`
	}
	_ = json.Unmarshal(body, &in)
	if in.Text != nil {
		c.Text = *in.Text
		c.Mentions = s.mentions(s.tasks[c.TaskID].ProjectID, c.Text)
	}
	s.event("comment:update", who.ID, map[string]any{"comment_id": c.ID, "task_id": c.TaskID, "person_id": who.ID})
	return s.commentJSON(c), nil
}

func (s *Server) addPreview(taskID, commentID string, who *Person) (any, *apiErr) {
	t, c := s.tasks[taskID], s.comments[commentID]
	if t == nil || c == nil || c.TaskID != taskID {
		return nil, notFound()
	}
	if !manager(who) && who.Role != "supervisor" {
		return nil, forbidden()
	}
	rev := 0
	for _, p := range s.previews {
		if p.TaskID == taskID && p.Revision > rev {
			rev = p.Revision
		}
	}
	p := &Preview{ID: s.id("preview"), TaskID: taskID, CommentID: commentID, PersonID: who.ID, Status: "processing",
		Extension: "mp4", Revision: rev + 1}
	s.previews[p.ID] = p
	c.Previews = append(c.Previews, p.ID)
	s.event("preview-file:new", who.ID, map[string]any{"preview_file_id": p.ID, "comment_id": commentID})
	s.event("comment:update", who.ID, map[string]any{"comment_id": commentID, "task_id": taskID})
	return map[string]any{"id": p.ID, "revision": p.Revision, "position": 1, "extension": p.Extension, "status": p.Status,
		"original_name": nil, "task_id": taskID, "person_id": who.ID, "file_size": 0, "validation_status": "neutral",
		"type": "PreviewFile"}, nil
}

var pictures = map[string]bool{"jpe": true, "jpeg": true, "jpg": true, "png": true}
var movies = map[string]bool{"avi": true, "m4v": true, "mkv": true, "mov": true, "mp4": true, "webm": true, "wmv": true}
var others = map[string]bool{"exr": true, "gif": true, "pdf": true, "psd": true, "svg": true, "zip": true, "blend": true}

func (s *Server) upload(r *http.Request, id string, body []byte, who *Person) (any, *apiErr) {
	p := s.previews[id]
	if p == nil {
		return nil, notFound()
	}
	if p.OriginalName != "" {
		return nil, zouErr(400, "This preview file has already been uploaded.")
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		return nil, zouErr(400, "File not provided.")
	}
	mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	var name string
	var size int
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "file" {
			name = part.FileName()
			b, _ := io.ReadAll(part)
			size = len(b)
		}
	}
	if name == "" {
		return nil, zouErr(400, "File not provided.")
	}
	ext := strings.ToLower(name[strings.LastIndex(name, ".")+1:])
	switch {
	case pictures[ext]:
		p.Extension, p.Status = "png", "ready"
	case movies[ext]:
		p.Extension, p.Status = "mp4", "processing"
	case others[ext]:
		p.Extension, p.Status = ext, "ready"
	default:
		delete(s.previews, id)
		c := s.comments[p.CommentID]
		for i, x := range c.Previews {
			if x == id {
				c.Previews = append(c.Previews[:i], c.Previews[i+1:]...)
				break
			}
		}
		return nil, zouErr(400, "Wrong file format, extension: "+ext)
	}
	p.OriginalName, p.Size = strings.TrimSuffix(name, "."+ext), size
	s.event("preview-file:add-file", who.ID, map[string]any{"comment_id": p.CommentID, "task_id": p.TaskID,
		"preview_file_id": p.ID, "revision": p.Revision, "extension": p.Extension, "status": p.Status})
	return map[string]any{"id": p.ID, "revision": p.Revision, "extension": p.Extension, "status": p.Status,
		"original_name": p.OriginalName, "task_id": p.TaskID, "file_size": size, "type": "PreviewFile"}, nil
}

func parseTime(v string) (time.Time, bool) {
	if t, err := time.Parse(layout, v); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// lastEvents is data/events/last.
func (s *Server) lastEvents(q url.Values, who *Person) (any, *apiErr) {
	if !manager(who) {
		return nil, forbidden()
	}
	var after, before time.Time
	if v := q.Get("after"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return nil, zouErr(400, "Wrong date format for after: "+v)
		}
		after = t
	}
	if v := q.Get("before"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return nil, zouErr(400, "Wrong date format for before: "+v)
		}
		before = t
	}
	var cursor *Event
	if v := q.Get("cursor_event_id"); v != "" {
		for _, e := range s.events {
			if e.ID == v {
				cursor = e
			}
		}
		if cursor == nil {
			return nil, zouErr(400, "No event found for cursor_event_id")
		}
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	limit = min(limit, 1000)
	var hits []*Event
	for _, e := range s.events {
		if !after.IsZero() && !e.Created.After(after) || !before.IsZero() && !e.Created.Before(before) ||
			cursor != nil && !e.Created.Before(cursor.Created) {
			continue
		}
		if v := q.Get("project_id"); v != "" && e.ProjectID != v {
			continue
		}
		if v := q.Get("name"); v != "" && e.Name != v {
			continue
		}
		if ps := q["name_prefixes"]; len(ps) > 0 {
			ok := false
			for _, p := range ps {
				ok = ok || strings.HasPrefix(e.Name, p+":")
			}
			if !ok {
				continue
			}
		}
		if !s.inTeam(who, e.ProjectID) {
			continue
		}
		hits = append(hits, e)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Created.After(hits[j].Created) })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := []map[string]any{}
	for _, e := range hits {
		out = append(out, map[string]any{"id": e.ID, "created_at": e.Created.UTC().Format(layout), "name": e.Name,
			"user_id": e.UserID, "project_id": e.ProjectID, "data": e.Data})
	}
	return out, nil
}
