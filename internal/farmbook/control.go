package farmbook

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// ControlActs are the farm-level acts the HUD signs as control records and
// books (DESIGN 5.5), and the field of each record's change that names the
// person it is about:
//   - bind: a deployment binding, the farm's or one deployment's, to its
//     owner, and a binding that follows another (an owner change);
//   - end-binding: a binding ended early, by its owner or the farm owner;
//   - grant and revoke: a person's submit grant to a tracker;
//   - people-confirm and people-revoke: the people map, a tracker login
//     confirmed as a person and that confirmation ended;
//   - enroll-offer and enroll: a host node offered and enrolled;
//   - disable and enable: a node taken out of scheduling and put back.
//
// Only signed acts are booked. A change made around C4 isn't, and comes
// in slice 2's violations as one (S2c). Tool publishing and Project, Pool
// and Tracker changes join as each gets its signed path.
var ControlActs = map[string]string{
	"bind":           "owner",
	"end-binding":    "owner",
	"grant":          "person",
	"revoke":         "person",
	"people-confirm": "person",
	"people-revoke":  "person",
	"enroll-offer":   "",
	"enroll":         "for",
	"disable":        "",
	"enable":         "",
}

// controlRecord is what a control record (control/v1) says that its entry
// repeats.
type controlRecord struct {
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Change json.RawMessage `json:"change"`
}

// ControlEntry is the entry that books a control record: its action,
// target, actor, person, expiry and change, read from the record itself,
// so the HUD writes what the record says and the queue can check it did.
// It is pending, written at at.
func ControlEntry(record []byte, at time.Time) (*Entry, error) {
	// The HUD made the record a moment ago: its payload is read here, and
	// the queue verifies it at the seal.
	var b provenance.Bundle
	if err := json.Unmarshal(record, &b); err != nil {
		return nil, fmt.Errorf("not a record: %w", err)
	}
	payload, err := base64.StdEncoding.DecodeString(b.DSSEEnvelope.Payload)
	if err != nil {
		return nil, fmt.Errorf("the record's payload: %w", err)
	}
	var st provenance.AnyStatement
	if err := json.Unmarshal(payload, &st); err != nil || st.PredicateType != provenance.PredicateControl {
		return nil, fmt.Errorf("not a control record (%s): %v", st.PredicateType, err)
	}
	e, err := controlEntry(st.Predicate)
	if err != nil {
		return nil, err
	}
	e.Order, e.At, e.Record = OrderPending, at.UTC().Format(time.RFC3339Nano), c4id(record)
	return e, nil
}

// controlEntry is the entry a control record's predicate makes.
func controlEntry(pred json.RawMessage) (*Entry, error) {
	var c controlRecord
	if err := json.Unmarshal(pred, &c); err != nil {
		return nil, fmt.Errorf("not a control record: %w", err)
	}
	field, ok := ControlActs[c.Action]
	if !ok {
		return nil, fmt.Errorf("%q is not a farm-level act the book takes", c.Action)
	}
	var change map[string]any
	json.Unmarshal(c.Change, &change)
	settings := ""
	if len(c.Change) > 0 && string(c.Change) != "null" {
		var err error
		if _, settings, err = canonical(c.Change); err != nil {
			return nil, fmt.Errorf("the control record's change: %w", err)
		}
	}
	e := &Entry{Kind: KindControl, Act: c.Action, Subject: c.Target, By: c.Actor, Settings: settings}
	if field != "" {
		e.Person, _ = change[field].(string)
	}
	switch c.Action {
	case "grant", "bind":
		key := map[string]string{"grant": "until", "bind": "end"}[c.Action]
		if s, _ := change[key].(string); s != "" {
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return nil, fmt.Errorf("the %s's %s %q: %w", c.Action, key, s, err)
			}
			e.Expires = t.UTC().Format(time.RFC3339Nano)
		}
		if c.Action == "bind" {
			e.Ends, _ = change["ends"].(string)
		}
	}
	return e, nil
}

// ControlChecker checks control entries for the sealer: each cites a
// control record in the book's store that verifies against the farm's
// roots, is the HUD's (component/hud, CheckSigner) on this farm, books a
// farm-level act, says the same as the entry (act, subject, actor, person,
// expiry and change), and isn't booked already.
type ControlChecker struct {
	Farm  string
	Roots *x509.CertPool
	Store Store

	booked   map[string]bool
	bindings bindings
}

func (c *ControlChecker) Kinds() []string { return []string{KindControl} }

func (c *ControlChecker) Admit(ctx context.Context, raw []byte, p Placement) ([]byte, time.Time, error) {
	var e Entry
	if err := Strict(raw, &e); err != nil {
		return nil, time.Time{}, err
	}
	if err := placeable(&e, p); err != nil {
		return nil, time.Time{}, err
	}
	if e.Kind != KindControl {
		return nil, time.Time{}, Refuse(ReasonUnknownKind, errors.New("not a control entry"))
	}
	if !IsC4ID(e.Record) {
		return nil, time.Time{}, Refuse(ReasonRecordGone, errors.New("a control entry cites its control record by C4 ID"))
	}
	if c.booked[e.Record] {
		return nil, time.Time{}, Refuse(ReasonBooked, fmt.Errorf("control record %.16s is booked already", e.Record))
	}
	rec, err := c.Store.Get(ctx, e.Record)
	if err != nil {
		return nil, time.Time{}, Refuse(ReasonRecordGone, fmt.Errorf("control record %.16s: %v", e.Record, err))
	}
	if got := c4id(rec); got != e.Record {
		return nil, time.Time{}, Refuse(ReasonRecordGone, fmt.Errorf("control record %.16s hashes to %.16s", e.Record, got))
	}
	v, err := provenance.VerifyBundle(rec, c.Roots)
	if err != nil {
		return nil, time.Time{}, Refuse(ReasonUnverified, fmt.Errorf("control record %.16s doesn't verify: %v", e.Record, err))
	}
	if v.PredicateType != provenance.PredicateControl {
		return nil, time.Time{}, Refuse(ReasonUnverified, fmt.Errorf("a control entry cites a %s record", v.PredicateType))
	}
	if err := provenance.CheckSigner(v); err != nil {
		return nil, time.Time{}, Refuse(ReasonWrongSigner, fmt.Errorf("control record %.16s: %v", e.Record, err))
	}
	if v.Farm != c.Farm {
		return nil, time.Time{}, Refuse(ReasonOtherFarm, fmt.Errorf("control record %.16s is signed under farm %.16s", e.Record, v.Farm))
	}
	want, err := controlEntry(v.Predicate)
	if err != nil {
		return nil, time.Time{}, Refuse(ReasonRejected, err)
	}
	for _, d := range []struct{ what, entry, record string }{
		{"the act", e.Act, want.Act}, {"the subject", e.Subject, want.Subject}, {"who acted", e.By, want.By},
		{"the person", e.Person, want.Person}, {"the expiry", e.Expires, want.Expires}, {"the change", e.Settings, want.Settings},
		{"the binding it ends", e.Ends, want.Ends},
		{"the device", e.Device, ""}, {"the touch key", e.TouchFor, ""},
	} {
		if d.entry != d.record {
			return nil, time.Time{}, Refuse(ReasonDisagrees, fmt.Errorf("the entry and its control record disagree on %s: %.80q, and the record %.80q", d.what, d.entry, d.record))
		}
	}
	apply, err := c.bindings.check(&e, v.Predicate, p.Time)
	if err != nil {
		return nil, time.Time{}, Refuse(ReasonRejected, err)
	}
	e.Farm, e.Position, e.Prev, e.Order = p.Farm, p.Position, p.Prev, p.Order
	sealed, _, err := canonical(&e)
	if err != nil {
		return nil, time.Time{}, err
	}
	if c.booked == nil {
		c.booked = map[string]bool{}
	}
	c.booked[e.Record] = true
	if apply != nil {
		apply()
	}
	return sealed, v.SignedAt, nil
}

// placeable checks an entry may be placed at p: written pending, with no
// position or entry before it and this farm's or no farm; or, given again
// as sealed, already at p.
func placeable(e *Entry, p Placement) error {
	pending := e.Order == OrderPending && e.Position == 0 && e.Prev == "" && (e.Farm == "" || e.Farm == p.Farm)
	placed := e.Order == p.Order && e.Position == p.Position && e.Prev == p.Prev && e.Farm == p.Farm
	if !pending && !placed {
		return Refuse(ReasonNotPending, errors.New("an entry is written pending, with no position"))
	}
	return nil
}

// IsControlAct reports whether the book takes a control record of this
// action.
func IsControlAct(action string) bool {
	_, ok := ControlActs[action]
	return ok
}
