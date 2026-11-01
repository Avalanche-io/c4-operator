package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/hud"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people/peopletest"
	"github.com/Avalanche-io/c4-operator/internal/provenance/provtest"
	"github.com/Avalanche-io/c4-operator/schema"
)

// On a one-person farm the browser's passkey is approved at the farm's
// machine (PEOPLE-KEYS-2 7, K4 item 5: c4 farm device approve): the HUD the
// manager embeds makes the passkey, shows the command with its device ID,
// watches the people's index, and once the owner's CLI books a confirmed
// approve-machine for it, the passkey signs its enrolment citing that
// approval, and the real issuer certifies it as noor's touch key for this
// browser. Node runs the browser's code (enrol.js, touch.js) with a
// stand-in authenticator; this test is the CLI, signing with noor's owner
// key and touch key from c4 farm up.
func TestEmbeddedHUDApprovesPasskey(t *testing.T) {
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
	os.WriteFile(filepath.Join(dir, "approve.mjs"), []byte(approveScript), 0o644)

	o := peopletest.OnePerson(t)
	o.Issuer.WebAuthn = &people.WebAuthnRP{ID: "localhost", Origin: "http://localhost:7070"}
	code := o.NewCode(t)

	in, _ := json.Marshal(map[string]any{"origin": o.Server.URL, "person": "noor", "code": code})
	cmd := exec.Command(node, filepath.Join(dir, "approve.mjs"))
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	lines := bufio.NewScanner(stdout)
	next := func() string {
		t.Helper()
		if !lines.Scan() {
			cmd.Wait()
			t.Fatalf("the browser's code stopped: %s", stderr.String())
		}
		return lines.Text()
	}

	// the page shows the command and the short form to compare
	var shown struct{ Command, Short, Device, State string }
	json.Unmarshal([]byte(next()), &shown)
	if shown.State != "approve" || shown.Command != "c4 farm device approve "+shown.Device || shown.Short != shown.Device[2:6]+" "+shown.Device[6:10] {
		t.Fatalf("the page shows %+v", shown)
	}

	// the CLI approves it: a confirmed approve-machine with noor's owner
	// and touch keys, booked at the issuer
	k := o.Owner.Key(t)
	k.Touch = o.Touch.Key(t)
	act := o.Scratch.SignAct(t, k, provtestAct(shown.Device))
	r, err := http.Post(o.URL+"/v1/people/acts", "application/json", bytes.NewReader(act))
	if err != nil {
		t.Fatal(err)
	}
	var booked map[string]any
	json.NewDecoder(r.Body).Decode(&booked)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("the CLI's approval was refused: %d %v", r.StatusCode, booked)
	}

	var out struct {
		State, Approval string
		Ready           bool
	}
	json.Unmarshal([]byte(next()), &out)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the browser's code: %v\n%s", err, stderr.String())
	}
	if !out.Ready || out.Approval == "" {
		t.Fatalf("the passkey was not certified on the approval: %+v", out)
	}
	r, _ = http.Get(o.URL + "/v1/people/devices?person=noor")
	var l struct {
		Devices []struct{ Device, TouchFor, Evidence string }
	}
	json.NewDecoder(r.Body).Decode(&l)
	r.Body.Close()
	found := false
	for _, d := range l.Devices {
		found = found || d.Device == shown.Device && d.TouchFor != "" && d.Evidence == schema.EvidenceApproval
	}
	if !found {
		t.Errorf("noor's devices lack the passkey as a touch key enrolled on the approval: %+v", l.Devices)
	}
}

// provtestAct is the act c4 farm device approve signs: approve-machine on
// the new key, by noor, confirmed with her touch key
func provtestAct(device string) provtest.Act {
	return provtest.Act{Subject: provenance.Subject{Name: "device:" + device, Digest: map[string]string{"c4": device}},
		Act: "approve-machine", Params: map[string]string{}, SignedAt: time.Now(), Confirmed: true,
		Chain: &schema.ActChain{By: schema.Actor{Person: "noor", Grade: schema.GradeConfirmed}, For: schema.For{Party: "noor", Grade: schema.GradeConfirmed}}}
}

// approveScript runs the embedded enrol.js and touch.js as a one-person
// farm's HUD page does, opened by c4 farm open with its one-time code. It
// writes one line when the page shows the approval command, then one when
// the passkey is certified.
const approveScript = `
import { readFileSync } from 'node:fs';
const i = JSON.parse(readFileSync(0, 'utf8'));
const ORIGIN = 'http://localhost:7070';
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
// a platform authenticator bound to this machine: present, verified, not backed up
const pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
const spki = new Uint8Array(await crypto.subtle.exportKey('spki', pair.publicKey));
const credId = crypto.getRandomValues(new Uint8Array(16));
const authenticator = {
  create: async (o) => {
    const ad = cat(await sha256(enc.encode(o.publicKey.rp.id)), new Uint8Array([0x45, 0, 0, 0, 0]));
    return { rawId: credId.buffer, response: { getPublicKey: () => spki.buffer, getAuthenticatorData: () => ad.buffer } };
  },
  get: async (o) => {
    const cd = enc.encode(JSON.stringify({ type: 'webauthn.get', challenge: b64url(new Uint8Array(o.publicKey.challenge)), origin: ORIGIN, crossOrigin: false }));
    const ad = cat(await sha256(enc.encode(o.publicKey.rpId)), new Uint8Array([0x05, 0, 0, 0, 0]));
    const sig = new Uint8Array(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, pair.privateKey, cat(ad, await sha256(cd))));
    return { response: { authenticatorData: ad.buffer, clientDataJSON: cd.buffer, signature: der(sig).buffer } };
  },
};
const keys = new Map();
const loc = { origin: ORIGIN, pathname: '/', search: '', hash: '#/me?browser=' + i.code };
enrol.stubs({
  issuer: (path, body) => fetch(i.origin + '/issuer/v1/people' + path, body === undefined ? {} : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body }),
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
  go: () => { throw new Error('a one-person farm has no sign-in to go to'); },
  session: () => ({ getItem: () => null, setItem: () => {}, removeItem: () => {} }),
  local: () => ({ getItem: () => null, setItem: () => {} }),
  now: () => Date.now(), hidden: () => true, dirty: () => false, changed: () => {},
});
touch.stubs({ create: authenticator.create, get: authenticator.get, origin: () => ORIGIN,
  save: async (farm, pk) => { const r = keys.get(farm); r.passkey = pk; return r; },
  wait: () => new Promise((ok) => setTimeout(ok, 50)) });
await enrol.start({ user: i.person });
if (enrol.status.state !== 'signing') throw new Error('the browser did not enrol on its code: ' + enrol.status.state + ' ' + enrol.status.message);
await touch.begin();
const pk = touch.passkey();
process.stdout.write(JSON.stringify({ state: enrol.status.touch, device: pk.device, command: touch.approveCommand(pk), short: touch.shortID(pk.device) }) + '\n');
await touch.awaitApproval();
await touch.finish();
process.stdout.write(JSON.stringify({ state: enrol.status.touch, approval: touch.passkey().approval, ready: touch.ready() }) + '\n');
enrol.reset();
`
