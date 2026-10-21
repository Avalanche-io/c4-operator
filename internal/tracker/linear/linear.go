// Package linear is c4-sync's Linear adapter (docs/c4-sync.md). It speaks
// Linear's GraphQL API as an API key or an OAuth app actor, for one team.
//
// What it does in Linear:
//   - reads the team's issues as work: labels and the workflow state's
//     type select them, dueDate (a date with no time) is the deadline;
//   - writes work states only: the issue's workflow state, chosen by type
//     (the team's first started state while the work runs) or by name from
//     the Tracker's statusOut.states, never a completed or canceled one;
//     one attachment per job, the job's status card, updated in place; one
//     comment when a person is needed, edited in place;
//   - publishes each output as an attachment carrying the output view's C4
//     ID, by its provenance URL, so it is made once;
//   - reads decisions back from the issue's history: a person moving the
//     issue to another state after its review wait opened, with the
//     comments they wrote as its text.
//
// It never moves an issue to a completed or canceled state: that is the
// reviewer's.
//
// Scope keys: team (the team's key, ENG). The credential Secret holds a
// personal API key (lin_api_...) or an OAuth access token.
package linear

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/effect"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// Kind is the adapter's registry name.
const Kind = "linear"

// DefaultEndpoint is Linear's GraphQL API.
const DefaultEndpoint = "https://api.linear.app/graphql"

func init() {
	tracker.Register(Kind, func(c tracker.Config) (tracker.Adapter, error) { return New(c) })
}

// Adapter is the Linear adapter for one team.
type Adapter struct {
	c    tracker.Config
	base string
	team string
	hc   *http.Client
	now  func() time.Time

	mu     sync.Mutex
	teamID string
	states []State
	self   string
	people map[string]tracker.Person
}

// State is one of a team's workflow states.
type State struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"` // triage, backlog, unstarted, started, completed, canceled
	Position float64 `json:"position"`
}

// New builds the adapter.
func New(c tracker.Config) (*Adapter, error) {
	base := strings.TrimSuffix(c.Endpoint, "/")
	if base == "" {
		base = DefaultEndpoint
	}
	team := c.Scope["team"]
	if team == "" {
		return nil, errors.New("linear: scope.team (the team's key, such as ENG) is required")
	}
	a := &Adapter{c: c, base: base, team: team, hc: c.HTTPClient, now: c.Now, people: map[string]tracker.Person{}}
	if a.hc == nil {
		a.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

func (a *Adapter) Kind() string { return Kind }

func (a *Adapter) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{Webhooks: true, EditableComments: true, DateOnlyDue: true}
}

// --- GraphQL ---

type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
		Type string `json:"type"`
	} `json:"extensions"`
}

// gql runs one named operation. An answer with errors is a refusal (the
// API evaluated the request and declined it; a mutation that errs makes no
// change); a rate limit is *tracker.RateLimited; a 5xx or a broken
// connection may have followed the request, and says nothing more.
func (a *Adapter) gql(ctx context.Context, op, query string, vars map[string]any, out any) error {
	b, err := json.Marshal(map[string]any{"operationName": op, "query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.c.Token != nil {
		tok, err := a.c.Token(ctx)
		if err != nil {
			return fmt.Errorf("linear: credential: %w", err)
		}
		// A personal API key is sent as it is; an OAuth token as a bearer.
		if strings.HasPrefix(tok, "lin_api_") {
			req.Header.Set("Authorization", tok)
		} else {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("linear: %s: %w", op, err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	_ = json.Unmarshal(data, &res)
	if resp.StatusCode == http.StatusTooManyRequests || limited(res.Errors) {
		return &tracker.RateLimited{Until: a.reset(resp), Msg: errText(resp.Status, res.Errors)}
	}
	switch {
	case resp.StatusCode >= 500:
		return fmt.Errorf("linear: %s: %s", op, resp.Status)
	case resp.StatusCode >= 400:
		return fmt.Errorf("linear: %s: %s: %w", op, errText(resp.Status, res.Errors), tracker.ErrRefused)
	case len(res.Errors) > 0:
		if notFound(res.Errors) {
			return fmt.Errorf("linear: %s: %w: %s", op, tracker.ErrNotFound, errText("", res.Errors))
		}
		return fmt.Errorf("linear: %s: %s: %w", op, errText("", res.Errors), tracker.ErrRefused)
	}
	if out != nil {
		return json.Unmarshal(res.Data, out)
	}
	return nil
}

// limited: Linear answers a rate limit with HTTP 400 and RATELIMITED in
// the errors' extensions (tools/linear.md, rate limits).
func limited(es []gqlError) bool {
	for _, e := range es {
		if e.Extensions.Code == "RATELIMITED" || e.Extensions.Type == "ratelimited" {
			return true
		}
	}
	return false
}

func notFound(es []gqlError) bool {
	for _, e := range es {
		if e.Extensions.Code == "ENTITY_NOT_FOUND" || strings.Contains(strings.ToLower(e.Message), "entity not found") {
			return true
		}
	}
	return false
}

// reset is when the rate limit lifts: X-RateLimit-Requests-Reset (or the
// complexity one) is a time in milliseconds; without either, a minute.
func (a *Adapter) reset(resp *http.Response) time.Time {
	for _, h := range []string{"X-RateLimit-Requests-Reset", "X-RateLimit-Complexity-Reset"} {
		if ms, err := strconv.ParseInt(resp.Header.Get(h), 10, 64); err == nil && ms > 0 {
			if t := time.UnixMilli(ms); t.After(a.now()) {
				return t
			}
		}
	}
	return a.now().Add(time.Minute)
}

func errText(status string, es []gqlError) string {
	var parts []string
	for _, e := range es {
		parts = append(parts, e.Message)
	}
	if len(parts) == 0 {
		return status
	}
	return strings.Join(parts, "; ")
}

// --- the team ---

const qTeam = `query Team($key: String!) {
  teams(filter: {key: {eq: $key}}) {
    nodes { id key states(first: 100) { nodes { id name type position } } }
  }
}`

// teamStates reads the team's id and workflow states once, or again when
// asked (a state the Tracker names was not found).
func (a *Adapter) teamStates(ctx context.Context, again bool) (string, []State, error) {
	a.mu.Lock()
	id, st := a.teamID, a.states
	a.mu.Unlock()
	if id != "" && !again {
		return id, st, nil
	}
	var res struct {
		Teams struct {
			Nodes []struct {
				ID     string `json:"id"`
				Key    string `json:"key"`
				States struct {
					Nodes []State `json:"nodes"`
				} `json:"states"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := a.gql(ctx, "Team", qTeam, map[string]any{"key": a.team}, &res); err != nil {
		return "", nil, err
	}
	if len(res.Teams.Nodes) == 0 {
		return "", nil, fmt.Errorf("linear: no team with key %q: %w", a.team, tracker.ErrNotFound)
	}
	t := res.Teams.Nodes[0]
	st = t.States.Nodes
	sort.SliceStable(st, func(i, j int) bool { return st[i].Position < st[j].Position })
	a.mu.Lock()
	a.teamID, a.states = t.ID, st
	a.mu.Unlock()
	return t.ID, st, nil
}

// stateFor is the state a status write means: by name, or the first (by
// position) of a type. A completed or canceled state is never c4-sync's to
// set.
func (a *Adapter) stateFor(ctx context.Context, name, typ string) (State, error) {
	for again := false; ; again = true {
		_, states, err := a.teamStates(ctx, again)
		if err != nil {
			return State{}, err
		}
		for _, s := range states {
			if (name != "" && s.Name == name) || (name == "" && s.Type == typ) {
				if s.Type == "completed" || s.Type == "canceled" {
					return State{}, fmt.Errorf("linear: state %q is %s: that is the reviewer's to set, never c4-sync's", s.Name, s.Type)
				}
				return s, nil
			}
		}
		if again {
			if name != "" {
				return State{}, fmt.Errorf("linear: team %s has no workflow state named %q", a.team, name)
			}
			return State{}, fmt.Errorf("linear: team %s has no workflow state of type %s", a.team, typ)
		}
	}
}

const qViewer = `query Viewer { viewer { id } }`

// viewer is c4-sync's own user (an app user for an OAuth app actor): its
// changes are echoes, never decisions.
func (a *Adapter) viewer(ctx context.Context) (string, error) {
	a.mu.Lock()
	id := a.self
	a.mu.Unlock()
	if id != "" {
		return id, nil
	}
	if a.c.Self != "" {
		id = a.c.Self
	} else {
		var res struct {
			Viewer struct {
				ID string `json:"id"`
			} `json:"viewer"`
		}
		if err := a.gql(ctx, "Viewer", qViewer, nil, &res); err != nil {
			return "", err
		}
		id = res.Viewer.ID
	}
	a.mu.Lock()
	a.self = id
	a.mu.Unlock()
	return id, nil
}

// --- reading ---

const issueFields = `id identifier title description url dueDate updatedAt estimate
  state { id name type } assignee { id name email displayName }
  labels(first: 20) { nodes { name } } team { key } project { name }`

type gUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

func (u gUser) person() tracker.Person {
	return tracker.Person{ID: or(u.Email, u.ID), Name: or(u.Name, u.DisplayName), Email: u.Email}
}

type gIssue struct {
	ID          string   `json:"id"`
	Identifier  string   `json:"identifier"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	URL         string   `json:"url"`
	DueDate     *string  `json:"dueDate"`
	UpdatedAt   string   `json:"updatedAt"`
	Estimate    *float64 `json:"estimate"`
	State       State    `json:"state"`
	Assignee    *gUser   `json:"assignee"`
	Labels      struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Team struct {
		Key string `json:"key"`
	} `json:"team"`
	Project *struct {
		Name string `json:"name"`
	} `json:"project"`
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// category is the loop's reading of a workflow state's type.
func category(typ string) string {
	return map[string]string{"triage": "planned", "backlog": "planned", "unstarted": "ready", "started": "active",
		"completed": "done", "canceled": "omitted"}[typ]
}

func (i gIssue) item() tracker.Item {
	it := tracker.Item{
		Ref:    tracker.Ref{Type: "Issue", ID: i.ID, URL: i.URL},
		Title:  i.Title,
		Body:   i.Description,
		Status: tracker.Status{Native: i.State.Name, Category: category(i.State.Type)},
		Fields: map[string]string{"identifier": i.Identifier, "state": i.State.Name, "stateType": i.State.Type, "team": i.Team.Key},
	}
	// The identifier (eng-12) is unique in its team, a Linear Tracker's
	// scope. An issue moved to another team is given a new one; its link
	// keeps the job's name.
	it.Name, it.NameUnique = strings.ToLower(i.Identifier), true
	if i.DueDate != nil {
		if t, err := time.Parse("2006-01-02", *i.DueDate); err == nil {
			it.Due = &tracker.Due{At: t, DateOnly: true}
		}
	}
	if t, err := time.Parse(time.RFC3339, i.UpdatedAt); err == nil {
		it.UpdatedAt, it.Version = t, i.UpdatedAt
	}
	if i.Assignee != nil {
		it.Assignees = []tracker.Person{i.Assignee.person()}
		it.Fields["assignee"] = i.Assignee.person().ID
	}
	for _, l := range i.Labels.Nodes {
		it.Labels = append(it.Labels, l.Name)
	}
	if i.Project != nil {
		it.Fields["project"] = i.Project.Name
	}
	// The estimate (points or hours, as the team counts): a forecast's
	// size, with perUnit saying how many slot-hours one is.
	if i.Estimate != nil && *i.Estimate > 0 {
		it.Fields["estimate"] = strconv.FormatFloat(*i.Estimate, 'f', -1, 64)
	}
	return it
}

const qIssue = `query Issue($id: String!) { issue(id: $id) { ` + issueFields + ` } }`

func (a *Adapter) Get(ctx context.Context, ref tracker.Ref) (tracker.Item, error) {
	if ref.Type != "Issue" {
		return tracker.Item{}, fmt.Errorf("linear: Get %s: %w", ref.Type, tracker.ErrUnsupported)
	}
	var res struct {
		Issue *gIssue `json:"issue"`
	}
	if err := a.gql(ctx, "Issue", qIssue, map[string]any{"id": ref.ID}, &res); err != nil {
		return tracker.Item{}, err
	}
	if res.Issue == nil {
		return tracker.Item{}, fmt.Errorf("linear: issue %s: %w", ref.ID, tracker.ErrNotFound)
	}
	return res.Issue.item(), nil
}

const qIssues = `query Issues($filter: IssueFilter, $after: String) {
  issues(filter: $filter, first: 50, after: $after, orderBy: updatedAt) {
    nodes { ` + issueFields + ` }
    pageInfo { hasNextPage endCursor }
  }
}`

type issuePage struct {
	Issues struct {
		Nodes    []gIssue `json:"nodes"`
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
	} `json:"issues"`
}

// fieldFilters are the intake match fields List takes, in Linear's own
// names, each an equality filter.
var fieldFilters = map[string]func(v string) map[string]any{
	"state": func(v string) map[string]any {
		return map[string]any{"state": map[string]any{"name": map[string]any{"eq": v}}}
	},
	"stateType": func(v string) map[string]any {
		return map[string]any{"state": map[string]any{"type": map[string]any{"eq": v}}}
	},
	"project": func(v string) map[string]any {
		return map[string]any{"project": map[string]any{"name": map[string]any{"eq": v}}}
	},
	"assignee": func(v string) map[string]any {
		return map[string]any{"assignee": map[string]any{"email": map[string]any{"eq": v}}}
	},
}

// List finds the team's issues carrying every label, in the state asked
// (open: neither completed nor canceled, the default; closed; all), and
// matching q.Fields: state (a state's name), stateType, project (a
// project's name), assignee (an email).
func (a *Adapter) List(ctx context.Context, q tracker.Query, page string) ([]tracker.Item, string, error) {
	if q.Type != "Issue" {
		return nil, "", fmt.Errorf("linear: List %s: %w", q.Type, tracker.ErrUnsupported)
	}
	and := []any{map[string]any{"team": map[string]any{"key": map[string]any{"eq": a.team}}}}
	for _, l := range q.Labels {
		and = append(and, map[string]any{"labels": map[string]any{"some": map[string]any{"name": map[string]any{"eq": l}}}})
	}
	closed := []string{"completed", "canceled"}
	switch q.State {
	case "", "open":
		and = append(and, map[string]any{"state": map[string]any{"type": map[string]any{"nin": closed}}})
	case "closed":
		and = append(and, map[string]any{"state": map[string]any{"type": map[string]any{"in": closed}}})
	case "all":
	default:
		return nil, "", fmt.Errorf("linear: intake state %q: want open, closed or all", q.State)
	}
	keys := make([]string, 0, len(q.Fields))
	for k := range q.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f, ok := fieldFilters[k]
		if !ok {
			return nil, "", fmt.Errorf("linear: intake field %q: Linear matches state, stateType, project and assignee", k)
		}
		and = append(and, f(q.Fields[k]))
	}
	vars := map[string]any{"filter": map[string]any{"and": and}}
	if page != "" {
		vars["after"] = page
	}
	var res issuePage
	if err := a.gql(ctx, "Issues", qIssues, vars, &res); err != nil {
		return nil, "", err
	}
	out := make([]tracker.Item, 0, len(res.Issues.Nodes))
	for _, i := range res.Issues.Nodes {
		out = append(out, i.item())
	}
	next := ""
	if res.Issues.PageInfo.HasNextPage {
		next = res.Issues.PageInfo.EndCursor
	}
	return out, next, nil
}

// changePages bounds what one Changes call reads; the rest is read next
// pass, from the cursor it reached.
const changePages = 10

// Changes finds the team's issues updated after the cursor (an RFC 3339
// time), oldest first. Linear keeps no event log to replay, so updatedAt is
// the cursor, polled under the webhooks; with no cursor it starts now, and
// intake and resync cover what came before. Issues updated in the same
// instant as the cursor are caught by resync.
func (a *Adapter) Changes(ctx context.Context, cursor string) ([]tracker.Ref, string, error) {
	if cursor == "" {
		return nil, a.now().UTC().Format(time.RFC3339Nano), nil
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor); err != nil {
		return nil, cursor, fmt.Errorf("linear: bad cursor %q", cursor)
	}
	filter := map[string]any{"and": []any{
		map[string]any{"team": map[string]any{"key": map[string]any{"eq": a.team}}},
		map[string]any{"updatedAt": map[string]any{"gt": cursor}},
	}}
	var refs []tracker.Ref
	last, after := cursor, ""
	for p := 0; p < changePages; p++ {
		vars := map[string]any{"filter": filter}
		if after != "" {
			vars["after"] = after
		}
		var res issuePage
		if err := a.gql(ctx, "Issues", qIssues, vars, &res); err != nil {
			return nil, cursor, err
		}
		for _, i := range res.Issues.Nodes {
			refs = append(refs, tracker.Ref{Type: "Issue", ID: i.ID, URL: i.URL})
			if later(i.UpdatedAt, last) {
				last = i.UpdatedAt
			}
		}
		if !res.Issues.PageInfo.HasNextPage {
			break
		}
		after = res.Issues.PageInfo.EndCursor
	}
	return refs, last, nil
}

func later(a, b string) bool {
	ta, err1 := time.Parse(time.RFC3339Nano, a)
	tb, err2 := time.Parse(time.RFC3339Nano, b)
	return err1 == nil && err2 == nil && ta.After(tb)
}

const qHistory = `query History($id: String!, $after: String) {
  issue(id: $id) {
    url
    history(first: 50, after: $after) {
      nodes { id createdAt actor { id name email displayName } fromState { id name type } toState { id name type } }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

type gHistory struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
	Actor     *gUser `json:"actor"`
	FromState *State `json:"fromState"`
	ToState   *State `json:"toState"`
}

// Decisions reads, from the issue's history, people's moves of the issue
// to another workflow state after since; each is a decision named
// state:<the state's name>, with the comments its author wrote on the
// issue since then as its text. Moves made by c4-sync's own user, and by
// automations (no actor), are not decisions.
func (a *Adapter) Decisions(ctx context.Context, on tracker.Ref, since time.Time) ([]tracker.Decision, error) {
	if on.Type != "Issue" {
		return nil, fmt.Errorf("linear: Decisions on %s: %w", on.Type, tracker.ErrUnsupported)
	}
	self, err := a.viewer(ctx)
	if err != nil {
		return nil, err
	}
	// Verdict reads the states' types: have them before any decision is
	// mapped, a restart included.
	if _, _, err := a.teamStates(ctx, false); err != nil {
		return nil, err
	}
	var hs []gHistory
	url := on.URL
	for after := ""; ; {
		vars := map[string]any{"id": on.ID}
		if after != "" {
			vars["after"] = after
		}
		var res struct {
			Issue *struct {
				URL     string `json:"url"`
				History struct {
					Nodes    []gHistory `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"history"`
			} `json:"issue"`
		}
		if err := a.gql(ctx, "History", qHistory, vars, &res); err != nil {
			return nil, err
		}
		if res.Issue == nil {
			return nil, fmt.Errorf("linear: issue %s: %w", on.ID, tracker.ErrNotFound)
		}
		url = or(url, res.Issue.URL)
		hs = append(hs, res.Issue.History.Nodes...)
		if !res.Issue.History.PageInfo.HasNextPage {
			break
		}
		after = res.Issue.History.PageInfo.EndCursor
	}
	var comments []gComment
	var out []tracker.Decision
	for _, h := range hs {
		at, err := time.Parse(time.RFC3339Nano, h.CreatedAt)
		if err != nil || !at.After(since) || h.ToState == nil || h.Actor == nil || h.Actor.ID == self {
			continue
		}
		if comments == nil {
			if comments, err = a.comments(ctx, on.ID); err != nil {
				return nil, err
			}
		}
		var b strings.Builder
		var said []gComment
		for _, c := range comments {
			ct, _ := time.Parse(time.RFC3339Nano, c.CreatedAt)
			if c.User == nil || c.User.ID != h.Actor.ID || !ct.After(since) {
				continue
			}
			said = append(said, c)
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(strings.TrimSpace(c.Body))
		}
		ev, _ := json.Marshal(map[string]any{"history": h, "comments": said})
		out = append(out, tracker.Decision{
			On:     on,
			Source: tracker.Ref{Type: "IssueHistory", ID: h.ID, URL: url},
			By:     h.Actor.person(), Native: "state:" + h.ToState.Name, Body: b.String(), At: at, Evidence: ev,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Verdict is the decision a move to a state means where the Tracker's
// verdictsIn names none (tracker.Verdicter): a completed-type state, by
// whatever name the team gives it, approves. Nothing else decides by
// default; a team's "Changes requested" is mapped in verdictsIn.
func (a *Adapter) Verdict(native string) string {
	name, ok := strings.CutPrefix(native, "state:")
	if !ok {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.states {
		if s.Name == name && s.Type == "completed" {
			return "approve"
		}
	}
	return ""
}

// FindReviews: what is reviewed in Linear is the issue itself (there is
// no pull request), so each link's review is its own issue, once it has a
// job.
func (a *Adapter) FindReviews(_ context.Context, links []tracker.LinkState) (map[string]tracker.Ref, error) {
	out := map[string]tracker.Ref{}
	for _, l := range links {
		if l.Job != "" && l.Ref.Type == "Issue" {
			out[l.Link] = l.Ref
		}
	}
	return out, nil
}

const qComments = `query Comments($id: String!, $after: String) {
  issue(id: $id) {
    comments(first: 100, after: $after) {
      nodes { id body createdAt user { id name email displayName } }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

type gComment struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	User      *gUser `json:"user"`
}

func (a *Adapter) comments(ctx context.Context, issue string) ([]gComment, error) {
	var out []gComment
	for after := ""; ; {
		vars := map[string]any{"id": issue}
		if after != "" {
			vars["after"] = after
		}
		var res struct {
			Issue *struct {
				Comments struct {
					Nodes    []gComment `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"comments"`
			} `json:"issue"`
		}
		if err := a.gql(ctx, "Comments", qComments, vars, &res); err != nil {
			return nil, err
		}
		if res.Issue == nil {
			return nil, fmt.Errorf("linear: issue %s: %w", issue, tracker.ErrNotFound)
		}
		out = append(out, res.Issue.Comments.Nodes...)
		if !res.Issue.Comments.PageInfo.HasNextPage {
			return out, nil
		}
		after = res.Issue.Comments.PageInfo.EndCursor
	}
}

// --- webhooks ---

// skew is how far a delivery's webhookTimestamp may be from now: Linear
// asks receivers to refuse one more than about a minute off, so a captured
// delivery cannot be replayed later.
const skew = time.Minute

// Receive verifies Linear-Signature (hex HMAC-SHA256 of the raw body with
// the webhook's secret) and the delivery's timestamp, and names the issue
// it concerns (an issue, or a comment's or attachment's issue). Deliveries
// are hints: the loop reads the issue under them.
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
		if len(secret) > 0 && !verify(body, secret, r.Header.Get("Linear-Signature")) {
			return tracker.Delivery{}, tracker.ErrBadSignature
		}
	}
	var p struct {
		Type             string `json:"type"`
		Action           string `json:"action"`
		WebhookTimestamp int64  `json:"webhookTimestamp"`
		Data             struct {
			ID      string `json:"id"`
			IssueID string `json:"issueId"`
		} `json:"data"`
		Actor *struct {
			ID string `json:"id"`
		} `json:"actor"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return tracker.Delivery{}, fmt.Errorf("linear: delivery: %w", err)
	}
	if d := a.now().Sub(time.UnixMilli(p.WebhookTimestamp)); d > skew || d < -skew {
		return tracker.Delivery{}, fmt.Errorf("%w: its timestamp is %s off", tracker.ErrBadSignature, d.Round(time.Second))
	}
	d := tracker.Delivery{ID: r.Header.Get("Linear-Delivery")}
	switch {
	case p.Type == "Issue" && p.Data.ID != "":
		d.Refs = []tracker.Ref{{Type: "Issue", ID: p.Data.ID}}
	case p.Data.IssueID != "":
		d.Refs = []tracker.Ref{{Type: "Issue", ID: p.Data.IssueID}}
	}
	a.mu.Lock()
	self := or(a.self, a.c.Self)
	a.mu.Unlock()
	d.Self = p.Actor != nil && self != "" && p.Actor.ID == self
	return d, nil
}

func verify(body, secret []byte, header string) bool {
	got, err := hex.DecodeString(header)
	if err != nil || len(got) == 0 {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// --- writing ---

// keyLine is the line a comment carries its write key in, so Probe can
// find a comment written before a crash.
func keyLine(key string) string { return "[c4-sync " + key + "]" }

const (
	mIssueUpdate = `mutation IssueUpdate($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) { success issue { id } }
}`
	mCommentCreate = `mutation CommentCreate($input: CommentCreateInput!) {
  commentCreate(input: $input) { success comment { id url } }
}`
	mCommentUpdate = `mutation CommentUpdate($id: String!, $input: CommentUpdateInput!) {
  commentUpdate(id: $id, input: $input) { success comment { id url } }
}`
	mAttachmentCreate = `mutation AttachmentCreate($input: AttachmentCreateInput!) {
  attachmentCreate(input: $input) { success attachment { id url } }
}`
)

// Apply makes one write. Kinds:
//   - status: the issue's workflow state, Fields["state"] (a name) or
//     Fields["stateType"] (the first state of that type);
//   - attachment: an attachment on the issue by Fields["url"], with title,
//     subtitle and metadata (JSON); the same URL updates it in place;
//   - comment: a comment on the issue, or with Note.Replace, new text for
//     that comment.
func (a *Adapter) Apply(ctx context.Context, w tracker.Write) (tracker.Ref, error) {
	switch w.Kind {
	case "status":
		s, err := a.stateFor(ctx, w.Fields["state"], w.Fields["stateType"])
		if err != nil {
			return tracker.Ref{}, err
		}
		// An issue a person moved to a completed or canceled state stays
		// there: that was the reviewer's word, and a work state written
		// after it (the job finishing up) does not take it back.
		cur, err := a.Get(ctx, w.On)
		if err != nil {
			return tracker.Ref{}, err
		}
		if t := cur.Fields["stateType"]; t == "completed" || t == "canceled" || cur.Fields["state"] == s.Name {
			return w.On, nil
		}
		var res struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		}
		if err := a.gql(ctx, "IssueUpdate", mIssueUpdate, map[string]any{"id": w.On.ID, "input": map[string]any{"stateId": s.ID}}, &res); err != nil {
			return tracker.Ref{}, err
		}
		if !res.IssueUpdate.Success {
			return tracker.Ref{}, fmt.Errorf("linear: moving issue %s to %s: not done: %w", w.On.ID, s.Name, tracker.ErrRefused)
		}
		return w.On, nil
	case "attachment":
		return a.attach(ctx, w.On.ID, w.Fields)
	case "comment":
		if w.Note == nil || w.Note.Body == "" {
			return tracker.Ref{}, errors.New("linear: comment without text")
		}
		body := w.Note.Body
		if w.Key != "" {
			body += "\n\n" + keyLine(w.Key)
		}
		var out struct {
			CommentCreate *commentPayload `json:"commentCreate"`
			CommentUpdate *commentPayload `json:"commentUpdate"`
		}
		var err error
		if r := w.Note.Replace; r != nil {
			err = a.gql(ctx, "CommentUpdate", mCommentUpdate, map[string]any{"id": r.ID, "input": map[string]any{"body": body}}, &out)
		} else {
			err = a.gql(ctx, "CommentCreate", mCommentCreate, map[string]any{"input": map[string]any{"issueId": w.On.ID, "body": body}}, &out)
		}
		if err != nil {
			return tracker.Ref{}, err
		}
		p := out.CommentCreate
		if p == nil {
			p = out.CommentUpdate
		}
		if p == nil || !p.Success {
			return tracker.Ref{}, fmt.Errorf("linear: comment on issue %s: not done: %w", w.On.ID, tracker.ErrRefused)
		}
		return tracker.Ref{Type: "Comment", ID: p.Comment.ID, URL: p.Comment.URL}, nil
	}
	return tracker.Ref{}, fmt.Errorf("linear: write kind %q: %w", w.Kind, tracker.ErrUnsupported)
}

type commentPayload struct {
	Success bool `json:"success"`
	Comment struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"comment"`
}

// attach makes or updates the issue's attachment at f["url"].
func (a *Adapter) attach(ctx context.Context, issue string, f map[string]string) (tracker.Ref, error) {
	if f["url"] == "" || f["title"] == "" {
		return tracker.Ref{}, errors.New("linear: an attachment needs a url and a title")
	}
	in := map[string]any{"issueId": issue, "url": f["url"], "title": f["title"]}
	if s := f["subtitle"]; s != "" {
		in["subtitle"] = s
	}
	if m := f["metadata"]; m != "" {
		var md map[string]any
		if err := json.Unmarshal([]byte(m), &md); err != nil {
			return tracker.Ref{}, fmt.Errorf("linear: attachment metadata: %w", err)
		}
		in["metadata"] = md
	}
	var res struct {
		AttachmentCreate *struct {
			Success    bool `json:"success"`
			Attachment struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"attachment"`
		} `json:"attachmentCreate"`
	}
	if err := a.gql(ctx, "AttachmentCreate", mAttachmentCreate, map[string]any{"input": in}, &res); err != nil {
		return tracker.Ref{}, err
	}
	if res.AttachmentCreate == nil || !res.AttachmentCreate.Success {
		return tracker.Ref{}, fmt.Errorf("linear: attachment on issue %s: not done: %w", issue, tracker.ErrRefused)
	}
	return tracker.Ref{Type: "Attachment", ID: res.AttachmentCreate.Attachment.ID, URL: res.AttachmentCreate.Attachment.URL}, nil
}

// Publish puts the output on the issue as an attachment at its provenance
// URL, with the output view's C4 ID in its metadata. Linear keys an
// attachment by its URL on the issue, so publishing again (after a crash)
// updates the same one: it is made once.
func (a *Adapter) Publish(ctx context.Context, on tracker.Ref, out tracker.Output, key string) (tracker.Ref, error) {
	if on.Type != "Issue" {
		return tracker.Ref{}, fmt.Errorf("linear: Publish on %s: %w", on.Type, tracker.ErrUnsupported)
	}
	md, _ := json.Marshal(map[string]any{"c4Id": out.View, "record": out.Record, "job": out.Name, "jobUrl": out.JobURL,
		"version": out.Version, "follows": out.Follows, "key": key})
	url := out.Custody
	if url == "" {
		url = out.JobURL + "#output=" + out.View
	}
	return a.attach(ctx, on.ID, map[string]string{"url": url, "title": fmt.Sprintf("C4 output, version %d", max(out.Version, 1)),
		"subtitle": out.Describe() + " · " + out.View, "metadata": string(md)})
}

// Guard: a state change and an attachment (keyed by its URL) are the same
// write sent again, and so is a comment edited in place; a new comment
// carries the key's line, which Probe finds.
func (a *Adapter) Guard(w tracker.Write) effect.Guard {
	switch w.Kind {
	case "status", "attachment":
		return effect.Idempotent
	case "comment":
		if w.Note != nil && w.Note.Replace != nil {
			return effect.Idempotent
		}
		if w.Key != "" {
			return effect.Probed
		}
	}
	return effect.Unguarded
}

// Probe finds a comment already written under w.Key.
func (a *Adapter) Probe(ctx context.Context, w tracker.Write) (bool, tracker.Ref, error) {
	if w.Kind != "comment" || w.Key == "" || w.Note == nil || w.Note.Replace != nil {
		return false, tracker.Ref{}, nil
	}
	cs, err := a.comments(ctx, w.On.ID)
	if err != nil {
		return false, tracker.Ref{}, err
	}
	for _, c := range cs {
		if strings.Contains(c.Body, keyLine(w.Key)) {
			return true, tracker.Ref{Type: "Comment", ID: c.ID}, nil
		}
	}
	return false, tracker.Ref{}, nil
}

const qUser = `query User($id: String!) { user(id: $id) { id name email displayName } }`

// Person reads a user by id. A person's ID is their email, which the
// Tracker's people map keys on.
func (a *Adapter) Person(ctx context.Context, id string) (tracker.Person, error) {
	a.mu.Lock()
	p, ok := a.people[id]
	a.mu.Unlock()
	if ok {
		return p, nil
	}
	var res struct {
		User *gUser `json:"user"`
	}
	if err := a.gql(ctx, "User", qUser, map[string]any{"id": id}, &res); err != nil {
		return tracker.Person{}, err
	}
	if res.User == nil {
		return tracker.Person{}, fmt.Errorf("linear: user %s: %w", id, tracker.ErrNotFound)
	}
	p = res.User.person()
	a.mu.Lock()
	a.people[id] = p
	a.mu.Unlock()
	return p, nil
}
