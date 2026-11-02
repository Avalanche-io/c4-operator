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
	"strings"
	"time"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
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

// ActIndex is the people's index as the HUD checks a person's act against
// it (people.Index).
type ActIndex interface {
	// CheckActNow is the live check: against every entry, pending ones
	// too, at the clock.
	CheckActNow(ctx context.Context, v *provenance.Verified) error
}

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
	// on is the act's subject, the control record's.
	on provenance.Subject
	// bundle is the act as signed, which the record cites by its C4 ID.
	bundle []byte
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
	case p.Chain.By.Unit != "":
		// a unit's act, only from that unit's own token (unitacts.go)
		return s.citeUnitAct(ctx, data, vr, p, v, subject)
	case p.Chain.By.Person == "" && p.Chain.By.User != "":
		// A relay's act, only for the relayed decision it carries and
		// signed by the sync its Tracker names (relayed(), relayCheck).
		rc := relayOf(ctx)
		if rc == nil {
			return nil, fmt.Errorf("%w is a relay's, and a relay's act is taken only with the decision it relays", errAct)
		}
		if err := rc.check(vr.Identity, p.Chain); err != nil {
			return nil, fmt.Errorf("%w %v", errAct, err)
		}
		if _, _, err := provenance.VerifyAct(data, s.trust(ctx).roots, time.Now(), s.actsSeen()); err != nil {
			return nil, fmt.Errorf("%w: %w", errAct, err)
		}
		// The record's grade is the farm's: relayed for a confirmed
		// login, unattributed for one no person has confirmed.
		return &citedAct{cite: &schema.ActCitation{Payload: provenance.PayloadHash(vr.Payload), Record: c4.Identify(bytes.NewReader(data)).String()},
			params: p.Params, grade: rc.grade, on: subject}, nil
	case p.Chain.By.Person == "":
		// An agent's act is taken under a grant in force for the viewer
		// (K5). Until it has its caller's check, it is refused.
		return nil, fmt.Errorf("%w is not a person's, a unit's or a relay's: an agent's act is not taken here yet", errAct)
	case vr.Identity != provenance.UserIdentity(p.Chain.By.Person):
		return nil, fmt.Errorf("%w names %s and is signed by %s, not by a device of theirs", errAct, p.Chain.By.Person, vr.Identity)
	case v == nil:
		return nil, fmt.Errorf("%w is %s's, and no one signed in sent it", errAct, p.Chain.By.Person)
	case v.User != p.Chain.By.Person && s.ownerFor(ctx, v) != p.Chain.By.Person:
		// The person is the viewer, or on a one-person farm the person a
		// role's sign-in acts for (ownerFor): noor signs as artist.
		return nil, fmt.Errorf("%w is %s's, and %s sent it", errAct, p.Chain.By.Person, v.User)
	}
	if s.PeopleIndex != nil {
		// The device in force at the index's position, now, and a
		// confirmed act's touch key that device's: a removed device's
		// certificate lives on up to 12 hours, and the index ends it at
		// once. Checked before the act is taken, so a refusal spends none.
		if err := s.PeopleIndex.CheckActNow(ctx, vr); err != nil {
			return nil, fmt.Errorf("%w: %v", errAct, err)
		}
	}
	if _, _, err := provenance.VerifyAct(data, s.trust(ctx).roots, time.Now(), s.actsSeen()); err != nil {
		return nil, fmt.Errorf("%w: %w", errAct, err)
	}
	return &citedAct{cite: &schema.ActCitation{Payload: provenance.PayloadHash(vr.Payload), Record: c4.Identify(bytes.NewReader(data)).String()},
		params: p.Params, grade: p.Chain.By.Grade, on: subject, bundle: data}, nil
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

// onAct puts a control record on the act it carries out: the act's
// subject, citing it, with its params and grade. With no act the record
// is as before, on subject.
func onAct(act *citedAct, subject provenance.Subject, pred *controlPredicate) provenance.Subject {
	if act == nil {
		return subject
	}
	pred.Act, pred.Params, pred.Custody, pred.actBundle = act.cite, act.params, act.grade, act.bundle
	return act.on
}

// citeOnProject cites the act of a request that makes something in a
// project: on the project.
func (s *Server) citeOnProject(ctx context.Context, r *http.Request, v *api.Viewer, m *model, project, action string) (*citedAct, error) {
	p := projectObject(m, project)
	if p == nil {
		if r.Header.Get(ActHeader) != "" {
			return nil, fmt.Errorf("%w: the HUD has not read project %s yet", errAct, project)
		}
		return nil, nil
	}
	return s.citeAct(ctx, r, v, projectSubject(p), action)
}

// The subjects acts are on (PEOPLE-KEYS-2 2.1), served with what a client
// can act on (api.ActOn) so it signs exactly what the HUD checks: an
// object by ActSubject, with the digest the HUD computes of it; an act
// that makes something, on the project it is made in, or on the farm.

// projectSubject is an act made in a project: on the project by UID, at
// its spec's digest.
func projectSubject(p *c4v1.Project) provenance.Subject {
	return provenance.Subject{Name: "project:" + p.Name + "@" + string(p.UID), Digest: map[string]string{"sha256": sha256JSON(p.Spec)}}
}

// farmSubject is an act on the farm itself, by its ID, as the people's
// issuer names it; ok is false while the HUD has no farm CA to name it by.
func (s *Server) farmSubject(ctx context.Context) (provenance.Subject, bool) {
	id := s.trust(ctx).farm
	return provenance.Subject{Name: "farm:" + id, Digest: map[string]string{"c4": id}}, id != ""
}

// grantSubject is a submit grant as an act on it names it.
func grantSubject(g *c4v1.SubmitGrant) provenance.Subject {
	return provenance.ActSubject("submitgrant", g.Namespace, g.Name, string(g.UID), sha256JSON(g.Spec))
}

// offerSubject is a Tracker's people map as an act confirming one of its
// offers names it: the Tracker by UID, at its offers' digest, so a change
// to the Tracker that offers nothing new leaves the act standing.
func offerSubject(tr *c4v1.Tracker) provenance.Subject {
	return provenance.ActSubject("tracker", tr.Namespace, tr.Name, string(tr.UID), sha256JSON(tr.Spec.People))
}

// confirmationSubject is a people-map confirmation as an act revoking it
// names it.
func confirmationSubject(tp *c4v1.TrackerPerson) provenance.Subject {
	return provenance.ActSubject("trackerperson", tp.Namespace, tp.Name, string(tp.UID), sha256JSON(tp.Spec))
}

// heldSubject is a held tracker write as an act confirming or denying it
// names it: the link's write by its field, the link by UID
// (trackerlink:<ns>/<link>#<field>@<uid>), at the write's digest.
func heldSubject(l *c4v1.TrackerLink, field string, wr c4v1.TrackerWritten) provenance.Subject {
	return provenance.ActSubject("trackerlink", l.Namespace, l.Name+"#"+field, string(l.UID), sha256JSON(wr))
}

// capacitySubject is a capacity request as an act approving it names it.
func capacitySubject(cr *c4v1.CapacityRequest) provenance.Subject {
	// the farm's, in no project: capacityrequest:<name>@<uid>
	return provenance.Subject{Name: "capacityrequest:" + cr.Name + "@" + string(cr.UID), Digest: map[string]string{"sha256": sha256JSON(cr.Spec)}}
}

// enrollSubject is a host runner's offer as an act enrolling it names it:
// the machine, by the C4 ID of the code it shows, which only that offer
// has. A client knows the machine and the code, not the node's UID; the
// code makes the act this offer's.
func enrollSubject(node, code string) provenance.Subject {
	return provenance.Subject{Name: "node:" + node, Digest: map[string]string{"c4": c4.Identify(strings.NewReader(normalCode(code))).String()}}
}

// handleActNeeds is GET /api/v1/acts/needs: what each act kind needs of
// its signer, so a client asks for a touch before it signs and keeps no
// list of its own.
func (s *Server) handleActNeeds(w http.ResponseWriter, r *http.Request) {
	if v := s.withViewer(w, r); v == nil {
		return
	}
	acts, params := provenance.ActNeedsList()
	out := api.ActNeeds{Acts: map[string]api.ActNeed{}, Params: params}
	for k, n := range acts {
		out.Acts[k] = api.ActNeed{Person: n.Person, Touch: n.Touch, Relayed: n.Relayed}
	}
	writeJSON(w, http.StatusOK, out)
}

// actOn is a subject as the API serves it.
func actOn(sub provenance.Subject) *api.ActOn {
	return &api.ActOn{Name: sub.Name, Digest: sub.Digest}
}
