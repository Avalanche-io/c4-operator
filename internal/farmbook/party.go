package farmbook

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// A Party's memberships (accountability ORGANIZATION 2) are in force by
// the one rule (InForceAt, ForceMembership); what each grants is the
// purposes its opening record names. The HUD and the people's issuer both
// read them here.

// MemberPurposes is what the membership on put in force grants: the
// purposes its record names. The record is read from store and checked
// as the HUD's control record on farm, saying what its entry says, since
// an entry pending the seal has not been checked by the queue yet.
func MemberPurposes(ctx context.Context, store Store, roots *x509.CertPool, farm string, on *Sealed) ([]string, error) {
	rec, err := store.Get(ctx, on.Entry.Record)
	if err != nil {
		return nil, err
	}
	if c4id(rec) != on.Entry.Record {
		return nil, fmt.Errorf("record %.16s hashes to %.16s", on.Entry.Record, c4id(rec))
	}
	vr, err := provenance.VerifyBundle(rec, roots)
	if err != nil {
		return nil, fmt.Errorf("record %.16s does not verify: %v", on.Entry.Record, err)
	}
	if err := provenance.CheckSigner(vr); err != nil || vr.PredicateType != provenance.PredicateControl {
		return nil, fmt.Errorf("record %.16s is not the HUD's control record: %v", on.Entry.Record, err)
	}
	if farm != "" && vr.Farm != farm {
		return nil, fmt.Errorf("record %.16s is signed under farm %.16s", on.Entry.Record, vr.Farm)
	}
	var p struct {
		Action string `json:"action"`
		Target string `json:"target"`
		Change struct {
			Purposes []string `json:"purposes"`
		} `json:"change"`
	}
	if err := json.Unmarshal(vr.Predicate, &p); err != nil {
		return nil, err
	}
	if p.Action != on.Entry.Act || p.Target != on.Entry.Subject {
		return nil, fmt.Errorf("record %.16s is %s on %s, and its entry %s on %s", on.Entry.Record, p.Action, p.Target, on.Entry.Act, on.Entry.Subject)
	}
	return p.Change.Purposes, nil
}

// HoldsByName says whether person holds purpose for party by a membership
// in their own name, in force in view at its Now: the question the
// people's issuer asks of a farm owner's admins, who are named, never a
// group (it knows a person, not their groups).
func HoldsByName(ctx context.Context, view *View, store Store, roots *x509.CertPool, party, person, purpose string) (bool, error) {
	on, err := view.InForceAt(ForceMembership, MemberKey(party, person), view.Position(), view.Now)
	if err != nil || on == nil {
		return false, err
	}
	purposes, err := MemberPurposes(ctx, store, roots, view.Book.Farm, on)
	if err != nil {
		return false, err
	}
	return slices.Contains(purposes, purpose), nil
}
