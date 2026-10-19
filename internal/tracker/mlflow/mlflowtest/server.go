// Package mlflowtest is a fake MLflow tracking server and model registry
// for testing c4-sync offline. It serves the REST API 2.0 subset c4-sync
// uses, in the shapes MLflow's REST reference documents
// (mlflow.org/docs/latest/api_reference/rest-api.html, read 2026-10-19):
//
//   - runs: POST runs/create, GET runs/get, POST runs/search (filter,
//     max_results, page_token; next_page_token), POST runs/update
//     (status, end_time), POST runs/set-tag; RunStatus RUNNING,
//     SCHEDULED, FINISHED, FAILED, KILLED;
//   - registry: GET registered-models/get (aliases as {alias, version}),
//     POST model-versions/create (name, source, run_id, tags,
//     description), GET model-versions/get, GET model-versions/search
//     (filter, page_token), POST model-versions/set-tag, POST and GET
//     registered-models/alias;
//   - errors as {error_code, message}; authentication as HTTP basic, as
//     the basic-auth app does.
//
// Webhook deliveries are signed as the self-hosting webhook guide
// documents: X-MLflow-Signature: v1,<base64 HMAC-SHA256 of
// "<delivery id>.<timestamp>.<body>">, with X-MLflow-Timestamp and
// X-MLflow-Delivery-Id. Model-version sources are checked as a real
// server checks them: an empty or relative path is refused. The guide
// gives no payload for alias events, so
// Payload's data fields are the guide's tag-event fields (name, version).
package mlflowtest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

// Tag is a run or model version tag.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Run is a run (info and tags).
type Run struct {
	ID, Name, Experiment, Status string
	Start, End                   int64
	Tags                         map[string]string
}

// ModelVersion is a registered model's version.
type ModelVersion struct {
	Name, Version, Source, RunID, Description, Status string
	Created, Updated                                  int64
	Tags                                              map[string]string
}

// Call is one request served.
type Call struct{ Method, Path, Body string }

// Server is the fake.
type Server struct {
	*httptest.Server
	User, Password string

	mu       sync.Mutex
	now      func() time.Time
	runs     map[string]*Run
	order    []string
	versions map[string][]*ModelVersion // by model name
	aliases  map[string]map[string]string
	calls    []Call
	nextRun  int
}

// New starts a fake with one registered model, name.
func New(model string) *Server {
	s := &Server{User: "c4-sync", Password: "pw", now: time.Now, runs: map[string]*Run{},
		versions: map[string][]*ModelVersion{model: nil}, aliases: map[string]map[string]string{model: {}}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// SetClock replaces time.Now.
func (s *Server) SetClock(now func() time.Time) { s.mu.Lock(); s.now = now; s.mu.Unlock() }

func (s *Server) ms() int64 { return s.now().UnixMilli() }

// AddRun creates a run as a client would (the project backend plugin, or
// a person), with status and tags.
func (s *Server) AddRun(experiment, name, status string, tags map[string]string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextRun++
	id := fmt.Sprintf("%032x", s.nextRun)
	r := &Run{ID: id, Name: name, Experiment: experiment, Status: status, Start: s.ms(), Tags: map[string]string{}}
	for k, v := range tags {
		r.Tags[k] = v
	}
	s.runs[id] = r
	s.order = append(s.order, id)
	return id
}

// GetRun returns a copy of a run.
func (s *Server) GetRun(id string) Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := *s.runs[id]
	r.Tags = copyTags(r.Tags)
	return r
}

// Versions returns a model's versions.
func (s *Server) Versions(model string) []ModelVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ModelVersion
	for _, v := range s.versions[model] {
		c := *v
		c.Tags = copyTags(v.Tags)
		out = append(out, c)
	}
	return out
}

// SetAlias points an alias at a version, as an approver with MANAGE does.
func (s *Server) SetAlias(model, alias, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aliases[model][alias] = version
}

// Alias is the version an alias points at.
func (s *Server) Alias(model, alias string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aliases[model][alias]
}

// SetVersionTag tags a version, as a reviewer does.
func (s *Server) SetVersionTag(model, version, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.versions[model] {
		if v.Version == version {
			v.Tags[key], v.Updated = value, s.ms()
		}
	}
}

// Calls returns the requests served.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Writes returns requests that changed something.
func (s *Server) Writes() []Call {
	var out []Call
	for _, c := range s.Calls() {
		p := strings.Split(c.Path, "?")[0]
		if c.Method == http.MethodPost && !strings.HasSuffix(p, "/search") {
			out = append(out, c)
		}
	}
	return out
}

func copyTags(t map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range t {
		out[k] = v
	}
	return out
}

// --- webhooks ---

// Payload is a webhook body: the documented envelope (entity, action,
// timestamp, data).
func Payload(entity, action string, data map[string]any, at time.Time) []byte {
	b, _ := json.Marshal(map[string]any{"entity": entity, "action": action, "timestamp": at.UTC().Format(time.RFC3339), "data": data})
	return b
}

// Deliver posts a signed delivery.
func Deliver(url string, body, secret []byte, deliveryID string, at time.Time) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MLflow-Delivery-Id", deliveryID)
	req.Header.Set("X-MLflow-Timestamp", ts)
	if secret != nil {
		req.Header.Set("X-MLflow-Signature", Sign(deliveryID, ts, body, secret))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Sign is the X-MLflow-Signature value.
func Sign(deliveryID, ts string, body, secret []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(deliveryID + "." + ts + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// --- the REST API ---

type apiErr struct {
	status int
	code   string
	msg    string
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.RequestURI(), Body: string(body)})
	w.Header().Set("Content-Type", "application/json")
	if u, p, ok := r.BasicAuth(); !ok || u != s.User || p != s.Password {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("You are not authenticated."))
		return
	}
	out, e := s.route(r, body)
	if e != nil {
		w.WriteHeader(e.status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error_code": e.code, "message": e.msg})
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) route(r *http.Request, body []byte) (any, *apiErr) {
	path := strings.TrimPrefix(r.URL.Path, "/api/2.0/mlflow/")
	q := r.URL.Query()
	var in map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, &apiErr{400, "INVALID_PARAMETER_VALUE", "malformed JSON"}
		}
	}
	str := func(k string) string { v, _ := in[k].(string); return v }
	nf := func(what string) *apiErr { return &apiErr{404, "RESOURCE_DOES_NOT_EXIST", what + " not found"} }
	switch r.Method + " " + path {
	case "GET runs/get":
		run := s.runs[q.Get("run_id")]
		if run == nil {
			return nil, nf("Run '" + q.Get("run_id") + "'")
		}
		return map[string]any{"run": s.runJSON(run)}, nil
	case "POST runs/search":
		return s.searchRuns(in)
	case "POST runs/update":
		run := s.runs[str("run_id")]
		if run == nil {
			return nil, nf("Run")
		}
		if st := str("status"); st != "" {
			switch st {
			case "RUNNING", "SCHEDULED", "FINISHED", "FAILED", "KILLED":
				run.Status = st
			default:
				return nil, &apiErr{400, "INVALID_PARAMETER_VALUE", "status " + st}
			}
		}
		if et, ok := in["end_time"].(float64); ok {
			run.End = int64(et)
		}
		return map[string]any{"run_info": s.runJSON(run)["info"]}, nil
	case "POST runs/set-tag":
		run := s.runs[str("run_id")]
		if run == nil {
			return nil, nf("Run")
		}
		if str("key") == "" {
			return nil, &apiErr{400, "INVALID_PARAMETER_VALUE", "key is required"}
		}
		run.Tags[str("key")] = str("value")
		return map[string]any{}, nil
	case "GET registered-models/get":
		name := q.Get("name")
		if _, ok := s.versions[name]; !ok {
			return nil, nf("Registered Model with name=" + name)
		}
		var al []map[string]string
		keys := make([]string, 0, len(s.aliases[name]))
		for a := range s.aliases[name] {
			keys = append(keys, a)
		}
		sort.Strings(keys)
		for _, a := range keys {
			al = append(al, map[string]string{"alias": a, "version": s.aliases[name][a]})
		}
		return map[string]any{"registered_model": map[string]any{"name": name, "aliases": al}}, nil
	case "POST model-versions/create":
		name := str("name")
		if _, ok := s.versions[name]; !ok {
			return nil, nf("Registered Model with name=" + name)
		}
		// As MLflow 3.17.1's server does (measured, integrations/mlflow-c4):
		// a source whose path is empty, or relative, is refused.
		if u, err := url.Parse(str("source")); err != nil || u.Scheme != "" && (strings.Trim(u.Path, "/") == "" || strings.Contains(u.Path, "..")) {
			return nil, &apiErr{400, "INVALID_PARAMETER_VALUE", "Invalid model version source: '" + str("source") + "'."}
		}
		v := &ModelVersion{Name: name, Version: strconv.Itoa(len(s.versions[name]) + 1), Source: str("source"),
			RunID: str("run_id"), Description: str("description"), Status: "READY", Created: s.ms(), Updated: s.ms(),
			Tags: map[string]string{}}
		if tags, ok := in["tags"].([]any); ok {
			for _, t := range tags {
				m, _ := t.(map[string]any)
				k, _ := m["key"].(string)
				val, _ := m["value"].(string)
				v.Tags[k] = val
			}
		}
		s.versions[name] = append(s.versions[name], v)
		return map[string]any{"model_version": s.mvJSON(v)}, nil
	case "GET model-versions/get":
		v := s.mv(q.Get("name"), q.Get("version"))
		if v == nil {
			return nil, nf("Model Version")
		}
		return map[string]any{"model_version": s.mvJSON(v)}, nil
	case "GET model-versions/search":
		return s.searchVersions(q.Get("filter"))
	case "POST model-versions/set-tag":
		v := s.mv(str("name"), str("version"))
		if v == nil {
			return nil, nf("Model Version")
		}
		v.Tags[str("key")], v.Updated = str("value"), s.ms()
		return map[string]any{}, nil
	case "POST registered-models/alias":
		if s.mv(str("name"), str("version")) == nil {
			return nil, nf("Model Version")
		}
		s.aliases[str("name")][str("alias")] = str("version")
		return map[string]any{}, nil
	case "GET registered-models/alias":
		ver, ok := s.aliases[q.Get("name")][q.Get("alias")]
		if !ok {
			return nil, nf("alias")
		}
		return map[string]any{"model_version": s.mvJSON(s.mv(q.Get("name"), ver))}, nil
	}
	return nil, &apiErr{404, "ENDPOINT_NOT_FOUND", "no endpoint " + r.Method + " " + path}
}

func (s *Server) mv(name, version string) *ModelVersion {
	for _, v := range s.versions[name] {
		if v.Version == version {
			return v
		}
	}
	return nil
}

func tagList(t map[string]string) []Tag {
	out := []Tag{}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, Tag{k, t[k]})
	}
	return out
}

func (s *Server) runJSON(r *Run) map[string]any {
	info := map[string]any{"run_id": r.ID, "run_uuid": r.ID, "run_name": r.Name, "experiment_id": r.Experiment,
		"status": r.Status, "start_time": r.Start, "artifact_uri": "mlflow-artifacts:/" + r.ID, "lifecycle_stage": "active"}
	if r.End > 0 {
		info["end_time"] = r.End
	}
	return map[string]any{"info": info, "data": map[string]any{"tags": tagList(r.Tags), "params": []any{}, "metrics": []any{}}}
}

func (s *Server) mvJSON(v *ModelVersion) map[string]any {
	var aliases []string
	for a, ver := range s.aliases[v.Name] {
		if ver == v.Version {
			aliases = append(aliases, a)
		}
	}
	sort.Strings(aliases)
	return map[string]any{"name": v.Name, "version": v.Version, "creation_timestamp": v.Created,
		"last_updated_timestamp": v.Updated, "source": v.Source, "run_id": v.RunID, "status": v.Status,
		"description": v.Description, "tags": tagList(v.Tags), "aliases": aliases, "current_stage": "None"}
}

// clause is one "and" term of a filter: attributes.status = 'X',
// tags.`k` = 'v', run_id = 'x', name = 'm'.
var clause = regexp.MustCompile("^\\s*(attributes\\.|tags\\.)?`?([A-Za-z0-9_.\\-]+)`?\\s*=\\s*'([^']*)'\\s*$")

func parse(filter string) ([][3]string, *apiErr) {
	var out [][3]string
	if strings.TrimSpace(filter) == "" {
		return nil, nil
	}
	for _, part := range regexp.MustCompile(`(?i)\s+and\s+`).Split(filter, -1) {
		m := clause.FindStringSubmatch(part)
		if m == nil {
			return nil, &apiErr{400, "INVALID_PARAMETER_VALUE", "Invalid filter '" + filter + "'"}
		}
		out = append(out, [3]string{m[1], m[2], m[3]})
	}
	return out, nil
}

func (s *Server) searchRuns(in map[string]any) (any, *apiErr) {
	cl, e := parse(fmt.Sprint(in["filter"]))
	if in["filter"] == nil {
		cl = nil
	} else if e != nil {
		return nil, e
	}
	exps := map[string]bool{}
	if ids, ok := in["experiment_ids"].([]any); ok {
		for _, x := range ids {
			exps[fmt.Sprint(x)] = true
		}
	}
	var hits []*Run
	for _, id := range s.order {
		r := s.runs[id]
		if len(exps) > 0 && !exps[r.Experiment] {
			continue
		}
		ok := true
		for _, c := range cl {
			var got string
			switch {
			case c[0] == "tags.":
				got = r.Tags[c[1]]
			case c[1] == "status":
				got = r.Status
			case c[1] == "run_name":
				got = r.Name
			default:
				return nil, &apiErr{400, "INVALID_PARAMETER_VALUE", "unsupported attribute " + c[1]}
			}
			ok = ok && got == c[2]
		}
		if ok {
			hits = append(hits, r)
		}
	}
	max := 1000
	if m, ok := in["max_results"].(float64); ok && m > 0 {
		max = int(m)
	}
	from := 0
	if tok, _ := in["page_token"].(string); tok != "" {
		from, _ = strconv.Atoi(tok)
	}
	to := min(from+max, len(hits))
	if from > to {
		from = to
	}
	runs := []map[string]any{}
	for _, r := range hits[from:to] {
		runs = append(runs, s.runJSON(r))
	}
	out := map[string]any{"runs": runs}
	if to < len(hits) {
		out["next_page_token"] = strconv.Itoa(to)
	}
	return out, nil
}

func (s *Server) searchVersions(filter string) (any, *apiErr) {
	cl, e := parse(filter)
	if e != nil {
		return nil, e
	}
	var out []map[string]any
	for _, vs := range s.versions {
		for _, v := range vs {
			ok := true
			for _, c := range cl {
				switch {
				case c[0] == "" && c[1] == "name":
					ok = ok && v.Name == c[2]
				case c[0] == "" && c[1] == "run_id":
					ok = ok && v.RunID == c[2]
				default:
					return nil, &apiErr{400, "INVALID_PARAMETER_VALUE", "unsupported filter " + c[1]}
				}
			}
			if ok {
				out = append(out, s.mvJSON(v))
			}
		}
	}
	return map[string]any{"model_versions": out}, nil
}
