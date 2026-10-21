// Package mlflow is c4-sync's MLflow adapter (docs/c4-sync.md). It speaks
// the REST API 2.0 of an MLflow tracking server and model registry.
//
// What it does in MLflow:
//   - reads runs a client created SCHEDULED and tagged c4.intake=true as
//     work (the project backend plugin, `mlflow run --backend c4`, makes
//     these); a run's c4.deadline tag (RFC 3339) is its due time. MLflow
//     has no deadline of its own, so without the tag
//     the deadline comes from a Milestone or the farm's suggestion;
//   - writes work states to the run: its status (SCHEDULED, RUNNING,
//     FINISHED, FAILED: MLflow's own enum) and tags c4.state, c4.eta,
//     c4.job, c4.why;
//   - publishes each output as a model version of the registered model,
//     source c4:///<view> (or the prefix the scope names), tagged with the
//     output view's C4 ID, the job and the provenance link. A server
//     refuses c4://<view>, whose path is empty, and accepts c4:///<view>
//     (measured against MLflow 3.17.1: integrations/mlflow-c4);
//   - reads decisions back from the registry: the approve alias (default
//     production) pointing at the version, or a review tag (default
//     c4.review: approve, rework or accept, with c4.note as its text).
//
// MLflow records no author for an alias or a tag. A decision read from
// the registry is therefore attributed to the registry itself, as person
// "mlflow-registry", which the Tracker's people map names (for example
// to ml-leads), and serves a party wait (party:ml-leads). Who may set the
// alias is the registry's own permission (MANAGE in the basic-auth app);
// the relayed record says so. It never sets the approve alias.
//
// Scope keys: experiment (id), model (the registered model), user (basic
// auth; the credential Secret holds the password, or a bearer token when
// user is empty), approve_alias, review_tag, source_prefix.
package mlflow

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/effect"
	"github.com/Avalanche-io/c4-operator/internal/tracker"
)

// Kind is the adapter's registry name.
const Kind = "mlflow"

// Registry is the person a registry decision is attributed to.
const Registry = "mlflow-registry"

const apiBase = "/api/2.0/mlflow/"

func init() {
	tracker.Register(Kind, func(c tracker.Config) (tracker.Adapter, error) { return New(c) })
}

// Adapter is the MLflow adapter for one experiment and registered model.
type Adapter struct {
	c                                              tracker.Config
	base, experiment, model, user                  string
	approveAlias, reviewTag, sourcePrefix, noteTag string
	hc                                             *http.Client
	now                                            func() time.Time
}

// New builds the adapter.
func New(c tracker.Config) (*Adapter, error) {
	base := strings.TrimSuffix(c.Endpoint, "/")
	if base == "" {
		return nil, errors.New("mlflow: endpoint (the tracking server URL) is required")
	}
	if c.Scope["experiment"] == "" || c.Scope["model"] == "" {
		return nil, errors.New("mlflow: scope.experiment and scope.model are required")
	}
	a := &Adapter{c: c, base: base, experiment: c.Scope["experiment"], model: c.Scope["model"], user: c.Scope["user"],
		approveAlias: or(c.Scope["approve_alias"], "production"), reviewTag: or(c.Scope["review_tag"], "c4.review"),
		sourcePrefix: or(c.Scope["source_prefix"], "c4:///"), noteTag: "c4.note", hc: c.HTTPClient, now: c.Now}
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

// Capabilities: MLflow has no event log and no run events; its webhooks
// (3.3+, experimental) cover the registry only and are hints.
func (a *Adapter) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{Webhooks: true, PublishForReview: true}
}

// --- HTTP ---

func (a *Adapter) do(ctx context.Context, method, path string, q url.Values, in, out any) error {
	u := a.base + apiBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.c.Token != nil {
		secret, err := a.c.Token(ctx)
		if err != nil {
			return fmt.Errorf("mlflow: credential: %w", err)
		}
		if a.user != "" {
			req.SetBasicAuth(a.user, secret)
		} else if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("mlflow: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Code string `json:"error_code"`
			Msg  string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		msg := strings.TrimSpace(e.Code + ": " + e.Msg)
		if e.Code == "" {
			msg = strings.TrimSpace(resp.Status + " " + string(data))
		}
		switch {
		case resp.StatusCode == http.StatusNotFound || e.Code == "RESOURCE_DOES_NOT_EXIST":
			return fmt.Errorf("%w: %s", tracker.ErrNotFound, msg)
		case resp.StatusCode == http.StatusTooManyRequests || e.Code == "REQUEST_LIMIT_EXCEEDED":
			// Databricks publishes per-endpoint limits; open-source MLflow
			// has none.
			wait := time.Minute
			if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				wait = time.Duration(n) * time.Second
			}
			return &tracker.RateLimited{Until: a.now().Add(wait), Msg: msg}
		}
		return fmt.Errorf("mlflow: %s %s: %s", method, path, msg)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type run struct {
	Info struct {
		RunID        string `json:"run_id"`
		RunName      string `json:"run_name"`
		Status       string `json:"status"`
		ExperimentID string `json:"experiment_id"`
		StartTime    int64  `json:"start_time"`
		UserID       string `json:"user_id"`
	} `json:"info"`
	Data struct {
		Tags []tag `json:"tags"`
	} `json:"data"`
}

type modelVersion struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Source      string   `json:"source"`
	RunID       string   `json:"run_id"`
	Updated     int64    `json:"last_updated_timestamp"`
	Tags        []tag    `json:"tags"`
	Aliases     []string `json:"aliases"`
	Description string   `json:"description"`
}

func tags(ts []tag) map[string]string {
	m := map[string]string{}
	for _, t := range ts {
		m[t.Key] = t.Value
	}
	return m
}

func (a *Adapter) runItem(r run) tracker.Item {
	tg := tags(r.Data.Tags)
	it := tracker.Item{
		Ref:    tracker.Ref{Type: "Run", ID: r.Info.RunID, URL: fmt.Sprintf("%s/#/experiments/%s/runs/%s", a.base, r.Info.ExperimentID, r.Info.RunID)},
		Title:  r.Info.RunName,
		Status: tracker.Status{Native: r.Info.Status},
		Fields: tg,
		// MLflow lets run names repeat: the job's name tells them apart.
		Name: r.Info.RunName,
	}
	it.Job = tg["c4.job"]
	if r.Info.UserID != "" {
		// MLflow names no assignee; the run's author is whose work it is.
		it.Assignees = []tracker.Person{{ID: r.Info.UserID}}
	}
	if d := tg["c4.deadline"]; d != "" {
		if t, err := time.Parse(time.RFC3339, d); err == nil {
			it.Due = &tracker.Due{At: t.UTC()}
		}
	}
	return it
}

func mvRef(name, version string) tracker.Ref {
	return tracker.Ref{Type: "ModelVersion", ID: name + "/" + version}
}

func splitMV(id string) (string, string, error) {
	i := strings.LastIndex(id, "/")
	if i <= 0 || i == len(id)-1 {
		return "", "", fmt.Errorf("mlflow: model version ref %q: want name/version", id)
	}
	return id[:i], id[i+1:], nil
}

// --- reading ---

func (a *Adapter) Get(ctx context.Context, ref tracker.Ref) (tracker.Item, error) {
	switch ref.Type {
	case "Run":
		var res struct {
			Run run `json:"run"`
		}
		if err := a.do(ctx, http.MethodGet, "runs/get", url.Values{"run_id": {ref.ID}}, nil, &res); err != nil {
			return tracker.Item{}, err
		}
		return a.runItem(res.Run), nil
	case "ModelVersion":
		mv, err := a.version(ctx, ref.ID)
		if err != nil {
			return tracker.Item{}, err
		}
		return tracker.Item{Ref: mvRef(mv.Name, mv.Version), Title: mv.Name + " v" + mv.Version, Fields: tags(mv.Tags)}, nil
	}
	return tracker.Item{}, fmt.Errorf("mlflow: Get %s: %w", ref.Type, tracker.ErrUnsupported)
}

func (a *Adapter) version(ctx context.Context, id string) (modelVersion, error) {
	name, ver, err := splitMV(id)
	if err != nil {
		return modelVersion{}, err
	}
	var res struct {
		MV modelVersion `json:"model_version"`
	}
	err = a.do(ctx, http.MethodGet, "model-versions/get", url.Values{"name": {name}, "version": {ver}}, nil, &res)
	return res.MV, err
}

// filterValue quotes a value for an MLflow filter string; values with a
// quote are refused rather than escaped, since the grammar has no escape
// documented.
func filterValue(v string) (string, error) {
	if strings.ContainsAny(v, "'\\") {
		return "", fmt.Errorf("mlflow: filter value %q has a quote or backslash", v)
	}
	return "'" + v + "'", nil
}

// List searches the experiment's runs. q.Fields are equality filters:
// "status" is the run's status, any other key is a tag.
func (a *Adapter) List(ctx context.Context, q tracker.Query, page string) ([]tracker.Item, string, error) {
	if q.Type != "Run" {
		return nil, "", fmt.Errorf("mlflow: List %s: %w", q.Type, tracker.ErrUnsupported)
	}
	keys := make([]string, 0, len(q.Fields))
	for k := range q.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v, err := filterValue(q.Fields[k])
		if err != nil {
			return nil, "", err
		}
		if k == "status" {
			parts = append(parts, "attributes.status = "+v)
		} else {
			parts = append(parts, "tags.`"+k+"` = "+v)
		}
	}
	in := map[string]any{"experiment_ids": []string{a.experiment}, "max_results": 500, "filter": strings.Join(parts, " and ")}
	if page != "" {
		in["page_token"] = page
	}
	var res struct {
		Runs []run  `json:"runs"`
		Next string `json:"next_page_token"`
	}
	if err := a.do(ctx, http.MethodPost, "runs/search", nil, in, &res); err != nil {
		return nil, "", err
	}
	out := make([]tracker.Item, 0, len(res.Runs))
	for _, r := range res.Runs {
		out = append(out, a.runItem(r))
	}
	return out, res.Next, nil
}

// Changes: MLflow has no event log or change feed for runs, so nothing is
// reported; intake, decisions read while a wait is open, and resync carry
// the loop.
func (a *Adapter) Changes(_ context.Context, cursor string) ([]tracker.Ref, string, error) {
	return nil, cursor, nil
}

// Decisions reads the registry for the model version: the approve alias
// pointing at it, and the review tag on it. Each is reported once per
// value (its Source names alias@version or tag=value), attributed to the
// registry.
func (a *Adapter) Decisions(ctx context.Context, on tracker.Ref, since time.Time) ([]tracker.Decision, error) {
	if on.Type != "ModelVersion" {
		return nil, fmt.Errorf("mlflow: Decisions on %s: %w", on.Type, tracker.ErrUnsupported)
	}
	mv, err := a.version(ctx, on.ID)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(mv)
	by := tracker.Person{ID: Registry, Name: "MLflow model registry"}
	var out []tracker.Decision
	for _, al := range mv.Aliases {
		if al != a.approveAlias {
			continue
		}
		out = append(out, tracker.Decision{On: on, By: by, Native: "alias:" + al, At: a.now(), Evidence: raw,
			Source: tracker.Ref{Type: "Alias", ID: al + "@" + mv.Name + "/" + mv.Version},
			Body:   fmt.Sprintf("alias %s set on %s version %s", al, mv.Name, mv.Version)})
	}
	tg := tags(mv.Tags)
	if v := tg[a.reviewTag]; v != "" {
		at := time.UnixMilli(mv.Updated)
		if !at.After(since) {
			at = a.now()
		}
		out = append(out, tracker.Decision{On: on, By: by, Native: "tag:" + v, At: at, Evidence: raw, Body: tg[a.noteTag],
			Source: tracker.Ref{Type: "Tag", ID: a.reviewTag + "=" + v + "@" + mv.Name + "/" + mv.Version}})
	}
	return out, nil
}

// --- webhooks ---

// Receive verifies X-MLflow-Signature (v1,<base64 HMAC-SHA256 of
// "<delivery id>.<timestamp>.<body>">), refuses a timestamp more than
// five minutes off, and parses the documented envelope. Registry events
// name a model version in data.name and data.version.
func (a *Adapter) Receive(r *http.Request) (tracker.Delivery, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		return tracker.Delivery{}, err
	}
	id, ts := r.Header.Get("X-MLflow-Delivery-Id"), r.Header.Get("X-MLflow-Timestamp")
	if a.c.WebhookSecret != nil {
		secret, err := a.c.WebhookSecret(r.Context())
		if err != nil {
			return tracker.Delivery{}, err
		}
		if len(secret) > 0 {
			if !verify(id, ts, body, secret, r.Header.Get("X-MLflow-Signature")) {
				return tracker.Delivery{}, tracker.ErrBadSignature
			}
			n, err := strconv.ParseInt(ts, 10, 64)
			if err != nil || math.Abs(a.now().Sub(time.Unix(n, 0)).Seconds()) > 300 {
				return tracker.Delivery{}, fmt.Errorf("%w: timestamp outside five minutes", tracker.ErrBadSignature)
			}
		}
	}
	var p struct {
		Entity string `json:"entity"`
		Data   struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return tracker.Delivery{}, fmt.Errorf("mlflow: delivery: %w", err)
	}
	d := tracker.Delivery{ID: id}
	if strings.HasPrefix(p.Entity, "model_version") && p.Data.Name != "" && p.Data.Version != "" {
		d.Refs = append(d.Refs, mvRef(p.Data.Name, p.Data.Version))
	}
	return d, nil
}

func verify(id, ts string, body, secret []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "v1,")
	if !ok {
		return false
	}
	got, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// --- writing ---

// Apply makes one write. Kinds:
//   - run: on a Run, Fields["status"] (an MLflow RunStatus; a terminal one
//     also sets end_time) and every other field as a tag; an empty value
//     is not written.
func (a *Adapter) Apply(ctx context.Context, w tracker.Write) (tracker.Ref, error) {
	if w.Kind != "run" || w.On.Type != "Run" {
		return tracker.Ref{}, fmt.Errorf("mlflow: write kind %q on %s: %w", w.Kind, w.On.Type, tracker.ErrUnsupported)
	}
	keys := make([]string, 0, len(w.Fields))
	for k := range w.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := w.Fields[k]
		if v == "" {
			continue
		}
		if k == "status" {
			in := map[string]any{"run_id": w.On.ID, "status": v}
			if v == "FINISHED" || v == "FAILED" || v == "KILLED" {
				in["end_time"] = a.now().UnixMilli()
			}
			if err := a.do(ctx, http.MethodPost, "runs/update", nil, in, nil); err != nil {
				return tracker.Ref{}, err
			}
			continue
		}
		if err := a.do(ctx, http.MethodPost, "runs/set-tag", nil, map[string]string{"run_id": w.On.ID, "key": k, "value": v}, nil); err != nil {
			return tracker.Ref{}, err
		}
	}
	return w.On, nil
}

// Publish registers the output as a version of the model, from the run,
// with source <source_prefix><view> and the C4 tags. A version of this
// run already tagged with the view's C4 ID is the one a crash left: it is
// returned, not registered again. MLflow's own artifact store records no
// digest, so the C4 ID is the version's content
// identity. The server stores the source without resolving its scheme;
// clients read it with the c4 artifact-repository plugin
// (integrations/mlflow-c4).
func (a *Adapter) Publish(ctx context.Context, on tracker.Ref, out tracker.Output, key string) (tracker.Ref, error) {
	if on.Type != "Run" {
		return tracker.Ref{}, fmt.Errorf("mlflow: Publish on %s: %w", on.Type, tracker.ErrUnsupported)
	}
	rid, err := filterValue(on.ID)
	if err != nil {
		return tracker.Ref{}, err
	}
	var found struct {
		MVs []modelVersion `json:"model_versions"`
	}
	if err := a.do(ctx, http.MethodGet, "model-versions/search", url.Values{"filter": {"run_id = " + rid}}, nil, &found); err != nil {
		return tracker.Ref{}, err
	}
	for _, mv := range found.MVs {
		if mv.Name == a.model && tags(mv.Tags)["c4.id"] == out.View {
			return mvRef(mv.Name, mv.Version), nil
		}
	}
	in := map[string]any{
		"name": a.model, "source": a.sourcePrefix + out.View, "run_id": on.ID,
		"description": out.Describe() + ".",
		"tags":        []tag{{"c4.id", out.View}, {"c4.job", out.Name}, {"c4.job_url", out.JobURL}, {"c4.custody", out.Custody}, {"c4.key", key}},
	}
	var res struct {
		MV modelVersion `json:"model_version"`
	}
	if err := a.do(ctx, http.MethodPost, "model-versions/create", nil, in, &res); err != nil {
		return tracker.Ref{}, err
	}
	return mvRef(res.MV.Name, res.MV.Version), nil
}

// Probe: run status and tag writes are idempotent by themselves.
// Guard: a run's status and tags are set, the same write sent again.
func (a *Adapter) Guard(w tracker.Write) effect.Guard {
	if w.Kind == "run" {
		return effect.Idempotent
	}
	return effect.Unguarded
}

func (a *Adapter) Probe(context.Context, tracker.Write) (bool, tracker.Ref, error) {
	return false, tracker.Ref{}, nil
}

// Person: MLflow names users only by id strings; the registry's
// decisions carry no author (Registry).
func (a *Adapter) Person(_ context.Context, id string) (tracker.Person, error) {
	return tracker.Person{ID: id}, nil
}
