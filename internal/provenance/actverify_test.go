package provenance_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// bundleOf reads a bundle for a test to change.
func bundleOf(t *testing.T, data []byte) provenance.Bundle {
	t.Helper()
	var b provenance.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func encode(t *testing.T, b provenance.Bundle) []byte {
	t.Helper()
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A person's device key is P-256: the CA certifies it with its device in
// the leaf, the verifier checks it by the leaf's key type, and the act
// passes the signer policy. The CA refuses a leaf naming a device that
// isn't its key's, and a key that is neither Ed25519 nor P-256.
func TestDeviceKeySignsAnAct(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	noor := f.Person(t, "noor", false)
	v, err := provenance.VerifyBundle(f.SignAct(t, noor, provtest.Act{}), f.Roots)
	if err != nil {
		t.Fatal(err)
	}
	if v.Identity != provenance.UserIdentity("noor") || v.Device != noor.Device || v.Farm != f.ID || v.Confirmed() {
		t.Fatalf("verified %+v", v)
	}
	if err := provenance.CheckSigner(v); err != nil {
		t.Fatal(err)
	}
	other := f.Person(t, "noor", false)
	if _, err := f.CA.IssueLeaf(&noor.P256.PublicKey, noor.Identity, other.Device, "", time.Hour, f.Now); err == nil {
		t.Error("the CA certified a leaf naming another key's device")
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := f.CA.Issue(&p384.PublicKey, noor.Identity, time.Hour, f.Now); err == nil {
		t.Error("the CA certified a P-384 key")
	}
}

// A P-256 signature has one form: raw r||s, 64 bytes, low S. Its high-S
// twin, which ECDSA alone accepts, is refused, as are DER and any other
// length.
func TestDeviceSignatureHasOneForm(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	noor := f.Person(t, "noor", false)
	good := f.SignAct(t, noor, provtest.Act{})
	if _, err := provenance.VerifyBundle(good, f.Roots); err != nil {
		t.Fatal(err)
	}
	b := bundleOf(t, good)
	raw, _ := base64.StdEncoding.DecodeString(b.DSSEEnvelope.Signatures[0].Sig)
	r, s := new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:])
	high := make([]byte, 64)
	r.FillBytes(high[:32])
	new(big.Int).Sub(elliptic.P256().Params().N, s).FillBytes(high[32:])
	payload, _ := base64.StdEncoding.DecodeString(b.DSSEEnvelope.Payload)
	if !ecdsa.Verify(&noor.P256.PublicKey, sha(provenance.PAE(provenance.PayloadType, payload)), r, new(big.Int).SetBytes(high[32:])) {
		t.Fatal("the high-S twin is not a valid ECDSA signature, so this test proves nothing")
	}
	der, _ := ecdsa.SignASN1(rand.Reader, noor.P256, sha(provenance.PAE(provenance.PayloadType, payload)))
	// S with a leading zero byte is the same number, so ECDSA alone takes
	// it: only the length tells it apart.
	padded := append(append(append([]byte{}, raw[:32]...), 0), raw[32:]...)
	for name, sig := range map[string][]byte{"high S": high, "DER": der, "S with a leading zero": padded, "65 bytes": append(raw, 0), "63 bytes": raw[:63]} {
		b.DSSEEnvelope.Signatures[0].Sig = base64.StdEncoding.EncodeToString(sig)
		if _, err := provenance.VerifyBundle(encode(t, b), f.Roots); !errors.Is(err, provenance.ErrSignatureForm) {
			t.Errorf("%s: %v, want ErrSignatureForm", name, err)
		}
	}
}

// A device key signs alone: one signature, its own, keyed by its leaf.
func TestDeviceKeySignsAlone(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	noor := f.Person(t, "noor", false)
	b := bundleOf(t, f.SignAct(t, noor, provtest.Act{}))
	own := b.DSSEEnvelope.Signatures[0]
	b.DSSEEnvelope.Signatures = append(b.DSSEEnvelope.Signatures, own)
	if _, err := provenance.VerifyBundle(encode(t, b), f.Roots); !errors.Is(err, provenance.ErrUntrusted) {
		t.Errorf("a device-signed act with two signatures: %v", err)
	}
	own.KeyID = "c4" + own.KeyID[2:]
	b.DSSEEnvelope.Signatures = []provenance.Signature{own}
	if _, err := provenance.VerifyBundle(encode(t, b), f.Roots); !errors.Is(err, provenance.ErrUntrusted) {
		t.Errorf("a signature keyed by another key ID: %v", err)
	}
}

// A confirmed act is exactly two signatures: the device key's, then the
// touch key's. Swapped, a third, or the touch key's missing, it is
// refused; a third is never ignored.
func TestConfirmedIsExactlyTwoSignatures(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	dana := f.Person(t, "dana", true)
	good := f.SignAct(t, dana, provtest.Act{Act: "grant", Params: map[string]any{"to": "agent:assistant"}, Confirmed: true})
	v, err := provenance.VerifyBundle(good, f.Roots)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Confirmed() || v.TouchDevice != dana.Touch.Device || v.Device != dana.Device {
		t.Fatalf("verified %+v", v)
	}
	if err := provenance.CheckSigner(v); err != nil {
		t.Fatal(err)
	}
	stranger := f.Person(t, "dana", false)
	payload, _ := base64.StdEncoding.DecodeString(bundleOf(t, good).DSSEEnvelope.Payload)
	third := provenance.Signature{KeyID: provenance.Fingerprint(stranger.Leaf()),
		Sig: base64.StdEncoding.EncodeToString(stranger.SignRaw(t, provenance.PAE(provenance.PayloadType, payload)))}
	for name, change := range map[string]func(*provenance.Bundle){
		"swapped": func(b *provenance.Bundle) {
			s := b.DSSEEnvelope.Signatures
			s[0], s[1] = s[1], s[0]
		},
		"a third":             func(b *provenance.Bundle) { b.DSSEEnvelope.Signatures = append(b.DSSEEnvelope.Signatures, third) },
		"the touch's twice":   func(b *provenance.Bundle) { b.DSSEEnvelope.Signatures[0] = b.DSSEEnvelope.Signatures[1] },
		"no touch signature":  func(b *provenance.Bundle) { b.DSSEEnvelope.Signatures = b.DSSEEnvelope.Signatures[:1] },
		"another's touch key": func(b *provenance.Bundle) { b.DSSEEnvelope.Signatures[1] = third },
	} {
		b := bundleOf(t, good)
		change(&b)
		if _, err := provenance.VerifyBundle(encode(t, b), f.Roots); !errors.Is(err, provenance.ErrUntrusted) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	// A touch key of another person's, signing in order, is refused too.
	noor := f.Person(t, "noor", true)
	borrowed := *dana
	borrowed.Touch = noor.Touch
	if _, err := provenance.VerifyBundle(f.SignAct(t, &borrowed, provtest.Act{Confirmed: true}), f.Roots); !errors.Is(err, provenance.ErrUntrusted) {
		t.Errorf("noor's touch key confirmed dana's act: %v", err)
	}
}

// The forbidden kinds (DESIGN 6) are a person's: an agent, a unit or a
// relay signing one is refused however well it signed, except that a
// relay carries a person's decision made in their tracker (PEOPLE-KEYS-2
// 6.1). The touch list (4.3) needs confirmed; revoking is a person's and
// silent.
func TestActKindsThatNeedAPersonOrATouch(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	agent := f.Agent(t, "assistant", "")
	unit := f.Unit(t, "show-a", "sh040-light-v007", "3", provtest.JobUID, 1)
	relay := f.Relay(t, "show-a", "flow-sync", "flow", "noor.k")
	dana := f.Person(t, "dana", true)
	past := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []struct {
		name string
		key  *provtest.Key
		act  provtest.Act
		ok   bool
	}{
		{"an agent grants", agent, provtest.Act{Act: "grant"}, false},
		{"an agent revokes", agent, provtest.Act{Act: "revoke"}, false},
		{"an agent accepts a binding", agent, provtest.Act{Act: "bind"}, false},
		{"an agent sends", agent, provtest.Act{Act: "send"}, false},
		{"an agent decides a person's wait", agent, provtest.Act{Act: "decide", Params: map[string]any{"unit": "8", "do": "approve", "waitsFor": "person:dana"}}, false},
		{"an agent decides a wait without saying whose", agent, provtest.Act{Act: "decide", Params: map[string]any{"unit": "8", "do": "approve"}}, false},
		{"an agent decides its own wait", agent, provtest.Act{Act: "decide", Params: map[string]any{"unit": "8", "do": "approve", "waitsFor": "agent:assistant"}}, true},
		{"an agent holds", agent, provtest.Act{Act: "hold"}, true},
		{"a unit adopts a violation", unit, provtest.Act{Act: "adopt"}, false},
		{"a unit supplies", unit, provtest.Act{Act: "supply", Params: map[string]any{"need": "plate"}}, true},
		{"a relay grants", relay, provtest.Act{Act: "grant"}, false},
		{"a relay changes strict accountability", relay, provtest.Act{Act: "accountability", Params: map[string]any{"mode": "strict"}}, false},
		{"a relay carries a person's decision", relay, provtest.Act{Act: "decide", Params: map[string]any{"unit": "8", "do": "approve"}}, true},
		{"a relay revokes", relay, provtest.Act{Act: "revoke"}, false},
		{"a person grants without a touch", dana, provtest.Act{Act: "grant"}, false},
		{"a person grants with a touch", dana, provtest.Act{Act: "grant", Confirmed: true}, true},
		{"a person revokes, silent", dana, provtest.Act{Act: "revoke"}, true},
		{"a person turns strict accountability on, silent", dana, provtest.Act{Act: "accountability", Params: map[string]any{"mode": "strict", "onUnattributed": "refuse"}}, true},
		{"a person turns it down without a touch", dana, provtest.Act{Act: "accountability", Params: map[string]any{"mode": "label"}}, false},
		{"a person removes a device from now, silent", dana, provtest.Act{Act: "remove-device"}, true},
		{"a person removes a device from the past without a touch", dana, provtest.Act{Act: "remove-device", Params: map[string]any{"from": past}}, false},
		{"a person removes a device from the past with a touch", dana, provtest.Act{Act: "remove-device", Params: map[string]any{"from": past}, Confirmed: true}, true},
		{"a person sets a cap by its C4 ID without a touch", dana, provtest.Act{Act: "set-cap", Params: provtest.ID("cap")}, false},
		{"a person approves a burst without a touch", dana, provtest.Act{Act: "approve-capacity"}, false},
		{"a person approves a burst with a touch", dana, provtest.Act{Act: "approve-capacity", Confirmed: true}, true},
		{"a person approves, silent", dana, provtest.Act{Act: "approve"}, true},
	} {
		v, err := provenance.VerifyBundle(f.SignAct(t, c.key, c.act), f.Roots)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if err := provenance.CheckSigner(v); (err == nil) != c.ok {
			t.Errorf("%s: policy %v, want allowed %v", c.name, err, c.ok)
		}
	}
}

// An act is signed by the identity its signer field names, for the farm
// it chains to, naming the device or grant its leaf names, and graded
// confirmed exactly when a touch key signed it.
func TestActSignerRule(t *testing.T) {
	f := provtest.NewFarm(t, time.Now())
	noor := f.Person(t, "noor", true)
	agent := f.Agent(t, "assistant", "")
	other := provtest.NewFarm(t, time.Now())
	confirmedChain := &schema.ActChain{By: schema.Actor{Person: "noor", Grade: schema.GradeConfirmed}, For: schema.For{Party: "noor", Grade: schema.GradeConfirmed}}
	signedChain := &schema.ActChain{By: schema.Actor{Person: "noor", Grade: schema.GradeSigned}, For: schema.For{Party: "noor", Grade: schema.GradeSigned}}
	otherGrant := &schema.ActChain{By: schema.Actor{Agent: "assistant", Grade: schema.GradeSigned}, For: schema.For{Party: "noor", Grade: schema.GradeSigned},
		Under: []schema.Delegation{{Grant: provtest.ID("another grant"), Kind: "sign", From: "noor"}}}
	for name, signed := range map[string][]byte{
		"signs as another person": f.SignAct(t, noor, provtest.Act{Signer: provenance.UserIdentity("dana"),
			Chain: &schema.ActChain{By: schema.Actor{Person: "dana", Grade: schema.GradeSigned}, For: schema.For{Party: "dana", Grade: schema.GradeSigned}}}),
		"graded confirmed, one signature": f.SignAct(t, noor, provtest.Act{Chain: confirmedChain}),
		"touched, graded signed":          provtest.Sign(t, f.Statement(t, noor, provtest.Act{Chain: signedChain}), noor, true),
		"an agent under another grant":    f.SignAct(t, agent, provtest.Act{Chain: otherGrant}),
	} {
		v, err := provenance.VerifyBundle(signed, f.Roots)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if provenance.CheckSigner(v) == nil {
			t.Errorf("%s: the policy accepted it", name)
		}
	}
	// Made for another farm: the statement names that farm.
	st := f.Statement(t, noor, provtest.Act{})
	st = []byte(strings.Replace(string(st), f.ID, other.ID, 1))
	v, err := provenance.VerifyBundle(provtest.Sign(t, st, noor, false), f.Roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := provenance.CheckSigner(v); err == nil || !strings.Contains(err.Error(), "farm") {
		t.Errorf("an act made for another farm: %v", err)
	}
	// A device record is the people's issuer's.
	queue := f.Part(t, provenance.ComponentIdentity("queue"))
	dev := provtest.Sign(t, []byte(`{"_type":"`+schema.StatementType+`","predicateType":"`+provenance.PredicateDevice+`","subject":[],"predicate":{"signedAt":"`+
		f.Now.UTC().Format(time.RFC3339)+`"}}`), queue, false)
	if v, err := provenance.VerifyBundle(dev, f.Roots); err != nil || provenance.CheckSigner(v) == nil {
		t.Errorf("the queue signed a device record: %v", err)
	}
}

// The farm takes each act once, by its payload's hash: the same act sent
// twice, or re-encoded, is refused, and so is an act signed more than
// five minutes from the verifier's clock.
func TestActTakenOnceAndOnTime(t *testing.T) {
	now := time.Now()
	f := provtest.NewFarm(t, now.Add(-time.Hour))
	noor := f.Person(t, "noor", false)
	seen := provenance.NewMemorySeen()
	act := f.SignAct(t, noor, provtest.Act{SignedAt: now})
	if _, _, err := provenance.VerifyAct(act, f.Roots, now, seen); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provenance.VerifyAct(act, f.Roots, now, seen); !errors.Is(err, provenance.ErrSeen) {
		t.Errorf("the same act again: %v", err)
	}
	var re map[string]any
	json.Unmarshal(act, &re)
	again, _ := json.MarshalIndent(re, "", "  ")
	if _, _, err := provenance.VerifyAct(again, f.Roots, now, seen); !errors.Is(err, provenance.ErrSeen) {
		t.Errorf("the act re-encoded: %v", err)
	}
	for _, d := range []time.Duration{-6 * time.Minute, 6 * time.Minute} {
		late := f.SignAct(t, noor, provtest.Act{SignedAt: now.Add(d)})
		if _, _, err := provenance.VerifyAct(late, f.Roots, now, seen); !errors.Is(err, provenance.ErrClock) {
			t.Errorf("signed %s from the clock: %v", d, err)
		}
	}
	near := f.SignAct(t, noor, provtest.Act{SignedAt: now.Add(-4 * time.Minute)})
	if _, _, err := provenance.VerifyAct(near, f.Roots, now, seen); err != nil {
		t.Errorf("signed four minutes ago: %v", err)
	}
}

// A seen-set keeps a key until its time, then forgets it; a key kept for
// ever is never forgotten.
func TestMemorySeenLapses(t *testing.T) {
	now := time.Now()
	s := &provenance.MemorySeen{Now: func() time.Time { return now }}
	if s.Take("a", now.Add(time.Minute)) != nil || s.Take("b", time.Time{}) != nil {
		t.Fatal("first takes refused")
	}
	if !errors.Is(s.Take("a", now.Add(time.Minute)), provenance.ErrSeen) {
		t.Error("a second take of a")
	}
	now = now.Add(2 * time.Minute)
	if s.Take("a", now.Add(time.Minute)) != nil {
		t.Error("a was not forgotten after its time")
	}
	if !errors.Is(s.Take("b", time.Time{}), provenance.ErrSeen) {
		t.Error("b, kept for ever, was forgotten")
	}
}

// A control record cites its act and must match it exactly: the payload
// and record, the subject by UID and digest, the action and the params.
// One act is never cited twice.
func TestControlCitesItsAct(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	f := provtest.NewFarm(t, now)
	hud, err := provenance.NewSigner(ctx, provenance.LocalIssuer(f.CA, provenance.ComponentIdentity("hud"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	noor := f.Person(t, "noor", false)
	params := map[string]any{"unit": "8", "do": "approve", "waitsFor": "person:noor"}
	actBundle := f.SignAct(t, noor, provtest.Act{Act: "decide", Params: params})
	act, _, err := provenance.VerifyAct(actBundle, f.Roots, now, provenance.NewMemorySeen())
	if err != nil {
		t.Fatal(err)
	}
	record := c4.Identify(strings.NewReader(string(actBundle))).String()
	cite := schema.ActCitation{Payload: provenance.PayloadHash(act.Payload), Record: record}
	subject := act.Subjects[0]
	control := func(subject provenance.Subject, action string, cite *schema.ActCitation, params any) *provenance.Verified {
		t.Helper()
		rec, err := provenance.SignStatement(ctx, hud, provenance.PredicateControl, []provenance.Subject{subject},
			map[string]any{"actor": "noor", "action": action, "act": cite, "params": params}, now)
		if err != nil {
			t.Fatal(err)
		}
		v, err := provenance.VerifyBundle(rec, f.Roots)
		if err != nil || provenance.CheckSigner(v) != nil {
			t.Fatalf("control record: %v %v", err, provenance.CheckSigner(v))
		}
		return v
	}
	reordered := map[string]any{"waitsFor": "person:noor", "do": "approve", "unit": "8"}
	cited := provenance.NewMemorySeen()
	if err := provenance.CheckCitation(control(subject, "decide", &cite, reordered), act, record, cited); err != nil {
		t.Fatal(err)
	}
	if err := provenance.CheckCitation(control(subject, "decide", &cite, params), act, record, cited); !errors.Is(err, provenance.ErrSeen) {
		t.Errorf("a second citation of one act: %v", err)
	}
	newUID := subject
	newUID.Name = strings.Replace(subject.Name, provtest.JobUID, "0000aaaa-0c3a-4f8e-a6d2-5b1e7c9f3a10", 1)
	newSpec := provenance.Subject{Name: subject.Name, Digest: map[string]string{"sha256": strings.Repeat("0", 64)}}
	otherPayload := cite
	otherPayload.Payload = strings.Repeat("0", 64)
	otherRecord := cite
	otherRecord.Record = provtest.ID("another record")
	for name, v := range map[string]*provenance.Verified{
		"another job of the name":  control(newUID, "decide", &cite, params),
		"another spec":             control(newSpec, "decide", &cite, params),
		"another action":           control(subject, "steer", &cite, params),
		"other params":             control(subject, "decide", &cite, map[string]any{"unit": "8", "do": "reject", "waitsFor": "person:noor"}),
		"another act's payload":    control(subject, "decide", &otherPayload, params),
		"another act's record":     control(subject, "decide", &otherRecord, params),
		"no act":                   control(subject, "decide", nil, params),
		"params by another C4 ID:": control(subject, "decide", &cite, provtest.ID("params")),
	} {
		if err := provenance.CheckCitation(v, act, record, provenance.NewMemorySeen()); !errors.Is(err, provenance.ErrUnattributed) {
			t.Errorf("%s: %v, want unattributed", name, err)
		}
	}
}

func fixture(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
}

// A confirmed act's touch key is a touch key for the device that signed
// first, by the device records (here the fixtures': dana's Mac and its
// Touch ID key). K2 passes the records in force.
func TestTouchKeyIsTheDevicesOwn(t *testing.T) {
	var mac, touch schema.Device
	fixture(t, "../../schema/fixtures/device/pass/mac-on-a-sign-in.json", &mac)
	fixture(t, "../../schema/fixtures/device/pass/touch-key-on-a-step-up.json", &touch)
	records := []*schema.Device{&mac, &touch}
	macID, touchID := mac.Subject[0].Digest["c4"], touch.Subject[0].Digest["c4"]
	if touch.Predicate.TouchFor != macID {
		t.Fatalf("the fixtures: the touch key confirms for %s, not dana's Mac %s", touch.Predicate.TouchFor, macID)
	}
	v := provenance.Verified{Identity: provenance.UserIdentity("dana"), Farm: touch.Predicate.Farm, Device: macID, TouchDevice: touchID}
	if err := provenance.CheckTouchKey(&v, records); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*provenance.Verified){
		"the keys swapped":       func(v *provenance.Verified) { v.Device, v.TouchDevice = touchID, macID },
		"another person's":       func(v *provenance.Verified) { v.Identity = provenance.UserIdentity("noor") },
		"another farm's":         func(v *provenance.Verified) { v.Farm = provtest.ID("another farm") },
		"for another device":     func(v *provenance.Verified) { v.Device = provtest.ID("another device") },
		"a key with no record":   func(v *provenance.Verified) { v.TouchDevice = provtest.ID("a key never enrolled") },
		"the device key's twice": func(v *provenance.Verified) { v.TouchDevice = macID },
	} {
		w := v
		change(&w)
		if provenance.CheckTouchKey(&w, records) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The device fixtures' proofs are in the one form VerifyP256 takes, and
// the one in DER is refused for its form.
func TestDeviceFixtureProofs(t *testing.T) {
	check := func(path string) error {
		var d schema.Device
		fixture(t, path, &d)
		spki, err := d.Predicate.PublicKey()
		if err != nil {
			return err
		}
		k, err := x509ParsePKIX(spki)
		if err != nil {
			return err
		}
		if w := d.Predicate.WebAuthn; w != nil {
			// A passkey's proof is its WebAuthn assertion over the request.
			dec := func(s string) []byte { b, _ := base64.RawURLEncoding.DecodeString(s); return b }
			a, err := provenance.VerifyWebAuthn(k, d.Predicate.ProofMessage(), provenance.WebAuthn{AuthenticatorData: dec(w.AuthenticatorData), ClientDataJSON: dec(w.ClientDataJSON)}, dec(w.Signature))
			if err != nil {
				return err
			}
			return a.CheckRP(w.RPID, w.Origin)
		}
		sig, err := base64.RawURLEncoding.DecodeString(d.Predicate.Proof)
		if err != nil {
			return err
		}
		return provenance.VerifyP256(k, d.Predicate.ProofMessage(), sig)
	}
	paths, _ := filepath.Glob("../../schema/fixtures/device/pass/*.json")
	if len(paths) == 0 {
		t.Fatal("no device fixtures")
	}
	for _, p := range paths {
		if err := check(p); err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
		}
	}
	if err := check("../../schema/fixtures/device/fail/proof-in-der.json"); !errors.Is(err, provenance.ErrSignatureForm) {
		t.Errorf("a proof in DER: %v", err)
	}
}

// The act fixtures, bound to a scratch farm and signed with scratch keys
// of the signers they name: each that passes the schema passes VerifyAct
// at its signing time; each that fails it is refused by the signer
// policy too.
func TestActFixturesVerify(t *testing.T) {
	for _, dir := range []string{"pass", "fail"} {
		paths, _ := filepath.Glob("../../schema/fixtures/act/" + dir + "/*.json")
		for _, p := range paths {
			if strings.HasSuffix(p, ".expect.json") {
				continue
			}
			name := dir + "/" + filepath.Base(p)
			var a schema.Act
			fixture(t, p, &a)
			at, err := time.Parse(time.RFC3339, a.Predicate.SignedAt)
			if err != nil {
				at, _ = time.Parse(time.RFC3339, "2026-10-31T21:40:07Z")
			}
			f := provtest.NewFarm(t, at)
			key := signerOf(t, f, &a)
			a.Predicate.Farm = f.ID
			if key.Device != "" && a.Predicate.Device != "" {
				a.Predicate.Device = key.Device
			}
			payload, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			bundle := provtest.Sign(t, payload, key, a.Predicate.Chain.By.Grade == schema.GradeConfirmed && key.Touch != nil)
			_, _, err = provenance.VerifyAct(bundle, f.Roots, at, provenance.NewMemorySeen())
			if (err == nil) != (dir == "pass") {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
}

// signerOf makes a scratch key for the identity an act's signer field
// names, with a touch key for a person, and the grant an agent's act
// acts under.
func signerOf(t *testing.T, f *provtest.Farm, a *schema.Act) *provtest.Key {
	t.Helper()
	signer := a.Predicate.Signer
	rest, _ := strings.CutPrefix(signer, provenance.IdentityBase)
	kind, rest, _ := strings.Cut(rest, "/")
	switch kind {
	case "user":
		return f.Person(t, rest, true)
	case "agent":
		grant := ""
		for _, d := range a.Predicate.Chain.Under {
			if d.Grant != "" {
				grant = d.Grant
			}
		}
		return f.Agent(t, rest, grant)
	case "unit":
		parts := strings.Split(rest, "/")
		unit, uid, _ := strings.Cut(parts[2], "@")
		attempt, _ := strconv.Atoi(parts[3])
		return f.Unit(t, parts[0], parts[1], unit, uid, attempt)
	}
	return f.Part(t, signer)
}

func sha(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func x509ParsePKIX(spki []byte) (*ecdsa.PublicKey, error) {
	k, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("not an ECDSA key")
	}
	return pub, nil
}
