package provenance_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

const (
	hudRP     = "hud.studio.example"
	hudOrigin = "https://hud.studio.example"
)

// A browser's touch is a passkey: a confirmed act whose touch key signed
// as WebAuthn verifies, and its record holds it to the HUD's relying party
// and origin.
func TestPasskeyConfirmsAnAct(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	dana := f.Person(t, "dana", true).Passkey(hudRP, hudOrigin)
	b := f.SignAct(t, dana, provtest.Act{Act: "grant", Params: map[string]any{"kind": "sign", "to": "agent:assistant"}, Confirmed: true})
	v, _, err := provenance.VerifyAct(b, f.Roots, f.Now, provenance.NewMemorySeen())
	if err != nil {
		t.Fatal(err)
	}
	if !v.Confirmed() || v.TouchAssertion == nil || v.TouchAssertion.Origin != hudOrigin {
		t.Fatalf("verified %+v", v)
	}
	records := []*schema.Device{f.TouchRecord(t, dana)}
	if err := provenance.CheckTouchKey(v, records); err != nil {
		t.Fatal(err)
	}
	// The same passkey's record at another origin, or for another relying
	// party, refuses the touch: a passkey used from another page.
	for name, w := range map[string]schema.DeviceWebAuthn{
		"another origin":         {RPID: hudRP, Origin: "https://hud.studio.example:8443"},
		"another relying party":  {RPID: "studio.example", Origin: hudOrigin},
		"a phishing site's page": {RPID: "hud-studio.example", Origin: "https://hud-studio.example"},
	} {
		r := f.TouchRecord(t, dana)
		r.Predicate.WebAuthn = &w
		if err := provenance.CheckTouchKey(v, []*schema.Device{r}); !errors.Is(err, provenance.ErrUntrusted) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	// A passkey's record with a raw touch, or a raw key's record with a
	// passkey's touch, is refused.
	raw := f.TouchRecord(t, dana)
	raw.Predicate.WebAuthn = nil
	if provenance.CheckTouchKey(v, []*schema.Device{raw}) == nil {
		t.Error("a passkey's touch passed against a raw touch key's record")
	}
	noor := f.Person(t, "noor", true)
	vr, _, err := provenance.VerifyAct(f.SignAct(t, noor, provtest.Act{Act: "grant", Confirmed: true}), f.Roots, f.Now, provenance.NewMemorySeen())
	if err != nil {
		t.Fatal(err)
	}
	pk := f.TouchRecord(t, noor)
	pk.Predicate.WebAuthn = &schema.DeviceWebAuthn{RPID: hudRP, Origin: hudOrigin}
	if provenance.CheckTouchKey(vr, []*schema.Device{pk}) == nil {
		t.Error("a raw touch passed against a passkey's record")
	}
}

// Each part of an assertion is checked: the client data's type, its
// challenge, another origin's frame, the flags, and the DER signature.
func TestPasskeyAssertionParts(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	k := f.Person(t, "dana", true).Passkey(hudRP, hudOrigin).Touch
	pub := &k.P256.PublicKey
	msg := []byte("the payload's PAE")
	clientData := func(edit func(m map[string]any)) []byte {
		m := map[string]any{"type": "webauthn.get", "challenge": provenance.WebAuthnChallenge(msg), "origin": hudOrigin}
		edit(m)
		b, _ := json.Marshal(m)
		return b
	}
	w, sig := k.Assertion(t, msg)
	a, err := provenance.VerifyWebAuthn(pub, msg, w, sig)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CheckRP(hudRP, hudOrigin); err != nil {
		t.Fatal(err)
	}
	// A high S verifies: authenticators sign either form.
	var rs struct{ R, S *big.Int }
	asn1.Unmarshal(sig, &rs)
	rs.S.Sub(elliptic.P256().Params().N, rs.S)
	flipped, _ := asn1.Marshal(rs)
	if _, err := provenance.VerifyWebAuthn(pub, msg, w, flipped); err != nil {
		t.Errorf("the other S: %v", err)
	}
	for name, try := range map[string]func() (provenance.WebAuthn, []byte){
		"a create, not a get": func() (provenance.WebAuthn, []byte) {
			return k.AssertionOf(t, clientData(func(m map[string]any) { m["type"] = "webauthn.create" }), provtest.UP|provtest.UV)
		},
		"another message's challenge": func() (provenance.WebAuthn, []byte) {
			return k.AssertionOf(t, clientData(func(m map[string]any) { m["challenge"] = provenance.WebAuthnChallenge([]byte("another")) }), provtest.UP|provtest.UV)
		},
		"in another origin's frame": func() (provenance.WebAuthn, []byte) {
			return k.AssertionOf(t, clientData(func(m map[string]any) { m["crossOrigin"] = true }), provtest.UP|provtest.UV)
		},
		"no origin": func() (provenance.WebAuthn, []byte) {
			return k.AssertionOf(t, clientData(func(m map[string]any) { delete(m, "origin") }), provtest.UP|provtest.UV)
		},
		"present, not verified": func() (provenance.WebAuthn, []byte) {
			return k.AssertionOf(t, clientData(func(map[string]any) {}), provtest.UP)
		},
		"verified, not present": func() (provenance.WebAuthn, []byte) {
			return k.AssertionOf(t, clientData(func(map[string]any) {}), provtest.UV)
		},
		"short authenticator data": func() (provenance.WebAuthn, []byte) {
			w, s := k.Assertion(t, msg)
			w.AuthenticatorData = w.AuthenticatorData[:36]
			return w, s
		},
		"the authenticator data changed": func() (provenance.WebAuthn, []byte) {
			w, s := k.Assertion(t, msg)
			w.AuthenticatorData[33] = 9 // the counter
			return w, s
		},
		"raw r||s, not DER": func() (provenance.WebAuthn, []byte) {
			w, _ := k.Assertion(t, msg)
			return w, k.SignRaw(t, msg)
		},
		"DER with bytes after it": func() (provenance.WebAuthn, []byte) {
			w, s := k.Assertion(t, msg)
			return w, append(s, 0)
		},
	} {
		w, s := try()
		if _, err := provenance.VerifyWebAuthn(pub, msg, w, s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provenance.VerifyWebAuthn(&other.PublicKey, msg, w, sig); err == nil {
		t.Error("another key's assertion accepted")
	}
	for _, rp := range [][2]string{{"studio.example", hudOrigin}, {hudRP, "https://evil.example"}} {
		if a.CheckRP(rp[0], rp[1]) == nil {
			t.Errorf("CheckRP(%s, %s) passed", rp[0], rp[1])
		}
	}
}

// The bundle: a WebAuthn assertion is only ever a touch key's, and a
// passkey's touch is never taken as raw.
func TestPasskeyInTheBundle(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	dana := f.Person(t, "dana", true).Passkey(hudRP, hudOrigin)
	payload := f.Statement(t, dana, provtest.Act{Act: "grant", Confirmed: true})
	edit := func(change func(b *provenance.Bundle)) []byte {
		var b provenance.Bundle
		json.Unmarshal(provtest.Sign(t, payload, dana, true), &b)
		change(&b)
		out, _ := json.Marshal(b)
		return out
	}
	for name, b := range map[string][]byte{
		"an assertion with no touch key": edit(func(b *provenance.Bundle) {
			b.VerificationMaterial.TouchCertificateChain = nil
			b.DSSEEnvelope.Signatures = b.DSSEEnvelope.Signatures[:1]
		}),
		"the assertion dropped, its DER signature read as raw": edit(func(b *provenance.Bundle) { b.VerificationMaterial.TouchWebAuthn = nil }),
		"the assertion's client data changed": edit(func(b *provenance.Bundle) {
			b.VerificationMaterial.TouchWebAuthn.ClientDataJSON = []byte(`{"type":"webauthn.get","challenge":"x","origin":"` + hudOrigin + `"}`)
		}),
	} {
		if _, err := provenance.VerifyBundle(b, f.Roots); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

// A passkey's enrolment proof: the device fixture's assertion verifies
// over its request, and is refused over another.
func TestPasskeyFixtureProof(t *testing.T) {
	var d schema.Device
	fixture(t, "../../schema/fixtures/device/pass/passkey-on-a-step-up.json", &d)
	w := d.Predicate.WebAuthn
	if w == nil || d.Predicate.Proof != "" || d.Predicate.TouchFor == "" {
		t.Fatalf("the fixture is not a passkey: %+v", d.Predicate)
	}
	spki, _ := d.Predicate.PublicKey()
	pk, err := parseP256(spki)
	if err != nil {
		t.Fatal(err)
	}
	dec := func(s string) []byte { b, _ := base64.RawURLEncoding.DecodeString(s); return b }
	a, err := provenance.VerifyWebAuthn(pk, d.Predicate.ProofMessage(), provenance.WebAuthn{AuthenticatorData: dec(w.AuthenticatorData), ClientDataJSON: dec(w.ClientDataJSON)}, dec(w.Signature))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CheckRP(w.RPID, w.Origin); err != nil {
		t.Fatal(err)
	}
	other := d.Predicate
	other.Person = "noor"
	if _, err := provenance.VerifyWebAuthn(pk, other.ProofMessage(), provenance.WebAuthn{AuthenticatorData: dec(w.AuthenticatorData), ClientDataJSON: dec(w.ClientDataJSON)}, dec(w.Signature)); err == nil {
		t.Error("the proof verified over another person's request")
	}
}

func parseP256(spki []byte) (*ecdsa.PublicKey, error) {
	k, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("not ECDSA")
	}
	return pub, nil
}
