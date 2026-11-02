package farmbook_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"os"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
)

func recordID(rec []byte) string { return c4.Identify(bytes.NewReader(rec)).String() }

// boundSigner signs as identity with a leaf naming binding (its record's
// C4 ID), as the issuer certifies a deployment under it.
func boundSigner(t *testing.T, f *provtest.Farm, identity, binding string) *provenance.Signer {
	t.Helper()
	s, err := provenance.NewSigner(context.Background(), func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		c, err := f.CA.IssueBound(pub, identity, provenance.Bound{ID: binding}, 48*time.Hour, f.Now)
		if err != nil {
			return nil, err
		}
		return []*x509.Certificate{c, f.CA.Cert}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func bindChange(owner, to string, kinds ...string) map[string]any {
	return map[string]any{"owner": owner, "to": to, "right": "sign", "for": kinds, "by": "dana"}
}

// refusedFor is the reason the seal refused the entry id, or "".
func refusedFor(res *farmbook.SealResult, id string) string {
	for _, f := range res.Refused {
		if f.Submission == id {
			return f.Reason
		}
	}
	return ""
}

// The queue checks bindings again at the seal: a binding for one
// deployment narrows only what the signer policy lets it sign (a node
// never signs control records), sits under the farm binding, and never
// overlaps another for the same deployment; one that follows the binding
// in force ends it at its own position; an end-binding ends a binding in
// force, not the farm's, and names its true owner.
func TestBindingsAtTheSeal(t *testing.T) {
	r := newRig(t)
	early := r.book(r.control(r.hud, "bind", "deploymentbinding:early", bindChange("noor", "studio/node/laptop-1", "task/v1"), r.now), nil)
	if res := r.seal(); refusedFor(res, early) != farmbook.ReasonRejected {
		t.Errorf("a node bound before the farm: refused %+v", res.Refused)
	}
	farmRec := r.control(r.hud, "bind", "deploymentbinding:farm", bindChange("dana", "studio/*", "task/v1", "control/v1"), r.now)
	r.book(farmRec, nil)
	r.seal()

	wide := r.book(r.control(r.hud, "bind", "deploymentbinding:wide", bindChange("noor", "studio/node/laptop-1", "task/v1", "control/v1"), r.now), nil)
	other := r.book(r.control(r.hud, "bind", "deploymentbinding:other", bindChange("noor", "kestrel/node/laptop-1", "task/v1"), r.now), nil)
	nodeRec := r.control(r.hud, "bind", "deploymentbinding:node-a", bindChange("noor", "studio/node/laptop-1", "task/v1"), r.now)
	node := r.book(nodeRec, nil)
	dup := r.book(r.control(r.hud, "bind", "deploymentbinding:node-b", bindChange("lee", "studio/node/laptop-1", "task/v1"), r.now), nil)
	res := r.seal()
	for what, id := range map[string]string{"a node binding for control records": wide, "another farm's deployment": other, "a second binding without follows": dup} {
		if refusedFor(res, id) != farmbook.ReasonRejected {
			t.Errorf("%s: refused %+v", what, res.Refused)
		}
	}
	if refusedFor(res, node) != "" {
		t.Fatalf("the node's binding was refused: %+v", res.Refused)
	}

	// a binding that follows the wrong one, then the right one
	c := bindChange("lee", "studio/node/laptop-1", "task/v1")
	c["follows"], c["ends"] = provtest.ID("another binding"), "deploymentbinding:node-a"
	wrong := r.book(r.control(r.hud, "bind", "deploymentbinding:node-c", c, r.now), nil)
	c = bindChange("lee", "studio/node/laptop-1", "task/v1")
	c["follows"], c["ends"] = recordID(nodeRec), "deploymentbinding:node-a"
	rotate := r.book(r.control(r.hud, "bind", "deploymentbinding:node-d", c, r.now), nil)
	res = r.seal()
	if refusedFor(res, wrong) != farmbook.ReasonRejected || refusedFor(res, rotate) != "" {
		t.Fatalf("follows: refused %+v", res.Refused)
	}
	b := r.read()
	pos := b.Head().Position
	if in, _ := b.InForce(farmbook.ForceBinding, "deploymentbinding:node-a", pos); in != nil {
		t.Errorf("the followed binding is still in force at the changeover")
	}
	if in, _ := b.InForce(farmbook.ForceBinding, "deploymentbinding:node-a", pos-1); in == nil {
		t.Errorf("the followed binding ended before the changeover")
	}
	if in, _ := b.InForce(farmbook.ForceBinding, "deploymentbinding:node-d", pos); in == nil {
		t.Errorf("the following binding is not in force at the changeover")
	}

	// ending: the wrong owner, the farm binding, then the binding itself
	end := func(subject, binding, owner string) string {
		return r.book(r.control(r.hud, "end-binding", subject, map[string]any{"binding": binding, "owner": owner, "by": "dana"}, r.now), nil)
	}
	rotated := recordID(r.control(r.hud, "bind", "deploymentbinding:node-d", c, r.now)) // not booked: no binding's record
	badOwner := end("deploymentbinding:node-d", rotated, "noor")
	farmEnd := end("deploymentbinding:farm", recordID(farmRec), "dana")
	res = r.seal()
	if refusedFor(res, badOwner) != farmbook.ReasonRejected || refusedFor(res, farmEnd) != farmbook.ReasonRejected {
		t.Errorf("ends refused %+v", res.Refused)
	}
	var dRec string
	for _, s := range r.read().Entries(0) {
		if s.Entry.Subject == "deploymentbinding:node-d" {
			dRec = s.Entry.Record
		}
	}
	good := end("deploymentbinding:node-d", dRec, "lee")
	again := r.book(r.control(r.hud, "end-binding", "deploymentbinding:node-d", map[string]any{"binding": dRec, "owner": "lee", "by": "lee"}, r.now), nil)
	res = r.seal()
	if refusedFor(res, good) != "" || refusedFor(res, again) == "" {
		t.Fatalf("end-binding: refused %+v", res.Refused)
	}
	b = r.read()
	if in, _ := b.InForce(farmbook.ForceBinding, "deploymentbinding:node-d", b.Head().Position); in != nil {
		t.Errorf("an ended binding is in force")
	}
}

// A binding with an end stops being in force by itself at it, judged at
// the time asked: the issuer's clock, or the time a position was sealed.
func TestBindingEndsAtItsEnd(t *testing.T) {
	r := newRig(t)
	end := r.now.Add(time.Hour)
	c := bindChange("dana", "studio/*", "task/v1")
	c["end"] = end.Format(time.RFC3339Nano)
	r.book(r.control(r.hud, "bind", "deploymentbinding:farm", c, r.now), nil)
	r.seal()
	b := r.read()
	if in, _ := b.InForceAt(farmbook.ForceBinding, "deploymentbinding:farm", 1, end.Add(-time.Second)); in == nil {
		t.Errorf("not in force before its end")
	}
	if in, _ := b.InForceAt(farmbook.ForceBinding, "deploymentbinding:farm", 1, end.Add(time.Second)); in != nil {
		t.Errorf("in force after its end")
	}
}

// A verifier follows a booked record's signer to its binding and owner,
// by book order: a leaf naming no binding, or one booked after the
// record, is labelled unattributed; a record booked after its binding
// ended early is labelled revoked; otherwise it names the owner.
func TestSignerFollowedToItsOwner(t *testing.T) {
	r := newRig(t)
	farmRec := r.control(r.hud, "bind", "deploymentbinding:farm", bindChange("dana", "studio/*", "control/v1"), r.now)
	r.book(farmRec, nil)
	r.seal()
	byFarm := boundSigner(t, r.farm, provenance.ComponentIdentity("hud"), recordID(farmRec))
	hudRec := r.control(byFarm, "bind", "deploymentbinding:hud-a", bindChange("dana", "studio/component/hud", "control/v1"), r.now)
	r.book(hudRec, nil)
	r.seal()
	nodeRec := r.control(byFarm, "bind", "deploymentbinding:n1", bindChange("noor", "studio/node/n1", "task/v1"), r.now)
	r.book(nodeRec, nil)
	r.seal()
	byHUD := boundSigner(t, r.farm, provenance.ComponentIdentity("hud"), recordID(hudRec))
	byOther := boundSigner(t, r.farm, provenance.ComponentIdentity("hud"), recordID(nodeRec))
	byNode := boundSigner(t, r.farm, provenance.ComponentIdentity("hud"), recordID(r.control(r.hud, "bind", "deploymentbinding:x", bindChange("dana", "studio/node/n1", "task/v1"), r.now)))
	grant := func(s *provenance.Signer, note string) {
		c := grantChange("dana", r.now.Add(time.Hour))
		c["note"] = note
		r.book(r.control(s, "grant", "submitgrant:saltmarsh/dana-flow", c, r.now), nil)
	}
	grant(byHUD, "bound")
	grant(r.hud, "unbound")
	grant(byNode, "not booked")
	grant(byOther, "another deployment's binding")
	r.seal()
	r.book(r.control(byFarm, "end-binding", "deploymentbinding:hud-a", map[string]any{"binding": recordID(hudRec), "owner": "dana", "by": "dana"}, r.now), nil)
	r.seal()
	grant(byHUD, "after its end")
	r.seal()

	rd := r.reader()
	v, err := rd.View(r.ctx, r.now)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		owner, dep, label string
	}{
		{"", "", farmbook.LabelUnattributed},             // 1: the farm's bind, signed before any binding
		{"dana", "*", ""},                                // 2: hud-a's bind, signed under the farm binding
		{"dana", "*", ""},                                // 3: n1's bind, signed under the farm binding
		{"dana", "component/hud", ""},                    // 4: under hud-a, most specific
		{"", "", farmbook.LabelUnattributed},             // 5: no binding named
		{"", "", farmbook.LabelUnattributed},             // 6: a binding the book never booked
		{"", "", farmbook.LabelUnattributed},             // 7: n1's binding, another deployment's
		{"dana", "*", ""},                                // 8: the end-binding, under the farm binding
		{"dana", "component/hud", farmbook.LabelRevoked}, // 9: booked after hud-a ended
	}
	if v.Position() != int64(len(want)) {
		t.Fatalf("the book holds %d entries, want %d", v.Position(), len(want))
	}
	for k, w := range want {
		got := rd.Signer(r.ctx, v, int64(k+1))
		if got == nil || got.Owner != w.owner || got.Deployment != w.dep || got.Label != w.label {
			t.Errorf("entry %d: %+v, want owner %q, deployment %q, label %q", k+1, got, w.owner, w.dep, w.label)
		}
	}
}

// The seal, the HUD and the API server take the same to: the CRD's
// pattern is c4v1.BindingToPattern.
func TestBindingToPatternIsTheCRDs(t *testing.T) {
	crd, err := os.ReadFile("../../config/crd/c4.avalanche.io_deploymentbindings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(crd, []byte("pattern: "+c4v1.BindingToPattern+"\n")) {
		t.Errorf("the CRD's pattern for to is not c4v1.BindingToPattern")
	}
}
