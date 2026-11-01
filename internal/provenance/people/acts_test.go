package people_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// book sends a signed act to the issuer.
func (r *rig) book(act []byte, host string) (int, string, *people.Booked) {
	var b people.Booked
	code, body := r.call("POST", "/v1/people/acts", host, act, &b)
	return code, body, &b
}

// checkAt checks an act against the device records in force at pos.
func (r *rig) checkAt(act []byte, pos int64) error {
	r.t.Helper()
	v, err := provenance.VerifyBundle(act, r.farm.Roots)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.index.CheckAct(context.Background(), v, pos)
}

// A removed device's certificates are no longer in force from the
// removal's position: an act it signed checks at a position before the
// removal and not at or after it, though its leaf still chains. It
// can't renew, and its key can't come back. Removing someone else's
// device takes an admin.
func TestRemovedDeviceOutOfForce(t *testing.T) {
	r := studio(t)
	d, other := newDev(t), newDev(t)
	k := r.key(d, r.signIn(d, "noor", noorSub))
	ko := r.key(other, r.signIn(other, "noor", noorSub))
	approve := r.act(k, "noor", "approve", provenance.Subject{}, nil, false)
	before, _ := r.index.Position(context.Background())
	if err := r.checkAt(approve, before); err != nil {
		t.Fatalf("before the removal: %v", err)
	}
	dana := newDev(t)
	kd := r.key(dana, r.signIn(dana, "dana", danaSub))
	if code, body, _ := r.book(r.act(ko, "noor", "remove-device", deviceSubject(dana.id), nil, false), ""); code != http.StatusForbidden || !strings.Contains(body, "an admin") {
		t.Errorf("noor removed dana's device: %d %s", code, body)
	}
	code, body, b := r.book(r.act(ko, "noor", "remove-device", deviceSubject(d.id), nil, false), "")
	if code != http.StatusOK || b.Entry.Kind != people.KindRemoval || b.Entry.Device != d.id || b.Entry.Person != "noor" || b.Entry.By != "noor" {
		t.Fatalf("remove: %d %s", code, body)
	}
	if err := r.checkAt(approve, b.Position); !errors.Is(err, people.ErrNotInForce) {
		t.Errorf("at the removal's position: %v", err)
	}
	if err := r.checkAt(approve, before); err != nil {
		t.Errorf("before the removal, after it was booked: %v", err)
	}
	if code, body, _ := r.renew(d, "noor", noorSub); code != http.StatusForbidden || !strings.Contains(body, "removed") {
		t.Errorf("a removed device renewed: %d %s", code, body)
	}
	e := r.enrolment(d, "noor", "", "browser")
	e.IDToken = r.sso.token(r.now, noorSub, d.id, nil)
	if code, _ := r.call("POST", "/v1/people/enroll", "", e, nil); code != http.StatusConflict {
		t.Errorf("a removed key enrolled again: %d", code)
	}
	// An admin removes all of a person's devices at once; one enrolled
	// after stays.
	if code, body, _ := r.book(r.act(kd, "dana", "remove-device", r.farmSubject(), map[string]string{"person": "noor"}, false), ""); code != http.StatusOK {
		t.Fatalf("remove all of noor's: %d %s", code, body)
	}
	later := newDev(t)
	r.signIn(later, "noor", noorSub)
	pos, _ := r.index.Position(context.Background())
	devs, _ := r.index.DevicesAt(context.Background(), pos, r.now)
	if n := countPerson(devs, "noor"); n != 1 {
		t.Errorf("noor has %d devices in force after the removal and one sign-in", n)
	}
}

func countPerson(devs []*schema.Device, person string) int {
	n := 0
	for _, d := range devs {
		if d.Predicate.Person == person {
			n++
		}
	}
	return n
}

// A person's act whose leaf names a device the index never booked is a
// forgery: the leaf chains, and still nothing is in force behind it.
func TestLeafWithoutRecordIsAForgery(t *testing.T) {
	r := studio(t)
	scratch := r.farm.Person(t, "dana", false)
	act := r.farm.SignAct(t, scratch, provtest.Act{SignedAt: r.now})
	if err := r.checkAt(act, 0); !errors.Is(err, people.ErrNotInForce) || !strings.Contains(err.Error(), "forgery") {
		t.Errorf("a leaf with no record: %v", err)
	}
}

// Until a touch can confirm it, the sign-in provider changes only with the
// install: an admin's signed set-sign-in-provider act is refused, so a
// page served by a compromised HUD can't sign one with the admin's browser
// key and point the farm at another client. Redeploying with other flags
// books a new install setting, and a token for the old client no longer
// certifies anyone.
func TestProviderSettingOnlyByInstall(t *testing.T) {
	r := studio(t)
	dana := newDev(t)
	kd := r.key(dana, r.signIn(dana, "dana", danaSub))
	next := map[string]string{"issuer": ssoIssuer, "clientID": "c4-farm-attacker"}
	if code, body, _ := r.book(r.act(kd, "dana", "set-sign-in-provider", r.farmSubject(), next, false), ""); code != http.StatusForbidden || !strings.Contains(body, "redeploying") {
		t.Errorf("an admin's act set the provider: %d %s", code, body)
	}
	var cfg people.Config
	r.call("GET", "/v1/people/config", "", nil, &cfg)
	if cfg.Provider == nil || cfg.Provider.ClientID != clientID || cfg.Provider.Grade != people.GradeInstall {
		t.Fatalf("config %+v", cfg.Provider)
	}
	r.iss.Provider = people.Provider{Issuer: ssoIssuer, ClientID: "c4-farm-2"}
	r.call("GET", "/v1/people/config", "", nil, &cfg)
	if cfg.Provider.ClientID != "c4-farm-2" || cfg.Provider.Grade != people.GradeInstall || len(r.entries(people.KindSetting)) != 2 {
		t.Errorf("redeployed: %+v", cfg.Provider)
	}
	d := newDev(t)
	e := r.enrolment(d, "dana", "", "browser")
	e.IDToken = r.sso.token(r.now, danaSub, d.id, nil)
	if code, _ := r.call("POST", "/v1/people/enroll", "", e, nil); code != http.StatusForbidden {
		t.Errorf("a token for the old client certified a key: %d", code)
	}
}

// A touch key on a second device is enrolled on a touch from an existing
// touch key: a confirmed approve-machine act naming the new key, then the
// new key enrols with evidence approval. An approval only signed, without
// the touch, is refused; so is an approval used for another key, or after
// it lapses. The new touch key then confirms the person's acts, checked
// against the index (CheckTouchKey).
func TestTouchKeyByApproval(t *testing.T) {
	r := studio(t)
	laptop, laptopTouch := newDev(t), newDev(t)
	k := r.key(laptop, r.signIn(laptop, "dana", danaSub))
	e := r.enrolment(laptopTouch, "dana", laptop.id, "mac")
	e.IDToken = r.sso.token(r.now, danaSub, laptopTouch.id, map[string]any{"auth_time": r.now.Unix(), "amr": []string{"hwk", "pin"}})
	var tc people.Certified
	if code, body := r.call("POST", "/v1/people/enroll", "", e, &tc); code != http.StatusOK {
		t.Fatalf("the first touch key: %d %s", code, body)
	}
	k.Touch = r.key(laptopTouch, &tc)

	desk := newDev(t)
	kdesk := r.key(desk, r.signIn(desk, "dana", danaSub))
	deskTouch := newDev(t)
	if code, _, _ := r.book(r.act(k, "dana", "approve-machine", deviceSubject(deskTouch.id), map[string]string{}, false), ""); code != http.StatusForbidden {
		t.Errorf("an untouched approval: %d", code)
	}
	code, body, b := r.book(r.act(k, "dana", "approve-machine", deviceSubject(deskTouch.id), map[string]string{}, true), "")
	if code != http.StatusOK || b.Entry.Kind != people.KindApproval {
		t.Fatalf("approve: %d %s", code, body)
	}
	wrong := newDev(t)
	ew := r.enrolment(wrong, "dana", desk.id, "mac")
	ew.Approval = b.Record
	if code, _ := r.call("POST", "/v1/people/enroll", "", ew, nil); code != http.StatusForbidden {
		t.Errorf("an approval used for another key: %d", code)
	}
	ed := r.enrolment(deskTouch, "dana", desk.id, "mac")
	ed.Approval = b.Record
	var dc people.Certified
	if code, body := r.call("POST", "/v1/people/enroll", "", ed, &dc); code != http.StatusOK {
		t.Fatalf("enrol on the approval: %d %s", code, body)
	}
	kdesk.Touch = r.key(deskTouch, &dc)
	grant := r.act(kdesk, "dana", "grant", provenance.Subject{}, map[string]string{"to": "agent assistant"}, true)
	pos, _ := r.index.Position(context.Background())
	if err := r.checkAt(grant, pos); err != nil {
		t.Errorf("a grant confirmed with the new touch key: %v", err)
	}
	// The desk's touch key confirms for the desk, not the laptop.
	k.Touch = kdesk.Touch
	if err := r.checkAt(r.act(k, "dana", "grant", provenance.Subject{}, nil, true), pos); err == nil {
		t.Error("the desk's touch key confirmed the laptop's act")
	}
	// An approval works for ApprovalTTL.
	k.Touch = r.key(laptopTouch, &tc)
	late := newDev(t)
	code, body, lb := r.book(r.act(k, "dana", "approve-machine", deviceSubject(late.id), map[string]string{}, true), "")
	if code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, body)
	}
	r.now = r.now.Add(people.ApprovalTTL + time.Second)
	el := r.enrolment(late, "dana", desk.id, "mac")
	el.Approval = lb.Record
	if code, body := r.call("POST", "/v1/people/enroll", "", el, nil); code != http.StatusForbidden || !strings.Contains(body, "lapsed") {
		t.Errorf("a lapsed approval: %d %s", code, body)
	}
}

// On a farm with a sign-in provider, every device renews on a fresh ID
// token for its person, whatever it was enrolled on, and an approval
// enrols only a touch key (review probe P2). A device approved from
// another would otherwise renew on its own proof for ever, and a leaver
// would keep it.
func TestLeaverKeepsNoApprovedDevice(t *testing.T) {
	r := studio(t)
	laptop, laptopTouch, desk := newDev(t), newDev(t), newDev(t)
	k := r.key(laptop, r.signIn(laptop, "dana", danaSub))
	r.signIn(desk, "dana", danaSub)
	e := r.enrolment(laptopTouch, "dana", laptop.id, "mac")
	e.IDToken = r.sso.token(r.now, danaSub, laptopTouch.id, map[string]any{"auth_time": r.now.Unix(), "amr": []string{"hwk", "pin"}})
	var tc people.Certified
	if code, body := r.call("POST", "/v1/people/enroll", "", e, &tc); code != http.StatusOK {
		t.Fatalf("touch: %d %s", code, body)
	}
	k.Touch = r.key(laptopTouch, &tc)
	approve := func(id string) string {
		code, body, b := r.book(r.act(k, "dana", "approve-machine", deviceSubject(id), map[string]string{}, true), "")
		if code != http.StatusOK {
			t.Fatalf("approve: %d %s", code, body)
		}
		return b.Record
	}
	stash := newDev(t)
	es := r.enrolment(stash, "dana", "", "linux")
	es.Approval = approve(stash.id)
	if code, body := r.call("POST", "/v1/people/enroll", "", es, nil); code != http.StatusForbidden || !strings.Contains(body, "enrols by signing in") {
		t.Fatalf("an approved plain device on a provider farm: %d %s", code, body)
	}
	deskTouch := newDev(t)
	ed := r.enrolment(deskTouch, "dana", desk.id, "mac")
	ed.Approval = approve(deskTouch.id)
	if code, body := r.call("POST", "/v1/people/enroll", "", ed, nil); code != http.StatusOK {
		t.Fatalf("an approved touch key: %d %s", code, body)
	}
	r.now = r.now.Add(6 * time.Hour)
	if code, body := r.call("POST", "/v1/people/renew", "", r.enrolment(deskTouch, "dana", desk.id, "mac"), nil); code != http.StatusForbidden || !strings.Contains(body, "fresh ID token") {
		t.Errorf("an approved touch key renewed on its proof: %d %s", code, body)
	}
	// dana leaves: no token of hers passes, and nothing of hers renews.
	r.iss.SignIns = people.StaticSignIns{"noor": {{Issuer: ssoIssuer, Subject: noorSub}}}
	for _, d := range []struct {
		dev      *dev
		touchFor string
	}{{laptop, ""}, {deskTouch, desk.id}, {laptopTouch, laptop.id}} {
		en := r.enrolment(d.dev, "dana", d.touchFor, "mac")
		en.IDToken = r.sso.token(r.now, danaSub, d.dev.id, nil)
		if code, body := r.call("POST", "/v1/people/renew", "", en, nil); code != http.StatusForbidden {
			t.Errorf("a leaver's device renewed: %d %s", code, body)
		}
	}
}

// An act the index books is refused a second time at another replica,
// whose seen-set in memory has never held it, or after a restart (review
// probe P7): the index is shared and lasting, and keeps each act's
// payload hash.
func TestActReplayedAtAnotherReplica(t *testing.T) {
	r := studio(t)
	laptop, touch, desk := newDev(t), newDev(t), newDev(t)
	k := r.key(laptop, r.signIn(laptop, "dana", danaSub))
	r.signIn(desk, "dana", danaSub)
	e := r.enrolment(touch, "dana", laptop.id, "mac")
	e.IDToken = r.sso.token(r.now, danaSub, touch.id, map[string]any{"auth_time": r.now.Unix(), "amr": []string{"pwd", "otp"}})
	var tc people.Certified
	if code, body := r.call("POST", "/v1/people/enroll", "", e, &tc); code != http.StatusOK {
		t.Fatalf("touch: %d %s", code, body)
	}
	k.Touch = r.key(touch, &tc)
	act := r.act(k, "dana", "approve-machine", deviceSubject(newDev(t).id), map[string]string{}, true)
	if code, body, _ := r.book(act, ""); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, body)
	}
	r.iss = &people.Issuer{CA: r.iss.CA, Index: &people.Index{Farm: r.farm.ID, Roots: r.farm.Roots, Store: r.store, Head: r.head},
		Now: r.iss.Now, Admin: r.iss.Admin, Provider: r.iss.Provider, SignIns: r.iss.SignIns, HTTPClient: r.iss.HTTPClient}
	r.now = r.now.Add(time.Minute)
	if code, body, _ := r.book(act, ""); code != http.StatusConflict || !strings.Contains(body, "booked already") {
		t.Errorf("the act replayed at another replica: %d %s", code, body)
	}
	if n := len(r.entries(people.KindApproval)); n != 1 {
		t.Errorf("%d approvals booked", n)
	}
}
