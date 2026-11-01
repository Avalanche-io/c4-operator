package people_test

import (
	"context"
	"crypto/x509"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/schema"
)

// A studio farm's first sign-in: dana's browser key is certified on an ID
// token whose nonce is its device ID. The leaf is user/dana naming the
// device, for 12 hours; the answer carries the identity as certified, the
// chain to the farm CA and the farm's ID. The index books the provider's
// key, then the device record, which the issuer signed, and which a
// verifier can check later against the booked key.
func TestEnrolOnASignIn(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	c := r.signIn(d, "dana", danaSub)
	if c.Identity != provenance.UserIdentity("dana") || c.Device != d.id || c.Farm != r.farm.ID || c.Order != people.OrderIssuer {
		t.Fatalf("certified %+v", c)
	}
	if len(c.Chain) != 2 {
		t.Fatalf("chain of %d", len(c.Chain))
	}
	leaf, _ := x509.ParseCertificate(c.Chain[0])
	ca, _ := x509.ParseCertificate(c.Chain[1])
	if provenance.CertIdentity(leaf) != c.Identity || provenance.CertDevice(leaf) != d.id || provenance.FarmID(ca) != r.farm.ID {
		t.Errorf("leaf %s device %s, CA %s", provenance.CertIdentity(leaf), provenance.CertDevice(leaf), provenance.FarmID(ca))
	}
	if got := leaf.NotAfter.Sub(r.now); got > people.LeafTTL {
		t.Errorf("the leaf lives %s", got)
	}
	keys, devs := r.entries(people.KindProviderKey), r.entries(people.KindDevice)
	if len(keys) != 1 || len(devs) != 1 || keys[0].Position != 2 || devs[0].Position != 3 || devs[0].Prev == "" {
		t.Fatalf("index: keys %+v devices %+v", keys, devs)
	}
	if set := r.entries(people.KindSetting); len(set) != 1 || set[0].Grade != people.GradeInstall || set[0].Issuer != ssoIssuer {
		t.Errorf("setting %+v", set)
	}
	rec, err := r.store.Get(context.Background(), devs[0].Record)
	if err != nil {
		t.Fatal(err)
	}
	v, err := provenance.VerifyBundle(rec, r.farm.Roots)
	if err != nil || provenance.CheckSigner(v) != nil || v.Identity != provenance.ComponentIdentity("issuer") {
		t.Fatalf("the record: %v, signed by %s", err, v.Identity)
	}
	dr, errs := schema.ParseDevice(v.Payload)
	if errs != nil || dr.Predicate.Evidence != schema.EvidenceIDToken || dr.Predicate.Person != "dana" {
		t.Fatalf("record %+v %v", dr, errs)
	}
	jwk, _ := r.store.Get(context.Background(), keys[0].Record)
	if dr.Predicate.ProviderKey != keys[0].Record || people.VerifyBookedToken(&dr.Predicate, jwk) != nil {
		t.Errorf("the record's token doesn't verify against the booked key %s", keys[0].Record)
	}
	// A second device of dana's signed by the same provider key books no
	// second copy of the key.
	r.signIn(newDev(t), "dana", danaSub)
	if n := len(r.entries(people.KindProviderKey)); n != 1 {
		t.Errorf("the provider key booked %d times", n)
	}
}

// refused enrols d for dana on a token with claims laid over, and wants
// 403 with want in the error.
func refused(t *testing.T, r *rig, d *dev, sub, nonce string, over map[string]any, want string) {
	t.Helper()
	e := r.enrolment(d, "dana", "", "browser")
	e.IDToken = r.sso.token(r.now, sub, nonce, over)
	code, body := r.call("POST", "/v1/people/enroll", "", e, nil)
	if code != http.StatusForbidden || !strings.Contains(body, want) {
		t.Errorf("%v: %d %s, want 403 with %q", over, code, body, want)
	}
}

// A token issued to another application at the same provider, for the
// same person, is refused: one whose audience is a tracker's, and one
// that names the farm among others but was issued to the tracker (azp).
func TestAnotherAudienceRefused(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	refused(t, r, d, danaSub, d.id, map[string]any{"aud": "tracker-flow"}, "expected audience")
	refused(t, r, d, danaSub, d.id, map[string]any{"aud": []string{clientID, "tracker-flow"}, "azp": "tracker-flow"}, "azp")
	refused(t, r, d, danaSub, d.id, map[string]any{"aud": []string{clientID, "tracker-flow"}}, "azp")
}

// The nonce binds the provider's word to this key and no other: a token
// whose nonce is another key's device ID, or anything else, is refused.
func TestNonceNotTheKeysHashRefused(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	refused(t, r, d, danaSub, newDev(t).id, nil, "nonce")
	refused(t, r, d, danaSub, "Um74e8b8wK_mlfErc_dPlQ", nil, "nonce")
}

// Only the provider the farm is configured with: a token from another
// issuer is refused, and with no provider at all nothing is certified on
// a sign-in.
func TestUnconfiguredIssuerRefused(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	refused(t, r, d, danaSub, d.id, map[string]any{"iss": "https://sso.other.example"}, "different provider")

	r = newRig(t)
	e := r.enrolment(d, "dana", "", "browser")
	e.IDToken = r.sso.token(r.now, danaSub, d.id, nil)
	if code, body := r.call("POST", "/v1/people/enroll", "", e, nil); code != http.StatusForbidden || !strings.Contains(body, "no sign-in provider") {
		t.Errorf("no provider: %d %s", code, body)
	}
}

// The subject is matched against the person's sign-ins, never the email:
// a token with dana's email and another subject is refused, as is noor's
// subject for dana's request.
func TestSubjectNotInSignInsRefused(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	refused(t, r, d, "00u-mallory", d.id, map[string]any{"email": "dana@studio.example"}, "no one's sign-in")
	refused(t, r, d, noorSub, d.id, nil, "noor's sign-in, not dana's")
}

// An expired token is refused, and so is one issued more than TokenAge
// ago, which a device can't have just been given.
func TestExpiredTokenRefused(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	refused(t, r, d, danaSub, d.id, map[string]any{"exp": r.now.Add(-time.Minute).Unix()}, "expired")
	refused(t, r, d, danaSub, d.id, map[string]any{"iat": r.now.Add(-time.Hour).Unix(), "exp": r.now.Add(time.Hour).Unix()}, "fresh one")
	refused(t, r, d, danaSub, d.id, map[string]any{"iat": r.now.Add(time.Hour).Unix()}, "not before now")
}

// A device's own proof is checked: a request whose proof another key made
// is refused, and so is one for another farm.
func TestProofIsTheKeys(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	e := r.enrolment(d, "dana", "", "browser")
	e.Proof = r.enrolment(newDev(t), "dana", "", "browser").Proof
	e.IDToken = r.sso.token(r.now, danaSub, d.id, nil)
	if code, body := r.call("POST", "/v1/people/enroll", "", e, nil); code != http.StatusForbidden || !strings.Contains(body, "proof") {
		t.Errorf("another key's proof: %d %s", code, body)
	}
}

// renew renews d for person on a fresh token.
func (r *rig) renew(d *dev, person, sub string) (int, string, *people.Certified) {
	e := r.enrolment(d, person, "", "browser")
	e.IDToken = r.sso.token(r.now, sub, d.id, nil)
	var c people.Certified
	code, body := r.call("POST", "/v1/people/renew", "", e, &c)
	return code, body, &c
}

// A renewal goes back to the provider with the same nonce, the device ID,
// and is booked: a device record of its own, naming the record it renews.
// A renewal with no fresh token is refused.
func TestRenewalBooked(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	first := r.signIn(d, "dana", danaSub)
	r.now = r.now.Add(6 * time.Hour)
	r.addr = "192.0.2.10"
	code, body, c := r.renew(d, "dana", danaSub)
	if code != http.StatusOK {
		t.Fatalf("renew: %d %s", code, body)
	}
	devs := r.entries(people.KindDevice)
	if len(devs) != 2 || c.Record != devs[1].Record || c.Position != devs[1].Position || c.Device != d.id {
		t.Fatalf("renewal %+v, index %+v", c, devs)
	}
	rec, _ := r.store.Get(context.Background(), c.Record)
	v, _ := provenance.VerifyBundle(rec, r.farm.Roots)
	dr, _ := schema.ParseDevice(v.Payload)
	if dr.Predicate.Renews != first.Record || dr.Predicate.Evidence != schema.EvidenceIDToken {
		t.Errorf("the renewal renews %q, evidence %s", dr.Predicate.Renews, dr.Predicate.Evidence)
	}
	if n := len(r.entries(people.KindFlag)); n != 0 {
		t.Errorf("a renewal from where the device was flagged %d times", n)
	}
	e := r.enrolment(d, "dana", "", "browser")
	if code, body := r.call("POST", "/v1/people/renew", "", e, nil); code != http.StatusForbidden || !strings.Contains(body, "fresh ID token") {
		t.Errorf("a renewal without a token: %d %s", code, body)
	}
}

// The same device renewing from two places at once is flagged, and its
// device list says so.
func TestTwoPlacesFlagged(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	r.signIn(d, "dana", danaSub)
	r.now = r.now.Add(10 * time.Minute)
	r.addr = "198.51.100.7"
	if code, body, _ := r.renew(d, "dana", danaSub); code != http.StatusOK {
		t.Fatalf("renew: %d %s", code, body)
	}
	flags := r.entries(people.KindFlag)
	if len(flags) != 1 || flags[0].Flag != people.FlagTwoPlaces || flags[0].Device != d.id || len(flags[0].Seen) != 2 {
		t.Fatalf("flags %+v", flags)
	}
	var list struct{ Devices []people.DeviceView }
	r.call("GET", "/v1/people/devices?person=dana", "", nil, &list)
	if len(list.Devices) != 1 || len(list.Devices[0].Flags) != 1 {
		t.Errorf("devices %+v", list.Devices)
	}
}

// A leaver: once the studio disables the account, no fresh token for
// them passes (here, their subject leaves the sign-ins), so renewal is
// refused and the last leaf, at most 12 hours, simply ends.
func TestLeaverCannotRenew(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	c := r.signIn(d, "noor", noorSub)
	delete(r.iss.SignIns.(people.StaticSignIns), "noor")
	r.now = r.now.Add(6 * time.Hour)
	if code, body, _ := r.renew(d, "noor", noorSub); code != http.StatusForbidden {
		t.Errorf("a leaver renewed: %d %s", code, body)
	}
	leaf, _ := x509.ParseCertificate(c.Chain[0])
	if leaf.NotAfter.After(r.now.Add(-6 * time.Hour).Add(people.LeafTTL)) {
		t.Errorf("the last leaf ends at %s", leaf.NotAfter)
	}
}

// A device that hasn't renewed for 90 days lapses: it is no longer in
// force, and renewing it is refused; one that renewed within 90 days is
// still in force.
func TestNinetyDayLapse(t *testing.T) {
	r := studio(t)
	quiet, busy := newDev(t), newDev(t)
	r.signIn(quiet, "dana", danaSub)
	r.signIn(busy, "dana", danaSub)
	r.now = r.now.Add(60 * 24 * time.Hour)
	if code, body, _ := r.renew(busy, "dana", danaSub); code != http.StatusOK {
		t.Fatalf("renew at 60 days: %d %s", code, body)
	}
	r.now = r.now.Add(31 * 24 * time.Hour)
	pos, _ := r.index.Position(context.Background())
	devs, err := r.index.DevicesAt(context.Background(), pos, r.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 {
		t.Fatalf("%d devices in force at 91 days", len(devs))
	}
	if spki, _ := devs[0].Predicate.PublicKey(); schema.DeviceID(spki) != busy.id {
		t.Error("the quiet device is the one in force")
	}
	if code, body, _ := r.renew(quiet, "dana", danaSub); code != http.StatusForbidden || !strings.Contains(body, "lapsed") {
		t.Errorf("a lapsed device renewed: %d %s", code, body)
	}
}

// A touch key needs a step-up sign-in: a plain token is refused, as is a
// step-up too long ago or with one factor; one with two factors in amr,
// or a multi-factor acr, is certified as a touch key for the device.
func TestTouchKeyNeedsStepUp(t *testing.T) {
	r := studio(t)
	d := newDev(t)
	r.signIn(d, "dana", danaSub)
	try := func(over map[string]any) (int, string) {
		touch := newDev(t)
		e := r.enrolment(touch, "dana", d.id, "mac")
		e.IDToken = r.sso.token(r.now, danaSub, touch.id, over)
		return r.call("POST", "/v1/people/enroll", "", e, nil)
	}
	for _, c := range []struct {
		over map[string]any
		want int
	}{
		{nil, http.StatusForbidden},
		{map[string]any{"auth_time": r.now.Unix(), "amr": []string{"pwd"}}, http.StatusForbidden},
		{map[string]any{"auth_time": r.now.Add(-time.Hour).Unix(), "amr": []string{"pwd", "otp"}}, http.StatusForbidden},
		{map[string]any{"auth_time": r.now.Unix(), "amr": []string{"pwd", "otp"}}, http.StatusOK},
		{map[string]any{"auth_time": r.now.Unix(), "acr": people.DefaultStepUpACR[0]}, http.StatusOK},
	} {
		if code, body := try(c.over); code != c.want {
			t.Errorf("%v: %d %s, want %d", c.over, code, body, c.want)
		}
	}
	var list struct{ Devices []people.DeviceView }
	r.call("GET", "/v1/people/devices?person=dana", "", nil, &list)
	if len(list.Devices) != 3 || list.Devices[1].TouchFor != d.id {
		t.Errorf("devices %+v", list.Devices)
	}
}

// On every path, a touch key's touchFor names a plain device key of the
// same person, in force: not a touch key, not another person's device,
// not a device never enrolled.
func TestTouchForNamesAPlainDeviceInForce(t *testing.T) {
	r := studio(t)
	d, noor := newDev(t), newDev(t)
	r.signIn(d, "dana", danaSub)
	r.signIn(noor, "noor", noorSub)
	stepUp := map[string]any{"auth_time": r.now.Unix(), "amr": []string{"pwd", "otp"}}
	enrol := func(touchFor string) (int, string) {
		touch := newDev(t)
		e := r.enrolment(touch, "dana", touchFor, "mac")
		e.IDToken = r.sso.token(r.now, danaSub, touch.id, stepUp)
		return r.call("POST", "/v1/people/enroll", "", e, nil)
	}
	first := newDev(t)
	e := r.enrolment(first, "dana", d.id, "mac")
	e.IDToken = r.sso.token(r.now, danaSub, first.id, stepUp)
	if code, body := r.call("POST", "/v1/people/enroll", "", e, nil); code != http.StatusOK {
		t.Fatalf("dana's touch key: %d %s", code, body)
	}
	for name, touchFor := range map[string]string{"a touch key": first.id, "noor's device": noor.id, "no device": newDev(t).id} {
		if code, body := enrol(touchFor); code != http.StatusForbidden {
			t.Errorf("touchFor %s: %d %s", name, code, body)
		}
	}
}
