package bindings_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/internal/bindings"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
)

func binding(name, owner, to, record string, start time.Time) c4v1.DeploymentBinding {
	return c4v1.DeploymentBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: c4v1.DeploymentBindingSpec{
		Owner: owner, To: to, For: []string{"task/v1"}, Start: metav1.NewTime(start), By: owner, Record: record}}
}

// The most specific binding in force wins, matched byte for byte: a
// binding of node/laptop-1 binds neither node/laptop-10 nor
// runner/laptop-1, which the farm binding binds; nothing binds an
// identity outside the farm's.
func TestMostSpecificByteForByte(t *testing.T) {
	now := time.Now()
	l := &bindings.Lookup{Now: now, Items: []c4v1.DeploymentBinding{
		binding("farm", "dana", "studio/*", provtest.ID("farm"), now.Add(-time.Hour)),
		binding("node-laptop-1", "noor", "studio/node/laptop-1", provtest.ID("laptop-1"), now.Add(-time.Minute)),
		binding("later", "lee", "studio/node/laptop-2", provtest.ID("laptop-2"), now.Add(time.Minute)),
	}}
	for identity, want := range map[string]string{
		provenance.NodeIdentity("laptop-1"):    "noor",
		provenance.NodeIdentity("laptop-10"):   "dana",
		provenance.NodeIdentity("laptop-1.x"):  "dana",
		provenance.RunnerIdentity("laptop-1"):  "dana",
		provenance.NodeIdentity("laptop-2"):    "dana", // not started yet
		"https://elsewhere.example/node/a":     "",
		provenance.ComponentIdentity("hud"):    "dana",
		provenance.UserIdentity("noor") + "/x": "dana",
	} {
		got := ""
		if b := l.For(identity); b != nil {
			got = b.Spec.Owner
		}
		if got != want {
			t.Errorf("%s is bound to %q, want %q", identity, got, want)
		}
	}
}

// Where the farm keeps a book, a binding is in force only while the book
// says so too: an end booked ends it though its object says nothing yet.
// A binding the book never booked (made before the book) is judged by its
// object.
func TestTheBookEndsABinding(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	f := provtest.NewFarm(t, now.Add(-time.Hour))
	hud, err := provenance.NewSigner(ctx, func(_ context.Context, pub ed25519.PublicKey) ([]*x509.Certificate, error) {
		c, err := f.CA.Issue(pub, provenance.ComponentIdentity("hud"), 48*time.Hour, f.Now)
		return []*x509.Certificate{c, f.CA.Cert}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	store, ptr := &farmbook.MemoryStore{}, &farmbook.MemoryPointer{}
	w := &farmbook.Writer{Store: store, Pointer: ptr}
	book := func(action, target string, change map[string]any) string {
		t.Helper()
		rec, err := provenance.SignStatement(ctx, hud, provenance.PredicateControl, []provenance.Subject{{Name: target, Digest: map[string]string{"sha256": strings.Repeat("0", 64)}}},
			map[string]any{"actor": "dana", "action": action, "target": target, "change": change}, now)
		if err != nil {
			t.Fatal(err)
		}
		e, err := farmbook.ControlEntry(rec, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Append(ctx, farmbook.Submission{Entry: e, Record: rec}); err != nil {
			t.Fatal(err)
		}
		return c4.Identify(bytes.NewReader(rec)).String()
	}
	farmRec := book("bind", "deploymentbinding:farm", map[string]any{"owner": "dana", "to": "studio/*"})
	nodeRec := book("bind", "deploymentbinding:node-a", map[string]any{"owner": "noor", "to": "studio/node/laptop-1"})
	book("end-binding", "deploymentbinding:node-a", map[string]any{"owner": "noor", "binding": nodeRec})
	r := &farmbook.Reader{Farm: f.ID, Roots: f.Roots, Store: store, Pointer: ptr}
	v, err := r.View(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	l := &bindings.Lookup{Now: now, View: v, Items: []c4v1.DeploymentBinding{
		binding("farm", "dana", "studio/*", farmRec, now.Add(-time.Hour)),
		binding("node-a", "noor", "studio/node/laptop-1", nodeRec, now.Add(-time.Hour)),
		binding("runner-a", "lee", "studio/runner/laptop-1", provtest.ID("made before the book"), now.Add(-time.Hour)),
	}}
	if b := l.For(provenance.NodeIdentity("laptop-1")); b == nil || b.Spec.Owner != "dana" {
		t.Errorf("node/laptop-1 is bound to %+v after the book ended its binding, not the farm binding", b)
	}
	if b := l.For(provenance.RunnerIdentity("laptop-1")); b == nil || b.Spec.Owner != "lee" {
		t.Errorf("a binding the book never booked: %+v", b)
	}
	// a record no book names: its leaf's binding, of its deployment
	if s := l.Signer(provenance.RunnerIdentity("laptop-1"), provtest.ID("made before the book")); s.Owner != "lee" || s.Label != "" {
		t.Errorf("signer %+v", s)
	}
	for _, c := range []struct{ identity, id string }{
		{provenance.RunnerIdentity("laptop-1"), ""},
		{provenance.RunnerIdentity("laptop-2"), provtest.ID("made before the book")},
		{provenance.RunnerIdentity("laptop-1"), provtest.ID("no such binding")},
	} {
		if s := l.Signer(c.identity, c.id); s.Label != farmbook.LabelUnattributed || s.Owner != "" {
			t.Errorf("%s under %.12q: %+v", c.identity, c.id, s)
		}
	}
}
