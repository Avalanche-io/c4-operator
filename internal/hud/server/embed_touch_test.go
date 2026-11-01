package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4-operator/internal/hud"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// The HUD the manager embeds makes the browser's touch key, a passkey
// (web/js/touch.js, PEOPLE-KEYS-2 K9), and confirms an act with it. The
// browser's own code runs in Node against the real issuer and the decide
// control; Node stands in for the authenticator (a P-256 key answering
// as a platform authenticator does, with user verification), IndexedDB,
// the address bar and the provider's redirect:
//   - dana's browser enrols its device key on her sign-in
//   - it makes a passkey, goes to the step-up with the passkey's device ID
//     as the nonce, and the passkey signs its own enrolment: the issuer
//     certifies it as her touch key for that device
//   - an act the farm says needs a touch is confirmed by it, and the decide
//     control takes it, graded confirmed
//
// A HUD whose touch drifts from the farm's WebAuthn form (the challenge,
// the assertion's bytes, the second signature, touchWebAuthn) fails here.
func TestEmbeddedHUDTouches(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the browser's touch code runs in Node")
	}
	dir := t.TempDir()
	for _, f := range []string{"enrol.js", "act.js", "touch.js"} {
		js, err := fs.ReadFile(hud.Web, "web/js/"+f)
		if err != nil {
			t.Fatalf("the embedded HUD has no %s: %v", f, err)
		}
		os.WriteFile(filepath.Join(dir, f), js, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "touch.mjs"), []byte(touchScript), 0o644)

	f := peopletest.Studio(t)
	f.Issuer.WebAuthn = &people.WebAuthnRP{ID: "hud.studio.example", Origin: "https://hud.studio.example"}
	s, decide := decideFarm(t)
	s.trusted, s.keysUntil = trustSet{roots: f.Scratch.Roots, legacy: map[string]ed25519.PublicKey{}}, time.Now().Add(time.Hour)
	rec := s.m.byKey["show-a/sh040-light-v007"]
	rec.job.UID = k8stypes.UID(provtest.JobUID)

	in, _ := json.Marshal(map[string]any{"origin": f.Server.URL, "person": "dana", "sub": peopletest.DanaSub,
		"job":  map[string]string{"namespace": "show-a", "name": "sh040-light-v007", "uid": provtest.JobUID, "specDigest": queue.SpecDigest(&rec.job)},
		"path": "/jobs/show-a/sh040-light-v007/decide", "body": map[string]string{"unit": "ruling", "do": "approve"}})
	cmd := exec.Command(node, filepath.Join(dir, "touch.mjs"))
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("the browser's touch code: %v\n%s", err, stderr.String())
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("the browser's touch code wrote %q: %v", b, err)
	}
	device, _ := out["device"].(string)
	passkey, _ := out["passkey"].(string)
	if out["enrolled"] != "signing" || device == "" || passkey == "" {
		t.Fatalf("the browser did not enrol and make a passkey: %v", out)
	}
	if out["nonce"] != passkey || out["maxAge"] == "" {
		t.Errorf("the step-up went with nonce %v and max_age %v; want the passkey's device ID %s and a max_age", out["nonce"], out["maxAge"], passkey)
	}
	if out["ready"] != true {
		t.Fatalf("the issuer did not certify the passkey: %v", out)
	}

	// the issuer holds it in force as dana's touch key for this browser
	r, err := http.Get(f.URL + "/v1/people/devices?person=dana")
	if err != nil {
		t.Fatal(err)
	}
	var l struct {
		Devices []struct{ Device, TouchFor, Protection string }
	}
	json.NewDecoder(r.Body).Decode(&l)
	r.Body.Close()
	found := false
	for _, d := range l.Devices {
		found = found || d.Device == passkey && d.TouchFor == device && d.Protection == "software-browser"
	}
	if !found {
		t.Errorf("dana's devices lack the passkey as a synced touch key for %s: %+v", device, l.Devices)
	}

	header, _ := out["header"].(string)
	if header == "" {
		t.Fatal("the browser confirmed nothing for an act the farm says needs a touch")
	}
	code, p := decide(decideRequest("wrangler", map[string]string{ActHeader: header}))
	if code != http.StatusOK {
		t.Fatalf("the farm refused an act the browser's passkey confirmed: %d", code)
	}
	raw, _ := base64.StdEncoding.DecodeString(header)
	var bundle provenance.Bundle
	json.Unmarshal(raw, &bundle)
	payload, _ := base64.StdEncoding.DecodeString(bundle.DSSEEnvelope.Payload)
	if p.Act == nil || p.Act.Payload != provenance.PayloadHash(payload) || p.Custody != CustodyConfirmed {
		t.Fatalf("the control record cites %+v, custody %q; want the act, confirmed", p.Act, p.Custody)
	}
	if bundle.VerificationMaterial.TouchWebAuthn == nil || len(bundle.DSSEEnvelope.Signatures) != 2 {
		t.Errorf("the act carries %d signatures and touchWebAuthn %v", len(bundle.DSSEEnvelope.Signatures), bundle.VerificationMaterial.TouchWebAuthn != nil)
	}
}

// touchScript runs the embedded enrol.js and touch.js as the HUD page
// does, with Node standing in for the authenticator, IndexedDB, the
// address bar and the provider's redirect. The provider's tokens come from
// peopletest's /sso/token: the step-up's for the nonce and max_age the
// browser sent.
const touchScript = `
import { readFileSync } from 'node:fs';
const i = JSON.parse(readFileSync(0, 'utf8'));
const ORIGIN = 'https://hud.studio.example';
globalThis.indexedDB = {};
const enrol = await import('./enrol.js');
const act = await import('./act.js');
const touch = await import('./touch.js');
const enc = new TextEncoder();
const sha256 = async (b) => new Uint8Array(await crypto.subtle.digest('SHA-256', b));
const b64url = (b) => btoa(String.fromCharCode(...b)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
const cat = (...xs) => { const o = new Uint8Array(xs.reduce((n, x) => n + x.length, 0)); let k = 0; for (const x of xs) { o.set(x, k); k += x.length; } return o; };
function der(raw) {
  const int = (b) => { let k = 0; while (k < b.length - 1 && b[k] === 0) k++; b = b.slice(k); if (b[0] & 0x80) b = cat(new Uint8Array([0]), b); return cat(new Uint8Array([2, b.length]), b); };
  const body = cat(int(raw.slice(0, 32)), int(raw.slice(32)));
  return cat(new Uint8Array([0x30, body.length]), body);
}
// a synced platform authenticator: user present and verified, backed up
const pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
const spki = new Uint8Array(await crypto.subtle.exportKey('spki', pair.publicKey));
const credId = crypto.getRandomValues(new Uint8Array(16));
const authenticator = {
  create: async (o) => {
    const ad = cat(await sha256(enc.encode(o.publicKey.rp.id)), new Uint8Array([0x5d, 0, 0, 0, 0]));
    return { rawId: credId.buffer, response: { getPublicKey: () => spki.buffer, getAuthenticatorData: () => ad.buffer } };
  },
  get: async (o) => {
    const cd = enc.encode(JSON.stringify({ type: 'webauthn.get', challenge: b64url(new Uint8Array(o.publicKey.challenge)), origin: ORIGIN, crossOrigin: false }));
    const ad = cat(await sha256(enc.encode(o.publicKey.rpId)), new Uint8Array([0x1d, 0, 0, 0, 0]));
    const sig = new Uint8Array(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, pair.privateKey, cat(ad, await sha256(cd))));
    return { response: { authenticatorData: ad.buffer, clientDataJSON: cd.buffer, signature: der(sig).buffer } };
  },
};
const keys = new Map();
const loc = { origin: ORIGIN, pathname: '/', search: '', hash: '#/me' };
const session = new Map();
let went = null;
const json = (v) => new Response(JSON.stringify(v), { headers: { 'Content-Type': 'application/json' } });
enrol.stubs({
  issuer: (path, body) => fetch(i.origin + '/issuer/v1/people' + path, body === undefined ? {} : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body }),
  fetch: async () => json({ authorization_endpoint: 'https://sso.studio.example/authorize', token_endpoint: 'https://sso.studio.example/token' }),
  token: async () => {
    const u = new URL(went);
    const t = await (await fetch(i.origin + '/sso/token?sub=' + encodeURIComponent(i.sub) + '&nonce=' + encodeURIComponent(u.searchParams.get('nonce')) + (u.searchParams.get('max_age') ? '&stepUp=1' : ''))).json();
    return json({ id_token: t.idToken });
  },
  deviceKey: async (farm) => {
    if (!keys.has(farm)) {
      const p = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
      const s = new Uint8Array(await crypto.subtle.exportKey('spki', p.publicKey));
      keys.set(farm, { key: p.privateKey, spki: s, device: await act.c4id(s) });
    }
    return keys.get(farm);
  },
  saveCertified: async (farm, c) => { const r = keys.get(farm); r.certified = c; return r; },
  forgetKey: async (farm) => { keys.delete(farm); },
  location: () => loc,
  replaceURL: (u) => { const n = new URL(u, loc.origin); Object.assign(loc, { pathname: n.pathname, search: n.search, hash: n.hash }); },
  go: (u) => { went = u; },
  session: () => ({ getItem: (k) => session.get(k) ?? null, setItem: (k, v) => session.set(k, v), removeItem: (k) => session.delete(k) }),
  local: () => ({ getItem: () => null, setItem: () => {} }),
  now: () => Date.now(), hidden: () => true, dirty: () => false, changed: () => {},
});
touch.stubs({ create: authenticator.create, get: authenticator.get, origin: () => ORIGIN,
  save: async (farm, pk) => { const r = keys.get(farm); r.passkey = pk; return r; } });
// the provider sends the page back with a code, and the page loads again
const back = async () => {
  const u = new URL(went);
  enrol.reset();
  loc.hash = '#code=from-the-provider&state=' + u.searchParams.get('state');
  await enrol.start({ user: i.person });
  return u;
};
const out = {};
await enrol.start({ user: i.person });
await back();
Object.assign(out, { enrolled: enrol.status.state, device: enrol.status.device });
await touch.begin();
const u = await back();
Object.assign(out, { passkey: touch.passkey().device, nonce: u.searchParams.get('nonce'), maxAge: u.searchParams.get('max_age') || '', touchState: enrol.status.touch });
await touch.finish();
out.ready = touch.ready();
act.learnJobs([i.job]);
act.learnNeeds({ acts: { decide: 'confirmed' } });
out.header = (await act.actHeaders(i.path, i.body))[act.ACT_HEADER] || '';
enrol.reset();
process.stdout.write(JSON.stringify(out));
`
