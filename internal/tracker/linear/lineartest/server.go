// Package lineartest is a fake Linear workspace for testing c4-sync
// offline. It serves the GraphQL operations the adapter sends, by
// operationName, in the shapes of Linear's public schema (the @linear/sdk
// types): teams with workflow states, issues and their history, comments,
// attachments, users and the viewer. It is not a GraphQL engine: it reads
// each operation's variables, and it applies IssueFilter as Linear does
// for the filters the adapter builds (and, team.key, labels.some.name,
// state.type and state.name, project.name, assignee.email, updatedAt.gt),
// so a filter the adapter gets wrong finds the wrong issues here too.
//
// As Linear does, it keys an attachment by its URL on the issue (making
// one again updates it), writes an issue history entry for each state
// change, answers a rate limit with HTTP 400 and RATELIMITED, and signs
// webhook deliveries with Linear-Signature: hex HMAC-SHA256 of the body.
package lineartest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// State is a workflow state.
type State struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Position float64 `json:"position"`
}

// User is a workspace member, or an app user.
type User struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

// Issue is one issue.
type Issue struct {
	ID, Identifier, Title, Description string
	DueDate                            string // YYYY-MM-DD, or ""
	Estimate                           float64
	StateID, AssigneeID, Project       string
	Labels                             []string
	UpdatedAt                          time.Time
}

// Comment is a comment on an issue.
type Comment struct {
	ID, IssueID, Body, UserID string
	CreatedAt                 time.Time
}

// Attachment is an attachment on an issue, keyed by URL.
type Attachment struct {
	ID, IssueID, URL, Title, Subtitle string
	Metadata                          map[string]any
	Updates                           int // times made again at the same URL
}

// History is one issue history entry: a state change.
type History struct {
	ID, IssueID, ActorID, From, To string
	CreatedAt                      time.Time
}

// Call is one operation served.
type Call struct {
	Op        string
	Variables map[string]any
}

// Server is the fake workspace.
type Server struct {
	*httptest.Server
	// Key is the personal API key it accepts (sent bare, or as a bearer
	// token, as an OAuth token is).
	Key string
	// Viewer is the user the key acts as: c4-sync's own.
	Viewer User
	// TeamKey is the one team's key.
	TeamKey string

	mu          sync.Mutex
	now         func() time.Time
	n           int
	teamID      string
	states      []State
	users       map[string]*User
	issues      map[string]*Issue
	comments    []*Comment
	attachments []*Attachment
	history     []*History
	calls       []Call
	limited     int
	lose        map[string]int
}

// New starts a workspace with one team (key) and Linear's default
// workflow: Backlog, Todo, In Progress, In Review, Done, Canceled.
func New(key string) *Server {
	s := &Server{Key: "lin_api_fake", TeamKey: key, now: time.Now, users: map[string]*User{}, issues: map[string]*Issue{},
		lose: map[string]int{}}
	s.teamID = s.id("team")
	for i, st := range [][2]string{{"Backlog", "backlog"}, {"Todo", "unstarted"}, {"In Progress", "started"},
		{"In Review", "started"}, {"Done", "completed"}, {"Canceled", "canceled"}} {
		s.states = append(s.states, State{ID: s.id("state"), Name: st[0], Type: st[1], Position: float64(i)})
	}
	s.Viewer = *s.AddUser("C4", "c4-sync@apps.linear.app")
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func (s *Server) id(kind string) string {
	s.n++
	return fmt.Sprintf("%s-%04d", kind, s.n)
}

// SetClock sets the workspace's clock.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// AddState adds a workflow state.
func (s *Server) AddState(name, typ string, position float64) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{ID: s.id("state"), Name: name, Type: typ, Position: position}
	s.states = append(s.states, st)
	return st
}

// AddUser adds a member.
func (s *Server) AddUser(name, email string) *User {
	if s.users == nil {
		s.users = map[string]*User{}
	}
	u := &User{ID: s.id("user"), Name: name, Email: email, DisplayName: strings.ToLower(strings.Fields(name)[0])}
	s.users[u.ID] = u
	return u
}

// AddIssue adds an issue in Todo.
func (s *Server) AddIssue(title, due string, assignee *User, labels ...string) *Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.issues) + 1
	i := &Issue{ID: s.id("issue"), Identifier: fmt.Sprintf("%s-%d", s.TeamKey, n), Title: title, DueDate: due,
		StateID: s.stateNamed("Todo").ID, Labels: labels, UpdatedAt: s.now()}
	if assignee != nil {
		i.AssigneeID = assignee.ID
	}
	s.issues[i.ID] = i
	return i
}

// Update changes an issue as a person would, and bumps updatedAt.
func (s *Server) Update(id string, fn func(*Issue)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.issues[id])
	s.issues[id].UpdatedAt = s.now()
}

// SetState moves an issue to the state named, as by, with its history.
func (s *Server) SetState(id, state string, by *User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.move(s.issues[id], s.stateNamed(state).ID, by.ID)
}

func (s *Server) move(i *Issue, stateID, actor string) {
	if i.StateID == stateID {
		return
	}
	s.history = append(s.history, &History{ID: s.id("history"), IssueID: i.ID, ActorID: actor, From: i.StateID, To: stateID, CreatedAt: s.now()})
	i.StateID = stateID
	i.UpdatedAt = s.now()
}

// AddComment writes a comment as by.
func (s *Server) AddComment(issue, body string, by *User) *Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &Comment{ID: s.id("comment"), IssueID: issue, Body: body, UserID: by.ID, CreatedAt: s.now()}
	s.comments = append(s.comments, c)
	s.issues[issue].UpdatedAt = s.now()
	return c
}

// Issue is a copy of an issue, and its state's name.
func (s *Server) Issue(id string) (Issue, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := *s.issues[id]
	return i, s.stateByID(i.StateID).Name
}

// Comments are an issue's comments, oldest first.
func (s *Server) Comments(issue string) []Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Comment
	for _, c := range s.comments {
		if c.IssueID == issue {
			out = append(out, *c)
		}
	}
	return out
}

// Attachments are an issue's attachments, oldest first.
func (s *Server) Attachments(issue string) []Attachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Attachment
	for _, a := range s.attachments {
		if a.IssueID == issue {
			out = append(out, *a)
		}
	}
	return out
}

// Calls are the operations served, oldest first.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Mutations counts the writes served.
func (s *Server) Mutations() int {
	n := 0
	for _, c := range s.Calls() {
		switch c.Op {
		case "IssueUpdate", "CommentCreate", "CommentUpdate", "AttachmentCreate":
			n++
		}
	}
	return n
}

// RateLimit answers the next n operations with Linear's rate-limit error.
func (s *Server) RateLimit(n int) {
	s.mu.Lock()
	s.limited = n
	s.mu.Unlock()
}

// LoseAnswer makes the next n operations named op happen, and then
// answers 502: the write is made and the caller cannot know.
func (s *Server) LoseAnswer(op string, n int) {
	s.mu.Lock()
	s.lose[op] = n
	s.mu.Unlock()
}

func (s *Server) stateNamed(name string) State {
	for _, st := range s.states {
		if st.Name == name {
			return st
		}
	}
	panic("lineartest: no state " + name)
}

func (s *Server) stateByID(id string) State {
	for _, st := range s.states {
		if st.ID == id {
			return st
		}
	}
	return State{}
}

// --- GraphQL ---

type gqlErr struct {
	Message    string         `json:"message"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
		http.NotFound(w, r)
		return
	}
	if a := r.Header.Get("Authorization"); a != s.Key && a != "Bearer "+s.Key {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"errors": []gqlErr{{Message: "Authentication required, not authenticated",
			Extensions: map[string]any{"code": "AUTHENTICATION_ERROR"}}}})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		Op        string         `json:"operationName"`
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Op == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"errors": []gqlErr{{Message: "a named operation is required"}}})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Op: req.Op, Variables: req.Variables})
	if s.limited > 0 {
		s.limited--
		w.Header().Set("X-RateLimit-Requests-Reset", strconv.FormatInt(s.now().Add(30*time.Second).UnixMilli(), 10))
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"errors": []gqlErr{{Message: "Rate limit exceeded",
			Extensions: map[string]any{"code": "RATELIMITED"}}}})
		return
	}
	data, errs := s.op(req.Op, req.Query, req.Variables)
	if n := s.lose[req.Op]; n > 0 {
		s.lose[req.Op] = n - 1
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	out := map[string]any{"data": data}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	json.NewEncoder(w).Encode(out)
}

func str(v map[string]any, k string) string { s, _ := v[k].(string); return s }

func (s *Server) op(op, query string, v map[string]any) (any, []gqlErr) {
	want := map[string]string{"Team": "teams(", "Viewer": "viewer", "Issue": "issue(", "Issues": "issues(", "History": "history(",
		"Comments": "comments(", "User": "user(", "IssueUpdate": "issueUpdate(", "CommentCreate": "commentCreate(",
		"CommentUpdate": "commentUpdate(", "AttachmentCreate": "attachmentCreate("}[op]
	if want == "" || !strings.Contains(query, want) {
		return nil, []gqlErr{{Message: fmt.Sprintf("unknown operation %s", op), Extensions: map[string]any{"code": "GRAPHQL_VALIDATION_FAILED"}}}
	}
	notFound := []gqlErr{{Message: "Entity not found", Extensions: map[string]any{"code": "ENTITY_NOT_FOUND"}}}
	switch op {
	case "Team":
		var nodes []any
		if f, _ := v["key"].(string); f == s.TeamKey {
			st := append([]State(nil), s.states...)
			nodes = append(nodes, map[string]any{"id": s.teamID, "key": s.TeamKey, "states": map[string]any{"nodes": st}})
		}
		return map[string]any{"teams": map[string]any{"nodes": nodes}}, nil
	case "Viewer":
		return map[string]any{"viewer": map[string]any{"id": s.Viewer.ID}}, nil
	case "User":
		u := s.users[str(v, "id")]
		if u == nil {
			return map[string]any{"user": nil}, notFound
		}
		return map[string]any{"user": u}, nil
	case "Issue":
		i := s.lookup(str(v, "id"))
		if i == nil {
			return map[string]any{"issue": nil}, notFound
		}
		return map[string]any{"issue": s.issueJSON(i)}, nil
	case "Issues":
		var match []*Issue
		for _, i := range s.issues {
			if s.matches(i, v["filter"]) {
				match = append(match, i)
			}
		}
		sort.Slice(match, func(a, b int) bool {
			if !match[a].UpdatedAt.Equal(match[b].UpdatedAt) {
				return match[a].UpdatedAt.Before(match[b].UpdatedAt)
			}
			return match[a].ID < match[b].ID
		})
		page, next := paginate(len(match), str(v, "after"), 50)
		var nodes []any
		for _, i := range match[page[0]:page[1]] {
			nodes = append(nodes, s.issueJSON(i))
		}
		return map[string]any{"issues": map[string]any{"nodes": nodes, "pageInfo": next}}, nil
	case "History":
		i := s.lookup(str(v, "id"))
		if i == nil {
			return map[string]any{"issue": nil}, notFound
		}
		var hs []*History
		for _, h := range s.history {
			if h.IssueID == i.ID {
				hs = append(hs, h)
			}
		}
		page, next := paginate(len(hs), str(v, "after"), 50)
		var nodes []any
		for _, h := range hs[page[0]:page[1]] {
			from, to := s.stateByID(h.From), s.stateByID(h.To)
			var actor any
			if u := s.users[h.ActorID]; u != nil {
				actor = u
			}
			nodes = append(nodes, map[string]any{"id": h.ID, "createdAt": h.CreatedAt.UTC().Format(time.RFC3339Nano),
				"actor": actor, "fromState": from, "toState": to})
		}
		return map[string]any{"issue": map[string]any{"url": s.url(i), "history": map[string]any{"nodes": nodes, "pageInfo": next}}}, nil
	case "Comments":
		i := s.lookup(str(v, "id"))
		if i == nil {
			return map[string]any{"issue": nil}, notFound
		}
		var cs []*Comment
		for _, c := range s.comments {
			if c.IssueID == i.ID {
				cs = append(cs, c)
			}
		}
		page, next := paginate(len(cs), str(v, "after"), 100)
		var nodes []any
		for _, c := range cs[page[0]:page[1]] {
			nodes = append(nodes, map[string]any{"id": c.ID, "body": c.Body, "createdAt": c.CreatedAt.UTC().Format(time.RFC3339Nano),
				"user": s.users[c.UserID]})
		}
		return map[string]any{"issue": map[string]any{"comments": map[string]any{"nodes": nodes, "pageInfo": next}}}, nil
	case "IssueUpdate":
		i := s.lookup(str(v, "id"))
		in, _ := v["input"].(map[string]any)
		if i == nil {
			return map[string]any{"issueUpdate": nil}, notFound
		}
		if sid := str(in, "stateId"); sid != "" {
			if s.stateByID(sid).ID == "" {
				return map[string]any{"issueUpdate": nil}, []gqlErr{{Message: "Argument Validation Error: stateId", Extensions: map[string]any{"code": "INVALID_INPUT"}}}
			}
			s.move(i, sid, s.Viewer.ID)
		}
		return map[string]any{"issueUpdate": map[string]any{"success": true, "issue": map[string]any{"id": i.ID}}}, nil
	case "CommentCreate":
		in, _ := v["input"].(map[string]any)
		i := s.lookup(str(in, "issueId"))
		if i == nil || str(in, "body") == "" {
			return map[string]any{"commentCreate": nil}, notFound
		}
		c := &Comment{ID: s.id("comment"), IssueID: i.ID, Body: str(in, "body"), UserID: s.Viewer.ID, CreatedAt: s.now()}
		s.comments = append(s.comments, c)
		return map[string]any{"commentCreate": map[string]any{"success": true, "comment": map[string]any{"id": c.ID, "url": s.url(i) + "#comment-" + c.ID}}}, nil
	case "CommentUpdate":
		in, _ := v["input"].(map[string]any)
		for _, c := range s.comments {
			if c.ID == str(v, "id") {
				c.Body = str(in, "body")
				return map[string]any{"commentUpdate": map[string]any{"success": true, "comment": map[string]any{"id": c.ID, "url": ""}}}, nil
			}
		}
		return map[string]any{"commentUpdate": nil}, notFound
	case "AttachmentCreate":
		in, _ := v["input"].(map[string]any)
		i := s.lookup(str(in, "issueId"))
		if i == nil || str(in, "url") == "" || str(in, "title") == "" {
			return map[string]any{"attachmentCreate": nil}, []gqlErr{{Message: "Argument Validation Error", Extensions: map[string]any{"code": "INVALID_INPUT"}}}
		}
		md, _ := in["metadata"].(map[string]any)
		for _, a := range s.attachments {
			if a.IssueID == i.ID && a.URL == str(in, "url") {
				a.Title, a.Subtitle, a.Metadata = str(in, "title"), str(in, "subtitle"), md
				a.Updates++
				return map[string]any{"attachmentCreate": map[string]any{"success": true, "attachment": map[string]any{"id": a.ID, "url": a.URL}}}, nil
			}
		}
		a := &Attachment{ID: s.id("attachment"), IssueID: i.ID, URL: str(in, "url"), Title: str(in, "title"), Subtitle: str(in, "subtitle"), Metadata: md}
		s.attachments = append(s.attachments, a)
		return map[string]any{"attachmentCreate": map[string]any{"success": true, "attachment": map[string]any{"id": a.ID, "url": a.URL}}}, nil
	}
	return nil, nil
}

// lookup finds an issue by id or identifier, as issue(id:) does.
func (s *Server) lookup(id string) *Issue {
	if i := s.issues[id]; i != nil {
		return i
	}
	for _, i := range s.issues {
		if i.Identifier == id {
			return i
		}
	}
	return nil
}

func (s *Server) url(i *Issue) string {
	return s.URL + "/acme/issue/" + i.Identifier
}

func (s *Server) issueJSON(i *Issue) map[string]any {
	var labels []any
	for _, l := range i.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	m := map[string]any{"id": i.ID, "identifier": i.Identifier, "title": i.Title, "description": i.Description, "url": s.url(i),
		"dueDate": nil, "updatedAt": i.UpdatedAt.UTC().Format(time.RFC3339Nano), "estimate": nil,
		"state": s.stateByID(i.StateID), "assignee": nil, "labels": map[string]any{"nodes": labels},
		"team": map[string]any{"key": s.TeamKey}, "project": nil}
	if i.DueDate != "" {
		m["dueDate"] = i.DueDate
	}
	if i.Estimate > 0 {
		m["estimate"] = i.Estimate
	}
	if u := s.users[i.AssigneeID]; u != nil {
		m["assignee"] = u
	}
	if i.Project != "" {
		m["project"] = map[string]any{"name": i.Project}
	}
	return m
}

func paginate(n int, after string, size int) ([2]int, map[string]any) {
	start := 0
	if after != "" {
		start, _ = strconv.Atoi(after)
	}
	start = min(start, n)
	end := min(start+size, n)
	return [2]int{start, end}, map[string]any{"hasNextPage": end < n, "endCursor": strconv.Itoa(end)}
}

// matches applies an IssueFilter: and, team.key.eq,
// labels.some.name.eq, state.type.{eq,in,nin}, state.name.eq,
// project.name.eq, assignee.email.eq, updatedAt.gt. A filter it does not
// know matches nothing, so an adapter's mistake shows.
func (s *Server) matches(i *Issue, f any) bool {
	m, ok := f.(map[string]any)
	if !ok {
		return f == nil
	}
	for k, cond := range m {
		c, _ := cond.(map[string]any)
		switch k {
		case "and":
			list, _ := cond.([]any)
			for _, x := range list {
				if !s.matches(i, x) {
					return false
				}
			}
		case "team":
			if !cmp(s.TeamKey, sub(c, "key")) {
				return false
			}
		case "labels":
			name := sub(sub(c, "some"), "name")
			found := false
			for _, l := range i.Labels {
				found = found || cmp(l, name)
			}
			if !found {
				return false
			}
		case "state":
			st := s.stateByID(i.StateID)
			if t := sub(c, "type"); t != nil && !cmp(st.Type, t) {
				return false
			}
			if n := sub(c, "name"); n != nil && !cmp(st.Name, n) {
				return false
			}
		case "project":
			if !cmp(i.Project, sub(c, "name")) {
				return false
			}
		case "assignee":
			email := ""
			if u := s.users[i.AssigneeID]; u != nil {
				email = u.Email
			}
			if !cmp(email, sub(c, "email")) {
				return false
			}
		case "updatedAt":
			gt, err := time.Parse(time.RFC3339Nano, str(c, "gt"))
			if err != nil || !i.UpdatedAt.After(gt) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func sub(m map[string]any, k string) map[string]any {
	if m == nil {
		return nil
	}
	x, _ := m[k].(map[string]any)
	return x
}

// cmp applies a string comparator: eq, in, nin.
func cmp(v string, c map[string]any) bool {
	if c == nil {
		return false
	}
	for op, x := range c {
		switch op {
		case "eq":
			if s, _ := x.(string); s != v {
				return false
			}
		case "in", "nin":
			list, _ := x.([]any)
			in := false
			for _, y := range list {
				in = in || y == v
			}
			if in != (op == "in") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// --- webhooks ---

// Payload is a webhook delivery's body for an entity change, as by actor,
// stamped now.
func (s *Server) Payload(typ, action string, data map[string]any, actor *User) []byte {
	s.mu.Lock()
	ts := s.now().UnixMilli()
	s.mu.Unlock()
	p := map[string]any{"type": typ, "action": action, "data": data, "webhookTimestamp": ts, "webhookId": "webhook-1",
		"createdAt": time.UnixMilli(ts).UTC().Format(time.RFC3339Nano)}
	if actor != nil {
		p["actor"] = map[string]any{"id": actor.ID, "name": actor.Name, "type": "user"}
	}
	b, _ := json.Marshal(p)
	return b
}

// Sign is a delivery's Linear-Signature.
func Sign(body, secret []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Deliver posts a signed delivery to url.
func Deliver(url string, body, secret []byte, id string) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Linear-Signature", Sign(body, secret))
	req.Header.Set("Linear-Delivery", id)
	req.Header.Set("Linear-Event", "Issue")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Moves is an issue's history of state changes, oldest first, as
// (actor's id, from state's name, to state's name).
func (s *Server) Moves(issue string) [][3]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][3]string
	for _, h := range s.history {
		if h.IssueID == issue {
			out = append(out, [3]string{h.ActorID, s.stateByID(h.From).Name, s.stateByID(h.To).Name})
		}
	}
	return out
}
