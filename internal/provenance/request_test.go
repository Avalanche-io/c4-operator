package provenance_test

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A signed request verifies as a record of its own kind, from each client
// that signs one, and is never taken as an act.
func TestSignedRequest(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	body := []byte(`{"node":"laptop-1","units":2}`)
	for name, k := range map[string]*provtest.Key{
		"a person's CLI": f.Person(t, "noor", false),
		"an agent":       f.Agent(t, "assistant", ""),
		"a host runner":  f.Part(t, provenance.RunnerIdentity("laptop-1")),
		"a unit, by pod": f.Unit(t, "saltmarsh", "sm0140-light", "7", provtest.JobUID, 1),
		"a relay":        f.Relay(t, "saltmarsh", "flow-sync", "flow", "noor.k"),
	} {
		h := f.SignRequest(t, k, "POST", "/api/v1/observed?since=7", body)
		b, err := base64.StdEncoding.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		v, err := provenance.VerifyBundle(b, f.Roots)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v.PredicateType != provenance.PredicateRequest || v.Identity != k.Identity {
			t.Errorf("%s: verified as %s by %s", name, v.PredicateType, v.Identity)
		}
		r, errs := schema.ParseRequest(v.Payload)
		if errs != nil {
			t.Fatalf("%s: %+v", name, errs)
		}
		if err := r.Matches("POST", "/api/v1/observed?since=7", body); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if r.Predicate.Farm != v.Farm || r.Predicate.Signer != v.Identity || r.Predicate.Device != v.Device {
			t.Errorf("%s: the request names farm %.12s, signer %s, device %q; its leaf %.12s, %s, %q",
				name, r.Predicate.Farm, r.Predicate.Signer, r.Predicate.Device, v.Farm, v.Identity, v.Device)
		}
		if _, _, err := provenance.VerifyAct(b, f.Roots, f.Now, provenance.NewMemorySeen()); err == nil {
			t.Errorf("%s: a signed request was taken as an act", name)
		}
	}
}

// The farm takes a signed request once, from one of its clients, signed
// within five minutes of its clock: the same request again is refused, as
// is one signed by a part of the farm (a node, a component), an act sent
// as a request, and one signed long before.
func TestVerifyRequest(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	body := []byte(`{"node":"laptop-1"}`)
	runner := f.Part(t, provenance.RunnerIdentity("laptop-1"))
	seen := provenance.NewMemorySeen()
	decode := func(h string) []byte {
		b, err := base64.StdEncoding.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := decode(f.SignRequest(t, runner, "POST", "/api/v1/nodes/laptop-1/observed", body))
	v, r, err := provenance.VerifyRequest(b, f.Roots, f.Now, seen)
	if err != nil || v.Identity != runner.Identity || r.Matches("POST", "/api/v1/nodes/laptop-1/observed", body) != nil {
		t.Fatalf("a runner's request: %v", err)
	}
	if _, _, err := provenance.VerifyRequest(b, f.Roots, f.Now, seen); err == nil {
		t.Fatal("the same request taken twice")
	}
	for name, k := range map[string]*provtest.Key{
		"a node":      f.Part(t, provenance.NodeIdentity("w1")),
		"a component": f.Part(t, provenance.ComponentIdentity("hud")),
	} {
		if _, _, err := provenance.VerifyRequest(decode(f.SignRequest(t, k, "GET", "/api/v1/snapshot", nil)), f.Roots, f.Now, seen); err == nil {
			t.Errorf("%s signed a request", name)
		}
	}
	act := f.SignAct(t, f.Person(t, "noor", false), provtest.Act{})
	if _, _, err := provenance.VerifyRequest(act, f.Roots, f.Now, seen); err == nil {
		t.Error("an act taken as a signed request")
	}
	late := decode(f.SignRequest(t, runner, "GET", "/api/v1/nodes/laptop-1/pods", nil))
	if _, _, err := provenance.VerifyRequest(late, f.Roots, f.Now.Add(10*time.Minute), seen); err == nil {
		t.Error("a request signed ten minutes ago")
	}
}

// A runner signs its own request with its key, and the farm takes it as
// that request: the same method, target and body, once.
func TestSignRequest(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	ca, err := provenance.NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	s, err := provenance.NewSigner(ctx, provenance.LocalIssuer(ca, provenance.RunnerIdentity("laptop-1"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"owners":["dana"]}`)
	const target = "/api/v1/nodes/laptop-1/observed"
	h, err := provenance.SignRequest(ctx, s, "POST", target, body, now)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	v, r, err := provenance.VerifyRequest(data, roots, now, provenance.NewMemorySeen())
	if err != nil || v.Identity != provenance.RunnerIdentity("laptop-1") || r.Predicate.Farm != provenance.FarmID(ca.Cert) {
		t.Fatalf("the runner's request: %v", err)
	}
	if err := r.Matches("POST", target, body); err != nil {
		t.Fatal(err)
	}
	if r.Matches("POST", target, []byte(`{"owners":["lee"]}`)) == nil || r.Matches("GET", target, body) == nil || r.Matches("POST", target+"?x=1", body) == nil {
		t.Fatal("a signed request matched another request")
	}
}
