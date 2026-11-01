package people_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

const (
	hudRP     = "hud.studio.example"
	hudOrigin = "https://hud.studio.example"
)

// passkeyEnrolment is d's enrolment as a browser's passkey touch key for
// touchFor: its proof a WebAuthn assertion over the request, made at
// origin for rp, on a step-up sign-in.
func (r *rig) passkeyEnrolment(d *dev, person, touchFor, rp, origin string) people.Enrolment {
	e := r.enrolment(d, person, touchFor, "browser")
	q := e.Request
	p := schema.DevicePredicate{Farm: q.Farm, Person: q.Person, Key: q.Key, TouchFor: q.TouchFor, Nonce: q.Nonce, RequestedAt: q.RequestedAt}
	k := &provtest.Key{P256: d.key, RPID: rp, Origin: origin}
	w, sig := k.Assertion(r.t, p.ProofMessage())
	cred := sha256.Sum256(d.spki)
	b64 := base64.RawURLEncoding.EncodeToString
	e.Proof = ""
	e.WebAuthn = &schema.DeviceWebAuthn{RPID: rp, Origin: origin, CredentialID: b64(cred[:]),
		AuthenticatorData: b64(w.AuthenticatorData), ClientDataJSON: b64(w.ClientDataJSON), Signature: b64(sig)}
	e.IDToken = r.sso.Token(r.now, danaSub, d.id, map[string]any{"auth_time": r.now.Unix(), "amr": []string{"pwd", "hwk"}})
	return e
}

// A browser's touch key is a passkey: enrolled on its WebAuthn assertion
// at the farm's HUD, after a step-up sign-in. Its record keeps the relying
// party and origin, and its touches then confirm the person's acts,
// checked against the index.
func TestPasskeyTouchKey(t *testing.T) {
	r := studio(t)
	r.iss.WebAuthn = &people.WebAuthnRP{ID: hudRP, Origin: hudOrigin}
	browser, pk := newDev(t), newDev(t)
	k := r.key(browser, r.signIn(browser, "dana", danaSub))

	var conf people.Config
	if code, body := r.call("GET", "/v1/people/config", "", nil, &conf); code != http.StatusOK || conf.WebAuthn == nil || *conf.WebAuthn != *r.iss.WebAuthn {
		t.Fatalf("config: %d %s", code, body)
	}

	var tc people.Certified
	if code, body := r.call("POST", "/v1/people/enroll", "", r.passkeyEnrolment(pk, "dana", browser.id, hudRP, hudOrigin), &tc); code != http.StatusOK {
		t.Fatalf("the passkey: %d %s", code, body)
	}
	rec, err := r.store.Get(context.Background(), tc.Record)
	if err != nil {
		t.Fatal(err)
	}
	v, err := provenance.VerifyBundle(rec, r.farm.Roots)
	if err != nil {
		t.Fatal(err)
	}
	d, errs := schema.ParseDevice(v.Payload)
	if errs != nil || d.Predicate.WebAuthn == nil || d.Predicate.WebAuthn.Origin != hudOrigin || d.Predicate.Proof != "" {
		t.Fatalf("the passkey's record: %+v %v", d, errs)
	}

	k.Touch = r.key(pk, &tc)
	k.Passkey(hudRP, hudOrigin)
	pos, _ := r.index.Position(context.Background())
	if err := r.checkAt(r.act(k, "dana", "grant", provenance.Subject{}, map[string]string{"to": "agent assistant"}, true), pos); err != nil {
		t.Errorf("a grant confirmed with the passkey: %v", err)
	}
	// The same passkey's touch made at another origin is refused.
	k.Passkey(hudRP, "https://hud.studio.example:8443")
	if err := r.checkAt(r.act(k, "dana", "grant", provenance.Subject{}, map[string]string{"to": "agent assistant"}, true), pos); err == nil {
		t.Error("a touch made at another origin confirmed the act")
	}
}

// A passkey is refused where the farm takes none, made for another site,
// signed over another request, or as anything but a touch key.
func TestPasskeyEnrolmentRefused(t *testing.T) {
	r := studio(t)
	browser := newDev(t)
	r.signIn(browser, "dana", danaSub)
	pk := newDev(t)
	if code, body := r.call("POST", "/v1/people/enroll", "", r.passkeyEnrolment(pk, "dana", browser.id, hudRP, hudOrigin), nil); code != http.StatusForbidden {
		t.Errorf("a farm with no HUD origin took a passkey: %d %s", code, body)
	}
	r.iss.WebAuthn = &people.WebAuthnRP{ID: hudRP, Origin: hudOrigin}
	for name, e := range map[string]people.Enrolment{
		"made for another site":    r.passkeyEnrolment(newDev(t), "dana", browser.id, "hud-studio.example", "https://hud-studio.example"),
		"signed at another origin": r.passkeyEnrolment(newDev(t), "dana", browser.id, hudRP, "https://evil.example"),
		"over another request": func() people.Enrolment {
			e := r.passkeyEnrolment(newDev(t), "dana", browser.id, hudRP, hudOrigin)
			e.Request.Nonce = "AAAAAAAAAAAAAAAAAAAAAA"
			return e
		}(),
		"the origin it names not the one it signed at": func() people.Enrolment {
			e := r.passkeyEnrolment(newDev(t), "dana", browser.id, hudRP, "https://evil.example")
			e.WebAuthn.Origin = hudOrigin
			return e
		}(),
	} {
		if code, body := r.call("POST", "/v1/people/enroll", "", e, nil); code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	plain := r.passkeyEnrolment(newDev(t), "dana", "", hudRP, hudOrigin)
	if code, body := r.call("POST", "/v1/people/enroll", "", plain, nil); code == http.StatusOK {
		t.Errorf("a passkey as a device key: %d %s", code, body)
	}
}
