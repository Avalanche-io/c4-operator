// Package flowtest is a fake Autodesk Flow Production Tracking site for
// testing c4-sync offline. It serves the REST API v1.1 subset c4-sync
// uses, in the shapes Autodesk's REST reference documents
// (developers.shotgridsoftware.com/rest-api, read 2026-10-19):
//
//   - POST /api/v1.1/auth/access_token, form-encoded client_credentials
//     (client_id = script name, client_secret = script key), answering
//     token_type, access_token, expires_in, refresh_token;
//   - GET /api/v1.1/entity/{entity}/{id}?fields=..., where {entity} is the
//     plural snake_case name (tasks, versions, event_log_entries);
//   - POST /api/v1.1/entity/{entity}/_search with array filters (content
//     type application/vnd+shotgun.api3_array+json), fields, sort and
//     page[number], page[size] as query parameters;
//   - POST /api/v1.1/entity/{entity} (create) and PUT .../{id} (update);
//   - GET /api/v1.1/entity/notes/{id}/thread_contents;
//   - uploads in three steps: GET .../{entity}/{id}/{field}/_upload?
//     filename= answers upload info (data) and links (upload,
//     complete_upload); a PUT to links.upload carries the bytes; a POST of
//     {upload_info, upload_data} to links.complete_upload sets the field
//     (image to a URL, any other field to an attachment) and answers 204;
//   - records as {type, id, attributes, relationships, links}, lists as
//     {data, links: {self, next}}, errors as {errors: [{status, code,
//     title, detail}]}.
//
// Every create and update writes an EventLogEntry (Shotgun_<Type>_New or
// _Change, with attribute_name and meta old_value and new_value), as the
// site does, so the event log replays everything. Webhook deliveries are
// signed as the webhook guide documents: X-SG-SIGNATURE: sha1=<hex HMAC
// of the body with the secret token>.
package flowtest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Link is an entity link value: {"type": "Shot", "id": 1180}.
type Link struct {
	Type string `json:"type"`
	ID   int    `json:"id"`
	Name string `json:"name,omitempty"`
}

// Record is one entity.
type Record struct {
	Type   string
	ID     int
	Fields map[string]any // attribute values, Link and []Link for links
}

// Call is one request served.
type Call struct {
	Method, Path, Body string
}

// Server is the fake site.
type Server struct {
	*httptest.Server
	// Script and Key are the API script the site knows.
	Script, Key string
	// TokenSeconds is the access tokens' life (default 600).
	TokenSeconds int
	// ScriptUser is the ApiUser record of the script.
	ScriptUser Link

	mu      sync.Mutex
	now     func() time.Time
	next    map[string]int
	recs    map[string]map[int]*Record
	tokens  map[string]time.Time // access token: expiry
	calls   []Call
	tokenN  int
	project int
	// uploads are the bytes a PUT sent, by upload id, until completed;
	// files are completed uploads, by type/id/field.
	uploads map[string]upload
	files   map[string][]byte
}

type upload struct {
	typ, field, name string
	id               int
	data             []byte
}

// noContent is a route's answer with no body (204).
type noContent struct{}

var plural = map[string]string{
	"Project": "projects", "Shot": "shots", "Asset": "assets", "Sequence": "sequences",
	"Task": "tasks", "Step": "steps", "Version": "versions", "PublishedFile": "published_files",
	"Note": "notes", "Reply": "replies", "HumanUser": "human_users", "ApiUser": "api_users",
	"EventLogEntry": "event_log_entries", "Playlist": "playlists",
}

func singular(p string) string {
	for k, v := range plural {
		if v == p {
			return k
		}
	}
	return ""
}

// New starts a fake site with one project, on a random local port.
func New() *Server {
	s := newServer()
	s.Server.Start()
	return s
}

// Unstarted makes a fake site that does not listen: serve Handler()
// yourself, beside other routes (hack/demo/fakeflow runs it as a service).
func Unstarted() *Server { return newServer() }

// Handler is the site's REST API, to mount beside other routes.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

func newServer() *Server {
	s := &Server{Script: "c4-sync", Key: "s3cretkey", TokenSeconds: 600, now: time.Now,
		next: map[string]int{}, recs: map[string]map[int]*Record{}, tokens: map[string]time.Time{},
		uploads: map[string]upload{}, files: map[string][]byte{}}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.mu.Lock()
	s.project = s.add("Project", map[string]any{"name": "Saltmarsh"}, nil).ID
	u := s.add("ApiUser", map[string]any{"firstname": "c4-sync", "lastname": "script"}, nil)
	s.ScriptUser = Link{Type: "ApiUser", ID: u.ID, Name: "c4-sync"}
	s.mu.Unlock()
	return s
}

// SetClock replaces time.Now for timestamps and token expiry.
func (s *Server) SetClock(now func() time.Time) { s.mu.Lock(); s.now = now; s.mu.Unlock() }

// Project is the site's project.
func (s *Server) Project() Link { return Link{Type: "Project", ID: s.project, Name: "Saltmarsh"} }

func (s *Server) add(typ string, f map[string]any, by *Link) *Record {
	if s.recs[typ] == nil {
		s.recs[typ] = map[int]*Record{}
	}
	if s.next[typ] == 0 {
		s.next[typ] = map[string]int{"Project": 122, "Task": 4410, "Version": 88119, "Shot": 1179, "Note": 99100, "EventLogEntry": 545100}[typ]
	}
	s.next[typ]++
	r := &Record{Type: typ, ID: s.next[typ], Fields: map[string]any{}}
	for k, v := range f {
		r.Fields[k] = v
	}
	now := s.now().UTC().Format(time.RFC3339)
	r.Fields["created_at"], r.Fields["updated_at"] = now, now
	if by != nil {
		r.Fields["created_by"] = *by
	}
	s.recs[typ][r.ID] = r
	if typ != "EventLogEntry" && by != nil {
		s.event(r, "Shotgun_"+typ+"_New", "", nil, nil, *by)
	}
	return r
}

func (s *Server) event(r *Record, typ, attr string, old, new any, by Link) {
	meta := map[string]any{"type": "new_entity", "entity_type": r.Type, "entity_id": r.ID}
	if attr != "" {
		meta = map[string]any{"type": "attribute_change", "entity_type": r.Type, "entity_id": r.ID,
			"attribute_name": attr, "old_value": old, "new_value": new}
	}
	f := map[string]any{"event_type": typ, "attribute_name": attr, "meta": meta,
		"entity": Link{Type: r.Type, ID: r.ID}, "user": by, "project": s.Project(),
		"session_uuid": "fake-session"}
	if attr == "" {
		delete(f, "attribute_name")
	}
	s.add("EventLogEntry", f, nil)
}

func (s *Server) update(typ string, id int, f map[string]any, by Link) error {
	r := s.recs[typ][id]
	if r == nil {
		return fmt.Errorf("no %s %d", typ, id)
	}
	for k, v := range f {
		old := r.Fields[k]
		if fmt.Sprint(old) == fmt.Sprint(v) {
			continue
		}
		r.Fields[k] = v
		s.event(r, "Shotgun_"+typ+"_Change", k, old, v, by)
	}
	r.Fields["updated_at"] = s.now().UTC().Format(time.RFC3339)
	return nil
}

// --- what people do in the site, for tests ---

// AddUser adds a HumanUser.
func (s *Server) AddUser(login, name, email string) Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.add("HumanUser", map[string]any{"login": login, "name": name, "email": email}, nil)
	return Link{Type: "HumanUser", ID: r.ID, Name: name}
}

// AddShot adds a Shot.
func (s *Server) AddShot(code string) Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.add("Shot", map[string]any{"code": code, "project": s.Project()}, nil)
	return Link{Type: "Shot", ID: r.ID, Name: code}
}

// AddTask adds a Task on entity, due on a date (YYYY-MM-DD), by a person.
func (s *Server) AddTask(entity Link, content, step, status, due string, by Link) Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.add("Task", map[string]any{"content": content, "entity": entity, "project": s.Project(),
		"step": Link{Type: "Step", ID: 7, Name: step}, "sg_status_list": status, "due_date": due}, &by)
	return Link{Type: "Task", ID: r.ID, Name: content}
}

// Set changes fields as a person in the site would.
func (s *Server) Set(l Link, f map[string]any, by Link) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.update(l.Type, l.ID, f, by); err != nil {
		panic(err)
	}
}

// AddNote writes a Note on what it is about, with replies, as a person.
func (s *Server) AddNote(about Link, subject, content string, by Link, replies ...string) Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.add("Note", map[string]any{"subject": subject, "content": content, "project": s.Project(),
		"note_links": []Link{about}, "user": by}, &by)
	for _, rp := range replies {
		s.add("Reply", map[string]any{"content": rp, "entity": Link{Type: "Note", ID: n.ID}, "user": by}, &by)
	}
	return Link{Type: "Note", ID: n.ID}
}

// Get returns a copy of a record's fields.
func (s *Server) Get(typ string, id int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[typ][id]
	if r == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range r.Fields {
		out[k] = v
	}
	return out
}

// All returns every record of a type, by id.
func (s *Server) All(typ string) []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Record
	for _, r := range s.recs[typ] {
		c := *r
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Calls returns the requests served.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Writes returns the requests that changed something (not reads, searches
// or token requests).
func (s *Server) Writes() []Call {
	var out []Call
	for _, c := range s.Calls() {
		if (c.Method == http.MethodPost && !strings.HasSuffix(strings.Split(c.Path, "?")[0], "/_search") &&
			!strings.HasSuffix(c.Path, "/auth/access_token")) || c.Method == http.MethodPut || c.Method == http.MethodDelete {
			out = append(out, c)
		}
	}
	return out
}

// Upload returns the file a completed upload set on a record's field, or
// nil.
func (s *Server) Upload(typ string, id int, field string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[fileKey(typ, id, field)]
}

func fileKey(typ string, id int, field string) string { return fmt.Sprintf("%s/%d/%s", typ, id, field) }

// ExpireTokens makes every access token expire now.
func (s *Server) ExpireTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.tokens {
		s.tokens[k] = time.Time{}
	}
}

// --- webhooks ---

// Payload is the webhook delivery body for one event log entry, as the
// webhook guide's example has it (non-batched).
func (s *Server) Payload(eventID int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.recs["EventLogEntry"][eventID]
	data := map[string]any{
		"id":                 fmt.Sprintf("%d.%d.0", e.ID, eventID%9973),
		"event_log_entry_id": e.ID,
		"event_type":         e.Fields["event_type"],
		"operation":          map[bool]string{true: "update", false: "create"}[e.Fields["attribute_name"] != nil],
		"entity":             e.Fields["entity"],
		"project":            e.Fields["project"],
		"user":               e.Fields["user"],
		"meta":               e.Fields["meta"],
		"attribute_name":     e.Fields["attribute_name"],
		"created_at":         e.Fields["created_at"],
		"delivery_id":        fmt.Sprintf("dlv-%d", e.ID),
		"session_uuid":       "fake-session",
	}
	b, _ := json.Marshal(map[string]any{"data": data, "timestamp": s.now().UTC().Format(time.RFC3339)})
	return b
}

// LastEvent is the newest event log entry's id.
func (s *Server) LastEvent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next["EventLogEntry"]
}

// Deliver posts a webhook delivery to url with the documented headers.
func Deliver(url string, body, secret []byte, deliveryID string) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("content-type", "application/json; charset=utf-8")
	req.Header.Set("x-sg-webhook-id", "wh-1")
	req.Header.Set("x-sg-delivery-id", deliveryID)
	req.Header.Set("x-sg-event-batch-size", "1")
	req.Header.Set("x-sg-event-batch-index", "0")
	if secret != nil {
		req.Header.Set("X-SG-SIGNATURE", Sign(body, secret))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Sign is the X-SG-SIGNATURE value for body.
func Sign(body, secret []byte) string {
	m := hmac.New(sha1.New, secret)
	m.Write(body)
	return "sha1=" + hex.EncodeToString(m.Sum(nil))
}

// --- the REST API ---

type apiErr struct {
	status, code  int
	title, detail string
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.RequestURI(), Body: string(body)})
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/v1.1/auth/access_token" && r.Method == http.MethodPost {
		s.token(w, r, body)
		return
	}
	who, e := s.auth(r)
	if e != nil {
		writeErr(w, *e)
		return
	}
	out, e := s.route(r, body, who)
	if e != nil {
		writeErr(w, *e)
		return
	}
	if _, ok := out.(noContent); ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodPost && !strings.HasSuffix(r.URL.Path, "/_search") {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func writeErr(w http.ResponseWriter, e apiErr) {
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{
		"id": "fake", "status": e.status, "code": e.code, "title": e.title, "detail": e.detail, "source": map[string]any{}, "meta": nil,
	}}})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request, body []byte) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		writeErr(w, apiErr{400, 103, "Bad Request", "form-encoded body expected"})
		return
	}
	v, _ := url.ParseQuery(string(body))
	if v.Get("grant_type") != "client_credentials" || v.Get("client_id") != s.Script || v.Get("client_secret") != s.Key {
		writeErr(w, apiErr{400, 102, "Bad Request", "Can't authenticate script"})
		return
	}
	s.tokenN++
	tok := fmt.Sprintf("tok-%d", s.tokenN)
	s.tokens[tok] = s.now().Add(time.Duration(s.TokenSeconds) * time.Second)
	_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "access_token": tok,
		"expires_in": s.TokenSeconds, "refresh_token": "refresh-" + tok})
}

func (s *Server) auth(r *http.Request) (Link, *apiErr) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	exp, known := s.tokens[tok]
	if !ok || !known {
		return Link{}, &apiErr{401, 102, "Unauthorized", "Invalid token"}
	}
	if !s.now().Before(exp) {
		return Link{}, &apiErr{401, 102, "Unauthorized", "Token Expired"}
	}
	return s.ScriptUser, nil
}

func (s *Server) route(r *http.Request, body []byte, who Link) (any, *apiErr) {
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1.1/"), "/")
	notFound := &apiErr{404, 104, "Not Found", "Record not found"}
	if len(p) < 2 || p[0] != "entity" {
		return nil, notFound
	}
	typ := singular(p[1])
	if typ == "" {
		return nil, &apiErr{400, 103, "Bad Request", "unknown entity " + p[1]}
	}
	fields := fieldList(r.URL.Query().Get("fields"))
	switch {
	case len(p) == 3 && p[2] == "_search" && r.Method == http.MethodPost:
		return s.search(r, typ, body, fields)
	case len(p) == 3 && r.Method == http.MethodGet:
		rec := s.recs[typ][atoi(p[2])]
		if rec == nil {
			return nil, notFound
		}
		return map[string]any{"data": s.render(rec, fields), "links": map[string]string{"self": r.URL.Path}}, nil
	case len(p) == 2 && r.Method == http.MethodPost:
		f, e := decode(body)
		if e != nil {
			return nil, e
		}
		rec := s.add(typ, f, &who)
		return map[string]any{"data": s.render(rec, nil), "links": map[string]string{"self": selfPath(rec)}}, nil
	case len(p) == 3 && r.Method == http.MethodPut:
		f, e := decode(body)
		if e != nil {
			return nil, e
		}
		id := atoi(p[2])
		if s.recs[typ][id] == nil {
			return nil, notFound
		}
		_ = s.update(typ, id, f, who)
		return map[string]any{"data": s.render(s.recs[typ][id], nil), "links": map[string]string{"self": r.URL.Path}}, nil
	case len(p) == 4 && typ == "Note" && p[3] == "thread_contents" && r.Method == http.MethodGet:
		n := s.recs["Note"][atoi(p[2])]
		if n == nil {
			return nil, notFound
		}
		data := []map[string]any{{"type": "Note", "id": n.ID, "content": n.Fields["content"], "created_at": n.Fields["created_at"],
			"user": n.Fields["user"]}}
		for _, rp := range s.sorted("Reply") {
			if l, ok := rp.Fields["entity"].(Link); ok && l.ID == n.ID {
				data = append(data, map[string]any{"type": "Reply", "id": rp.ID, "content": rp.Fields["content"],
					"created_at": rp.Fields["created_at"], "user": rp.Fields["user"]})
			}
		}
		return map[string]any{"data": data, "links": map[string]string{"self": r.URL.Path}}, nil
	case len(p) == 5 && p[4] == "_upload":
		return s.upload(r, typ, atoi(p[2]), p[3], body, who)
	}
	return nil, notFound
}

// upload serves the three steps of an upload to a record's field.
func (s *Server) upload(r *http.Request, typ string, id int, field string, body []byte, who Link) (any, *apiErr) {
	rec := s.recs[typ][id]
	if rec == nil {
		return nil, &apiErr{404, 104, "Not Found", "Record not found"}
	}
	path := r.URL.Path
	q := r.URL.Query()
	switch r.Method {
	case http.MethodGet:
		name := q.Get("filename")
		if name == "" {
			return nil, &apiErr{400, 103, "Bad Request", "filename is required"}
		}
		s.next["upload"]++
		uid := fmt.Sprintf("upload-%d", s.next["upload"])
		s.uploads[uid] = upload{typ: typ, id: id, field: field, name: name}
		kind := "Attachment"
		if field == "image" || field == "filmstrip_image" {
			kind = "Thumbnail"
		}
		return map[string]any{
			"data": map[string]any{"timestamp": s.now().UTC().Format(time.RFC3339), "upload_type": kind,
				"upload_id": uid, "storage_service": "sg", "original_filename": name, "multipart_upload": false},
			"links": map[string]string{
				"upload":          path + "?upload_id=" + uid + "&filename=" + url.QueryEscape(name),
				"complete_upload": path,
			}}, nil
	case http.MethodPut:
		u, ok := s.uploads[q.Get("upload_id")]
		if !ok || u.typ != typ || u.id != id || u.field != field {
			return nil, &apiErr{400, 103, "Bad Request", "no such upload"}
		}
		u.data = append([]byte(nil), body...)
		s.uploads[q.Get("upload_id")] = u
		return map[string]any{"data": map[string]any{"upload_id": q.Get("upload_id"), "original_filename": u.name}}, nil
	case http.MethodPost:
		var in struct {
			Info map[string]any `json:"upload_info"`
			Data map[string]any `json:"upload_data"`
		}
		if err := json.Unmarshal(body, &in); err != nil || in.Info == nil {
			return nil, &apiErr{400, 103, "Bad Request", "body: {upload_info, upload_data}"}
		}
		uid, _ := in.Info["upload_id"].(string)
		u, ok := s.uploads[uid]
		if !ok || u.typ != typ || u.id != id || u.field != field || u.data == nil {
			return nil, &apiErr{400, 103, "Bad Request", "no such upload, or its bytes never came"}
		}
		delete(s.uploads, uid)
		s.files[fileKey(typ, id, field)] = u.data
		var v any = fmt.Sprintf("/thumbnail/%s/%d/%s", plural[typ], id, url.PathEscape(u.name))
		if u.field != "image" && u.field != "filmstrip_image" {
			v = map[string]any{"type": "Attachment", "name": u.name, "link_type": "upload", "content_type": http.DetectContentType(u.data),
				"url": fmt.Sprintf("/file_serve/attachment/%s/%d/%s", plural[typ], id, url.PathEscape(u.name))}
		}
		_ = s.update(typ, id, map[string]any{field: v}, who)
		return noContent{}, nil
	}
	return nil, &apiErr{405, 103, "Method Not Allowed", r.Method}
}

func decode(body []byte) (map[string]any, *apiErr) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, &apiErr{400, 103, "Bad Request", "body is not a JSON object"}
	}
	out := map[string]any{}
	for k, v := range raw {
		var l Link
		if err := json.Unmarshal(v, &l); err == nil && l.Type != "" && l.ID != 0 {
			out[k] = l
			continue
		}
		var ls []Link
		if err := json.Unmarshal(v, &ls); err == nil && len(ls) > 0 && ls[0].Type != "" {
			out[k] = ls
			continue
		}
		var x any
		_ = json.Unmarshal(v, &x)
		if _, isNum := x.(float64); numberFields[k] && x != nil && !isNum {
			// a site refuses a string in a number or currency field
			return nil, &apiErr{400, 103, "Bad Request", fmt.Sprintf("%s: a number is expected, got %s", k, v)}
		}
		out[k] = x
	}
	return out, nil
}

// numberFields are the fields typed as numbers on the fake site: the
// Version's frames, and the currency field a site admin makes for C4's
// spend (FLOW.md).
var numberFields = map[string]bool{"sg_first_frame": true, "sg_last_frame": true, "sg_c4_spend": true}

func selfPath(r *Record) string { return fmt.Sprintf("/api/v1.1/entity/%s/%d", plural[r.Type], r.ID) }

// render is a record as REST returns it: links under relationships, the
// rest under attributes, only the fields asked for (all with none or *).
func (s *Server) render(r *Record, fields []string) map[string]any {
	attrs, rels := map[string]any{}, map[string]any{}
	want := func(k string) bool {
		if len(fields) == 0 || (len(fields) == 1 && fields[0] == "*") {
			return true
		}
		for _, f := range fields {
			if f == k {
				return true
			}
		}
		return false
	}
	for k, v := range r.Fields {
		if !want(k) {
			continue
		}
		switch x := v.(type) {
		case Link:
			rels[k] = map[string]any{"data": s.named(x)}
		case []Link:
			var ls []Link
			for _, l := range x {
				ls = append(ls, s.named(l))
			}
			rels[k] = map[string]any{"data": ls}
		default:
			attrs[k] = v
		}
	}
	return map[string]any{"type": r.Type, "id": r.ID, "attributes": attrs, "relationships": rels,
		"links": map[string]string{"self": selfPath(r)}}
}

// named fills in a link's display name, as REST relationship data has it.
func (s *Server) named(l Link) Link {
	if r := s.recs[l.Type][l.ID]; r != nil && l.Name == "" {
		for _, k := range []string{"code", "content", "name", "login"} {
			if v, ok := r.Fields[k].(string); ok {
				l.Name = v
				break
			}
		}
	}
	return l
}

func (s *Server) sorted(typ string) []*Record {
	var out []*Record
	for _, r := range s.recs[typ] {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Server) search(r *http.Request, typ string, body []byte, fields []string) (any, *apiErr) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/vnd+shotgun.api3_array+json") {
		return nil, &apiErr{400, 103, "Bad Request", "array filters need Content-Type application/vnd+shotgun.api3_array+json"}
	}
	var in struct {
		Filters [][]json.RawMessage `json:"filters"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, &apiErr{400, 103, "Bad Request", "filters"}
	}
	var hits []*Record
	for _, rec := range s.sorted(typ) {
		ok := true
		for _, f := range in.Filters {
			if len(f) != 3 || !s.match(rec, f) {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, rec)
		}
	}
	if srt := r.URL.Query().Get("sort"); srt != "" {
		desc := strings.HasPrefix(srt, "-")
		k := strings.TrimPrefix(srt, "-")
		sort.SliceStable(hits, func(i, j int) bool {
			a, b := fmt.Sprint(sortVal(hits[i], k)), fmt.Sprint(sortVal(hits[j], k))
			if k == "id" {
				return (hits[i].ID < hits[j].ID) != desc
			}
			return (a < b) != desc
		})
	}
	size, num := 500, 1
	if v := r.URL.Query().Get("page[size]"); v != "" {
		size = atoi(v)
	}
	if v := r.URL.Query().Get("page[number]"); v != "" {
		num = atoi(v)
	}
	from := (num - 1) * size
	if from > len(hits) {
		from = len(hits)
	}
	to := min(from+size, len(hits))
	data := []map[string]any{}
	for _, rec := range hits[from:to] {
		data = append(data, s.render(rec, fields))
	}
	links := map[string]any{"self": r.URL.RequestURI()}
	if to < len(hits) {
		q := r.URL.Query()
		q.Set("page[number]", strconv.Itoa(num+1))
		links["next"] = r.URL.Path + "?" + q.Encode()
	}
	return map[string]any{"data": data, "links": links}, nil
}

func sortVal(r *Record, k string) any {
	if k == "id" {
		return r.ID
	}
	return r.Fields[k]
}

// match applies one [field, operator, value] filter. A field may reach
// through a link: "step.Step.short_name" reads the linked record's field.
func (s *Server) match(r *Record, f []json.RawMessage) bool {
	var field, op string
	_ = json.Unmarshal(f[0], &field)
	_ = json.Unmarshal(f[1], &op)
	var got any
	if parts := strings.Split(field, "."); len(parts) == 3 {
		l, ok := r.Fields[parts[0]].(Link)
		if !ok || l.Type != parts[1] {
			return false
		}
		if lr := s.recs[l.Type][l.ID]; lr != nil {
			got = lr.Fields[parts[2]]
		} else if parts[2] == "short_name" || parts[2] == "code" || parts[2] == "name" {
			got = l.Name
		}
	} else if field == "id" {
		got = r.ID
	} else {
		got = r.Fields[field]
	}
	var want any
	_ = json.Unmarshal(f[2], &want)
	switch op {
	case "is", "is_not":
		eq := equal(got, f[2], want)
		return eq == (op == "is")
	case "in":
		var opts []json.RawMessage
		_ = json.Unmarshal(f[2], &opts)
		for _, o := range opts {
			var w any
			_ = json.Unmarshal(o, &w)
			if equal(got, o, w) {
				return true
			}
		}
		return false
	case "greater_than", "less_than":
		gt := compare(got, want)
		if op == "greater_than" {
			return gt > 0
		}
		return gt < 0
	case "contains":
		return strings.Contains(fmt.Sprint(got), fmt.Sprint(want))
	}
	return false
}

func equal(got any, raw json.RawMessage, want any) bool {
	var l Link
	if err := json.Unmarshal(raw, &l); err == nil && l.Type != "" {
		switch g := got.(type) {
		case Link:
			return g.Type == l.Type && g.ID == l.ID
		case []Link: // a multi-entity field "is" a link when it holds it
			for _, x := range g {
				if x.Type == l.Type && x.ID == l.ID {
					return true
				}
			}
		}
		return false
	}
	return fmt.Sprint(got) == fmt.Sprint(want)
}

// compare orders numbers as numbers and times as RFC 3339 strings.
func compare(got, want any) int {
	gf, gok := toFloat(got)
	wf, wok := toFloat(want)
	if gok && wok {
		switch {
		case gf > wf:
			return 1
		case gf < wf:
			return -1
		}
		return 0
	}
	return strings.Compare(fmt.Sprint(got), fmt.Sprint(want))
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func fieldList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
