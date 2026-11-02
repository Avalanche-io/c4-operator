package people

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// Agents outside a job (PEOPLE-KEYS-2 5.2, slice K5). An agent reaches the
// farm through the MCP server, which makes a P-256 key of its own under the
// agent's name; the agent never holds the person's key. The person grants
// the agent that key with a touched act, which the issuer books in the farm
// book; while the grant is in force the issuer certifies the key as
// agent/<name>, naming the key's device and the grant, and the HUD takes the
// agent's acts under it for that person only.
//
// The grant is DESIGN 10's one exception (P5): it ends when the integration
// is removed (the person's revoke), or once the agent has made no act under
// it for farmbook.AgentLapse, and nothing renews it silently. A touched c4
// farm integrate makes a fresh grant. The agent's acts are booked as its use
// of the grant, at most once a day, and the lapse is booked when the issuer
// sees it. "In force" is the farm book's (farmbook.ForceAgentGrant), so a
// farm without a farm book takes no agent grants.
//
//   - grant: on device:<the agent key's ID>, confirmed, params {agent,
//     device}, by the person who grants. Its record's C4 ID is the grant's ID.
//   - revoke: on grant:<ID>, by the grant's person or an admin: the
//     integration removed.

// The entries of an agent's grant, as the farm book names them.
const (
	KindAgentGrant = farmbook.KindAgentGrant
	KindAgentUse   = farmbook.KindAgentUse
	KindGrantEnd   = farmbook.KindGrantEnd
)

// The acts that make and end an agent's grant.
const (
	ActGrant  = "grant"
	ActRevoke = "revoke"
)

// Why a grant ended.
const (
	// EndRemoved: the person removed the integration (their revoke).
	EndRemoved = "removed"
	// EndLapsed: the agent made no act under it for farmbook.AgentLapse.
	EndLapsed = "lapsed"
)

// ErrNoGrant means an agent's key has no grant in force: never granted,
// removed or lapsed, or a grant for another agent or key.
var ErrNoGrant = errors.New("no agent grant in force")

// errNoBook refuses an agent grant on a farm that keeps no farm book.
var errNoBook = refuse(http.StatusNotImplemented, "an agent's grant is booked in the farm book, and this farm keeps none: install it with --system-store")

var agentName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// agentOf is the agent an identity names, or "".
func agentOf(identity string) string {
	a, ok := strings.CutPrefix(identity, provenance.IdentityBase+"agent/")
	if !ok {
		return ""
	}
	if u, err := url.PathUnescape(a); err == nil {
		return u
	}
	return a
}

// grantSubject is the subject a revoke of a grant is on.
func grantSubject(s schema.Subject) string {
	if id := s.Digest["c4"]; id != "" && s.Name == "grant:"+id {
		return id
	}
	return ""
}

// grants is each agent grant's state up to position pos, by the rule
// the farm book uses too (farmbook.GrantStates), with the entries' own
// times (when each counts from), and the entries it read.
func (x *Index) grants(pos int64) (map[string]*farmbook.GrantState, []*Entry) {
	es := x.upTo(pos)
	var evs []farmbook.GrantEvent
	for k, e := range es {
		switch e.Kind {
		case KindAgentGrant, KindAgentUse, KindGrantEnd:
			evs = append(evs, farmbook.GrantEvent{Kind: e.Kind, Grant: e.Grant, Time: e.time(), Ref: k})
		}
	}
	return farmbook.GrantStates(evs), es
}

// liveGrant is the grant entry booked up to pos with this ID, if it hasn't
// ended there. Its lapse by time is judged by the caller's clock.
func (x *Index) liveGrant(grant string, pos int64) *Entry {
	gs, es := x.grants(pos)
	if g := gs[grant]; g != nil && !g.Ended {
		return es[g.Ref]
	}
	return nil
}

// lastUse is when a grant was last used up to pos, by when its entries
// count from: the grant, or the agent's latest act under it.
func (x *Index) lastUse(grant string, pos int64) time.Time {
	gs, _ := x.grants(pos)
	if g := gs[grant]; g != nil {
		return g.Last
	}
	return time.Time{}
}

// loadAgent checks an agent grant's entry against the act it books: a
// grant, confirmed, by a person's device in force, naming the agent and
// its key, which is no person's device; an act of the agent's under a
// grant booked and not ended, signed by the key and grant its leaf names;
// or the person's revoke of a live grant. Each was booked within ActSkew
// of being signed, and each act is booked once.
func (x *Index) loadAgent(e *Entry, b []byte, pending bool) error {
	if e.Kind == KindGrantEnd && e.End == EndLapsed {
		return x.loadLapse(e, b)
	}
	v, err := provenance.VerifyBundle(b, x.Roots)
	if err != nil {
		return err
	}
	if v.PredicateType != provenance.PredicateAct {
		return fmt.Errorf("a %s entry books a %s record, not an act", e.Kind, v.PredicateType)
	}
	if err := provenance.CheckSigner(v); err != nil {
		return err
	}
	a, errs := schema.ParseAct(v.Payload)
	if errs != nil {
		return fmt.Errorf("not an act: %s: %s", errs[0].Path, errs[0].Message)
	}
	if d := e.at().Sub(v.SignedAt); d > provenance.ActSkew || d < -provenance.ActSkew {
		return fmt.Errorf("the %s entry was booked %s from when its act was signed", e.Kind, d.Round(time.Second))
	}
	p, by := a.Predicate, a.Predicate.Chain.By.Person
	// A booked act is judged at the time its entry counts from, as loadAct
	// judges one (in the issuer's own order, the act's signedAt).
	at := v.SignedAt
	if inBook(e) || x.Book != nil || !x.sealing.IsZero() {
		at = e.time()
	}
	says := func(ok bool, what string) error {
		if ok {
			return nil
		}
		return farmbook.Refuse(farmbook.ReasonDisagrees, fmt.Errorf("the %s entry and its %s act disagree on %s", e.Kind, p.Act, what))
	}
	before := e.Position - 1
	switch e.Kind {
	case KindAgentGrant:
		if err := x.checkAct(v, before, at); err != nil {
			return err
		}
		agent, device, err := grantParams(p.Params)
		if err != nil {
			return err
		}
		sub := a.Subject[0]
		err = says(p.Act == ActGrant && by != "" && e.By == by && e.Person == by, "the verb and who granted")
		if err == nil {
			err = says(e.Agent == agent && e.Device == device && sub.Name == "device:"+device && sub.Digest["c4"] == device, "the agent and its key")
		}
		if err == nil {
			err = says(e.Grant == e.Record, "the grant's ID, its record's")
		}
		if err == nil && x.at(before)[device] != nil {
			err = farmbook.Refuse(farmbook.ReasonRejected, fmt.Errorf("key %.16s is a person's device: an agent's key is its own", device))
		}
		if err != nil {
			return err
		}
	case KindAgentUse:
		g := x.liveGrant(e.Grant, before)
		switch {
		case g == nil:
			return farmbook.Refuse(farmbook.ReasonNotInForce, fmt.Errorf("an agent's act under grant %.16s, which is not booked or has ended", e.Grant))
		case v.Grant != e.Grant || agentOf(v.Identity) != g.Agent || v.Device != g.Device:
			return farmbook.Refuse(farmbook.ReasonDisagrees, fmt.Errorf("the act is %s's under grant %.16s with key %.16s, and the grant is %s's key %.16s", v.Identity, v.Grant, v.Device, g.Agent, g.Device))
		}
		if err := says(e.Agent == g.Agent && e.Device == g.Device && e.Person == g.Person, "the grant's agent, key and person"); err != nil {
			return err
		}
	case KindGrantEnd:
		if err := x.checkAct(v, before, at); err != nil {
			return err
		}
		id := grantSubject(a.Subject[0])
		g := x.liveGrant(id, before)
		if g == nil {
			return farmbook.Refuse(farmbook.ReasonNotInForce, fmt.Errorf("a revoke of grant %.16s, which is not booked or has ended", id))
		}
		if err := says(p.Act == ActRevoke && by != "" && e.By == by && e.Grant == id && e.End == EndRemoved &&
			e.Agent == g.Agent && e.Device == g.Device && e.Person == g.Person, "the grant it ends"); err != nil {
			return err
		}
	}
	hash := provenance.PayloadHash(v.Payload)
	if x.acts[hash] || x.pendActs[hash] {
		return fmt.Errorf("act %.16s is booked twice", hash)
	}
	acts := &x.acts
	if pending {
		acts = &x.pendActs
	}
	if *acts == nil {
		*acts = map[string]bool{}
	}
	(*acts)[hash] = true
	return nil
}

// loadLapse checks a lapse: of a grant booked and not ended, which the
// agent hadn't used for AgentLapse by the lapse's own time, booking the
// issuer's signed record of it, which says what the entry says.
func (x *Index) loadLapse(e *Entry, b []byte) error {
	v, err := provenance.VerifyBundle(b, x.Roots)
	if err != nil {
		return err
	}
	var p lapsePredicate
	json.Unmarshal(v.Predicate, &p)
	switch {
	case v.PredicateType != PredicateGrantLapse || v.Identity != provenance.ComponentIdentity("issuer"):
		return farmbook.Refuse(farmbook.ReasonWrongSigner, fmt.Errorf("a lapse books the people's issuer's grant-lapse record, not a %s record signed by %s", v.PredicateType, v.Identity))
	case v.Farm != x.Farm || p.Farm != x.Farm || p.Grant != e.Grant || p.Agent != e.Agent || p.Device != e.Device || p.Person != e.Person:
		return farmbook.Refuse(farmbook.ReasonDisagrees, errors.New("the lapse entry and its record disagree on the grant"))
	}
	if d := e.at().Sub(v.SignedAt); d > provenance.ActSkew || d < -provenance.ActSkew {
		return fmt.Errorf("the lapse was booked %s from when it was signed", d.Round(time.Second))
	}
	g := x.liveGrant(e.Grant, e.Position-1)
	switch {
	case g == nil:
		return farmbook.Refuse(farmbook.ReasonNotInForce, fmt.Errorf("a lapse of grant %.16s, which is not booked or has ended", e.Grant))
	case e.End != EndLapsed || e.Agent != g.Agent || e.Device != g.Device || e.Person != g.Person:
		return farmbook.Refuse(farmbook.ReasonDisagrees, errors.New("a lapse names the grant's agent, key and person"))
	case e.time().Sub(x.lastUse(e.Grant, e.Position-1)) < farmbook.AgentLapse:
		// The grant is still in force: the lapse says what the book doesn't.
		return farmbook.Refuse(farmbook.ReasonDisagrees, fmt.Errorf("grant %.16s was used within %s of its lapse", e.Grant, farmbook.AgentLapse))
	}
	return nil
}

// grantParams reads a grant's params: the agent and its key's device ID,
// and nothing else.
func grantParams(raw json.RawMessage) (agent, device string, err error) {
	var p map[string]string
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("a grant's params are {agent, device}: %v", err)
	}
	for k := range p {
		if k != "agent" && k != "device" {
			return "", "", fmt.Errorf("a grant's params are {agent, device}, not %s", k)
		}
	}
	if !agentName.MatchString(p["agent"]) || p["device"] == "" {
		return "", "", fmt.Errorf("a grant names its agent (letters, digits, '.', '_' and '-', at most 64) and the agent key's device ID")
	}
	return p["agent"], p["device"], nil
}

// agentGrant books a person's grant of an agent's key: confirmed (the
// verifier has checked the touch), on the key, naming it and the agent.
func (i *Issuer) agentGrant(data []byte, by, device string, params map[string]string) (func() (*Entry, error), error) {
	raw, _ := json.Marshal(params)
	agent, key, err := grantParams(raw)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, "%v", err)
	}
	if device == "" || key != device {
		return nil, refuse(http.StatusBadRequest, "a grant is on the agent's key, device:<ID>, and its params name the same key")
	}
	x := i.Index
	if x.Book == nil {
		return nil, errNoBook
	}
	return func() (*Entry, error) {
		if x.at(x.pos)[device] != nil {
			return nil, refuse(http.StatusForbidden, "key %.16s is a person's device: an agent's key is its own", device)
		}
		return &Entry{Kind: KindAgentGrant, Agent: agent, Device: device, Person: by, Grant: c4id(data)}, nil
	}, nil
}

// agentRevoke books a grant's end when its integration is removed: by the
// grant's person, or an admin, who revokes and never grants.
func (i *Issuer) agentRevoke(r *http.Request, by, grant string, params map[string]string) (func() (*Entry, error), error) {
	if grant == "" || len(params) > 0 {
		return nil, refuse(http.StatusBadRequest, "revoke is on grant:<ID>, with no params")
	}
	x := i.Index
	if x.Book == nil {
		return nil, errNoBook
	}
	admin := i.admin(r.Context(), by)
	return func() (*Entry, error) {
		g := x.liveGrant(grant, x.pos)
		if g == nil {
			return nil, refuse(http.StatusNotFound, "grant %.16s is not booked here, or has ended", grant)
		}
		if by != g.Person && !admin {
			return nil, refuse(http.StatusForbidden, "%s may remove their own agents' grants, and an admin anyone's; grant %.16s is %s's", by, grant, g.Person)
		}
		return &Entry{Kind: KindGrantEnd, End: EndRemoved, Grant: grant, Agent: g.Agent, Device: g.Device, Person: g.Person}, nil
	}, nil
}

// agentView is the farm book as the issuer judges an agent's grant,
// sealed and pending, read now, and the agents' entries the index read
// from it, in order.
func (x *Index) agentView(ctx context.Context) (*farmbook.View, []Entry, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.Book == nil {
		return nil, nil, errNoBook
	}
	if err := x.sync(ctx); err != nil {
		return nil, nil, err
	}
	return x.view, x.agentEntries(), nil
}

// agentEntries are the agents' entries the index holds, sealed and
// pending, in order, as copies.
func (x *Index) agentEntries() []Entry {
	var out []Entry
	for _, e := range x.all() {
		switch e.Kind {
		case KindAgentGrant, KindAgentUse, KindGrantEnd:
			out = append(out, *e)
		}
	}
	return out
}

// grantEntry is a grant's own entry in a view, as the issuer wrote it.
func grantEntry(s *farmbook.Sealed) (*Entry, error) {
	var e Entry
	if err := json.Unmarshal(s.Raw, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// AgentGrant is an agent's grant in force.
type AgentGrant struct {
	Grant  string `json:"grant"`
	Agent  string `json:"agent"`
	Device string `json:"device"`
	Person string `json:"person"`
	// Position is the book position it was judged at, pending entries
	// counted, and Lapses when it lapses if the agent makes no act.
	Position int64     `json:"position"`
	Lapses   time.Time `json:"lapses"`
}

// grantIn is grant id as v holds it, every entry counted, judged at now:
// in force, and the grant's entry. A live decision (certifying a key,
// taking an act) passes the issuer's clock, so a grant past AgentLapse of
// disuse is refused at once, before its lapse is booked; a verifier would
// pass a position's seal time instead.
func grantIn(v *farmbook.View, id string, now time.Time) (*AgentGrant, error) {
	pos := v.Position()
	on, err := v.InForceAt(farmbook.ForceAgentGrant, id, pos, now)
	if err != nil || on == nil {
		return nil, err
	}
	e, err := grantEntry(on)
	if err != nil {
		return nil, err
	}
	lapses, err := v.GrantLapses(id, pos)
	if err != nil {
		return nil, err
	}
	return &AgentGrant{Grant: id, Agent: e.Agent, Device: e.Device, Person: e.Person, Position: pos, Lapses: lapses}, nil
}

// AgentGrantFor is the grant an agent's verified record was signed under,
// if it is in force now for that agent and that key: what the HUD takes an
// agent's act by. A grant that has lapsed unused is booked as lapsed here,
// and refused.
func (i *Issuer) AgentGrantFor(ctx context.Context, v *provenance.Verified) (*AgentGrant, error) {
	agent := agentOf(v.Identity)
	if agent == "" || v.Grant == "" || v.Device == "" {
		return nil, fmt.Errorf("%w: %s's certificate names no grant and key: an agent's is the people's issuer's", ErrNoGrant, v.Identity)
	}
	view, _, err := i.Index.agentView(ctx)
	if err != nil {
		return nil, err
	}
	g, err := grantIn(view, v.Grant, i.now())
	if err != nil {
		return nil, err
	}
	if g == nil {
		i.BookLapses(ctx)
		return nil, fmt.Errorf("%w: grant %.16s has ended or lapsed: a person renews it with a touched c4 farm integrate", ErrNoGrant, v.Grant)
	}
	if g.Agent != agent || g.Device != v.Device {
		return nil, fmt.Errorf("%w: grant %.16s is agent %s's key %.16s, not %s's key %.16s", ErrNoGrant, v.Grant, g.Agent, g.Device, agent, v.Device)
	}
	return g, nil
}

// UseGrant books an agent's act as its use of the grant it was signed
// under, when the last booked use is AgentUseEvery old or more: the
// agent's own acts are what keep its grant from lapsing. The HUD calls it
// once it has taken the act.
func (i *Issuer) UseGrant(ctx context.Context, v *provenance.Verified, bundle []byte) error {
	x, now := i.Index, i.now()
	_, err := x.append(ctx, now, i.sign, func() ([]pending, error) {
		if x.view == nil {
			return nil, errNoBook
		}
		g, err := grantIn(x.view, v.Grant, now)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return nil, fmt.Errorf("%w: grant %.16s", ErrNoGrant, v.Grant)
		}
		if now.Sub(g.Lapses.Add(-farmbook.AgentLapse)) < farmbook.AgentUseEvery {
			return nil, nil
		}
		return []pending{{entry: &Entry{Kind: KindAgentUse, Grant: g.Grant, Agent: g.Agent, Device: g.Device, Person: g.Person}, record: bundle}}, nil
	})
	return err
}

// BookLapses books the lapse of every grant that has gone AgentLapse
// without the agent's use and isn't ended yet. The manager runs it now and
// then; the issuer also runs it when it refuses a lapsed grant.
func (i *Issuer) BookLapses(ctx context.Context) ([]*Entry, error) {
	x, now := i.Index, i.now()
	if x.Book == nil {
		return nil, nil
	}
	return x.append(ctx, now, i.sign, func() ([]pending, error) {
		var ps []pending
		for _, e := range x.agentEntries() {
			if e.Kind != KindAgentGrant {
				continue
			}
			g := x.liveGrant(e.Grant, x.pos)
			if g == nil {
				continue
			}
			if c, err := grantIn(x.view, e.Grant, now); err != nil || c != nil {
				continue
			}
			// The lapse is the issuer's reading of the book, signed by the
			// issuer as its writer: the record it books.
			rec, err := i.lapseRecord(ctx, g, x.lastUse(g.Grant, x.pos), now)
			if err != nil {
				return nil, err
			}
			ps = append(ps, pending{entry: &Entry{Kind: KindGrantEnd, End: EndLapsed, Grant: g.Grant, Agent: g.Agent, Device: g.Device, Person: g.Person}, record: rec})
		}
		return ps, nil
	})
}

// PredicateGrantLapse is a grant's lapse as the people's issuer read it in
// the farm book and signed it (component/issuer): the record a lapse's
// entry books.
const PredicateGrantLapse = "https://cccc.io/provenance/grant-lapse/v1"

// lapsePredicate is what a lapse record says: the grant, whose it was, and
// its last use by the entries' own times.
type lapsePredicate struct {
	Farm    string `json:"farm"`
	Grant   string `json:"grant"`
	Agent   string `json:"agent"`
	Device  string `json:"device"`
	Person  string `json:"person"`
	LastUse string `json:"lastUse"`
}

// lapseRecord signs a grant's lapse as the issuer.
func (i *Issuer) lapseRecord(ctx context.Context, g *Entry, last, now time.Time) ([]byte, error) {
	s, err := i.sign(ctx)
	if err != nil {
		return nil, err
	}
	return provenance.SignStatement(ctx, s, PredicateGrantLapse, []provenance.Subject{{Name: "grant:" + g.Grant, Digest: map[string]string{"c4": g.Grant}}},
		lapsePredicate{Farm: i.Farm(), Grant: g.Grant, Agent: g.Agent, Device: g.Device, Person: g.Person, LastUse: last.UTC().Format(time.RFC3339Nano)}, now)
}

// AgentRequest is an agent key's request for its certificate, which the
// key signs (ProofMessage): the farm, the agent, the key, a nonce and when.
type AgentRequest struct {
	Farm        string `json:"farm"`
	Agent       string `json:"agent"`
	Key         string `json:"key"`
	Nonce       string `json:"nonce"`
	RequestedAt string `json:"requestedAt"`
}

// ProofMessage is what the agent's key signs: the request as canonical
// JSON. It names an agent where a device's names a person, so neither
// stands for the other.
func (q AgentRequest) ProofMessage() []byte {
	raw, _ := json.Marshal(q)
	b, _ := schema.Canonical(raw) // strings only, so this cannot fail
	return b
}

// AgentEnrolment is the request and the key's raw r||s signature over it,
// base64url.
type AgentEnrolment struct {
	Request AgentRequest `json:"request"`
	Proof   string       `json:"proof"`
}

// AgentCertified is an agent key's certificate: agent/<name>, naming its
// key's device ID and the grant it acts under, living at most LeafTTL and
// never past the grant's lapse.
type AgentCertified struct {
	AgentGrant
	Farm     string    `json:"farm"`
	Identity string    `json:"identity"`
	Chain    [][]byte  `json:"chain"`
	Expires  time.Time `json:"expires"`
}

func (i *Issuer) handleAgent(w http.ResponseWriter, r *http.Request) {
	var e AgentEnrolment
	if err := decode(r, &e); err != nil {
		fail(w, err)
		return
	}
	c, err := i.CertifyAgent(r.Context(), e)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// CertifyAgent certifies an agent's key on its proof of possession and
// the grant in force for that agent and key: the person's, booked. With
// none in force, nothing is certified, whatever else the request shows.
func (i *Issuer) CertifyAgent(ctx context.Context, e AgentEnrolment) (*AgentCertified, error) {
	q, now := e.Request, i.now()
	switch {
	case q.Farm != i.Farm():
		return nil, refuse(http.StatusBadRequest, "the request is for farm %.16s, and this is %.16s", q.Farm, i.Farm())
	case !agentName.MatchString(q.Agent):
		return nil, refuse(http.StatusBadRequest, "an agent's name is letters, digits, '.', '_' and '-', at most 64")
	case q.Nonce == "":
		return nil, refuse(http.StatusBadRequest, "the request carries a nonce")
	}
	at, err := time.Parse(time.RFC3339, q.RequestedAt)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, "requestedAt %q is not RFC 3339", q.RequestedAt)
	}
	if d := now.Sub(at); d > provenance.ActSkew || d < -provenance.ActSkew {
		return nil, fmt.Errorf("%w: the request was made %s from the issuer's clock", provenance.ErrClock, d.Round(time.Second))
	}
	spki, err := base64.RawURLEncoding.DecodeString(q.Key)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, "the key is not base64url")
	}
	pk, err := x509.ParsePKIXPublicKey(spki)
	pub, ok := pk.(*ecdsa.PublicKey)
	if err != nil || !ok || pub.Curve != elliptic.P256() {
		return nil, refuse(http.StatusBadRequest, "an agent's key is an ECDSA P-256 SubjectPublicKeyInfo")
	}
	sig, err := base64.RawURLEncoding.DecodeString(e.Proof)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, "the proof is not base64url")
	}
	if err := provenance.VerifyP256(pub, q.ProofMessage(), sig); err != nil {
		return nil, refuse(http.StatusForbidden, "the proof is not the key's signature over its request: %v", err)
	}
	device := schema.DeviceID(spki)
	view, es, err := i.Index.agentView(ctx)
	if err != nil {
		return nil, err
	}
	// The latest grant in force for this agent and this key: a fresh
	// integrate's, where a person granted it again.
	var g *AgentGrant
	for _, e := range es {
		if e.Kind != KindAgentGrant || e.Device != device {
			continue
		}
		if c, err := grantIn(view, e.Grant, now); err == nil && c != nil && c.Agent == q.Agent {
			g = c
		}
	}
	if g == nil {
		i.BookLapses(ctx)
		return nil, refuse(http.StatusForbidden, "%v: agent %s's key %.16s is granted by no one now: a person grants it with a touched c4 farm integrate", ErrNoGrant, q.Agent, device)
	}
	ttl := min(LeafTTL, g.Lapses.Sub(now))
	if ttl <= 0 {
		return nil, refuse(http.StatusForbidden, "%v: grant %.16s lapses now", ErrNoGrant, g.Grant)
	}
	leaf, err := i.CA.IssueLeaf(pub, provenance.AgentIdentity(q.Agent), device, g.Grant, ttl, now)
	if err != nil {
		return nil, err
	}
	return &AgentCertified{AgentGrant: *g, Farm: i.Farm(), Identity: provenance.CertIdentity(leaf),
		Chain: [][]byte{leaf.Raw, i.CA.Cert.Raw}, Expires: leaf.NotAfter}, nil
}

// GrantView is one agent grant as GET /v1/people/agents lists it.
type GrantView struct {
	Grant     string `json:"grant"`
	Agent     string `json:"agent"`
	Device    string `json:"device"`
	Person    string `json:"person"`
	Granted   int64  `json:"granted"`
	GrantedAt string `json:"grantedAt"`
	// InForce, and Lapses when the agent's disuse ends it; else End says
	// why it ended (removed or lapsed) and Ended where.
	InForce  bool   `json:"inForce"`
	Lapses   string `json:"lapses,omitempty"`
	LastUse  string `json:"lastUse,omitempty"`
	End      string `json:"end,omitempty"`
	Ended    int64  `json:"ended,omitempty"`
	EndedBy  string `json:"endedBy,omitempty"`
	Position int64  `json:"position"`
}

// handleAgents is GET /v1/people/agents[?person=]: the agents' grants, a
// person's own, or anyone's for an admin.
func (i *Issuer) handleAgents(w http.ResponseWriter, r *http.Request) {
	who, err := caller(r)
	if err != nil {
		fail(w, err)
		return
	}
	person := r.URL.Query().Get("person")
	if person == "" {
		person = who
	}
	if !i.sees(r.Context(), who, person) && person != "*" || person == "*" && !i.admin(r.Context(), who) {
		fail(w, refuse(http.StatusForbidden, "%s reads their own agents' grants; an admin reads anyone's", who))
		return
	}
	view, es, err := i.Index.agentView(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	pos := view.Position()
	out := []GrantView{}
	ends := map[string]Entry{}
	uses := map[string]Entry{}
	for _, e := range es {
		switch e.Kind {
		case KindGrantEnd:
			if _, ok := ends[e.Grant]; !ok {
				ends[e.Grant] = e
			}
		case KindAgentUse:
			uses[e.Grant] = e
		}
	}
	for _, e := range es {
		if e.Kind != KindAgentGrant || person != "*" && e.Person != person {
			continue
		}
		gv := GrantView{Grant: e.Grant, Agent: e.Agent, Device: e.Device, Person: e.Person, Granted: e.Position,
			GrantedAt: e.At, Position: pos}
		if g, _ := grantIn(view, e.Grant, i.now()); g != nil {
			gv.InForce, gv.Lapses = true, g.Lapses.UTC().Format(time.RFC3339)
		}
		if u, ok := uses[e.Grant]; ok {
			gv.LastUse = u.At
		}
		if end, ok := ends[e.Grant]; ok {
			gv.End, gv.Ended, gv.EndedBy = end.End, end.Position, end.By
		} else if !gv.InForce {
			gv.End = EndLapsed
		}
		out = append(out, gv)
	}
	slices.SortFunc(out, func(a, b GrantView) int { return int(a.Granted - b.Granted) })
	writeJSON(w, http.StatusOK, map[string]any{"farm": i.Farm(), "position": pos, "grants": out})
}
