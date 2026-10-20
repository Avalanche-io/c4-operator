// Package farm is c4-sync's side of the farm: the few things the sync loop
// asks of C4, behind one interface. The real implementation calls the HUD
// API as c4-sync's own identity, so every change c4-sync makes is checked,
// signed and booked like a person's (internal/hud/API.md).
package farm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
)

// JobRef names a job.
type JobRef struct{ Namespace, Name string }

func (r JobRef) String() string { return r.Namespace + "/" + r.Name }

// JobRequest is a job the sync submits for a tracker entity.
type JobRequest struct {
	Namespace string
	Name      string
	Spec      c4v1.WorkJobSpec
	// TrackerRef is <tracker>/<type>/<id>: the job carries it (annotation
	// c4.avalanche.io/tracker-ref, label c4.avalanche.io/tracker) and its
	// record names it.
	TrackerRef string
	// Inputs are views the job mounts besides its spec's (a note it
	// answers).
	Inputs []api.SubmitInput
	// Follows is the job this one is the next version of (spec.follows,
	// in its record too), so lineage links the versions.
	Follows string
}

// Relay is a decision made in a tracker, relayed to a wait (GAPS 3).
type Relay struct {
	// Project is the job's project, whose store keeps the evidence.
	Project string
	Unit    string // the wait's name or index
	Do      string // approve, a wait choice, accept, rework or fail
	Note    string // the decision's text
	// Reply sends the note as the decision's content, for a wait that asks
	// for one (the HUD stores it as reply.md, which the units the wait
	// holds up mount).
	Reply bool
	// Via says where it was made: the tracker, the entity that carries it,
	// and the person, as the tracker names them and as C4 does.
	Tracker, Source, By, Person string
	// Evidence is the tracker's own record of the decision, stored by C4
	// ID so the relayed decision can cite it.
	Evidence []byte
}

// Farm is what the sync loop asks of C4.
type Farm interface {
	// Submit creates the job, or finds it when it exists already: a
	// retried submission is not an error.
	Submit(ctx context.Context, r JobRequest) (JobRef, error)
	// Job reads a job as the HUD shows it; ErrNotFound when it is gone.
	Job(ctx context.Context, j JobRef) (*api.Job, error)
	// SetDeadline writes a tracker-owned deadline (deadlineMode Tracking).
	SetDeadline(ctx context.Context, j JobRef, at time.Time) error
	// Decide relays a tracker decision to an open wait.
	Decide(ctx context.Context, j JobRef, r Relay) error
	// StoreView stores files in the project's store as a view and returns
	// its C4 ID.
	StoreView(ctx context.Context, project string, files map[string][]byte) (string, error)
	// JobURL is the job's page in the HUD, for links written to trackers.
	JobURL(j JobRef) string
}

// ErrNotFound is a job the farm does not have, or will not show.
var ErrNotFound = errors.New("farm: not found")

// ErrNotOpen is a decision for a wait that is not open (decided already,
// or not open yet); the HUD answers 409.
var ErrNotOpen = errors.New("farm: nothing open waits for that decision")

// HUD is the Farm over the HUD API.
type HUD struct {
	Base string
	// Token returns c4-sync's bearer token for the HUD.
	Token func(ctx context.Context) (string, error)
	// Agent is what the HUD books c4-sync's controls as (X-C4-Agent).
	Agent string
	// As selects a development viewer (?as=) on HUDs started with
	// --hud-dev-viewers (tests and demos only), which ignore bearer tokens.
	As string
	// Public is the HUD's address as people reach it, for links written
	// to trackers (an ingress); empty uses Base.
	Public string
	HTTP   *http.Client
}

// APIError is a refusal from the HUD.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("HUD %d: %s", e.Status, e.Msg) }

func (h *HUD) do(ctx context.Context, method, path string, ctype string, body []byte, out any) error {
	u := strings.TrimSuffix(h.Base, "/") + "/api/v1" + path
	if h.As != "" {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + "as=" + url.QueryEscape(h.As)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-C4-Client", "c4-sync")
	if h.Agent != "" {
		req.Header.Set("X-C4-Agent", h.Agent)
	}
	if body != nil {
		req.Header.Set("Content-Type", ctype)
	}
	if h.Token != nil {
		tok, err := h.Token(ctx)
		if err != nil {
			return err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	hc := h.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("HUD at %s: %w", h.Base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (h *HUD) doJSON(ctx context.Context, method, path string, in, out any) error {
	var b []byte
	if in != nil {
		var err error
		if b, err = json.Marshal(in); err != nil {
			return err
		}
	}
	return h.do(ctx, method, path, "application/json", b, out)
}

func jobPath(j JobRef) string {
	return "/jobs/" + url.PathEscape(j.Namespace) + "/" + url.PathEscape(j.Name)
}

func (h *HUD) Submit(ctx context.Context, r JobRequest) (JobRef, error) {
	spec, err := json.Marshal(r.Spec)
	if err != nil {
		return JobRef{}, err
	}
	in := api.SubmitRequest{Namespace: r.Namespace, Name: r.Name, Type: "tracker", Spec: spec, Inputs: r.Inputs,
		TrackerRef: r.TrackerRef, Follows: r.Follows}
	var out api.SubmitResponse
	err = h.doJSON(ctx, http.MethodPost, "/jobs", in, &out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		// The name is taken: an earlier attempt created it.
		return JobRef{r.Namespace, r.Name}, nil
	}
	if err != nil {
		return JobRef{}, err
	}
	return JobRef{out.Namespace, out.Name}, nil
}

func (h *HUD) Job(ctx context.Context, j JobRef) (*api.Job, error) {
	var out api.Job
	err := h.doJSON(ctx, http.MethodGet, jobPath(j), nil, &out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetDeadline sends the deadline alone: on a job whose mode is Tracking the
// HUD keeps the mode. The sync loop moves only deadlines that follow
// tracking; one a person pinned stays theirs.
func (h *HUD) SetDeadline(ctx context.Context, j JobRef, at time.Time) error {
	return h.doJSON(ctx, http.MethodPost, jobPath(j)+"/nudge",
		map[string]string{"deadline": at.UTC().Format(time.RFC3339)}, nil)
}

// Decide stores the evidence in the project's store and relays the
// decision. Until the HUD takes `via` (GAPS 3), the relay's facts go in
// the note, so the signed control record still carries them.
func (h *HUD) Decide(ctx context.Context, j JobRef, r Relay) error {
	evidence := ""
	if len(r.Evidence) > 0 && r.Project != "" {
		id := c4.Identify(bytes.NewReader(r.Evidence))
		if err := h.do(ctx, http.MethodPut, "/objects/"+url.PathEscape(r.Project)+"/"+id.String(),
			"application/octet-stream", r.Evidence, nil); err != nil {
			return fmt.Errorf("storing the decision's evidence: %w", err)
		}
		evidence = id.String()
	}
	body := map[string]any{"unit": r.Unit, "do": r.Do, "note": RelayNote(r, evidence)}
	if r.Reply && r.Do != "fail" {
		body["reply"] = map[string]string{"text": RelayNote(r, evidence)}
	}
	err := h.doJSON(ctx, http.MethodPost, jobPath(j)+"/decide", body, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		return fmt.Errorf("%w: %s", ErrNotOpen, ae.Msg)
	}
	return err
}

// RelayNote is the decision's note with the relay's facts: who decided,
// where, and the evidence's C4 ID.
func RelayNote(r Relay, evidence string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(r.Note))
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Relayed from %s %s by %s", r.Tracker, r.Source, r.By)
	if r.Person != "" {
		fmt.Fprintf(&b, " (%s)", r.Person)
	}
	if evidence != "" {
		fmt.Fprintf(&b, "; evidence %s", evidence)
	}
	b.WriteString(".")
	return b.String()
}

// StoreView puts each file's bytes in the project's store, then records
// the tree (PUT /objects, POST /views; API.md "Submitting").
func (h *HUD) StoreView(ctx context.Context, project string, files map[string][]byte) (string, error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	req := api.ViewRequest{}
	for _, p := range paths {
		b := files[p]
		id := c4.Identify(bytes.NewReader(b)).String()
		if err := h.do(ctx, http.MethodPut, "/objects/"+url.PathEscape(project)+"/"+id, "application/octet-stream", b, nil); err != nil {
			return "", err
		}
		req.Files = append(req.Files, api.ViewFile{Path: p, ID: id, Size: int64(len(b))})
	}
	var out api.ViewResponse
	if err := h.doJSON(ctx, http.MethodPost, "/views/"+url.PathEscape(project), req, &out); err != nil {
		return "", err
	}
	return out.View, nil
}

func (h *HUD) JobURL(j JobRef) string {
	base := h.Public
	if base == "" {
		base = h.Base
	}
	return strings.TrimSuffix(base, "/") + "/#/job/" + j.Namespace + "/" + j.Name
}
