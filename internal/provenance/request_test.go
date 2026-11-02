package provenance_test

import (
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
