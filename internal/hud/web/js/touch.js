// The touch (PEOPLE-KEYS-2 4.2 and 4.3, slice K9): an act on the touch
// list (one that gives authority away, lets work leave the farm, or
// changes who is accountable) is confirmed as well as signed. In the
// browser the touch key is a passkey: it signs only with the person
// present and verified (a fingerprint, a face, a PIN), and only for this
// HUD's origin.
//
// One passkey per browser profile, for the device key it confirms for
// (touchFor), made the first time the person asks for one on their page:
//   1 make it: navigator.credentials.create, user verification required,
//     labelled by device, never over another passkey of theirs
//     (excludeCredentials)
//   2 the step-up: a full sign-in with multi-factor at the farm's provider,
//     its nonce the passkey's device ID (enrol.js stepUpFor), once per
//     device and never again
//   3 back on the page, at the person's press (a browser wants a gesture
//     for an assertion), the passkey signs its own enrolment request, and
//     the issuer certifies it (POST /issuer/v1/people/enroll, webauthn in
//     place of proof)
// Then each act that needs a touch carries a second signature: the
// passkey's assertion over the act's challenge, base64url of the SHA-256 of
// its DSSE pre-authentication encoding, with the passkey's chain and what
// the authenticator signed (touchFor -> act.js signAct's touch).
//
// A synced passkey (the authenticator says it may be backed up) travels
// with the person's account to their other devices, so the HUD states it as
// a software key; one bound to this device's authenticator, as hardware.
//
// On a one-person farm there is no provider to step up at: the browser's
// passkey is approved instead by a touch on the touch key c4 farm up made,
// at the farm's machine (K4: c4 farm device approve <device ID>). The page
// shows that command with the passkey's device ID, and its short form to
// compare with what the machine shows: the comparison is the check, so the
// page never adds the device's kind or label, which are only what a
// machine says of itself. The page then watches the people's index for
// the approval, and the passkey signs its enrolment citing it.

import { b64, b64url, unb64, canonical, c4id, hex, savePasskey, useToucher } from './act.js';
import * as enrol from './enrol.js';

const enc = new TextEncoder();
const sha256 = async (bytes) => new Uint8Array(await globalThis.crypto.subtle.digest('SHA-256', bytes));
// the authenticator data's flags (WebAuthn 6.1): may be backed up
const BACKUP_ELIGIBLE = 0x08;

const real = {
  create: (o) => navigator.credentials.create(o),
  get: (o) => navigator.credentials.get(o),
  save: savePasskey,
  origin: () => location.origin,
  wait: () => new Promise((ok) => setTimeout(ok, 3000)),
};
let deps = real;
export function stubs(d) { deps = d ? { ...real, ...d } : real; }

// passkey: this browser's passkey record, or null
export const passkey = () => { const r = enrol.current().rec; return (r && r.passkey) || null; };
// ready: it is certified, so acts can be confirmed with it
export const ready = () => { const p = passkey(); return !!(p && p.certified && Date.parse(p.certified.expires) > Date.now()); };

// where: whether this browser can make a touch key here, and if not why
// via: approval on a one-person farm (the farm's machine approves it),
// else a step-up sign-in
export function where() {
  const { cfg } = enrol.current();
  if (!cfg) return { can: false };
  if (!cfg.webauthn) return { can: false, why: 'no-origin' };
  if (cfg.webauthn.origin !== deps.origin()) return { can: false, why: 'other-origin', origin: cfg.webauthn.origin };
  return { can: true, via: cfg.provider ? 'step-up' : 'approval' };
}

// The command that approves a passkey on a one-person farm, at the farm's
// machine, and the short form of a device ID both screens show, as the CLI
// prints it: the ID's first eight characters after c4, in two groups of
// four (K4 item 5)
export const APPROVE = 'c4 farm device approve';
export const shortID = (id) => `${id.slice(2, 6)} ${id.slice(6, 10)}`;
export const approveCommand = (pk) => `${APPROVE} ${pk.device}`;

// begin: make this browser's passkey and send the person to the step-up
export async function begin({ others = [] } = {}) {
  const { cfg, rec, person } = enrol.current();
  const w = where();
  if (!w.can) throw new Error('This browser can’t make a touch key on this farm.');
  if (!rec || !rec.certified) throw new Error('This browser’s key is not certified yet: a touch key confirms for it.');
  const label = enrol.browserLabel();
  const cred = await deps.create({ publicKey: {
    rp: { id: cfg.webauthn.id, name: 'C4' },
    user: { id: globalThis.crypto.getRandomValues(new Uint8Array(16)), name: person, displayName: `${person} · ${label}` },
    challenge: globalThis.crypto.getRandomValues(new Uint8Array(32)),
    pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
    authenticatorSelection: { userVerification: 'required', residentKey: 'preferred' },
    excludeCredentials: others.map((id) => ({ type: 'public-key', id: unb64(id) })),
  } });
  const spki = new Uint8Array(cred.response.getPublicKey());
  const ad = new Uint8Array(cred.response.getAuthenticatorData());
  const pk = { credentialId: b64url(new Uint8Array(cred.rawId)), spki: b64url(spki), device: await c4id(spki), label,
    backedUp: !!(ad[32] & BACKUP_ELIGIBLE) };
  if (w.via === 'approval') {
    // the index is read from here on for the approval naming it
    enrol.refreshed(await deps.save(cfg.farm, { ...pk, since: cfg.position || 0 }));
    awaitApproval();
    return null;
  }
  enrol.refreshed(await deps.save(cfg.farm, pk));
  return enrol.stepUpFor(pk.device);
}

// awaitApproval: watch the people's index for the approval naming this
// browser's passkey; found, the passkey waits for the person's press
// (finish). One watch at a time, while the page is open.
let watching = null;
export function awaitApproval() {
  const pk = passkey();
  if (!pk || pk.certified) return null;
  if (pk.approval) { enrol.status.touch = 'finish'; return null; }
  enrol.status.touch = 'approve';
  if (watching) return watching;
  watching = (async () => {
    try {
      for (;;) {
        const { cfg } = enrol.current();
        const p = passkey();
        if (!cfg || !p || p.certified) return;
        const x = await enrol.read(`/index?from=${p.since || 0}`).catch(() => null);
        const a = x && (x.entries || []).map((e) => e.entry).find((e) => e && e.kind === 'approval' && e.device === p.device);
        if (a) {
          enrol.refreshed(await deps.save(cfg.farm, { ...p, approval: a.record }));
          enrol.status.touch = 'finish';
          return;
        }
        await deps.wait();
      }
    } finally {
      watching = null;
    }
  })();
  return watching;
}

// others: the credential IDs of the person's other passkeys, from their
// device records, so a new passkey is never made over one of them
// (excludeCredentials); a record that can't be read is left out
export async function others(devices) {
  const mine = passkey();
  const ids = [];
  for (const d of devices) {
    if (!d.touchFor || (mine && d.device === mine.device)) continue;
    try {
      const b = await enrol.read('/records/' + encodeURIComponent(d.record));
      const st = JSON.parse(new TextDecoder().decode(unb64(b.dsseEnvelope.payload)));
      const w = st.predicate && st.predicate.webauthn;
      if (w && w.credentialId) ids.push(w.credentialId);
    } catch (e) { /* not this one */ }
  }
  return ids;
}

// assertion: the passkey's signature for a challenge (bytes): what the
// authenticator signed and its DER signature
async function assertion(pk, challenge) {
  const { cfg } = enrol.current();
  const a = await deps.get({ publicKey: {
    challenge,
    rpId: cfg.webauthn.id,
    allowCredentials: [{ type: 'public-key', id: unb64(pk.credentialId) }],
    userVerification: 'required',
  } });
  return { authenticatorData: new Uint8Array(a.response.authenticatorData), clientDataJSON: new Uint8Array(a.response.clientDataJSON),
    signature: new Uint8Array(a.response.signature) };
}

// finish: after the step-up, or the approval at the farm's machine, the
// passkey signs its enrolment, at the person's press, and the issuer
// certifies it as their touch key
export async function finish() {
  const { cfg, rec, person, stepUp } = enrol.current();
  const pk = rec && rec.passkey;
  if (!pk || !(stepUp || pk.approval)) throw new Error(cfg && !cfg.provider ? 'Approve it on the farm’s machine first.' : 'Sign in again first: a touch key is certified on a fresh sign-in.');
  const req = { ...enrol.request(cfg.farm, person, unb64(pk.spki)), touchFor: rec.device };
  const a = await assertion(pk, await sha256(enc.encode(canonical(req))));
  const c = await enrol.post('/enroll', {
    request: req,
    webauthn: { rpId: cfg.webauthn.id, origin: deps.origin(), credentialId: pk.credentialId,
      authenticatorData: b64url(a.authenticatorData), clientDataJSON: b64url(a.clientDataJSON), signature: b64url(a.signature) },
    protection: pk.backedUp ? 'software-browser' : 'hardware',
    kind: 'browser',
    label: `${pk.label} passkey`,
    ...(stepUp ? { idToken: stepUp.idToken } : { approval: pk.approval }),
  });
  enrol.refreshed(await deps.save(cfg.farm, { ...pk, certified: c }));
  enrol.status.touch = null;
  return c;
}

// touch: signAct's touch for one act, or null when this browser has no
// certified passkey: the passkey's assertion over the act's challenge
export function touch() {
  if (!ready()) return null;
  const pk = passkey();
  const chain = pk.certified.chain.map(unb64);
  return async (signed) => {
    const a = await assertion(pk, await sha256(signed));
    return { keyid: hex(await sha256(chain[0])), sig: b64(a.signature), chain, webauthn: a };
  };
}

// every act that needs a touch asks this browser's passkey for it
useToucher(touch);
