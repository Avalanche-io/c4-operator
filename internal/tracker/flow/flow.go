// Package flow is c4-sync's Autodesk Flow Production Tracking adapter
// (proposals/integrations tools/flow-production-tracking.md, LAUNCH.md
// 3.1). It speaks the REST API v1.1 with a script's client credentials
// and never uses Toolkit, whose licence forbids running it for third
// parties.
//
// What it does in Flow:
//   - reads Tasks (due_date, a date with no time) as work;
//   - writes work states only: the Task's status from the Tracker's
//     statusOut.states map, and optionally an ETA field and a link field;
//     one Note on the Task when a person is needed, edited in place;
//   - publishes each output as a Version on the Task's entity, in the
//     site's pending-review status, carrying the output view's C4 ID;
//   - reads decisions back from the event log: a person changing a
//     Version's status, with the Notes written on it.
//
// It never sets an approval: approval stays with the supervisor in Flow.
//
// Scope keys: project (the Project id), script (the API script's name;
// the credential Secret holds its key), and optionally review_status
// (default rev), id_field (default sg_c4_id), eta_field and link_field
// (written only when named: custom fields the site admin creates).
package flow

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// Kind is the adapter's registry name.
const Kind = "flow"

const apiBase = "/api/v1.1"

// arrayJSON is the content type _search needs for array filters.
const arrayJSON = "application/vnd+shotgun.api3_array+json"

func init() {
	tracker.Register(Kind, func(c tracker.Config) (tracker.Adapter, error) { return New(c) })
}

// Adapter is the Flow adapter for one project on one site.
type Adapter struct {
	c       tracker.Config
	base    string
	project int
	script  string
	hc      *http.Client
	now     func() time.Time

	reviewStatus, idField, etaField, linkField string

	mu      sync.Mutex
	token   string
	expires time.Time
	people  map[string]tracker.Person
}

// New builds the adapter.
func New(c tracker.Config) (*Adapter, error) {
	base := strings.TrimSuffix(c.Endpoint, "/")
	if base == "" {
		return nil, errors.New("flow: endpoint (the site URL) is required")
	}
	p, err := strconv.Atoi(c.Scope["project"])
	if err != nil || p <= 0 {
		return nil, fmt.Errorf("flow: scope.project must be a Project id, got %q", c.Scope["project"])
	}
	if c.Scope["script"] == "" {
		return nil, errors.New("flow: scope.script (the API script's name) is required")
	}
	a := &Adapter{c: c, base: base, project: p, script: c.Scope["script"], hc: c.HTTPClient, now: c.Now,
		reviewStatus: or(c.Scope["review_status"], "rev"), idField: or(c.Scope["id_field"], "sg_c4_id"),
		etaField: c.Scope["eta_field"], linkField: c.Scope["link_field"], people: map[string]tracker.Person{}}
	if a.hc == nil {
		a.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func (a *Adapter) Kind() string { return Kind }

func (a *Adapter) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{EventLog: true, Webhooks: true, EditableComments: true, CustomFields: true,
		DateOnlyDue: true, PublishForReview: true}
}

// --- HTTP ---

// link is an entity link as REST writes it.
type link struct {
	Type string `json:"type"`
	ID   int    `json:"id"`
	Name string `json:"name,omitempty"`
}

// record is a REST record.
type record struct {
	Type          string                     `json:"type"`
	ID            int                        `json:"id"`
	Attributes    map[string]json.RawMessage `json:"attributes"`
	Relationships map[string]struct {
		Data json.RawMessage `json:"data"`
	} `json:"relationships"`
}

func (r record) str(k string) string {
	var s string
	_ = json.Unmarshal(r.Attributes[k], &s)
	return s
}

func (r record) link(k string) *link {
	rel, ok := r.Relationships[k]
	if !ok {
		return nil
	}
	var l link
	if json.Unmarshal(rel.Data, &l) != nil || l.Type == "" {
		return nil
	}
	return &l
}

type errBody struct {
	Errors []struct {
		Status int    `json:"status"`
		Code   int    `json:"code"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	} `json:"errors"`
}

// accessToken exchanges the script's credentials for an access token
// (client_credentials), kept until a minute before it expires.
func (a *Adapter) accessToken(ctx context.Context, renew bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !renew && a.token != "" && a.now().Before(a.expires.Add(-time.Minute)) {
		return a.token, nil
	}
	key := ""
	if a.c.Token != nil {
		k, err := a.c.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("flow: credential: %w", err)
		}
		key = k
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {a.script}, "client_secret": {key}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+apiBase+"/auth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("flow: authenticating: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("flow: authenticating script %s: %s", a.script, errText(resp.Status, data))
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &t); err != nil || t.AccessToken == "" {
		return "", fmt.Errorf("flow: authenticating: no access token in the answer")
	}
	a.token, a.expires = t.AccessToken, a.now().Add(time.Duration(t.ExpiresIn)*time.Second)
	return a.token, nil
}

func errText(status string, data []byte) string {
	var e errBody
	if json.Unmarshal(data, &e) == nil && len(e.Errors) > 0 {
		x := e.Errors[0]
		return strings.TrimSpace(x.Title + ": " + x.Detail)
	}
	return status
}

// do calls the API, renewing the access token once if it expired.
func (a *Adapter) do(ctx context.Context, method, path, ctype string, in, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := a.accessToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		var body io.Reader
		if in != nil {
			b, err := json.Marshal(in)
			if err != nil {
				return err
			}
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		if in != nil {
			req.Header.Set("Content-Type", or(ctype, "application/json"))
		}
		resp, err := a.hc.Do(req)
		if err != nil {
			return fmt.Errorf("flow: %s %s: %w", method, path, err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			continue // the token expired early, or was revoked: one fresh try
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: %s", tracker.ErrNotFound, errText(resp.Status, data))
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			// Flow publishes no request limits (tools/flow-production-
			// tracking.md); 503 is "down for maintenance or too busy".
			wait := time.Minute
			if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				wait = time.Duration(n) * time.Second
			}
			return &tracker.RateLimited{Until: a.now().Add(wait), Msg: errText(resp.Status, data)}
		case resp.StatusCode >= 300:
			return fmt.Errorf("flow: %s %s: %s", method, path, errText(resp.Status, data))
		}
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

func entityPath(typ string) string {
	p := map[string]string{"Task": "tasks", "Version": "versions", "Note": "notes", "Reply": "replies",
		"HumanUser": "human_users", "ApiUser": "api_users", "EventLogEntry": "event_log_entries",
		"Shot": "shots", "Asset": "assets", "PublishedFile": "published_files", "Project": "projects"}[typ]
	if p == "" {
		p = snakePlural(typ)
	}
	return apiBase + "/entity/" + p
}

// snakePlural is the REST name for other entities: plural snake_case.
func snakePlural(t string) string {
	var b strings.Builder
	for i, r := range t {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String() + "s"
}

func (a *Adapter) projectLink() link { return link{Type: "Project", ID: a.project} }

// search runs one _search page; page is "" for the first, then a next
// link from the answer.
func (a *Adapter) search(ctx context.Context, typ string, filters [][]any, fields []string, sort string, page string) ([]record, string, error) {
	path := page
	if path == "" {
		q := url.Values{"fields": {strings.Join(fields, ",")}, "page[size]": {"500"}}
		if sort != "" {
			q.Set("sort", sort)
		}
		path = entityPath(typ) + "/_search?" + q.Encode()
	} else if !strings.HasPrefix(path, apiBase+"/") {
		return nil, "", fmt.Errorf("flow: refusing a next link off the API: %s", path)
	}
	var res struct {
		Data  []record `json:"data"`
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	}
	if err := a.do(ctx, http.MethodPost, path, arrayJSON, map[string]any{"filters": filters}, &res); err != nil {
		return nil, "", err
	}
	return res.Data, res.Links.Next, nil
}

func (a *Adapter) searchAll(ctx context.Context, typ string, filters [][]any, fields []string, sort string) ([]record, error) {
	var out []record
	page := ""
	for {
		rs, next, err := a.search(ctx, typ, filters, fields, sort, page)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
		if next == "" {
			return out, nil
		}
		page = next
	}
}

func (a *Adapter) read(ctx context.Context, typ string, id string, fields []string) (record, error) {
	var res struct {
		Data record `json:"data"`
	}
	q := url.Values{"fields": {strings.Join(fields, ",")}}
	err := a.do(ctx, http.MethodGet, entityPath(typ)+"/"+url.PathEscape(id)+"?"+q.Encode(), "", nil, &res)
	return res.Data, err
}

// --- reading ---

var taskFields = []string{"content", "due_date", "sg_status_list", "entity", "step", "project", "task_assignees", "updated_at"}

func (a *Adapter) taskItem(r record) tracker.Item {
	it := tracker.Item{
		Ref:    tracker.Ref{Type: "Task", ID: strconv.Itoa(r.ID), URL: a.base + "/detail/Task/" + strconv.Itoa(r.ID)},
		Title:  r.str("content"),
		Status: tracker.Status{Native: r.str("sg_status_list")},
		Fields: map[string]string{},
	}
	if d := r.str("due_date"); d != "" {
		if t, err := time.Parse("2006-01-02", d); err == nil {
			it.Due = &tracker.Due{At: t, DateOnly: true}
		}
	}
	if u := r.str("updated_at"); u != "" {
		it.UpdatedAt, _ = time.Parse(time.RFC3339, u)
		it.Version = u
	}
	if e := r.link("entity"); e != nil {
		it.Parent = &tracker.Ref{Type: e.Type, ID: strconv.Itoa(e.ID)}
		it.Fields["entity.type"], it.Fields["entity.id"], it.Fields["entity.name"] = e.Type, strconv.Itoa(e.ID), e.Name
	}
	if s := r.link("step"); s != nil {
		it.Fields["step"] = s.Name
	}
	return it
}

func (a *Adapter) Get(ctx context.Context, ref tracker.Ref) (tracker.Item, error) {
	switch ref.Type {
	case "Task":
		r, err := a.read(ctx, "Task", ref.ID, taskFields)
		if err != nil {
			return tracker.Item{}, err
		}
		return a.taskItem(r), nil
	case "Version":
		r, err := a.read(ctx, "Version", ref.ID, []string{"code", "sg_status_list", "entity", "sg_task", a.idField, "updated_at"})
		if err != nil {
			return tracker.Item{}, err
		}
		return tracker.Item{Ref: tracker.Ref{Type: "Version", ID: ref.ID, URL: a.base + "/detail/Version/" + ref.ID},
			Title: r.str("code"), Status: tracker.Status{Native: r.str("sg_status_list")},
			Fields: map[string]string{"c4_id": r.str(a.idField)}}, nil
	}
	return tracker.Item{}, fmt.Errorf("flow: Get %s: %w", ref.Type, tracker.ErrUnsupported)
}

// List finds Tasks in the project matching q.Fields (equality filters in
// Flow's own field names, which may reach through a link:
// step.Step.short_name). Labels are not a Flow concept and are refused.
func (a *Adapter) List(ctx context.Context, q tracker.Query, page string) ([]tracker.Item, string, error) {
	if q.Type != "Task" {
		return nil, "", fmt.Errorf("flow: List %s: %w", q.Type, tracker.ErrUnsupported)
	}
	if len(q.Labels) > 0 {
		return nil, "", errors.New("flow: intake matches fields, not labels")
	}
	filters := [][]any{{"project", "is", a.projectLink()}}
	keys := make([]string, 0, len(q.Fields))
	for k := range q.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		filters = append(filters, []any{k, "is", q.Fields[k]})
	}
	rs, next, err := a.search(ctx, "Task", filters, taskFields, "id", page)
	if err != nil {
		return nil, "", err
	}
	out := make([]tracker.Item, 0, len(rs))
	for _, r := range rs {
		out = append(out, a.taskItem(r))
	}
	return out, next, nil
}

// Changes reads the event log after the cursor (an EventLogEntry id),
// oldest first: Flow's replay mechanism (the shotgunEvents daemon reads it
// the same way). With no cursor it starts at the newest entry rather than
// replaying the site's history; intake and resync cover what came before.
// Entries can commit out of id order; an entry that lands behind the
// cursor is caught by the loop's resync.
func (a *Adapter) Changes(ctx context.Context, cursor string) ([]tracker.Ref, string, error) {
	fields := []string{"event_type", "entity", "attribute_name", "user"}
	if cursor == "" {
		rs, _, err := a.search(ctx, "EventLogEntry", [][]any{{"project", "is", a.projectLink()}}, fields, "-id", "")
		if err != nil || len(rs) == 0 {
			return nil, "0", err
		}
		return nil, strconv.Itoa(rs[0].ID), nil
	}
	last, err := strconv.Atoi(cursor)
	if err != nil {
		return nil, cursor, fmt.Errorf("flow: bad cursor %q", cursor)
	}
	rs, err := a.searchAll(ctx, "EventLogEntry", [][]any{{"project", "is", a.projectLink()}, {"id", "greater_than", last}}, fields, "id")
	if err != nil {
		return nil, cursor, err
	}
	var refs []tracker.Ref
	seen := map[string]bool{}
	for _, r := range rs {
		if r.ID > last {
			last = r.ID
		}
		if e := r.link("entity"); e != nil {
			ref := tracker.Ref{Type: e.Type, ID: strconv.Itoa(e.ID)}
			if !seen[ref.String()] {
				seen[ref.String()] = true
				refs = append(refs, ref)
			}
		}
	}
	return refs, strconv.Itoa(last), nil
}

// Decisions reads, from the event log, people's changes to the Version's
// status after since, each with the Notes written on the Version since
// then (their replies included) as its text. Changes made by API scripts,
// c4-sync's own included, are not decisions.
func (a *Adapter) Decisions(ctx context.Context, on tracker.Ref, since time.Time) ([]tracker.Decision, error) {
	if on.Type != "Version" {
		return nil, fmt.Errorf("flow: Decisions on %s: %w", on.Type, tracker.ErrUnsupported)
	}
	id, _ := strconv.Atoi(on.ID)
	v := link{Type: "Version", ID: id}
	after := since.UTC().Format(time.RFC3339)
	events, err := a.searchAll(ctx, "EventLogEntry", [][]any{
		{"entity", "is", v}, {"attribute_name", "is", "sg_status_list"}, {"created_at", "greater_than", after},
	}, []string{"meta", "user", "created_at", "event_type"}, "id")
	if err != nil {
		return nil, err
	}
	var out []tracker.Decision
	for _, e := range events {
		u := e.link("user")
		if u == nil || u.Type != "HumanUser" {
			continue
		}
		var meta struct {
			New any `json:"new_value"`
		}
		_ = json.Unmarshal(e.Attributes["meta"], &meta)
		code := fmt.Sprint(meta.New)
		at, _ := time.Parse(time.RFC3339, e.str("created_at"))
		who, err := a.Person(ctx, "HumanUser/"+strconv.Itoa(u.ID))
		if err != nil {
			return nil, err
		}
		body, notes, err := a.notesOn(ctx, v, since)
		if err != nil {
			return nil, err
		}
		ev, _ := json.Marshal(map[string]any{"event": e, "notes": notes})
		out = append(out, tracker.Decision{
			On:     on,
			Source: tracker.Ref{Type: "EventLogEntry", ID: strconv.Itoa(e.ID), URL: a.base + "/detail/EventLogEntry/" + strconv.Itoa(e.ID)},
			By:     who, Native: "status:" + code, Body: body, At: at, Evidence: ev,
		})
	}
	return out, nil
}

// notesOn is the text of the Notes on v since a time, each with its
// thread (thread_contents), and the raw threads as evidence.
func (a *Adapter) notesOn(ctx context.Context, v link, since time.Time) (string, []json.RawMessage, error) {
	notes, err := a.searchAll(ctx, "Note", [][]any{
		{"note_links", "is", v}, {"created_at", "greater_than", since.UTC().Format(time.RFC3339)},
	}, []string{"subject", "content", "user", "created_at"}, "id")
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	var raw []json.RawMessage
	for _, n := range notes {
		var th struct {
			Data []struct {
				Type    string `json:"type"`
				Content string `json:"content"`
			} `json:"data"`
		}
		var rm json.RawMessage
		if err := a.do(ctx, http.MethodGet, entityPath("Note")+"/"+strconv.Itoa(n.ID)+"/thread_contents", "", nil, &rm); err != nil {
			return "", nil, err
		}
		raw = append(raw, rm)
		_ = json.Unmarshal(rm, &th)
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if s := n.str("subject"); s != "" {
			b.WriteString(s + ": ")
		}
		for i, x := range th.Data {
			if i > 0 {
				b.WriteString("\n> ")
			}
			b.WriteString(strings.TrimSpace(x.Content))
		}
	}
	return b.String(), raw, nil
}

// --- webhooks ---

// Receive verifies X-SG-SIGNATURE (sha1=<hex HMAC-SHA1 of the body>) when
// a secret token is configured and parses a delivery, batched or not.
// Deliveries are hints: the event log is read under them.
func (a *Adapter) Receive(r *http.Request) (tracker.Delivery, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		return tracker.Delivery{}, err
	}
	if a.c.WebhookSecret != nil {
		secret, err := a.c.WebhookSecret(r.Context())
		if err != nil {
			return tracker.Delivery{}, err
		}
		if len(secret) > 0 && !verify(body, secret, r.Header.Get("X-SG-SIGNATURE")) {
			return tracker.Delivery{}, tracker.ErrBadSignature
		}
	}
	type event struct {
		Entity     *link  `json:"entity"`
		User       *link  `json:"user"`
		DeliveryID string `json:"delivery_id"`
	}
	var p struct {
		Data struct {
			event
			Deliveries []event `json:"deliveries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return tracker.Delivery{}, fmt.Errorf("flow: delivery: %w", err)
	}
	events := p.Data.Deliveries
	if len(events) == 0 {
		events = []event{p.Data.event}
	}
	d := tracker.Delivery{ID: or(r.Header.Get("x-sg-delivery-id"), p.Data.DeliveryID), Self: true}
	for _, e := range events {
		if e.Entity != nil {
			d.Refs = append(d.Refs, tracker.Ref{Type: e.Entity.Type, ID: strconv.Itoa(e.Entity.ID)})
		}
		// A script's change, c4-sync's own included, is not a person's.
		if e.User == nil || e.User.Type != "ApiUser" {
			d.Self = false
		}
	}
	return d, nil
}

func verify(body, secret []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha1=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha1.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// --- writing ---

// keyLine is the line a Note carries its write key in, so Probe can find
// a Note written before a crash. Flow notes are plain text; the line is
// short and says what it is.
func keyLine(key string) string { return "[c4-sync " + key + "]" }

// Apply makes one write. Kinds:
//   - update: fields on w.On (a Task), as Flow field names; an empty value
//     is not written;
//   - note: a Note on w.On (subject from Fields["subject"]), or with
//     Note.Replace, new content for that Note.
func (a *Adapter) Apply(ctx context.Context, w tracker.Write) (tracker.Ref, error) {
	switch w.Kind {
	case "update":
		body := map[string]any{}
		for k, v := range w.Fields {
			if v != "" {
				body[k] = v
			}
		}
		if len(body) == 0 {
			return tracker.Ref{}, nil
		}
		if err := a.do(ctx, http.MethodPut, entityPath(w.On.Type)+"/"+url.PathEscape(w.On.ID), "", body, nil); err != nil {
			return tracker.Ref{}, err
		}
		return w.On, nil
	case "note":
		if w.Note == nil || w.Note.Body == "" {
			return tracker.Ref{}, errors.New("flow: note without content")
		}
		content := w.Note.Body
		if w.Key != "" {
			content += "\n\n" + keyLine(w.Key)
		}
		if r := w.Note.Replace; r != nil {
			if err := a.do(ctx, http.MethodPut, entityPath("Note")+"/"+url.PathEscape(r.ID), "", map[string]any{"content": content}, nil); err != nil {
				return tracker.Ref{}, err
			}
			return *r, nil
		}
		id, _ := strconv.Atoi(w.On.ID)
		var res struct {
			Data record `json:"data"`
		}
		in := map[string]any{"project": a.projectLink(), "subject": or(w.Fields["subject"], "C4"), "content": content,
			"note_links": []link{{Type: w.On.Type, ID: id}}}
		if err := a.do(ctx, http.MethodPost, entityPath("Note"), "", in, &res); err != nil {
			return tracker.Ref{}, err
		}
		return tracker.Ref{Type: "Note", ID: strconv.Itoa(res.Data.ID)}, nil
	}
	return tracker.Ref{}, fmt.Errorf("flow: write kind %q: %w", w.Kind, tracker.ErrUnsupported)
}

// Publish makes the output a Version on the Task's entity, linked to the
// Task, in the site's pending-review status, with the output view's C4 ID
// in the id field and the job link in its description. A Version that
// already carries the view's C4 ID for this Task is the one a crash left:
// it is returned, not made again.
func (a *Adapter) Publish(ctx context.Context, on tracker.Ref, out tracker.Output, key string) (tracker.Ref, error) {
	if on.Type != "Task" {
		return tracker.Ref{}, fmt.Errorf("flow: Publish on %s: %w", on.Type, tracker.ErrUnsupported)
	}
	tid, _ := strconv.Atoi(on.ID)
	task := link{Type: "Task", ID: tid}
	found, _, err := a.search(ctx, "Version", [][]any{{"sg_task", "is", task}, {a.idField, "is", out.View}}, []string{"code"}, "", "")
	if err != nil {
		return tracker.Ref{}, err
	}
	if len(found) > 0 {
		id := strconv.Itoa(found[0].ID)
		return tracker.Ref{Type: "Version", ID: id, URL: a.base + "/detail/Version/" + id}, nil
	}
	t, err := a.read(ctx, "Task", on.ID, taskFields)
	if err != nil {
		return tracker.Ref{}, err
	}
	entity := t.link("entity")
	if entity == nil {
		return tracker.Ref{}, fmt.Errorf("flow: Task %s has no entity to publish a Version on", on.ID)
	}
	step := ""
	if s := t.link("step"); s != nil {
		step = s.Name
	}
	code := VersionCode(entity.Name, step, t.str("content"), out.Version)
	desc := fmt.Sprintf("%s\nC4 job: %s\nOutput view: %s\nProvenance: %s", out.Summary, out.JobURL, out.View, out.Custody)
	in := map[string]any{"project": a.projectLink(), "code": code, "entity": link{Type: entity.Type, ID: entity.ID},
		"sg_task": task, "sg_status_list": a.reviewStatus, "description": desc, a.idField: out.View}
	if a.linkField != "" {
		in[a.linkField] = out.JobURL
	}
	var res struct {
		Data record `json:"data"`
	}
	if err := a.do(ctx, http.MethodPost, entityPath("Version"), "", in, &res); err != nil {
		return tracker.Ref{}, err
	}
	id := strconv.Itoa(res.Data.ID)
	return tracker.Ref{Type: "Version", ID: id, URL: a.base + "/detail/Version/" + id}, nil
}

// VersionCode names a Version as studios commonly do: entity, step, and a
// three-digit version (sh040_lgt_v007). The step's short name, else the
// task's name, lower-cased.
func VersionCode(entity, step, task string, n int) string {
	s := strings.ToLower(or(step, task))
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, s)
	if n < 1 {
		n = 1
	}
	return fmt.Sprintf("%s_%s_v%03d", entity, s, n)
}

// Probe finds a Note already written under w.Key. Field updates need no
// probe: writing the same values again changes nothing.
func (a *Adapter) Probe(ctx context.Context, w tracker.Write) (bool, tracker.Ref, error) {
	if w.Kind != "note" || w.Key == "" || w.Note == nil || w.Note.Replace != nil {
		return false, tracker.Ref{}, nil
	}
	id, _ := strconv.Atoi(w.On.ID)
	ns, err := a.searchAll(ctx, "Note", [][]any{{"note_links", "is", link{Type: w.On.Type, ID: id}},
		{"content", "contains", keyLine(w.Key)}}, []string{"subject"}, "id")
	if err != nil || len(ns) == 0 {
		return false, tracker.Ref{}, err
	}
	return true, tracker.Ref{Type: "Note", ID: strconv.Itoa(ns[0].ID)}, nil
}

// Person reads a user by "HumanUser/<id>"; its ID is the login, which the
// Tracker's people map keys on.
func (a *Adapter) Person(ctx context.Context, id string) (tracker.Person, error) {
	a.mu.Lock()
	p, ok := a.people[id]
	a.mu.Unlock()
	if ok {
		return p, nil
	}
	typ, n, found := strings.Cut(id, "/")
	if !found {
		typ, n = "HumanUser", id
	}
	r, err := a.read(ctx, typ, n, []string{"login", "name", "email"})
	if err != nil {
		return tracker.Person{}, err
	}
	p = tracker.Person{ID: r.str("login"), Name: r.str("name"), Email: r.str("email")}
	a.mu.Lock()
	a.people[id] = p
	a.mu.Unlock()
	return p, nil
}
