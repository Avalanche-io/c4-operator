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
	"slices"
	"testing"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/Avalanche-io/c4-operator/internal/hud"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/internal/work/queue"
)

// The HUD the manager embeds enrols its browser key at the farm's people's
// issuer (web/js/enrol.js, PEOPLE-KEYS-2 K3), and what the issuer
// certifies signs acts the farm takes. The browser's own code runs in
// Node against the real issuer (peopletest); Node stands in only for
// IndexedDB, the address bar and the provider's redirect:
//   - a one-person farm: the browser enrols on the one-time code its owner
//     booked, renews on its own proof, lists itself among noor's devices,
//     and removes itself with a signed act
//   - a studio farm: the browser goes to the provider with its device ID
//     as the nonce, enrols on the ID token that comes back, and its first
//     act, a decide, is verified, taken and cited, graded signed
//
// A HUD whose enrolment drifts from the issuer's (the request's canonical
// form, the proof, the evidence, the Certified it reads) fails here.
func TestEmbeddedHUDEnrols(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the browser's enrolment code runs in Node")
	}
	dir := t.TempDir()
	for _, f := range []string{"enrol.js", "act.js"} {
		js, err := fs.ReadFile(hud.Web, "web/js/"+f)
		if err != nil {
			t.Fatalf("the embedded HUD has no %s: %v", f, err)
		}
		os.WriteFile(filepath.Join(dir, f), js, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "enrol.mjs"), []byte(enrolScript), 0o644)
	run := func(in map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(in)
		cmd := exec.Command(node, filepath.Join(dir, "enrol.mjs"))
		cmd.Stdin = bytes.NewReader(b)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("the browser's enrolment code: %v\n%s", err, stderr.String())
		}
		var o map[string]any
		if err := json.Unmarshal(out, &o); err != nil {
			t.Fatalf("the browser's enrolment code wrote %q: %v", out, err)
		}
		return o
	}
	devices := func(f *peopletest.Farm, person string) []string {
		t.Helper()
		r, err := http.Get(f.URL + "/v1/people/devices?person=" + person)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var l struct{ Devices []struct{ Device string } }
		json.NewDecoder(r.Body).Decode(&l)
		var ids []string
		for _, d := range l.Devices {
			ids = append(ids, d.Device)
		}
		return ids
	}

	t.Run("one-person", func(t *testing.T) {
		o := peopletest.OnePerson(t)
		out := run(map[string]any{"origin": o.Server.URL, "person": "noor", "code": o.NewCode(t)})
		device, _ := out["device"].(string)
		if out["enrolled"] != "signing" || device == "" {
			t.Fatalf("the browser did not enrol on its code: %v", out)
		}
		if out["renewed"] != "signing" || out["renewedExpires"] == out["expires"] {
			t.Errorf("the browser did not renew on its own proof: %v", out)
		}
		if !slices.Contains(toStrings(out["listed"]), device) {
			t.Errorf("noor's device list, as the browser read it, lacks this browser: %v", out["listed"])
		}
		if out["removed"] != "open" {
			t.Errorf("after removing itself the browser is %v, want open (enrol anew with c4 farm open)", out["removed"])
		}
		if slices.Contains(devices(o.Farm, "noor"), device) {
			t.Error("the browser removed itself, and the issuer still holds it in force")
		}
	})

	t.Run("studio", func(t *testing.T) {
		f := peopletest.Studio(t)
		s, decide := decideFarm(t)
		s.trusted, s.keysUntil = trustSet{roots: f.Scratch.Roots, legacy: map[string]ed25519.PublicKey{}}, time.Now().Add(time.Hour)
		rec := s.m.byKey["show-a/sh040-light-v007"]
		rec.job.UID = k8stypes.UID(provtest.JobUID)
		out := run(map[string]any{"origin": f.Server.URL, "person": "dana", "sub": peopletest.DanaSub,
			"job":  map[string]string{"namespace": "show-a", "name": "sh040-light-v007", "uid": provtest.JobUID, "specDigest": queue.SpecDigest(&rec.job)},
			"path": "/jobs/show-a/sh040-light-v007/decide", "body": map[string]string{"unit": "ruling", "do": "approve"}})
		device, _ := out["device"].(string)
		if out["nonce"] != device || out["prompt"] != "none" || out["clientID"] != peopletest.ClientID {
			t.Errorf("the browser went to the provider with nonce %v, prompt %v, client %v; want its device ID %s, none, %s", out["nonce"], out["prompt"], out["clientID"], device, peopletest.ClientID)
		}
		if out["enrolled"] != "signing" {
			t.Fatalf("the browser did not enrol on its ID token: %v", out)
		}
		if !slices.Contains(devices(f, "dana"), device) {
			t.Error("dana's devices lack the browser that enrolled")
		}
		header, _ := out["header"].(string)
		if header == "" {
			t.Fatal("the enrolled browser signed nothing for a decide on a job it can name")
		}
		code, p := decide(decideRequest("wrangler", map[string]string{ActHeader: header}))
		if code != http.StatusOK {
			t.Fatalf("the farm refused the act of a browser its issuer certified: %d", code)
		}
		b, _ := base64.StdEncoding.DecodeString(header)
		var bundle provenance.Bundle
		json.Unmarshal(b, &bundle)
		payload, _ := base64.StdEncoding.DecodeString(bundle.DSSEEnvelope.Payload)
		if p.Act == nil || p.Act.Payload != provenance.PayloadHash(payload) || p.Custody != CustodySigned {
			t.Fatalf("the control record cites %+v, custody %q", p.Act, p.Custody)
		}
	})
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// enrolScript runs the embedded enrol.js as the HUD page does, with Node
// standing in for IndexedDB (an in-memory key store of non-extractable
// WebCrypto keys), the address bar and the provider's redirect: the
// provider's token comes from peopletest's /sso/token, for the nonce the
// browser sent. It writes what it saw as one JSON object.
const enrolScript = `
import { readFileSync } from 'node:fs';
const i = JSON.parse(readFileSync(0, 'utf8'));
globalThis.indexedDB = {};
const enrol = await import('./enrol.js');
const act = await import('./act.js');
const keys = new Map();
const loc = { origin: 'http://127.0.0.1', pathname: '/', search: '', hash: i.code ? '#/me?browser=' + i.code : '#/me' };
const session = new Map();
let went = null;
const json = (v) => new Response(JSON.stringify(v), { headers: { 'Content-Type': 'application/json' } });
enrol.stubs({
  issuer: (path, body) => fetch(i.origin + '/issuer/v1/people' + path, body === undefined ? {} : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body }),
  fetch: async (url) => json({ authorization_endpoint: 'https://sso.studio.example/authorize', token_endpoint: 'https://sso.studio.example/token' }),
  token: async () => {
    const t = await (await fetch(i.origin + '/sso/token?sub=' + encodeURIComponent(i.sub) + '&nonce=' + encodeURIComponent(new URL(went).searchParams.get('nonce')))).json();
    return json({ id_token: t.idToken });
  },
  deviceKey: async (farm) => {
    if (!keys.has(farm)) {
      const pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
      const spki = new Uint8Array(await crypto.subtle.exportKey('spki', pair.publicKey));
      keys.set(farm, { key: pair.privateKey, spki, device: await act.c4id(spki) });
    }
    return keys.get(farm);
  },
  saveCertified: async (farm, c) => { const r = keys.get(farm); r.certified = c; return r; },
  forgetKey: async (farm) => { keys.delete(farm); },
  location: () => loc,
  replaceURL: (u) => { const n = new URL(u, loc.origin); Object.assign(loc, { pathname: n.pathname, search: n.search, hash: n.hash }); },
  go: (u) => { went = u; },
  session: () => ({ getItem: (k) => session.get(k) ?? null, setItem: (k, v) => session.set(k, v), removeItem: (k) => session.delete(k) }),
  now: () => Date.now(),
  hidden: () => true,
  dirty: () => false,
  changed: () => {},
  // the seal of a provisional enrolment is the farm book's; no book here
  book: async () => { throw Object.assign(new Error('no book'), { status: 404 }); }, pause: async () => {},
  local: () => ({ getItem: () => null, setItem: () => {} }),
});
const out = {};
await enrol.start({ user: i.person });
if (went) {
  // the provider sends the page back with a code, and the page loads again
  const u = new URL(went);
  Object.assign(out, { nonce: u.searchParams.get('nonce'), prompt: u.searchParams.get('prompt'), clientID: u.searchParams.get('client_id') });
  enrol.reset();
  if (u.searchParams.get('response_mode') !== 'fragment') throw new Error('the code must come back in the fragment');
  loc.hash = '#code=from-the-provider&state=' + u.searchParams.get('state');
  await enrol.start({ user: i.person });
}
Object.assign(out, { enrolled: enrol.status.state, device: enrol.status.device, expires: enrol.status.expires, message: enrol.status.message });
if (i.job) {
  act.learnJobs([i.job]);
  out.header = (await act.actHeaders(i.path, i.body))[act.ACT_HEADER] || '';
}
if (i.code) {
  await new Promise((ok) => setTimeout(ok, 1100)); // a renewal's leaf ends a second later
  await enrol.renew();
  Object.assign(out, { renewed: enrol.status.state, renewedExpires: enrol.status.expires });
  out.listed = (await enrol.devices(i.person)).devices.map((d) => d.device);
  await enrol.remove({ device: out.device });
  out.removed = enrol.status.state;
}
enrol.reset();
process.stdout.write(JSON.stringify(out));
`
