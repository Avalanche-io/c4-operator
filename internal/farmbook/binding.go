package farmbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// Deployment bindings at the seal (DESIGN 3.3, S2b). The HUD checks each
// bind and end-binding before it signs one; the queue checks them again
// here, against the bindings the book holds, so a binding the HUD should
// have refused is never sealed:
//   - a binding for one deployment is under the farm binding in force, and
//     its for only narrows what the signer policy lets that deployment
//     sign (provenance.NarrowsOnly): a node never signs control records;
//   - two bindings for one deployment never overlap: a new one either
//     finds none in force, or follows the one in force and ends it at its
//     own position, the changeover;
//   - an end-binding ends a binding in force, not the farm binding, and
//     names its true owner, which decides whether the act needed a touch
//     (provenance.ActNeedsBy).

// BindingSubject is what a binding's control records are on, and the key
// it is in force by (ForceBinding): deploymentbinding:<name>.
func BindingSubject(name string) string { return "deploymentbinding:" + name }

var bindingTo = regexp.MustCompile(c4v1.BindingToPattern)

// bindState is a binding as the entries before leave it.
type bindState struct {
	subject, record, to, owner string
	expires                    time.Time
	open                       bool
}

// inForce reports whether the binding is open and not past its end at t.
func (b *bindState) inForce(t time.Time) bool {
	return b != nil && b.open && (b.expires.IsZero() || !t.After(b.expires))
}

// bindings is what a control checker keeps of the bindings booked.
type bindings struct {
	subjects map[string]*bindState
	records  map[string]*bindState
}

// farm is the farm binding in force at t, or nil.
func (bs *bindings) farm(t time.Time) *bindState {
	for _, b := range bs.subjects {
		if c4v1.DeploymentOf(b.to) == "*" && b.inForce(t) {
			return b
		}
	}
	return nil
}

// bindingChange is what a bind or an end-binding record's change says.
type bindingChange struct {
	Owner   string   `json:"owner"`
	To      string   `json:"to"`
	For     []string `json:"for"`
	Follows string   `json:"follows"`
	Ends    string   `json:"ends"`
	Binding string   `json:"binding"`
}

// errBinding marks a binding the seal refuses.
var errBinding = errors.New("binding")

// check checks a bind or end-binding entry against the bindings before
// it, at t, and returns how to apply it once it is admitted; nil, nil
// for any other act.
func (bs *bindings) check(e *Entry, pred json.RawMessage, t time.Time) (func(), error) {
	if e.Act != "bind" && e.Act != "end-binding" {
		return nil, nil
	}
	var rec struct {
		Change bindingChange `json:"change"`
	}
	if err := json.Unmarshal(pred, &rec); err != nil {
		return nil, fmt.Errorf("%w: its change: %v", errBinding, err)
	}
	ch := rec.Change
	if !strings.HasPrefix(e.Subject, BindingSubject("")) {
		return nil, fmt.Errorf("%w: a %s is on a deploymentbinding, not %.80q", errBinding, e.Act, e.Subject)
	}
	if e.Act == "end-binding" {
		return bs.checkEnd(e, ch, t)
	}
	if !bindingTo.MatchString(ch.To) {
		return nil, fmt.Errorf("%w: to %.80q is neither <farm>/* nor one deployment of the farm", errBinding, ch.To)
	}
	nb := &bindState{subject: e.Subject, record: e.Record, to: ch.To, owner: ch.Owner, open: true, expires: e.expires()}
	add := func() {
		bs.subjects[nb.subject], bs.records[nb.record] = nb, nb
	}
	if bs.subjects == nil {
		bs.subjects, bs.records = map[string]*bindState{}, map[string]*bindState{}
	}
	dep := c4v1.DeploymentOf(ch.To)
	if dep == "*" {
		// the farm binding, as before bindings for one deployment
		if ch.Follows != "" || e.Ends != "" {
			return nil, fmt.Errorf("%w: the farm binding's owner change is not built yet", errBinding)
		}
		return add, nil
	}
	farm := bs.farm(t)
	switch {
	case farm == nil:
		return nil, fmt.Errorf("%w: %s is bound before the farm is: no farm binding is in force", errBinding, ch.To)
	case c4v1.FarmOf(ch.To) != c4v1.FarmOf(farm.to):
		return nil, fmt.Errorf("%w: %s is not a deployment of farm %s", errBinding, ch.To, c4v1.FarmOf(farm.to))
	case len(ch.For) == 0:
		return nil, fmt.Errorf("%w: %s's binding is for no record kind", errBinding, ch.To)
	}
	if wider := provenance.NarrowsOnly(dep, ch.For); len(wider) > 0 {
		return nil, fmt.Errorf("%w: %s may not sign %s: a binding only narrows what the signer policy lets a deployment sign", errBinding, dep, strings.Join(wider, ", "))
	}
	var old *bindState
	for _, b := range bs.subjects {
		if b.to == ch.To && b.subject != e.Subject && b.inForce(t) {
			old = b
		}
	}
	switch {
	case ch.Follows == "" && e.Ends == "" && old != nil:
		return nil, fmt.Errorf("%w: %s is bound already (%.16s): a new owner follows that binding", errBinding, ch.To, old.record)
	case ch.Follows == "" && e.Ends == "":
		return add, nil
	case old == nil || ch.Follows != old.record || e.Ends != old.subject:
		return nil, fmt.Errorf("%w: it follows %.16s and ends %.80q, and the binding in force for %s is not that one", errBinding, ch.Follows, e.Ends, ch.To)
	}
	return func() { add(); old.open = false }, nil
}

// checkEnd checks an end-binding: it ends a binding in force, not the
// farm binding, and names that binding and its owner truly.
func (bs *bindings) checkEnd(e *Entry, ch bindingChange, t time.Time) (func(), error) {
	b := bs.subjects[e.Subject]
	switch {
	case !b.inForce(t):
		return nil, fmt.Errorf("%w: %s is not a binding in force", errBinding, e.Subject)
	case c4v1.DeploymentOf(b.to) == "*":
		return nil, fmt.Errorf("%w: the farm binding is not ended: a new farm owner follows it", errBinding)
	case ch.Binding != b.record:
		return nil, fmt.Errorf("%w: it ends binding %.16s, and %s is %.16s", errBinding, ch.Binding, e.Subject, b.record)
	case ch.Owner != b.owner || e.Person != b.owner:
		return nil, fmt.Errorf("%w: it names %.80q as the binding's owner, and its owner is %s", errBinding, ch.Owner, b.owner)
	}
	return func() { b.open = false }, nil
}
