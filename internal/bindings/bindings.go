// Package bindings finds the deployment binding in force for a signer
// (accountability DESIGN 3.3, S2b): the issuer certifies a deployment
// under it, naming it in the leaf, and a verifier follows a leaf's binding
// to its owner.
//
// A binding is in force now when its DeploymentBinding says so (started,
// not ended early, not past its end) and, on a farm that keeps a farm
// book, the book does too (farmbook.InForceAt, pending entries counted),
// once the book holds the binding's record. A binding made before its
// farm kept a book is judged by the object alone, so a farm bound before
// the book stays bound.
//
// The most specific binding in force wins: one whose to names the
// signer's deployment exactly, byte for byte (<farm>/node/laptop-1 for
// https://cccc.io/id/node/laptop-1), else the farm binding (<farm>/*). No
// other prefix or glob counts.
package bindings

import (
	"context"
	"crypto/x509"
	"fmt"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// Lookup is what a binding's in force is judged against: the objects, and
// the farm book's view when the farm keeps one.
type Lookup struct {
	Items []c4v1.DeploymentBinding
	// View is the farm book with its pending entries, or nil.
	View *farmbook.View
	Now  time.Time
}

// InForce reports whether b is in force at the lookup's time.
func (l *Lookup) InForce(b *c4v1.DeploymentBinding) bool {
	s := &b.Spec
	switch {
	case s.Record == "" || b.Ended != nil || l.Now.Before(s.Start.Time):
		return false
	case s.End != nil && l.Now.After(s.End.Time):
		return false
	case l.View == nil || l.View.Booking(s.Record) == 0:
		// no book, or a binding made before it: the object says
		return true
	}
	in, err := l.View.InForceAt(farmbook.ForceBinding, farmbook.BindingSubject(b.Name), l.View.Position(), l.Now)
	return err == nil && in != nil && in.Entry.Record == s.Record
}

// Farm is the farm binding in force, or nil.
func (l *Lookup) Farm() *c4v1.DeploymentBinding {
	for i := range l.Items {
		if b := &l.Items[i]; b.Spec.FarmWide() && l.InForce(b) {
			return b
		}
	}
	return nil
}

// For is the binding in force for identity, the most specific: the one
// naming its deployment exactly, else the farm binding; nil for an
// identity none covers.
func (l *Lookup) For(identity string) *c4v1.DeploymentBinding {
	dep := provenance.IdentityDeployment(identity)
	if dep == "" {
		return nil
	}
	farm := l.Farm()
	var exact *c4v1.DeploymentBinding
	for i := range l.Items {
		b := &l.Items[i]
		if b.Spec.FarmWide() || c4v1.DeploymentOf(b.Spec.To) != dep || !l.InForce(b) {
			continue
		}
		if farm != nil && c4v1.FarmOf(b.Spec.To) != c4v1.FarmOf(farm.Spec.To) {
			continue
		}
		if exact == nil || b.Spec.Start.After(exact.Spec.Start.Time) {
			exact = b
		}
	}
	if exact != nil {
		return exact
	}
	return farm
}

// Of is the binding with this record, or nil.
func (l *Lookup) Of(record string) *c4v1.DeploymentBinding {
	for i := range l.Items {
		if l.Items[i].Spec.Record == record && record != "" {
			return &l.Items[i]
		}
	}
	return nil
}

// Covers reports whether binding b names identity's deployment: exactly,
// or as the farm binding.
func Covers(b *c4v1.DeploymentBinding, identity string) bool {
	dep := provenance.IdentityDeployment(identity)
	return dep != "" && (b.Spec.FarmWide() || c4v1.DeploymentOf(b.Spec.To) == dep)
}

// Signer follows a record no book names yet (a task, resolution,
// decision, job or import record, kept in a project's store) to its
// binding and owner: the binding its leaf names, a binding of this farm
// naming its signer's deployment. With no book position it is never
// judged revoked; the issuer stopped certifying under a binding at its
// end, so a leaf naming one was issued while it was in force.
func (l *Lookup) Signer(identity, id string) *api.SignerBinding {
	out := &api.SignerBinding{Binding: id, Label: farmbook.LabelUnattributed}
	b := l.Of(id)
	if b == nil || !Covers(b, identity) {
		return out
	}
	out.Owner, out.Deployment, out.Label = b.Spec.Owner, c4v1.DeploymentOf(b.Spec.To), ""
	return out
}

// Bound is a binding as the issuer certifies under it.
func Bound(b *c4v1.DeploymentBinding) provenance.Bound {
	if b == nil {
		return provenance.Bound{}
	}
	out := provenance.Bound{ID: b.Spec.Record}
	if b.Spec.End != nil {
		out.End = b.Spec.End.Time
	}
	return out
}

// Resolver reads the bindings and the farm book to answer for a signer.
type Resolver struct {
	// Client reads the DeploymentBindings; an uncached reader, so an end
	// counts at once.
	Client client.Reader
	// Book is the farm book's reader, or nil on a farm that keeps none.
	Book *farmbook.Reader
	Now  func() time.Time
}

// Lookup reads the bindings and the book as of now.
func (r *Resolver) Lookup(ctx context.Context) (*Lookup, error) {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	var list c4v1.DeploymentBindingList
	if err := r.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("reading the deployment bindings: %w", err)
	}
	l := &Lookup{Items: list.Items, Now: now}
	if r.Book != nil && len(list.Items) > 0 {
		v, err := r.Book.View(ctx, now)
		if err != nil {
			return nil, fmt.Errorf("the farm book: %w", err)
		}
		l.View = v
	}
	return l, nil
}

// Bind is a provenance.BindFunc: the binding in force now for identity.
func (r *Resolver) Bind(ctx context.Context, identity string) (provenance.Bound, error) {
	l, err := r.Lookup(ctx)
	if err != nil {
		return provenance.Bound{}, err
	}
	return Bound(l.For(identity)), nil
}

// Stale is a component signer's renewal check (provenance.Signer
// RenewWhen): its leaf is stale when the binding in force for it isn't the
// one it names, or ends before it does. It asks at most once each every,
// and never on a failed read.
func (r *Resolver) Stale(every time.Duration) func(leaf *x509.Certificate) bool {
	var mu sync.Mutex
	var last time.Time
	return func(leaf *x509.Certificate) bool {
		mu.Lock()
		now := time.Now()
		if now.Sub(last) < every {
			mu.Unlock()
			return false
		}
		last = now
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b, err := r.Bind(ctx, provenance.CertIdentity(leaf))
		if err != nil {
			return false
		}
		return b.ID != provenance.CertBinding(leaf) || !b.End.IsZero() && leaf.NotAfter.After(b.End)
	}
}
