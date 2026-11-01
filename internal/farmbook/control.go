package farmbook

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// ControlActs are the farm-level acts the HUD signs as control records and
// books (DESIGN 5.5), and the field of each record's change that names the
// person it is about:
//   - bind: the farm binding to its owner;
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
	if c.Action == "grant" {
		if s, _ := change["until"].(string); s != "" {
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return nil, fmt.Errorf("the grant's until %q: %w", s, err)
			}
			e.Expires = t.UTC().Format(time.RFC3339Nano)
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

	booked map[string]bool
}

func (c *ControlChecker) Kinds() []string { return []string{KindControl} }

func (c *ControlChecker) Admit(ctx context.Context, e *Entry, _ []byte) (time.Time, error) {
	if e.Record == "" {
		return time.Time{}, fmt.Errorf("a control entry cites its control record")
	}
	if c.booked[e.Record] {
		return time.Time{}, fmt.Errorf("control record %.16s is booked already", e.Record)
	}
	rec, err := c.Store.Get(ctx, e.Record)
	if err != nil {
		return time.Time{}, fmt.Errorf("control record %.16s: %v", e.Record, err)
	}
	if got := c4id(rec); got != e.Record {
		return time.Time{}, fmt.Errorf("control record %.16s hashes to %.16s", e.Record, got)
	}
	v, err := provenance.VerifyBundle(rec, c.Roots)
	if err != nil {
		return time.Time{}, fmt.Errorf("control record %.16s doesn't verify: %v", e.Record, err)
	}
	if v.PredicateType != provenance.PredicateControl {
		return time.Time{}, fmt.Errorf("a control entry cites a %s record", v.PredicateType)
	}
	if err := provenance.CheckSigner(v); err != nil {
		return time.Time{}, fmt.Errorf("control record %.16s: %v", e.Record, err)
	}
	if v.Farm != c.Farm {
		return time.Time{}, fmt.Errorf("control record %.16s is signed under farm %.16s", e.Record, v.Farm)
	}
	want, err := controlEntry(v.Predicate)
	if err != nil {
		return time.Time{}, err
	}
	for _, d := range []struct{ what, entry, record string }{
		{"the act", e.Act, want.Act}, {"the subject", e.Subject, want.Subject}, {"who acted", e.By, want.By},
		{"the person", e.Person, want.Person}, {"the expiry", e.Expires, want.Expires}, {"the change", e.Settings, want.Settings},
		{"the device", e.Device, ""},
	} {
		if d.entry != d.record {
			return time.Time{}, fmt.Errorf("the entry and its control record disagree on %s: %q, and the record %q", d.what, d.entry, d.record)
		}
	}
	if c.booked == nil {
		c.booked = map[string]bool{}
	}
	c.booked[e.Record] = true
	return v.SignedAt, nil
}

// IsControlAct reports whether the book takes a control record of this
// action.
func IsControlAct(action string) bool {
	_, ok := ControlActs[action]
	return ok
}
