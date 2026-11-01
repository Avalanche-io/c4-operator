package provenance

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4-operator/schema"
)

// Verifying an act (PEOPLE-KEYS-2 2): who may sign it, which kinds need a
// person or a touch, its clock, taking it once, and the control record
// that cites it. VerifyBundle has checked the signatures and the chain;
// whether a device, a grant or a relay's binding is in force at the act's
// book position is the farm book's, from the device records (K2), the
// grants (K5) and the people map (K6).

// ActSkew is how far an act's signedAt may be from the verifier's clock.
const ActSkew = 5 * time.Minute

// ActNeed is what an act kind asks of its signer beyond its own key.
type ActNeed struct {
	// Person: only a person may make it. One signed by a unit, an agent,
	// a CI job or a relay is refused, whatever the HUD allowed (DESIGN 6,
	// what an agent may never do; PEOPLE-KEYS-2 2.2).
	Person bool
	// Touch: it needs the confirmed grade, the person's touch on their
	// device (PEOPLE-KEYS-2 4.3). A touch-list act is a person's too: no
	// agent, CI job or relay does one, with or without a touch.
	Touch bool
	// Relayed: a person's act that a tracker's relay may carry, naming the
	// person (PEOPLE-KEYS-2 6.1): a decision made in the tracker. No other
	// person's act is relayed.
	Relayed bool
}

var (
	personOnly = ActNeed{Person: true}
	touched    = ActNeed{Person: true, Touch: true}
)

// actNeeds is the product's list, by the verb an act and its control
// record share. A verb not here needs neither: approvals of reviews,
// holds and releases of a hold, boosts, deadlines, submissions, notes and
// everything else (PEOPLE-KEYS-2 4.3, off the list). An organisation or a
// project may raise an act class to confirmed (minGrade, DESIGN 4.1); no
// one can take a verb off this list.
var actNeeds = map[string]ActNeed{
	// Authority given away (4.3), and what DESIGN 6 forbids an agent.
	"grant":           touched, // DESIGN 6: grant
	"extend-grant":    touched,
	"add-member":      touched, // memberships
	"set-purposes":    touched, // and purposes
	"bind":            touched, // DESIGN 6: accept a deployment binding
	"people-confirm":  touched, // a tracker login may speak for the person (6.1)
	"stand-in-policy": touched, // decide.by
	"approve-machine": touched, // a second machine on a one-person farm (7)
	"enroll":          touched, // a host runner: a machine that runs the person's work under their sign-in
	// Who can sign in (3.1).
	"set-sign-in-provider": touched,
	"set-sign-ins":         touched,
	// Leaving the farm: DESIGN 6's send.
	"send":            touched,
	"approve-egress":  touched, // a standing send approval (D10)
	"tag":             touched,
	"publish-release": touched, // a release of work, not a hold's release
	// Money.
	"approve-spend":    touched, // spend over a cap
	"approve-capacity": touched, // a burst approval: spend, which SPEND 4.2 makes approve-spend
	"set-cap":          touched, // raising a cap; the act doesn't show the cap it replaces
	// Accountability: DESIGN 6's adopt a violation.
	"adopt":  touched,
	"disown": touched,
	// Taking authority away is never harder than giving it: a person's,
	// and silent (DESIGN 6: revoke).
	"revoke":        personOnly,
	"people-revoke": personOnly,
}

// ActNeeds is what an act of this kind, with these params, made at this
// time, needs of its signer. Where the need turns on the params and they
// can't be read (an act's params by their C4 ID), the act needs the most
// the kind can.
func ActNeeds(act string, params json.RawMessage, at time.Time) ActNeed {
	if n, ok := actNeeds[act]; ok {
		return n
	}
	var p map[string]any
	readable := json.Unmarshal(params, &p) == nil
	switch act {
	case "decide":
		// Deciding a person's wait is a person's (DESIGN 6). An agent may
		// decide a wait that names it, when the params say whose wait it
		// is; the HUD checks they say it truly. A person may decide in their
		// tracker, and its relay carries the decision (6.1).
		if w, _ := p["waitsFor"].(string); readable && strings.HasPrefix(w, "agent:") {
			return ActNeed{}
		}
		return ActNeed{Person: true, Relayed: true}
	case "accountability":
		// Changing strict accountability is a person's (DESIGN 6), and
		// turning it down needs a touch (4.3). Without the setting it
		// replaces, only turning it fully on is known not to be down.
		if readable && strictestAccountability(p) {
			return personOnly
		}
		return touched
	case "remove-device":
		// Removing a device takes a key's authority away, a revocation, so
		// it is a person's; removing it from a time before the act
		// disowns what it signed since, so that needs a touch (4.3).
		from, _ := p["from"].(string)
		if t, err := time.Parse(time.RFC3339, from); readable && (from == "" || err == nil && !t.Before(at)) {
			return personOnly
		}
		return touched
	}
	return ActNeed{}
}

// strictestAccountability reports whether an accountability setting
// turns strict accountability on and nothing down: mode strict, and any
// on-key refuse.
func strictestAccountability(p map[string]any) bool {
	if p["mode"] != "strict" {
		return false
	}
	for k, v := range p {
		switch {
		case k == "mode":
		case strings.HasPrefix(k, "on") && v == "refuse":
		default:
			return false
		}
	}
	return true
}

// checkAct is CheckSigner's rule for act/v1 (PEOPLE-KEYS-2 2.2): the act
// is signed by the identity in its signer field, which follows chain.by;
// a person's names the device its leaf names, an agent's acts under the
// grant its leaf names; it is graded confirmed exactly when a touch key
// signed it; it was made for the farm it chains to; and a kind that
// needs a person or a touch has one.
func checkAct(v *Verified) (*schema.Act, error) {
	a, errs := schema.ParseAct(v.Payload)
	if errs != nil {
		return nil, fmt.Errorf("not an act: %s: %s", errs[0].Path, errs[0].Message)
	}
	p := &a.Predicate
	by := p.Chain.By
	if v.Identity != p.Signer {
		return nil, fmt.Errorf("signed by %s, but the act names its signer %s", v.Identity, p.Signer)
	}
	if p.Farm != v.Farm {
		return nil, fmt.Errorf("the act was made for farm %s, and is signed under farm %s", p.Farm, v.Farm)
	}
	switch {
	case by.Person != "":
		if want := UserIdentity(by.Person); p.Signer != want {
			return nil, fmt.Errorf("%s's act is signed by %s, not %s", by.Person, want, p.Signer)
		}
		if v.Device == "" || p.Device != v.Device {
			return nil, fmt.Errorf("the act names device %s, its leaf device %q", p.Device, v.Device)
		}
	case by.User != "":
		// A relayed decision: the relay signs. Its binding to the tracker
		// and the people-map confirmation for the person are K6's.
		if !strings.HasPrefix(p.Signer, IdentityBase+"sa/") {
			return nil, fmt.Errorf("a relayed act is signed by its relay, not %s", p.Signer)
		}
	case by.Unit != "":
		if !unitSigns(p.Signer, by.Unit) {
			return nil, fmt.Errorf("unit %s's act is signed by %s, another unit", by.Unit, p.Signer)
		}
	case by.Agent != "":
		if want := AgentIdentity(by.Agent); p.Signer != want {
			return nil, fmt.Errorf("agent %s's act is signed by %s, not %s", by.Agent, want, p.Signer)
		}
		if v.Grant == "" || !underGrant(p.Chain.Under, v.Grant) {
			return nil, fmt.Errorf("agent %s acts under the grant its leaf names (%q), and the act's under must name it", by.Agent, v.Grant)
		}
	default:
		return nil, errors.New("an act is by a person, a unit, an agent or a tracker user")
	}
	switch confirmed := by.Grade == schema.GradeConfirmed; {
	case confirmed && !v.Confirmed():
		return nil, errors.New("the act is graded confirmed, and no touch key signed it")
	case !confirmed && v.Confirmed():
		return nil, fmt.Errorf("a touch key signed the act, and it is graded %s", by.Grade)
	}
	need := ActNeeds(p.Act, p.Params, v.SignedAt)
	if need.Person && by.Person == "" && !(need.Relayed && by.User != "") {
		return nil, fmt.Errorf("%s is a person's act: a unit, an agent, a CI job or a relay may plan it, and a person makes it", p.Act)
	}
	if need.Touch && !v.Confirmed() {
		return nil, fmt.Errorf("%s %w: it is confirmed, signed by the device key and its touch key", p.Act, ErrNeedsTouch)
	}
	return a, nil
}

// ErrNeedsTouch means an act's kind is on the touch list and no touch key
// signed it, so a caller can tell a missing touch from any other refusal.
// checkAct checks it last: an act refused for it alone has passed every
// other rule.
var ErrNeedsTouch = errors.New("needs the person's touch")

// unitSigns reports whether a unit's identity,
// unit/<ns>/<job>/<unit>@<uid>/<attempt>, is the unit a chain names,
// <ns>/<job>@<uid>/<unit> (schema.UnitRef).
func unitSigns(signer, ref string) bool {
	parts := strings.Split(strings.TrimPrefix(signer, IdentityBase+"unit/"), "/")
	if len(parts) != 4 || !strings.HasPrefix(signer, IdentityBase+"unit/") {
		return false
	}
	unit, uid, ok := strings.Cut(parts[2], "@")
	return ok && schema.UnitRef(parts[0], parts[1], uid, unit) == ref
}

func underGrant(under []schema.Delegation, grant string) bool {
	for _, d := range under {
		if d.Grant == grant {
			return true
		}
	}
	return false
}

// ErrSeen means a seen-set holds the key already: an act taken twice, or
// cited twice.
var ErrSeen = errors.New("seen already")

// SeenSet takes each key once. The HUD keeps one of payload hashes, so
// each act is carried out once (PEOPLE-KEYS-2 2.1, replay), and a book's
// verifier one of the acts its control records cite, so none is cited
// twice. MemorySeen keeps one in memory; the people's issuer's index and
// the farm book back one with a store.
type SeenSet interface {
	// Take records key, kept until until (the zero time keeps it for
	// ever), and fails with ErrSeen if it holds key already.
	Take(key string, until time.Time) error
}

// MemorySeen is a SeenSet in memory. It forgets a key once its time has
// passed.
type MemorySeen struct {
	// Now is the clock keys lapse by; nil is time.Now.
	Now func() time.Time

	mu    sync.Mutex
	keys  map[string]time.Time
	sweep int
}

// NewMemorySeen makes an empty seen-set.
func NewMemorySeen() *MemorySeen { return &MemorySeen{} }

// Take records key until until, refusing one it holds.
func (s *MemorySeen) Take(key string, until time.Time) error {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = map[string]time.Time{}
	}
	if u, ok := s.keys[key]; ok && (u.IsZero() || now.Before(u)) {
		return ErrSeen
	}
	if len(s.keys) >= s.sweep {
		maps.DeleteFunc(s.keys, func(_ string, u time.Time) bool { return !u.IsZero() && !now.Before(u) })
		s.sweep = max(1024, 2*len(s.keys))
	}
	s.keys[key] = until
	return nil
}

// ErrClock means an act was signed more than ActSkew from the verifier's
// clock.
var ErrClock = errors.New("the act was signed more than five minutes from this clock")

// VerifyAct checks an act as the farm takes it: the bundle
// (VerifyBundle), an act/v1 record, its signer (CheckSigner), its
// signedAt within ActSkew of now, and taken once by its payload hash,
// kept for as long as its certificates live.
func VerifyAct(data []byte, roots *x509.CertPool, now time.Time, seen SeenSet) (*Verified, *schema.Act, error) {
	v, err := VerifyBundle(data, roots)
	if err != nil {
		return nil, nil, err
	}
	if v.PredicateType != PredicateAct {
		return nil, nil, fmt.Errorf("a %s record, not an act", v.PredicateType)
	}
	a, err := checkAct(v)
	if err != nil {
		return nil, nil, err
	}
	if d := now.Sub(v.SignedAt); d > ActSkew || d < -ActSkew {
		return nil, nil, fmt.Errorf("%w: signed at %s, %s from %s", ErrClock, v.SignedAt.UTC().Format(time.RFC3339), d.Round(time.Second), now.UTC().Format(time.RFC3339))
	}
	until := v.Expires
	if t := v.SignedAt.Add(ActSkew); t.After(until) {
		until = t
	}
	if err := seen.Take(PayloadHash(v.Payload), until); err != nil {
		return nil, nil, fmt.Errorf("the act was taken once already (payload %.16s): %w", PayloadHash(v.Payload), err)
	}
	return v, a, nil
}

// ErrUnattributed means a control record cites no act, or one it doesn't
// match: unattributed, a violation in every accountability mode
// (PEOPLE-KEYS-2 2.2).
var ErrUnattributed = errors.New("unattributed")

// controlCitation is what a control record says of the act it carried
// out: its action, the act it cites, and the request's params as the
// actor signed them.
type controlCitation struct {
	Action string              `json:"action"`
	Act    *schema.ActCitation `json:"act"`
	Params json.RawMessage     `json:"params"`
}

// CheckCitation checks a control record against the act it cites
// (PEOPLE-KEYS-2 10.4 item 9): the act's payload hash and record, read by
// actRecord, its C4 ID; the same subject, by UID and digest; the same
// action; and the same params by ParamsID. cited takes each act once, so
// a second citation of one act is refused. Both records have verified,
// and CheckSigner passed each.
func CheckCitation(control, act *Verified, actRecord string, cited SeenSet) error {
	if control.PredicateType != PredicateControl || act.PredicateType != PredicateAct {
		return fmt.Errorf("a control record cites an act, not a %s record a %s", control.PredicateType, act.PredicateType)
	}
	var c controlCitation
	if err := json.Unmarshal(control.Predicate, &c); err != nil {
		return err
	}
	if c.Act == nil {
		return fmt.Errorf("%w: the control record cites no act", ErrUnattributed)
	}
	a, errs := schema.ParseAct(act.Payload)
	if errs != nil {
		return fmt.Errorf("not an act: %s: %s", errs[0].Path, errs[0].Message)
	}
	hash := PayloadHash(act.Payload)
	switch {
	case c.Act.Payload != hash:
		return fmt.Errorf("%w: the control record cites act payload %.16s, not %.16s", ErrUnattributed, c.Act.Payload, hash)
	case c.Act.Record != actRecord:
		return fmt.Errorf("%w: the control record cites act record %s, not %s", ErrUnattributed, c.Act.Record, actRecord)
	case !sameSubjects(control.Subjects, act.Subjects):
		return fmt.Errorf("%w: the control record is on %v, the act on %v", ErrUnattributed, control.Subjects, act.Subjects)
	case c.Action != a.Predicate.Act:
		return fmt.Errorf("%w: the control record carried out %s, the act is %s", ErrUnattributed, c.Action, a.Predicate.Act)
	}
	want, err := schema.ParamsID(a.Predicate.Params)
	if err != nil {
		return fmt.Errorf("the act's params: %w", err)
	}
	got, err := schema.ParamsID(c.Params)
	if err != nil || got != want {
		return fmt.Errorf("%w: the control record's params are not the act's", ErrUnattributed)
	}
	if err := cited.Take(hash, time.Time{}); err != nil {
		return fmt.Errorf("act %.16s is cited twice: %w", hash, err)
	}
	return nil
}

// sameSubjects reports whether two records are on exactly the same
// subjects, by name and every digest.
func sameSubjects(a, b []Subject) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || !maps.Equal(a[i].Digest, b[i].Digest) {
			return false
		}
	}
	return true
}

// CheckTouchKey checks a confirmed act's touch key against the device
// records in force at the act's book position, which the people's issuer
// keeps (K2): the touch key's record is the same person's, on the same
// farm, and names the device key that signed first as the one it
// confirms for (touchFor); a passkey's touch was made at the relying
// party and origin its record names. An act that isn't confirmed passes.
func CheckTouchKey(v *Verified, devices []*schema.Device) error {
	if !v.Confirmed() {
		return nil
	}
	person, ok := strings.CutPrefix(v.Identity, IdentityBase+"user/")
	if !ok {
		return fmt.Errorf("only a person confirms an act, not %s", v.Identity)
	}
	if p, err := url.PathUnescape(person); err == nil {
		person = p
	}
	for _, d := range devices {
		spki, err := d.Predicate.PublicKey()
		if err != nil || schema.DeviceID(spki) != v.TouchDevice {
			continue
		}
		switch {
		case d.Predicate.Farm != v.Farm:
			return fmt.Errorf("touch key %s is enrolled at another farm", v.TouchDevice)
		case d.Predicate.Person != person:
			return fmt.Errorf("touch key %s is %s's, the act %s's", v.TouchDevice, d.Predicate.Person, person)
		case d.Predicate.TouchFor != v.Device:
			return fmt.Errorf("key %s is not a touch key for device %s (its record names %q)", v.TouchDevice, v.Device, d.Predicate.TouchFor)
		}
		// A passkey's touch is held to the relying party and origin its
		// record was enrolled at, so a passkey used from another page is
		// refused; a passkey never signs raw.
		switch w := d.Predicate.WebAuthn; {
		case w == nil && v.TouchAssertion != nil:
			return fmt.Errorf("touch key %s signed as a passkey, and its record is not a passkey's", v.TouchDevice)
		case w != nil && v.TouchAssertion == nil:
			return fmt.Errorf("touch key %s is a passkey, which signs only as WebAuthn", v.TouchDevice)
		case w != nil:
			if err := v.TouchAssertion.CheckRP(w.RPID, w.Origin); err != nil {
				return fmt.Errorf("touch key %s: %w", v.TouchDevice, err)
			}
		}
		return nil
	}
	return fmt.Errorf("no device record makes %s a touch key", v.TouchDevice)
}
