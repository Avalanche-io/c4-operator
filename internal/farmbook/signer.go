package farmbook

import (
	"context"
	"encoding/json"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// Labels a record's signer gets where its binding leads to no owner
// (accountability DESIGN 3.3, 4.3, 5.4). Every slice before strict
// accountability (K8) labels; none refuses.
const (
	// LabelUnattributed: the signer's certificate names no binding, or
	// one that wasn't in force for it at the record's book position.
	LabelUnattributed = "unattributed: deployment has no owner"
	// LabelRevoked: the record was booked after its binding ended early,
	// by an end-binding or a binding that followed it. Certificates
	// issued before the end live out their hour or day; what they sign
	// after it is booked, and labelled.
	LabelRevoked = "revoked"
)

// Signer follows the record the entry at pos books to its deployment
// binding and owner, as a verifier does (DESIGN 3.3): certificate (the
// binding its leaf names, Verified.Binding), binding (booked before pos,
// naming the signer's deployment exactly or as the farm binding, and in
// force at pos by the one rule, at the time pos counts from), owner. A
// record that doesn't verify, or an entry booking none, is nil: never a
// guess.
func (r *Reader) Signer(ctx context.Context, v *View, pos int64) *api.SignerBinding {
	s := v.at(pos)
	if s == nil || s.Entry.Record == "" {
		return nil
	}
	rec := r.verified(ctx, s.Entry.Record)
	if rec == nil {
		return nil
	}
	return r.judgeBinding(ctx, v, rec.Identity, rec.Binding, pos, s)
}

func (r *Reader) judgeBinding(ctx context.Context, v *View, identity, id string, pos int64, s *Sealed) *api.SignerBinding {
	out := &api.SignerBinding{Binding: id, Label: LabelUnattributed}
	if id == "" {
		return out
	}
	bp := v.Booking(id)
	if bp == 0 || bp >= pos {
		return out
	}
	opened := v.at(bp)
	if opened == nil || opened.Entry.Kind != KindControl || opened.Entry.Act != "bind" {
		return out
	}
	br := r.verified(ctx, id)
	if br == nil {
		return out
	}
	var c struct {
		Change bindingChange `json:"change"`
	}
	if json.Unmarshal(br.Predicate, &c) != nil || !bindingTo.MatchString(c.Change.To) {
		return out
	}
	dep := c4v1.DeploymentOf(c.Change.To)
	if want := provenance.IdentityDeployment(identity); want == "" || dep != "*" && dep != want {
		// a binding of another deployment
		return out
	}
	out.Owner, out.Deployment = c.Change.Owner, dep
	subject := opened.Entry.Subject
	in, err := v.InForceAt(ForceBinding, subject, pos-1, s.Time)
	if err == nil && in != nil && in.Entry.Record == id {
		out.Label = ""
		return out
	}
	for p := bp + 1; p < pos; p++ {
		e := v.at(p)
		if e == nil || e.Entry.Kind != KindControl {
			continue
		}
		if e.Entry.Act == "end-binding" && e.Entry.Subject == subject || e.Entry.Act == "bind" && e.Entry.Ends == subject {
			out.Label = LabelRevoked
			return out
		}
	}
	return out
}
