// Package farm is c4-sync's side of the farm: the few things the sync loop
// asks of C4, behind one interface. The real implementation calls the HUD
// API as c4-sync's own identity, so every change c4-sync makes is checked,
// signed and booked like a person's (internal/hud/API.md). Operator
// primitives that are designed but not built (proposals/integrations
// GAPS.md: Milestone, external refs, relayed decisions) are reached through
// this interface in the shapes GAPS.md gives, so the loop does not change
// when they land.
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
	// TrackerRef is <tracker>/<type>/<id> (GAPS 2). Carried by the job once
	// the operator takes it; until then the TrackerLink holds the mapping.
	TrackerRef string
	Tracker    string
}

// Relay is a decision made in a tracker, relayed to a wait (GAPS 3).
type Relay struct {
	// Project is the job's project, whose store keeps the evidence.
	Project string
	Unit    string // the wait's name or index
	Do      string // approve, a wait choice, accept, rework or fail
	Note    string // the decision's text
	// Via says where it was made: the tracker, the entity that carries it,
	// and the person, as the tracker names them and as C4 does.
	Tracker, Source, By, Person string
	// Evidence is the tracker's own record of the decision, stored by C4
	// ID so the relayed decision can cite it.
	Evidence []byte
}

// Milestone is a due date owned by a tracker, which jobs roll up to
// (GAPS 1).
type Milestone struct {
	Namespace, Name string
	Due             time.Time
	Selector        map[string]string
	Tracker, Ref    string
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
	// EnsureMilestone creates or updates a Milestone (GAPS 1).
	EnsureMilestone(ctx context.Context, m Milestone) error
	// JobURL is the job's page in the HUD, for links written to trackers.
	JobURL(j JobRef) string
}

// ErrNotFound is a job the farm does not have, or will not show.
var ErrNotFound = errors.New("farm: not found")

// ErrNotBuilt is an operator primitive that is designed but not built yet.
var ErrNotBuilt = errors.New("farm: designed, not built in the operator yet")

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
	HTTP  *http.Client
}

// APIError is a refusal from the HUD.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("HUD %d: %s", e.Status, e.Msg) }

func (h *HUD) do(ctx context.Context, method, path string, ctype string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(h.Base, "/")+"/api/v1"+path, bytes.NewReader(body))
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
	in := api.SubmitRequest{Namespace: r.Namespace, Name: r.Name, Type: "tracker", Spec: spec}
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
// HUD keeps the mode (a deadline with deadlineMode Tracking in one nudge
// is refused, internal/hud/server/controls.go).
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
	err := h.doJSON(ctx, http.MethodPost, jobPath(j)+"/decide", map[string]any{
		"unit": r.Unit, "do": r.Do, "note": RelayNote(r, evidence),
	}, nil)
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

func (h *HUD) EnsureMilestone(context.Context, Milestone) error { return ErrNotBuilt }

func (h *HUD) JobURL(j JobRef) string {
	return strings.TrimSuffix(h.Base, "/") + "/#/job/" + j.Namespace + "/" + j.Name
}
