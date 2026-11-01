package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// Signed acts (PEOPLE-KEYS-2 2): a request may carry its act, signed by
// its actor's own key, and the control record that carries it out cites
// it. Until every client signs (K8), a request without one is taken as
// before and graded as today; one with an act that doesn't verify, or
// that is for another job, spec, action or params, is refused.

// ActHeader carries a request's signed act: its bundle (act/v1), base64.
const ActHeader = "X-C4-Act"

// errAct marks a request whose act the HUD refuses.
var errAct = errors.New("the request's signed act")

// actsSeen is the set of acts the HUD has taken, by payload hash: Acts,
// or one in this process's memory.
func (s *Server) actsSeen() provenance.SeenSet {
	if s.Acts != nil {
		return s.Acts
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acts == nil {
		s.acts = provenance.NewMemorySeen()
	}
	return s.acts
}

// citedAct is a request's signed act as its control record cites it.
type citedAct struct {
	cite   *schema.ActCitation
	params json.RawMessage
	grade  string
}

// citeAct verifies the act a request carries, if it carries one, against
// what the HUD is about to carry out: a verified act of the farm's, on
// subject, of action, with the request's own body as its params, by the
// viewer when a person signed it, taken once. It returns nil for a
// request that carries none.
func (s *Server) citeAct(ctx context.Context, r *http.Request, v *api.Viewer, subject provenance.Subject, action string) (*citedAct, error) {
	h := r.Header.Get(ActHeader)
	if h == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not base64", errAct, ActHeader)
	}
	body, err := requestBody(r)
	if err != nil {
		return nil, err
	}
	// Checked before it is taken, so a request that doesn't match spends
	// nothing; VerifyAct takes it last.
	vr, err := provenance.VerifyBundle(data, s.trust(ctx).roots)
	if err != nil {
		return nil, fmt.Errorf("%w does not verify: %v", errAct, err)
	}
	a, errs := schema.ParseAct(vr.Payload)
	if errs != nil {
		return nil, fmt.Errorf("%w is not an act: %s", errAct, errs[0].Message)
	}
	p := a.Predicate
	want, err := schema.ParamsID(p.Params)
	if err != nil {
		return nil, fmt.Errorf("%w: params: %v", errAct, err)
	}
	got, err := schema.ParamsID(body)
	switch {
	case a.Subject[0].Name != subject.Name || !maps.Equal(a.Subject[0].Digest, subject.Digest):
		return nil, fmt.Errorf("%w is on %s, and this request on %s at spec %.12s: the job changed since it was signed, or it is another job", errAct, a.Subject[0].Name, subject.Name, subject.Digest["sha256"])
	case p.Act != action:
		return nil, fmt.Errorf("%w is %s, and this request is %s", errAct, p.Act, action)
	case err != nil || got != want:
		return nil, fmt.Errorf("%w's params are not this request's body", errAct)
	case p.Chain.By.Person == "":
		// A unit's act is taken from that unit's own token (K5), an
		// agent's under a grant in force for the viewer (K5) and a relay's
		// only through relayed() with its signer bound to the Tracker (K6).
		// Until each has its caller's check, only a person's own act is.
		return nil, fmt.Errorf("%w is not a person's: a unit's, an agent's or a relay's act is not taken here yet", errAct)
	case vr.Identity != provenance.UserIdentity(p.Chain.By.Person):
		return nil, fmt.Errorf("%w names %s and is signed by %s, not by a device of theirs", errAct, p.Chain.By.Person, vr.Identity)
	case v == nil:
		return nil, fmt.Errorf("%w is %s's, and no one signed in sent it", errAct, p.Chain.By.Person)
	case v.User != p.Chain.By.Person && s.ownerFor(ctx, v) != p.Chain.By.Person:
		// The person is the viewer, or on a one-person farm the person a
		// role's sign-in acts for (ownerFor): noor signs as artist.
		return nil, fmt.Errorf("%w is %s's, and %s sent it", errAct, p.Chain.By.Person, v.User)
	}
	if _, _, err := provenance.VerifyAct(data, s.trust(ctx).roots, time.Now(), s.actsSeen()); err != nil {
		return nil, fmt.Errorf("%w: %v", errAct, err)
	}
	return &citedAct{cite: &schema.ActCitation{Payload: provenance.PayloadHash(vr.Payload), Record: c4.Identify(bytes.NewReader(data)).String()},
		params: p.Params, grade: p.Chain.By.Grade}, nil
}

// requestBody is the request's body as its client sent it, {} for none.
// decodeBody keeps it for a request that carries an act.
func requestBody(r *http.Request) (json.RawMessage, error) {
	if r.Body == nil {
		return json.RawMessage("{}"), nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	if len(bytes.TrimSpace(b)) == 0 {
		return json.RawMessage("{}"), nil
	}
	return b, nil
}

// controlStatus is the status a failed control record answers: 403 when
// the request's act was refused, else 503, the HUD unable to sign.
func controlStatus(err error) int {
	if errors.Is(err, errAct) {
		return http.StatusForbidden
	}
	return http.StatusServiceUnavailable
}
