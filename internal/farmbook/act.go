package farmbook

import (
	"context"
	"encoding/json"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A person's act, as the HUD shows it beside an entry (internal/hud/API.md,
// A person's act): derived when it is served, from the records the entry
// cites and the book before it, never written into the entry, so no
// entry's ID changes for it.

// Act is the person's act that the entry at position pos books, as the HUD
// shows it, or nil when it books none. An entry books a person's act when
// its record is the act (the people's issuer's removals, approvals, codes
// and countersignatures) or is a control record that cites one, the act
// stored beside it. Grade and As are the act's; Kind, Label and Protection
// are the device record in force for the device that signed it, judged as
// a verifier judges the act: by the entries before pos, at the time pos
// counts from (its seal; for a pending entry, the view's now), by the one
// rule (InForceAt). Anything that doesn't establish all of it (no act
// cited, an act not a person's, a record the store lacks or that doesn't
// verify, no device record in force) is nil: never a guess.
func (r *Reader) Act(ctx context.Context, v *View, pos int64) *api.Act {
	s := v.at(pos)
	if s == nil || s.Entry.Record == "" {
		return nil
	}
	rec := r.verified(ctx, s.Entry.Record)
	if rec == nil {
		return nil
	}
	act := rec
	switch rec.PredicateType {
	case provenance.PredicateAct:
	case provenance.PredicateControl:
		var c struct {
			Act *schema.ActCitation `json:"act"`
		}
		if json.Unmarshal(rec.Predicate, &c) != nil || c.Act == nil {
			return nil
		}
		if act = r.verified(ctx, c.Act.Record); act == nil || act.PredicateType != provenance.PredicateAct {
			return nil
		}
		if provenance.CheckCitation(rec, act, c.Act.Record, provenance.NewMemorySeen()) != nil {
			return nil
		}
	default:
		return nil
	}
	a, errs := schema.ParseAct(act.Payload)
	if errs != nil {
		return nil
	}
	person := a.Predicate.Chain.By.Person
	if person == "" || act.Device == "" {
		return nil
	}
	if rec == act && s.Entry.By != person {
		return nil
	}
	in, err := v.InForceAt(ForceDevice, act.Device, pos-1, s.Time)
	if err != nil || in == nil {
		return nil
	}
	d := r.device(ctx, in.Entry.Record)
	if d == nil || d.Person != person || d.Farm != r.Farm {
		return nil
	}
	if spki, err := d.PublicKey(); err != nil || schema.DeviceID(spki) != act.Device {
		return nil
	}
	grade := schema.GradeSigned
	if act.Confirmed() {
		grade = schema.GradeConfirmed
	}
	return &api.Act{Grade: grade, Kind: d.Kind, Label: d.Label, Protection: d.Protection, As: a.Predicate.As}
}

// verified is the record with this C4 ID from the store, verified against
// the farm's roots and signed as its signer policy says (CheckSigner), or
// nil.
func (r *Reader) verified(ctx context.Context, id string) *provenance.Verified {
	if !IsC4ID(id) {
		return nil
	}
	b, err := r.Store.Get(ctx, id)
	if err != nil || c4id(b) != id {
		return nil
	}
	v, err := provenance.VerifyBundle(b, r.Roots)
	if err != nil || provenance.CheckSigner(v) != nil || v.Farm != r.Farm {
		return nil
	}
	return v
}

// device is the device record with this C4 ID, the people's issuer's, or
// nil.
func (r *Reader) device(ctx context.Context, id string) *schema.DevicePredicate {
	v := r.verified(ctx, id)
	if v == nil || v.PredicateType != provenance.PredicateDevice {
		return nil
	}
	d, errs := schema.ParseDevice(v.Payload)
	if errs != nil {
		return nil
	}
	return &d.Predicate
}

// at is the entry at position pos, sealed or pending, or nil.
func (v *View) at(pos int64) *Sealed {
	sealed := int64(len(v.Book.entries))
	switch {
	case pos < 1:
		return nil
	case pos <= sealed:
		return v.Book.entries[pos-1]
	case pos-sealed <= int64(len(v.pending)):
		return v.pending[pos-sealed-1]
	}
	return nil
}

// Booking is the position of the entry booking record: its first sealed
// one, else a pending one; 0 when the view holds none.
func (v *View) Booking(record string) int64 {
	if p, ok := v.Book.PositionOf(record); ok {
		return p
	}
	for _, s := range v.pending {
		if s.Entry.Record == record {
			return s.Entry.Position
		}
	}
	return 0
}
