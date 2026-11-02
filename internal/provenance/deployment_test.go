package provenance_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
)

// A deployment certified under a binding names it in its leaf, and the
// leaf never outlives the binding's end; an unbound one is certified
// naming none; a binding already ended certifies nothing.
func TestIssueBound(t *testing.T) {
	now := time.Now()
	f := provtest.NewFarm(t, now)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	node := provenance.NodeIdentity("laptop-1")
	id := provtest.ID("a binding")

	leaf, err := f.CA.IssueBound(pub, node, provenance.Bound{ID: id}, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if provenance.CertBinding(leaf) != id || provenance.CertIdentity(leaf) != node {
		t.Errorf("leaf names %q as %q", provenance.CertBinding(leaf), provenance.CertIdentity(leaf))
	}
	end := now.Add(time.Hour).Truncate(time.Second)
	leaf, err = f.CA.IssueBound(pub, node, provenance.Bound{ID: id, End: end}, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.NotAfter.After(end) {
		t.Errorf("the leaf ends %s, after its binding's end %s", leaf.NotAfter, end)
	}
	leaf, err = f.CA.IssueBound(pub, node, provenance.Bound{}, 24*time.Hour, now)
	if err != nil || provenance.CertBinding(leaf) != "" {
		t.Errorf("unbound: %v, names %q", err, provenance.CertBinding(leaf))
	}
	if _, err := f.CA.IssueBound(pub, node, provenance.Bound{ID: id, End: now}, 24*time.Hour, now); err == nil {
		t.Errorf("certified under a binding that has ended")
	}

	// the verifier reads it back
	s, err := provenance.NewSigner(t.Context(), provenance.BoundLocalIssuer(f.CA, node, time.Hour, func(_ context.Context, identity string) (provenance.Bound, error) {
		return provenance.Bound{ID: id}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := provenance.SignStatement(t.Context(), s, provenance.PredicateTask, []provenance.Subject{{Name: "out.exr", Digest: map[string]string{"c4": id}}},
		map[string]any{"runtime": map[string]any{"node": "laptop-1"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	v, err := provenance.VerifyBundle(rec, f.Roots)
	if err != nil || v.Binding != id {
		t.Errorf("verified %v, binding %q", err, v.Binding)
	}
}

// A binding for one deployment only narrows what the signer policy lets
// it sign: a node never signs control records, whatever a binding says.
func TestNarrowsOnly(t *testing.T) {
	for _, c := range []struct {
		dep   string
		kinds []string
		wider []string
	}{
		{"node/laptop-1", []string{"task/v1"}, nil},
		{"node/laptop-1", []string{"task/v1", "control/v1"}, []string{"control/v1"}},
		{"runner/laptop-1", []string{"request/v1"}, nil},
		{"component/hud", []string{"control/v1"}, nil},
		{"component/hud", []string{"job/v1"}, []string{"job/v1"}},
		{"component/queue", []string{"decision/v1", "job/v1", "book/v1"}, nil},
		{"import/show-a", []string{"import/v1", "ingest/v1"}, nil},
		{"sync/flow", []string{"control/v1"}, []string{"control/v1"}},
		{"component/issuer", []string{"device/v1"}, []string{"device/v1"}},
		{"node/", []string{"task/v1"}, []string{"task/v1"}},
	} {
		if got := provenance.NarrowsOnly(c.dep, c.kinds); !slices.Equal(got, c.wider) {
			t.Errorf("%s for %v: wider %v, want %v", c.dep, c.kinds, got, c.wider)
		}
	}
}

// A binding's owner ending their own binding gives up only their own
// authority, silent like a revoke; anyone else ending it needs a touch.
func TestEndBindingNeedsATouchFromAnyoneButItsOwner(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	noor, dana := f.Person(t, "noor", true), f.Person(t, "dana", true)
	for _, c := range []struct {
		name    string
		key     *provtest.Key
		owner   any
		confirm bool
		touch   bool
	}{
		{"its owner, silent", noor, "noor", false, false},
		{"the farm owner, silent", dana, "noor", false, true},
		{"the farm owner, confirmed", dana, "noor", true, false},
		{"no owner named", noor, nil, false, true},
	} {
		params := map[string]any{}
		if c.owner != nil {
			params["owner"] = c.owner
		}
		b := f.SignAct(t, c.key, provtest.Act{Act: "end-binding", Params: params, Confirmed: c.confirm})
		v, err := provenance.VerifyBundle(b, f.Roots)
		if err != nil {
			t.Fatal(err)
		}
		err = provenance.CheckSigner(v)
		if got := errors.Is(err, provenance.ErrNeedsTouch); got != c.touch || err != nil && !got {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if n := provenance.ActNeeds("end-binding", []byte(`{"owner":"noor"}`), time.Now()); !n.Touch || !n.Person {
		t.Errorf("not knowing the signer, it needs the most: %+v", n)
	}
	_, params := provenance.ActNeedsList()
	if params["end-binding"] == "" {
		t.Errorf("the act list doesn't say how end-binding reads its params")
	}
}
