package people_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/schema"
)

// The review's probes (2026-11-01), each as an attack that must be
// refused. Where the fixes changed an API a probe called, the probe is
// the same attack through the API as it is now.

// P1: a browser presenting a one-time code enrols a "touch key" naming
// the browser's own device, so two silent keys would sign confirmed acts.
func TestProbeBrowserCodeMintsTouchKey(t *testing.T) {
	r := onePerson(t)
	cli := newDev(t)
	_, _, owner := r.up(cli, nil)
	code1 := codeOf(t)
	if s, b := r.browserCode(cli, owner.Owner, code1); s != http.StatusOK {
		t.Fatalf("code1: %d %s", s, b)
	}
	b1 := newDev(t)
	e1 := r.enrolment(b1, "noor", "", "browser")
	e1.Code = code1
	var c1 people.Certified
	if s, b := r.call("POST", "/v1/people/browser", local, e1, &c1); s != http.StatusOK {
		t.Fatalf("browser 1: %d %s", s, b)
	}
	code2 := codeOf(t)
	if s, b := r.browserCode(cli, owner.Owner, code2); s != http.StatusOK {
		t.Fatalf("code2: %d %s", s, b)
	}
	fake := newDev(t)
	e2 := r.enrolment(fake, "noor", b1.id, "browser")
	e2.Code = code2
	if s, body := r.call("POST", "/v1/people/browser", local, e2, nil); s == http.StatusOK {
		t.Fatalf("VULNERABLE: the browser path enrolled a touch key: %s", body)
	}
}

// P1b: the browser path's touchFor names the owner's own touch key.
func TestProbeBrowserTouchForAnything(t *testing.T) {
	r := onePerson(t)
	cli, touch := newDev(t), newDev(t)
	_, _, owner := r.up(cli, touch)
	code := codeOf(t)
	r.browserCode(cli, owner.Owner, code)
	e := r.enrolment(newDev(t), "noor", touch.id, "browser")
	e.Code = code
	if s, body := r.call("POST", "/v1/people/browser", local, e, nil); s == http.StatusOK {
		t.Fatalf("VULNERABLE: touchFor a touch key: %s", body)
	}
}

// P2: on a studio farm, a device enrolled on an approval renews on its own
// proof for ever, so a leaver keeps it.
func TestProbeLeaverKeepsApprovedDevice(t *testing.T) {
	r := studio(t)
	laptop, laptopTouch := newDev(t), newDev(t)
	k := r.key(laptop, r.signIn(laptop, "dana", danaSub))
	e := r.enrolment(laptopTouch, "dana", laptop.id, "mac")
	e.IDToken = r.sso.Token(r.now, danaSub, laptopTouch.id, map[string]any{"auth_time": r.now.Unix(), "amr": []string{"hwk", "pin"}})
	var tc people.Certified
	if code, body := r.call("POST", "/v1/people/enroll", "", e, &tc); code != http.StatusOK {
		t.Fatalf("touch: %d %s", code, body)
	}
	k.Touch = r.key(laptopTouch, &tc)
	stash := newDev(t)
	code, body, b := r.book(r.act(k, "dana", "approve-machine", deviceSubject(stash.id), map[string]string{}, true), "")
	if code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, body)
	}
	es := r.enrolment(stash, "dana", "", "linux")
	es.Approval = b.Record
	if code, _ := r.call("POST", "/v1/people/enroll", "", es, nil); code == http.StatusOK {
		r.iss.SignIns = people.StaticSignIns{"noor": {{Issuer: ssoIssuer, Subject: noorSub}}}
		r.now = r.now.Add(11 * time.Hour)
		if code, _ := r.call("POST", "/v1/people/renew", "", r.enrolment(stash, "dana", "", "linux"), nil); code == http.StatusOK {
			t.Fatal("VULNERABLE: the approved device renewed with no ID token after dana left")
		}
	}
}

// P3: a head at the position read, naming another entry, panics sync.
func TestProbeSyncForkPanics(t *testing.T) {
	r := studio(t)
	r.signIn(newDev(t), "dana", danaSub)
	ctx := context.Background()
	if err := r.index.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	h, _ := r.head.Read(ctx)
	_, ids, _ := r.index.Entries(ctx, 0)
	if err := r.head.Write(ctx, h, people.HeadState{ID: ids[0], Position: h.Position}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("VULNERABLE: Sync panicked: %v", p)
		}
	}()
	if err := r.index.Sync(ctx); err == nil {
		t.Fatal("VULNERABLE: the forked head was read")
	}
}

// P4: a writer of the store and the head adds an approval entry citing an
// unrelated act, naming a key and a person; a fresh issuer reads it and
// certifies the key.
func TestProbeForgedApprovalEntry(t *testing.T) {
	r := studio(t)
	ctx := context.Background()
	laptop, junk := newDev(t), newDev(t)
	k := r.key(laptop, r.signIn(laptop, "dana", danaSub))
	r.signIn(junk, "dana", danaSub)
	code, body, rm := r.book(r.act(k, "dana", "remove-device", deviceSubject(junk.id), map[string]string{}, false), "")
	if code != http.StatusOK {
		t.Fatalf("remove: %d %s", code, body)
	}
	attacker := newDev(t)
	h, _ := r.head.Read(ctx)
	e := people.Entry{Farm: r.farm.ID, Position: h.Position + 1, Prev: h.ID, Order: people.OrderIssuer, At: r.now.UTC().Format(time.RFC3339Nano),
		Kind: people.KindApproval, Record: rm.Record, Device: attacker.id, Person: "noor", By: "noor",
		Expires: r.now.Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(e)
	b, err := schema.Canonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	id := c4.Identify(bytes.NewReader(b)).String()
	r.store.Put(ctx, id, b)
	// Unsigned, as the store's and the Secret's credentials alone can
	// write it; and signed as the issuer, as if its key had leaked, so
	// the entry's own check is what refuses it.
	for _, signed := range [][]byte{nil, r.signedHead("issuer", id, h.Position+1)} {
		cur, _ := r.head.Read(ctx)
		if err := r.head.Write(ctx, cur, people.HeadState{ID: id, Position: h.Position + 1, Signed: signed}); err != nil {
			t.Fatal(err)
		}
		fresh := &people.Index{Farm: r.farm.ID, Roots: r.farm.Roots, Store: r.store, Head: r.head}
		if err := fresh.Sync(ctx); err == nil {
			t.Fatalf("VULNERABLE: a fresh read took the forged entry (signed head: %v)", signed != nil)
		}
	}
}

// P5: a booked record's ID token is checked only for signature and nonce:
// dana's token passes on a record naming noor, and a token for another
// issuer and audience passes too.
func TestProbeVerifyBookedTokenIgnoresSubject(t *testing.T) {
	r := studio(t)
	ctx := context.Background()
	d := newDev(t)
	c := r.signIn(d, "dana", danaSub)
	jwk, _ := r.store.Get(ctx, r.entries(people.KindProviderKey)[0].Record)
	b, _ := r.store.Get(ctx, c.Record)
	v, err := provenance.VerifyBundle(b, r.farm.Roots)
	if err != nil {
		t.Fatal(err)
	}
	dev, _ := schema.ParseDevice(v.Payload)
	set := people.Provider{Issuer: ssoIssuer, ClientID: clientID}
	ins := r.iss.SignIns.(people.StaticSignIns)
	p := dev.Predicate
	p.Person = "noor"
	if err := people.CheckBookedToken(&p, jwk, set, ins); err == nil {
		t.Error("VULNERABLE: dana's token passed on a record naming noor")
	}
	p.IDToken = r.sso.Token(r.now, noorSub, d.id, map[string]any{"aud": "some-other-app", "iss": "https://evil.example"})
	if err := people.CheckBookedToken(&p, jwk, set, ins); err == nil {
		t.Error("VULNERABLE: a token for another audience and issuer passed")
	}
}

// P6: any caller reads every person's raw ID tokens and addresses.
func TestProbeReadTokens(t *testing.T) {
	r := studio(t)
	c := r.signIn(newDev(t), "dana", danaSub)
	r.caller = ""
	if code, _ := r.call("GET", "/v1/people/index", "", nil, nil); code == http.StatusOK {
		t.Error("VULNERABLE: the index was read by no one")
	}
	if code, _ := r.call("GET", "/v1/people/records/"+c.Record, "", nil, nil); code == http.StatusOK {
		t.Error("VULNERABLE: a device record was read by no one")
	}
}

// P6b: a raw, live ID token read unauthenticated.
func TestProbeReadTokens2(t *testing.T) {
	r := studio(t)
	c := r.signIn(newDev(t), "dana", danaSub)
	r.caller = "noor"
	_, body := r.call("GET", "/v1/people/records/"+c.Record, "", nil, nil)
	if v, err := provenance.VerifyBundle([]byte(body), r.farm.Roots); err == nil {
		if d, _ := schema.ParseDevice(v.Payload); d.Predicate.IDToken != "" {
			t.Error("VULNERABLE: noor read dana's raw ID token")
		}
	}
}

// P7: an act replayed to another replica within its five minutes is
// booked again: an old provider setting reverts a newer one.
func TestProbeReplayRevertsSetting(t *testing.T) {
	r := studio(t)
	dana := newDev(t)
	kd := r.key(dana, r.signIn(dana, "dana", danaSub))
	p1 := map[string]string{"issuer": "https://login-a.studio.example", "clientID": "c4-a"}
	a1 := r.act(kd, "dana", "set-sign-in-provider", r.farmSubject(), p1, false)
	if code, _, _ := r.book(a1, ""); code == http.StatusOK {
		t.Fatal("VULNERABLE: an admin's act set the provider")
	}
	other := &people.Issuer{CA: r.iss.CA, Index: &people.Index{Farm: r.farm.ID, Roots: r.farm.Roots, Store: r.store, Head: r.head},
		Now: r.iss.Now, Admin: r.iss.Admin, Provider: r.iss.Provider}
	r.iss = other
	r.now = r.now.Add(2 * time.Minute)
	if code, _, _ := r.book(a1, ""); code == http.StatusOK {
		t.Fatal("VULNERABLE: the act was booked at another replica")
	}
	var cfg people.Config
	r.call("GET", "/v1/people/config", "", nil, &cfg)
	if cfg.Provider == nil || cfg.Provider.Issuer != ssoIssuer {
		t.Errorf("VULNERABLE: the provider in force is %+v", cfg.Provider)
	}
}

// P8: two replicas race one code.
func TestProbeCodeRaceTwoReplicas(t *testing.T) {
	r := onePerson(t)
	cli := newDev(t)
	_, _, owner := r.up(cli, nil)
	code := codeOf(t)
	r.browserCode(cli, owner.Owner, code)
	other := &people.Issuer{CA: r.iss.CA, Index: &people.Index{Farm: r.farm.ID, Roots: r.farm.Roots, Store: r.store, Head: r.head},
		Now: r.iss.Now, Owner: r.iss.Owner, InstallerToken: r.iss.InstallerToken, LocalOnly: r.iss.LocalOnly}
	ok := make(chan int, 2)
	for _, iss := range []*people.Issuer{r.iss, other} {
		e := r.enrolment(newDev(t), "noor", "", "browser")
		e.Code = code
		go func(iss *people.Issuer, b []byte) {
			req := httptest.NewRequest("POST", "/v1/people/browser", bytes.NewReader(b))
			req.RemoteAddr = local
			w := httptest.NewRecorder()
			iss.ServeHTTP(w, req)
			ok <- w.Code
		}(iss, mustJSON(t, e))
	}
	if a, b := <-ok, <-ok; a == http.StatusOK && b == http.StatusOK {
		t.Error("VULNERABLE: one code enrolled two browsers")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
