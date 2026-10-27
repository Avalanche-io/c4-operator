// Package kitsu is c4-sync's Kitsu adapter (CGWire's open-source
// production tracker). It speaks the REST API of Kitsu's server, Zou,
// through the routes the Gazu client documents, as a bot: a Kitsu Person with is_bot set, whose API token is
// the credential, used as the bearer.
//
// What it does in Kitsu:
//   - reads Tasks (due_date, a date the UI sets at midnight) as work, with
//     their task type, entity (a shot and its sequence, or an asset),
//     assignees and estimation (in minutes, the bid a forecast follows);
//   - writes work states only, by commenting: Kitsu changes a Task's
//     status only through a comment carrying the new status. The status
//     comes from the Tracker's statusOut.states map (short names), else
//     from the statuses' flags (is_wip for running, is_feedback_request
//     for review). The ETA and the job link go to the Task's data, under
//     keys the Tracker names (scope eta_field, link_field), which Kitsu
//     shows where the studio adds Task metadata descriptors for them. One
//     comment when a person is needed, edited in place;
//   - publishes each output as a comment that moves the Task to its
//     review status, carrying the output view's C4 ID, with the output's
//     still as the comment's preview (a new revision), as
//     gazu.task.publish_preview does;
//   - reads decisions back from the Task's comments: a person's comment
//     that moves the Task out of review, with the notes written since.
//     By default a status flagged is_done approves and one flagged
//     is_retake asks for rework; the Tracker's verdictsIn, by short name
//     (status:<short_name>), wins.
//
// It never sets a done status: approval stays with the supervisor in
// Kitsu.
//
// Scope keys: project (the production's id or name), and optionally
// review_status (the short name of the status a publish sets; default the
// one flagged is_feedback_request), eta_field and link_field (Task data
// keys, written only when named).
//
// The bot must be in the production's team with the manager role: Kitsu
// lets managers set any status and read the event log, and nobody else
// both.
package kitsu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/effect"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// Kind is the adapter's registry name.
const Kind = "kitsu"

// timeLayout is how Zou writes and reads datetimes: naive UTC, whole
// seconds.
const timeLayout = "2006-01-02T15:04:05"

func init() {
	tracker.Register(Kind, func(c tracker.Config) (tracker.Adapter, error) { return New(c) })
}

// Adapter is the Kitsu adapter for one production on one site.
type Adapter struct {
	c       tracker.Config
	site    string // the Kitsu site, for links people follow
	api     string // site + /api
	project string // as the scope names it: an id or a name
	hc      *http.Client
	now     func() time.Time

	reviewStatus, etaField, linkField, spendField string

	mu          sync.Mutex
	projectID   string
	self        string // the bot's own person id
	statuses    []status
	types       map[string]string // task type id: name
	entityTypes map[string]string // entity type id: name
	people      map[string]tracker.Person
}

// New builds the adapter.
func New(c tracker.Config) (*Adapter, error) {
	site := strings.TrimSuffix(c.Endpoint, "/")
	if site == "" {
		return nil, errors.New("kitsu: endpoint (the Kitsu site's URL) is required")
	}
	site = strings.TrimSuffix(site, "/api")
	if c.Scope["project"] == "" {
		return nil, errors.New("kitsu: scope.project (the production's id or name) is required")
	}
	a := &Adapter{c: c, site: site, api: site + "/api", project: c.Scope["project"], hc: c.HTTPClient, now: c.Now,
		reviewStatus: c.Scope["review_status"], etaField: c.Scope["eta_field"], linkField: c.Scope["link_field"], spendField: c.Scope["spend_field"],
		people: map[string]tracker.Person{}}
	if a.hc == nil {
		a.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

func (a *Adapter) Kind() string { return Kind }

// Capabilities: Kitsu keeps an event log (data/events/last) but pushes
// only over Socket.IO, which replays nothing, so the adapter polls the log
// and receives no deliveries.
func (a *Adapter) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{EventLog: true, EditableComments: true, CustomFields: true, DateOnlyDue: true,
		PublishForReview: true, PublishPreview: true, Mentions: true}
}

// --- HTTP ---

// do calls the API under /api. in is JSON unless it is a *multipart
// body; out receives the JSON answer.
func (a *Adapter) do(ctx context.Context, method, path string, in, out any) error {
	tok := ""
	if a.c.Token != nil {
		t, err := a.c.Token(ctx)
		if err != nil {
			return fmt.Errorf("kitsu: credential: %w", err)
		}
		tok = t
	}
	var body io.Reader
	ctype := ""
	switch x := in.(type) {
	case nil:
	case *form:
		body, ctype = bytes.NewReader(x.body), x.ctype
	default:
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, a.api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("kitsu: %s %s: %w", method, path, err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	switch c := resp.StatusCode; {
	case c == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s: %s", tracker.ErrNotFound, method, path, errText(resp.Status, data))
	case c == http.StatusTooManyRequests || c == http.StatusServiceUnavailable:
		// Zou has no request limit; 503 is its media concurrency cap, or
		// a proxy in front of it.
		wait := time.Minute
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			wait = time.Duration(n) * time.Second
		}
		return &tracker.RateLimited{Until: a.now().Add(wait), Msg: errText(resp.Status, data)}
	case c == http.StatusUnauthorized || c == http.StatusUnprocessableEntity:
		// 401 for a revoked or expired token (a bot's token is revoked
		// when it is regenerated), 422 for one that is not a token.
		return fmt.Errorf("kitsu: the bot's token was refused (%s): %w", errText(resp.Status, data), tracker.ErrRefused)
	case c >= 400 && c < 500:
		return fmt.Errorf("kitsu: %s %s: %s: %w", method, path, errText(resp.Status, data), tracker.ErrRefused)
	case c >= 300:
		return fmt.Errorf("kitsu: %s %s: %s", method, path, errText(resp.Status, data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// errText reads Zou's three error shapes: {"error": true, "message"},
// flask-jwt-extended's {"msg"}, and CRUD's {"message"}.
func errText(status string, data []byte) string {
	var e struct {
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(data, &e) == nil {
		if e.Message != "" {
			return e.Message
		}
		if e.Msg != "" {
			return e.Msg
		}
	}
	return status
}

// form is a multipart body.
type form struct {
	body  []byte
	ctype string
}

func fileForm(field, name string, data []byte) (*form, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	p, err := w.CreateFormFile(field, name)
	if err != nil {
		return nil, err
	}
	if _, err := p.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return &form{body: b.Bytes(), ctype: w.FormDataContentType()}, nil
}

// --- what the site is: the production, the bot, statuses, types ---

type status struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	ShortName         string `json:"short_name"`
	IsDone            bool   `json:"is_done"`
	IsRetake          bool   `json:"is_retake"`
	IsFeedbackRequest bool   `json:"is_feedback_request"`
	IsWIP             bool   `json:"is_wip"`
	IsDefault         bool   `json:"is_default"`
	ForConcept        bool   `json:"for_concept"`
	Archived          bool   `json:"archived"`
}

func (s status) category() string {
	switch {
	case s.IsDone:
		return "done"
	case s.IsRetake:
		return "rework"
	case s.IsFeedbackRequest:
		return "review"
	case s.IsWIP:
		return "active"
	case s.IsDefault:
		return "planned"
	}
	return "ready"
}

var uuidLike = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// productionID is the scope's production, by id or name, looked up once.
func (a *Adapter) productionID(ctx context.Context) (string, error) {
	a.mu.Lock()
	id := a.projectID
	a.mu.Unlock()
	if id != "" {
		return id, nil
	}
	if uuidLike.MatchString(a.project) {
		id = a.project
	} else {
		var ps []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := a.do(ctx, http.MethodGet, "/data/projects?name="+url.QueryEscape(a.project), nil, &ps); err != nil {
			return "", err
		}
		for _, p := range ps {
			if p.Name == a.project {
				id = p.ID
			}
		}
		if id == "" {
			return "", fmt.Errorf("kitsu: no production named %q that the bot can see (is it in the team?)", a.project)
		}
	}
	a.mu.Lock()
	a.projectID = id
	a.mu.Unlock()
	return id, nil
}

// selfID is the bot's own person id (auth/authenticated: bots may not
// read data/user/context).
func (a *Adapter) selfID(ctx context.Context) (string, error) {
	a.mu.Lock()
	id := a.self
	a.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var r struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := a.do(ctx, http.MethodGet, "/auth/authenticated", nil, &r); err != nil {
		return "", err
	}
	if r.User.ID == "" {
		return "", errors.New("kitsu: auth/authenticated named no user")
	}
	a.mu.Lock()
	a.self = r.User.ID
	a.mu.Unlock()
	return r.User.ID, nil
}

// statusList is the site's task statuses, read again when one is missing:
// statuses are studio-wide and change rarely.
func (a *Adapter) statusList(ctx context.Context, fresh bool) ([]status, error) {
	a.mu.Lock()
	ss := a.statuses
	a.mu.Unlock()
	if ss != nil && !fresh {
		return ss, nil
	}
	if err := a.do(ctx, http.MethodGet, "/data/task-status", nil, &ss); err != nil {
		return nil, err
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].ShortName < ss[j].ShortName })
	a.mu.Lock()
	a.statuses = ss
	a.mu.Unlock()
	return ss, nil
}

func (a *Adapter) statusBy(ctx context.Context, match func(status) bool) (status, bool, error) {
	for _, fresh := range []bool{false, true} {
		ss, err := a.statusList(ctx, fresh)
		if err != nil {
			return status{}, false, err
		}
		for _, s := range ss {
			if match(s) {
				return s, true, nil
			}
		}
	}
	return status{}, false, nil
}

func (a *Adapter) statusID(ctx context.Context, id string) (status, error) {
	s, ok, err := a.statusBy(ctx, func(s status) bool { return s.ID == id })
	if err == nil && !ok {
		err = fmt.Errorf("kitsu: no task status %s", id)
	}
	return s, err
}

// flagged is the one status with a flag (is_wip, is_feedback_request...),
// for a production that has not named one: the first by short name among
// the shot and asset statuses, when a studio has several.
func flagged(s status, flag string) bool {
	if s.Archived || s.ForConcept {
		return false
	}
	switch flag {
	case "is_wip":
		return s.IsWIP
	case "is_feedback_request":
		return s.IsFeedbackRequest
	case "is_retake":
		return s.IsRetake
	case "is_done":
		return s.IsDone
	}
	return false
}

// resolveStatus finds a status by short name, or by flag when name is "".
func (a *Adapter) resolveStatus(ctx context.Context, name, flag string) (status, error) {
	s, ok, err := a.statusBy(ctx, func(s status) bool {
		if name != "" {
			return s.ShortName == name
		}
		return flagged(s, flag)
	})
	if err == nil && !ok {
		if name != "" {
			err = fmt.Errorf("kitsu: no task status with the short name %q", name)
		} else {
			err = fmt.Errorf("kitsu: no task status flagged %s; name one in the Tracker", flag)
		}
	}
	return s, err
}

// reviewStatusOf is the status a publish sets.
func (a *Adapter) reviewStatusOf(ctx context.Context) (status, error) {
	return a.resolveStatus(ctx, a.reviewStatus, "is_feedback_request")
}

// typeNames maps a cache of id: name read from a list route.
func (a *Adapter) typeNames(ctx context.Context, path string, cache *map[string]string) (map[string]string, error) {
	a.mu.Lock()
	m := *cache
	a.mu.Unlock()
	if m != nil {
		return m, nil
	}
	var ts []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := a.do(ctx, http.MethodGet, path, nil, &ts); err != nil {
		return nil, err
	}
	m = map[string]string{}
	for _, t := range ts {
		m[t.ID] = t.Name
	}
	a.mu.Lock()
	*cache = m
	a.mu.Unlock()
	return m, nil
}

func (a *Adapter) taskTypes(ctx context.Context) (map[string]string, error) {
	return a.typeNames(ctx, "/data/task-types", &a.types)
}

func (a *Adapter) entityTypeNames(ctx context.Context) (map[string]string, error) {
	return a.typeNames(ctx, "/data/entity-types", &a.entityTypes)
}

// --- reading ---

type task struct {
	ID           string         `json:"id"`
	ProjectID    string         `json:"project_id"`
	EntityID     string         `json:"entity_id"`
	TaskTypeID   string         `json:"task_type_id"`
	TaskStatusID string         `json:"task_status_id"`
	DueDate      string         `json:"due_date"`
	Estimation   float64        `json:"estimation"`
	Priority     int            `json:"priority"`
	UpdatedAt    string         `json:"updated_at"`
	Assignees    []string       `json:"assignees"`
	Data         map[string]any `json:"data"`
}

type entity struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ParentID     string `json:"parent_id"`
	EntityTypeID string `json:"entity_type_id"`
}

// place is where a task sits: its entity, the entity's kind, its sequence
// and episode.
type place struct {
	entity, kind, sequence, episode string
}

// TaskURL is a Task's page in Kitsu.
func (a *Adapter) TaskURL(projectID, kind, id string) string {
	section := "shots"
	switch kind {
	case "Shot", "":
	case "Sequence":
		section = "sequences"
	case "Episode":
		section = "episodes"
	case "Edit":
		section = "edits"
	default:
		section = "assets"
	}
	return a.site + "/productions/" + projectID + "/" + section + "/tasks/" + id
}

func (a *Adapter) item(ctx context.Context, t task, p place) (tracker.Item, error) {
	st, err := a.statusID(ctx, t.TaskStatusID)
	if err != nil {
		return tracker.Item{}, err
	}
	types, err := a.taskTypes(ctx)
	if err != nil {
		return tracker.Item{}, err
	}
	typ := types[t.TaskTypeID]
	it := tracker.Item{
		Ref:    tracker.Ref{Type: "Task", ID: t.ID, URL: a.TaskURL(t.ProjectID, p.kind, t.ID)},
		Parent: &tracker.Ref{Type: or(p.kind, "Entity"), ID: t.EntityID},
		Title:  strings.TrimSpace(p.entity + " " + typ),
		// The shot and the task type (sh040-lighting): a shot's name may
		// repeat in another sequence.
		Name:   strings.TrimSpace(p.entity + " " + typ),
		Status: tracker.Status{Native: st.ShortName, Category: st.category()},
		Fields: map[string]string{"task_type": typ, "task_status": st.ShortName, "entity.name": p.entity,
			"entity.type": p.kind, "sequence": p.sequence, "episode": p.episode, "priority": strconv.Itoa(t.Priority)},
		Version: t.UpdatedAt,
	}
	if t.UpdatedAt != "" {
		it.UpdatedAt, _ = time.Parse(timeLayout, t.UpdatedAt)
	}
	// The UI sets due dates at midnight: a date. Any other time is one a
	// script set, naive UTC.
	if d, err := time.Parse(timeLayout, t.DueDate); err == nil {
		it.Due = &tracker.Due{At: d, DateOnly: d.Hour() == 0 && d.Minute() == 0 && d.Second() == 0}
	}
	// The bid, in minutes: what sizes a forecast (DESIGN.md, forecast jobs).
	if t.Estimation > 0 {
		it.Fields["estimation"] = strconv.FormatFloat(t.Estimation, 'f', -1, 64)
	}
	for _, id := range t.Assignees {
		p, err := a.Person(ctx, id)
		if err != nil {
			return tracker.Item{}, err
		}
		it.Assignees = append(it.Assignees, p)
	}
	return it, nil
}

func (a *Adapter) Get(ctx context.Context, ref tracker.Ref) (tracker.Item, error) {
	if ref.Type != "Task" {
		return tracker.Item{}, fmt.Errorf("kitsu: Get %s: %w", ref.Type, tracker.ErrUnsupported)
	}
	var f struct {
		task
		Entity     *entity                `json:"entity"`
		EntityType *struct{ Name string } `json:"entity_type"`
		Sequence   *struct{ Name string } `json:"sequence"`
		Episode    *struct{ Name string } `json:"episode"`
	}
	if err := a.do(ctx, http.MethodGet, "/data/tasks/"+url.PathEscape(ref.ID)+"/full", nil, &f); err != nil {
		return tracker.Item{}, err
	}
	var p place
	if f.Entity != nil {
		p.entity = f.Entity.Name
	}
	if f.EntityType != nil {
		p.kind = f.EntityType.Name
	}
	if f.Sequence != nil {
		p.sequence = f.Sequence.Name
	}
	if f.Episode != nil {
		p.episode = f.Episode.Name
	}
	return a.item(ctx, f.task, p)
}

// List finds the production's Tasks matching q.Fields, in Kitsu's own
// words: task_type (a task type's name), task_status (a status's short
// name), entity.type (Shot, Sequence, or an asset type), entity.name,
// sequence and episode (names); any other key is a Task column, compared
// as Zou compares it. Labels are not a Kitsu concept and are refused. One
// page: Zou's pages are ordered by last update, so they shift under a
// reader; the filters keep the answer small.
func (a *Adapter) List(ctx context.Context, q tracker.Query, page string) ([]tracker.Item, string, error) {
	if q.Type != "Task" {
		return nil, "", fmt.Errorf("kitsu: List %s: %w", q.Type, tracker.ErrUnsupported)
	}
	if len(q.Labels) > 0 {
		return nil, "", errors.New("kitsu: intake matches fields, not labels")
	}
	pid, err := a.productionID(ctx)
	if err != nil {
		return nil, "", err
	}
	v := url.Values{"project_id": {pid}, "relations": {"true"}}
	local := map[string]string{}
	for k, val := range q.Fields {
		switch k {
		case "task_type":
			types, err := a.taskTypes(ctx)
			if err != nil {
				return nil, "", err
			}
			id := ""
			for tid, n := range types {
				if strings.EqualFold(n, val) {
					id = tid
				}
			}
			if id == "" {
				return nil, "", fmt.Errorf("kitsu: no task type named %q", val)
			}
			v.Set("task_type_id", id)
		case "task_status":
			s, err := a.resolveStatus(ctx, val, "")
			if err != nil {
				return nil, "", err
			}
			v.Set("task_status_id", s.ID)
		case "entity.type", "entity.name", "sequence", "episode":
			local[k] = val
		default:
			v.Set(k, val)
		}
	}
	var ts []task
	if err := a.do(ctx, http.MethodGet, "/data/tasks?"+v.Encode(), nil, &ts); err != nil {
		return nil, "", err
	}
	places, err := a.places(ctx, ts)
	if err != nil {
		return nil, "", err
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].ID < ts[j].ID })
	var out []tracker.Item
	for _, t := range ts {
		p := places[t.EntityID]
		if !matches(p, local) {
			continue
		}
		it, err := a.item(ctx, t, p)
		if err != nil {
			return nil, "", err
		}
		out = append(out, it)
	}
	return out, "", nil
}

func matches(p place, want map[string]string) bool {
	for k, v := range want {
		got := map[string]string{"entity.type": p.kind, "entity.name": p.entity, "sequence": p.sequence, "episode": p.episode}[k]
		if !strings.EqualFold(got, v) {
			return false
		}
	}
	return true
}

// places reads the tasks' entities and their parents, fifty ids a call
// (Zou's IN filter is a JSON array in the query).
func (a *Adapter) places(ctx context.Context, ts []task) (map[string]place, error) {
	kinds, err := a.entityTypeNames(ctx)
	if err != nil {
		return nil, err
	}
	ents := map[string]entity{}
	read := func(ids []string) error {
		var need []string
		for _, id := range ids {
			if _, ok := ents[id]; !ok && id != "" && !slicesContains(need, id) {
				need = append(need, id)
			}
		}
		for len(need) > 0 {
			n := min(50, len(need))
			b, _ := json.Marshal(need[:n])
			var es []entity
			if err := a.do(ctx, http.MethodGet, "/data/entities?id="+url.QueryEscape(string(b)), nil, &es); err != nil {
				return err
			}
			for _, e := range es {
				ents[e.ID] = e
			}
			need = need[n:]
		}
		return nil
	}
	var ids []string
	for _, t := range ts {
		ids = append(ids, t.EntityID)
	}
	// The entities, their parents (a shot's sequence), and theirs (the
	// sequence's episode).
	for range 3 {
		if err := read(ids); err != nil {
			return nil, err
		}
		next := ids[:0:0]
		for _, id := range ids {
			if e, ok := ents[id]; ok && e.ParentID != "" {
				next = append(next, e.ParentID)
			}
		}
		ids = next
	}
	out := map[string]place{}
	for _, t := range ts {
		e := ents[t.EntityID]
		p := place{entity: e.Name, kind: kinds[e.EntityTypeID]}
		for par := ents[e.ParentID]; par.ID != ""; par = ents[par.ParentID] {
			switch kinds[par.EntityTypeID] {
			case "Sequence":
				p.sequence = par.Name
			case "Episode":
				p.episode = par.Name
			}
		}
		out[t.EntityID] = p
	}
	return out, nil
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// event is one row of Zou's event log.
type event struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Name      string `json:"name"`
	UserID    string `json:"user_id"`
	Data      struct {
		TaskID string `json:"task_id"`
	} `json:"data"`
}

// eventPages bounds how far back one read pages; the loop's resync covers
// anything older.
const eventPages = 20

// Changes reads the production's event log after the cursor (the newest
// event's time, naive UTC to the second), marking the Tasks that task and
// comment events name. Zou returns the newest events first, a thousand at
// most, so a long gap is read in pages backwards; it compares times
// strictly but stores them finer than it writes them, so events in the
// cursor's own second come again, which costs a look and nothing else.
// With no cursor it starts at the newest event rather than replaying the
// production's history; intake and resync cover what came before.
func (a *Adapter) Changes(ctx context.Context, cursor string) ([]tracker.Ref, string, error) {
	pid, err := a.productionID(ctx)
	if err != nil {
		return nil, cursor, err
	}
	base := url.Values{"project_id": {pid}, "limit": {"1000"}}
	if cursor == "" {
		base.Set("limit", "1")
		var es []event
		if err := a.do(ctx, http.MethodGet, "/data/events/last?"+base.Encode(), nil, &es); err != nil {
			return nil, cursor, err
		}
		if len(es) == 0 {
			return nil, "1970-01-01T00:00:00", nil
		}
		return nil, es[0].CreatedAt, nil
	}
	if _, err := time.Parse(timeLayout, cursor); err != nil {
		return nil, cursor, fmt.Errorf("kitsu: bad cursor %q", cursor)
	}
	base.Set("after", cursor)
	base["name_prefixes"] = []string{"task", "comment", "preview-file"}
	var all []event
	q := base
	for range eventPages {
		var es []event
		if err := a.do(ctx, http.MethodGet, "/data/events/last?"+q.Encode(), nil, &es); err != nil {
			return nil, cursor, err
		}
		all = append(all, es...)
		if len(es) < 1000 {
			break
		}
		q = url.Values{}
		for k, v := range base {
			q[k] = v
		}
		q.Set("cursor_event_id", es[len(es)-1].ID)
	}
	next := cursor
	var refs []tracker.Ref
	seen := map[string]bool{}
	for _, e := range all {
		if e.CreatedAt > next {
			next = e.CreatedAt
		}
		if id := e.Data.TaskID; id != "" && !seen[id] {
			seen[id] = true
			refs = append(refs, tracker.Ref{Type: "Task", ID: id})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, next, nil
}

// comment is a Task comment as data/tasks/{id}/comments returns it.
type comment struct {
	ID           string `json:"id"`
	CreatedAt    string `json:"created_at"`
	Text         string `json:"text"`
	TaskStatusID string `json:"task_status_id"`
	PersonID     string `json:"person_id"`
	TaskStatus   struct {
		ShortName string `json:"short_name"`
	} `json:"task_status"`
	Previews []preview `json:"previews"`
	Replies  []struct {
		Text      string `json:"text"`
		PersonID  string `json:"person_id"`
		CreatedAt string `json:"created_at"`
	} `json:"replies"`
}

type preview struct {
	ID           string `json:"id"`
	Revision     int    `json:"revision"`
	Status       string `json:"status"`
	OriginalName string `json:"original_name"`
	Extension    string `json:"extension"`
}

// comments are a Task's comments, oldest first (Zou sends newest first),
// each with its raw JSON.
func (a *Adapter) comments(ctx context.Context, taskID string) ([]comment, []json.RawMessage, error) {
	var raw []json.RawMessage
	if err := a.do(ctx, http.MethodGet, "/data/tasks/"+url.PathEscape(taskID)+"/comments", nil, &raw); err != nil {
		return nil, nil, err
	}
	cs := make([]comment, len(raw))
	for i, r := range raw {
		if err := json.Unmarshal(r, &cs[i]); err != nil {
			return nil, nil, fmt.Errorf("kitsu: comment: %w", err)
		}
	}
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
		raw[i], raw[j] = raw[j], raw[i]
	}
	return cs, raw, nil
}

func at(s string) time.Time {
	t, _ := time.Parse(timeLayout, s)
	return t
}

// Decisions reads a Task's comments after since: a person's comment that
// moves the Task out of its review status is a decision (status:<short
// name>), its text the notes people wrote since, replies included, up to
// and with it. The bot's own comments decide nothing.
func (a *Adapter) Decisions(ctx context.Context, on tracker.Ref, since time.Time) ([]tracker.Decision, error) {
	if on.Type != "Task" {
		return nil, fmt.Errorf("kitsu: Decisions on %s: %w", on.Type, tracker.ErrUnsupported)
	}
	self, err := a.selfID(ctx)
	if err != nil {
		return nil, err
	}
	review, err := a.reviewStatusOf(ctx)
	if err != nil {
		return nil, err
	}
	cs, raw, err := a.comments(ctx, on.ID)
	if err != nil {
		return nil, err
	}
	var out []tracker.Decision
	var notes []string
	var evidence []json.RawMessage
	for i, c := range cs {
		when := at(c.CreatedAt)
		if !when.After(since) || c.PersonID == self {
			continue
		}
		if t := strings.TrimSpace(c.Text); t != "" {
			notes = append(notes, t)
		}
		for _, r := range c.Replies {
			if t := strings.TrimSpace(r.Text); t != "" && r.PersonID != self {
				notes = append(notes, "> "+t)
			}
		}
		evidence = append(evidence, raw[i])
		if c.TaskStatusID == review.ID {
			continue // a note on what is in review, not a decision
		}
		st, err := a.statusID(ctx, c.TaskStatusID)
		if err != nil {
			return nil, err
		}
		who, err := a.Person(ctx, c.PersonID)
		if err != nil {
			return nil, err
		}
		ev, _ := json.Marshal(map[string]any{"comments": evidence})
		out = append(out, tracker.Decision{
			On:     on,
			Source: tracker.Ref{Type: "Comment", ID: c.ID, URL: on.URL},
			By:     who, Native: "status:" + st.ShortName, Body: strings.Join(notes, "\n"), At: when, Evidence: ev,
		})
	}
	return out, nil
}

// Verdict is Kitsu's default decision for a status (tracker.Verdicter):
// by its flags, so a studio's renamed statuses keep their meaning.
func (a *Adapter) Verdict(native string) string {
	short, ok := strings.CutPrefix(native, "status:")
	if !ok {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.statuses {
		if s.ShortName != short {
			continue
		}
		switch {
		case s.IsDone:
			return "approve"
		case s.IsRetake:
			return "rework"
		}
	}
	return ""
}

// Receive: Kitsu pushes events only over Socket.IO, with no replay; the
// event log is read instead.
func (a *Adapter) Receive(*http.Request) (tracker.Delivery, error) {
	return tracker.Delivery{}, fmt.Errorf("kitsu: deliveries: %w", tracker.ErrUnsupported)
}

// --- writing ---

// keyLine is the line a comment carries its write key in, so Probe can
// find a comment written before a crash.
func keyLine(key string) string { return "[c4-sync " + key + "]" }

// hashtag is what Zou reads as a hashtag: a comment saying #lighting or
// #all is posted again on the entity's other tasks.
var hashtag = regexp.MustCompile(`#([A-Za-z])`)

// defang keeps text c4-sync writes from being read as hashtags (a full
// width number sign reads the same and is not one).
func defang(s string) string { return hashtag.ReplaceAllString(s, "＃$1") }

func withKey(body, key string) string {
	body = defang(body)
	if key != "" {
		body += "\n\n" + keyLine(key)
	}
	return body
}

// currentStatus is a Task's status id now.
func (a *Adapter) currentStatus(ctx context.Context, taskID string) (string, error) {
	var t task
	if err := a.do(ctx, http.MethodGet, "/data/tasks/"+url.PathEscape(taskID), nil, &t); err != nil {
		return "", err
	}
	return t.TaskStatusID, nil
}

// post comments on a Task with a status (the current one to leave it).
func (a *Adapter) post(ctx context.Context, taskID, statusID, text string) (comment, error) {
	var c comment
	err := a.do(ctx, http.MethodPost, "/actions/tasks/"+url.PathEscape(taskID)+"/comment",
		map[string]any{"task_status_id": statusID, "comment": text}, &c)
	return c, err
}

// Apply makes one write. Kinds:
//   - status: a comment on the Task that moves it to a status, by short
//     name (Fields["task_status"]) or flag (Fields["flag"]); none when the
//     Task is in that status already;
//   - update: keys of the Task's data, or of another entity's (a shot's
//     spend total), which Zou merges in; an empty ETA is cleared (null),
//     any other empty value is not written, and the spend is a number;
//   - comment, note: a comment on the Task in its current status, or with
//     Note.Replace, new text for that comment;
//   - mention: a comment that @-mentions Fields["user"] (an email), so
//     Kitsu notifies them.
func (a *Adapter) Apply(ctx context.Context, w tracker.Write) (tracker.Ref, error) {
	if w.On.Type != "Task" && w.Kind != "update" {
		return tracker.Ref{}, fmt.Errorf("kitsu: %s on %s: %w", w.Kind, w.On.Type, tracker.ErrUnsupported)
	}
	switch w.Kind {
	case "status":
		st, err := a.resolveStatus(ctx, w.Fields["task_status"], w.Fields["flag"])
		if err != nil {
			return tracker.Ref{}, err
		}
		cur, err := a.currentStatus(ctx, w.On.ID)
		if err != nil || cur == st.ID {
			return tracker.Ref{}, err
		}
		// A done Task is the supervisor's approval: c4-sync never moves
		// it, whatever the job does after.
		if now, err := a.statusID(ctx, cur); err != nil || now.IsDone {
			return tracker.Ref{}, err
		}
		body := ""
		if w.Note != nil {
			body = w.Note.Body
		}
		c, err := a.post(ctx, w.On.ID, st.ID, withKey(body, w.Key))
		if err != nil {
			return tracker.Ref{}, err
		}
		return tracker.Ref{Type: "Comment", ID: c.ID, URL: w.On.URL}, nil
	case "update":
		data := map[string]any{}
		for k, v := range w.Fields {
			switch {
			case v == "" && k == a.etaField:
				// a job no longer running has no ETA: clear it (Zou keeps
				// a key set to null), so a finished Task shows none
				data[k] = nil
			case v == "":
			case k == a.spendField:
				// one number, so a number descriptor sums and sorts
				n, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return tracker.Ref{}, fmt.Errorf("kitsu: spend %q: %w", v, err)
				}
				data[k] = n
			default:
				data[k] = v
			}
		}
		if len(data) == 0 {
			return tracker.Ref{}, nil // an empty data object would clear the Task's data
		}
		path := "/data/tasks/" + url.PathEscape(w.On.ID)
		if w.On.Type != "Task" {
			// a shot, an asset, a sequence: Zou's entities, data merged
			path = "/data/entities/" + url.PathEscape(w.On.ID)
		}
		if err := a.do(ctx, http.MethodPut, path, map[string]any{"data": data}, nil); err != nil {
			return tracker.Ref{}, err
		}
		return w.On, nil
	case "note", "comment", "mention":
		if w.Note == nil || w.Note.Body == "" {
			return tracker.Ref{}, errors.New("kitsu: comment without text")
		}
		body := w.Note.Body
		if w.Kind == "mention" {
			if w.Note.Replace != nil {
				return tracker.Ref{}, errors.New("kitsu: a mention is a new comment")
			}
			name, err := a.fullName(ctx, w.Fields["user"])
			if err != nil {
				return tracker.Ref{}, err
			}
			// Zou finds "@First Last" followed by a space or the end.
			body = "@" + name + " " + body
		}
		if r := w.Note.Replace; r != nil {
			if err := a.do(ctx, http.MethodPut, "/data/comments/"+url.PathEscape(r.ID), map[string]any{"text": withKey(body, w.Key)}, nil); err != nil {
				return tracker.Ref{}, err
			}
			return *r, nil
		}
		cur, err := a.currentStatus(ctx, w.On.ID)
		if err != nil {
			return tracker.Ref{}, err
		}
		c, err := a.post(ctx, w.On.ID, cur, withKey(body, w.Key))
		if err != nil {
			return tracker.Ref{}, err
		}
		return tracker.Ref{Type: "Comment", ID: c.ID, URL: w.On.URL}, nil
	}
	return tracker.Ref{}, fmt.Errorf("kitsu: write kind %q: %w", w.Kind, tracker.ErrUnsupported)
}

// fullName is the person with an email, as a mention names them.
func (a *Adapter) fullName(ctx context.Context, email string) (string, error) {
	if email == "" {
		return "", errors.New("kitsu: a mention names no user")
	}
	var ps []struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		FullName  string `json:"full_name"`
	}
	if err := a.do(ctx, http.MethodGet, "/data/persons?email="+url.QueryEscape(email), nil, &ps); err != nil {
		return "", err
	}
	if len(ps) == 0 {
		return "", fmt.Errorf("kitsu: no person with the email %q to mention", email)
	}
	if n := ps[0].FullName; n != "" {
		return n, nil
	}
	return strings.TrimSpace(ps[0].FirstName + " " + ps[0].LastName), nil
}

// viewLine is the line a published comment names its output view in;
// Publish finds an earlier publish of the same view by it.
func viewLine(view string) string { return "Output view: " + view }

// Publish comments on the Task in its review status with what the output
// is, its C4 IDs and links, and attaches the output's still as the
// comment's preview: a new revision, which Kitsu plays in review. A
// comment of the bot's that already names the view is the one a crash
// left: it is finished (its preview uploaded), not made again. The review
// is the Task itself.
func (a *Adapter) Publish(ctx context.Context, on tracker.Ref, out tracker.Output, key string) (tracker.Ref, error) {
	if on.Type != "Task" {
		return tracker.Ref{}, fmt.Errorf("kitsu: Publish on %s: %w", on.Type, tracker.ErrUnsupported)
	}
	self, err := a.selfID(ctx)
	if err != nil {
		return tracker.Ref{}, err
	}
	cs, _, err := a.comments(ctx, on.ID)
	if err != nil {
		return tracker.Ref{}, err
	}
	var c *comment
	for i := range cs {
		if cs[i].PersonID == self && strings.Contains(cs[i].Text, viewLine(out.View)) {
			c = &cs[i]
		}
	}
	if c == nil {
		st, err := a.reviewStatusOf(ctx)
		if err != nil {
			return tracker.Ref{}, err
		}
		text := fmt.Sprintf("%s.\nC4 job: %s\n%s\nProvenance: %s", out.Describe(), out.JobURL, viewLine(out.View), out.Custody)
		if out.Record != "" {
			text += "\nJob record: " + out.Record
		}
		made, err := a.post(ctx, on.ID, st.ID, withKey(text, key))
		if err != nil {
			return tracker.Ref{}, err
		}
		c = &made
	}
	if out.Preview != nil {
		if err := a.attach(ctx, on.ID, *c, *out.Preview); err != nil {
			return tracker.Ref{}, err
		}
	}
	if on.URL == "" {
		pid, err := a.productionID(ctx)
		if err != nil {
			return tracker.Ref{}, err
		}
		on.URL = a.TaskURL(pid, "", on.ID)
	}
	return on, nil
}

// attach gives a published comment its preview, once: a preview already
// uploaded is kept; one made and never uploaded (a crash between the two
// calls; Kitsu shows it processing forever) gets the upload.
func (a *Adapter) attach(ctx context.Context, taskID string, c comment, s tracker.Still) error {
	var target string
	for _, p := range c.Previews {
		if p.OriginalName != "" || p.Status == "ready" {
			return nil
		}
		target = p.ID
	}
	if target == "" {
		var p preview
		if err := a.do(ctx, http.MethodPost, "/actions/tasks/"+url.PathEscape(taskID)+"/comments/"+url.PathEscape(c.ID)+"/add-preview",
			map[string]any{}, &p); err != nil {
			return err
		}
		target = p.ID
	}
	f, err := fileForm("file", s.Name, s.Data)
	if err != nil {
		return err
	}
	return a.do(ctx, http.MethodPost, "/pictures/preview-files/"+url.PathEscape(target), f, nil)
}

// Guard: a data update and a comment edited in place are the same write
// sent again; a new comment, status or not, carries the key's line, which
// Probe finds.
func (a *Adapter) Guard(w tracker.Write) effect.Guard {
	switch w.Kind {
	case "update":
		return effect.Idempotent
	case "status", "note", "comment", "mention":
		if w.Note != nil && w.Note.Replace != nil {
			return effect.Idempotent
		}
		if w.Key != "" {
			return effect.Probed
		}
	}
	return effect.Unguarded
}

// Probe finds a comment already written under w.Key on the Task.
func (a *Adapter) Probe(ctx context.Context, w tracker.Write) (bool, tracker.Ref, error) {
	if w.Key == "" || w.Kind == "update" || (w.Note != nil && w.Note.Replace != nil) {
		return false, tracker.Ref{}, nil
	}
	cs, _, err := a.comments(ctx, w.On.ID)
	if err != nil {
		return false, tracker.Ref{}, err
	}
	for _, c := range cs {
		if strings.Contains(c.Text, keyLine(w.Key)) {
			return true, tracker.Ref{Type: "Comment", ID: c.ID, URL: w.On.URL}, nil
		}
	}
	return false, tracker.Ref{}, nil
}

// Person reads a person by id (or "Person/<id>"). Its ID is the email,
// which the Tracker's people map keys on, else the Kitsu id: a bot may
// have no email.
func (a *Adapter) Person(ctx context.Context, id string) (tracker.Person, error) {
	id = strings.TrimPrefix(id, "Person/")
	a.mu.Lock()
	p, ok := a.people[id]
	a.mu.Unlock()
	if ok {
		return p, nil
	}
	var r struct {
		ID        string `json:"id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		FullName  string `json:"full_name"`
		Email     string `json:"email"`
	}
	if err := a.do(ctx, http.MethodGet, "/data/persons/"+url.PathEscape(id), nil, &r); err != nil {
		return tracker.Person{}, err
	}
	p = tracker.Person{ID: strings.ToLower(r.Email), Name: or(r.FullName, strings.TrimSpace(r.FirstName+" "+r.LastName)), Email: r.Email}
	if p.ID == "" {
		p.ID = r.ID
	}
	a.mu.Lock()
	a.people[id] = p
	a.mu.Unlock()
	return p, nil
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
