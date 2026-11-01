package provenance_test

import (
	"crypto/ecdsa"
	"encoding/json"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// p256 signs as a provtest key does, through the client's interface.
type p256 struct{ key *ecdsa.PrivateKey }

func (k p256) Sign(msg []byte) ([]byte, error) { return provenance.SignP256(k.key, msg) }

func certOf(k *provtest.Key) provenance.DeviceCert {
	c := provenance.DeviceCert{Identity: k.Identity, Device: k.Device}
	for _, cert := range k.Chain {
		c.Chain = append(c.Chain, cert.Raw)
	}
	return c
}

// A client signs a person's act the farm verifies: silent, with the device
// key alone; confirmed, with the touch key beside it. A confirmed act with
// no touch key is refused before anything is signed.
func TestAClientSignsAPersonsAct(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	noor := f.Person(t, "noor", true)
	touch := certOf(noor.Touch)
	on := provenance.ActSubject("workjob", "show-a", "sh040-light-v007", provtest.JobUID, provtest.JobSpecDigest)
	for _, confirmed := range []bool{false, true} {
		act := provenance.PersonAct{Farm: f.ID, Subject: on, Act: "grant", Params: json.RawMessage(`{"tracker":"github"}`),
			Person: "noor", As: "artist", Confirmed: confirmed}
		b, err := provenance.SignPersonAct(act, p256{noor.P256}, certOf(noor), p256{noor.Touch.P256}, &touch)
		if err != nil {
			t.Fatal(err)
		}
		v, a, err := provenance.VerifyAct(b, f.Roots, time.Now(), provenance.NewMemorySeen())
		if confirmed != (err == nil) {
			// grant is on the touch list: silent, it is refused
			t.Fatalf("confirmed %v: %v", confirmed, err)
		}
		if err != nil {
			continue
		}
		p := a.Predicate
		if v.Identity != noor.Identity || !v.Confirmed() || p.Chain.By.Person != "noor" || p.Chain.By.Grade != schema.GradeConfirmed ||
			p.As != "artist" || p.Device != noor.Device || a.Subject[0].Name != on.Name || string(p.Params) != `{"tracker":"github"}` {
			t.Errorf("the act: %+v, %+v", v, p)
		}
	}
	silent := provenance.PersonAct{Farm: f.ID, Subject: on, Act: "approve", Person: "noor"}
	b, err := provenance.SignPersonAct(silent, p256{noor.P256}, certOf(noor), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, a, err := provenance.VerifyAct(b, f.Roots, time.Now(), provenance.NewMemorySeen()); err != nil || v.Confirmed() || string(a.Predicate.Params) != "{}" {
		t.Errorf("a silent act: %v", err)
	}
	if _, err := provenance.SignPersonAct(provenance.PersonAct{Farm: f.ID, Subject: on, Act: "grant", Person: "noor", Confirmed: true},
		p256{noor.P256}, certOf(noor), nil, nil); err == nil {
		t.Error("a confirmed act with no touch key was signed")
	}
}
