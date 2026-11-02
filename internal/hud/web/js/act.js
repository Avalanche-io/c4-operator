// Signed acts (PEOPLE-KEYS-2 2 and 3, slice K3): every act a person takes
// in the HUD is signed by a key of this browser's own, and the farm's
// control record cites it, so the record names a device, not the HUD's
// word for who clicked.
//
// The key is ECDSA P-256, made non-extractable the first time the HUD
// loads and kept in the HUD's own IndexedDB, one per farm. The farm's
// people's issuer (K2) certifies it; until it has, or for a job whose
// summary does not yet name its uid and specDigest, a write goes unsigned
// and the farm takes it as before (K8 ends that).
//
// An act is an in-toto statement (act/v1) in a DSSE envelope, in the
// farm's attestation bundle with the device's certificate chain
// (c4-operator internal/provenance): its subject what it acts on, its act
// the endpoint's action, its params the request's body exactly as sent
// (the farm compares them in canonical form, RFC 8785; {} for none). A job
// control is on the job by UID and spec digest; every other person's act
// (a grant and its revocation, a tracker login confirmed or revoked, a
// held write said to have happened or not, a burst approved) on the
// subject the farm serves for it (actOn), learnt as the HUD reads it.
// The signature is raw r||s, 64 bytes, over the SHA-256 of the DSSE
// pre-authentication encoding, with S folded into the lower half of the
// group, the one form the farm accepts.

export const ACT_HEADER = 'X-C4-Act';
export const STATEMENT_TYPE = 'https://in-toto.io/Statement/v1';
export const ACT_TYPE = 'https://cccc.io/provenance/act/v1';
export const PAYLOAD_TYPE = 'application/vnd.in-toto+json';
export const BUNDLE_TYPE = 'application/vnd.cccc.attestation.v1+json';

const enc = new TextEncoder();
const subtle = () => globalThis.crypto.subtle;

// ------------------------------------------------------------ the act a write is

// actOf: the act a job control is, by its path and body, in the farm's own
// action names (internal/hud/server, controlJob), or null for a write that
// takes none yet: { ns, name, act }
const CONTROLS = [
  [/^nudge$/, () => 'nudge'],
  [/^requeue$/, () => 'requeue'],
  [/^accept-validated$/, () => 'accept-validated'],
  [/^ack$/, (b) => (b && b.kind) || 'acknowledge'],
  [/^blacklist$/, (b) => (b && b.remove ? 'allow-node' : 'avoid-node')],
  [/^decide$/, () => 'decide'],
  [/^steer$/, () => 'steer'],
  [/^supply$/, () => 'supply'],
  [/^needs\/[^/]+\/withdraw$/, () => 'withdraw-needs'],
  [/^withdraw$/, () => 'withdraw'],
];
export function actOf(path, body) {
  const m = /^\/jobs\/([^/?]+)\/([^/?]+)\/([^?]+)$/.exec(path);
  if (!m) return null;
  for (const [re, act] of CONTROLS) {
    if (re.test(m[3])) return { ns: decodeURIComponent(m[1]), name: decodeURIComponent(m[2]), act: act(body) };
  }
  return null;
}

// subjectOf: what an act on a job names, the job by UID with the SHA-256
// of its spec as the farm served it (provenance.ActSubject)
export function subjectOf(ns, name, uid, specDigest) {
  return { name: `workjob:${ns}/${name}@${uid}`, digest: { sha256: specDigest } };
}

// writeOf: the act a write that isn't a job control is, by its method,
// path and body, and the key its subject is learnt under (learnRead,
// learnSnapshot), or null for a write that takes none: { act, key }
const WRITES = [
  ['POST', /^\/grants$/, (m, b) => ({ act: 'grant', key: `project:${b && (b.project || b.namespace)}` })],
  ['DELETE', /^\/grants\/([^/]+)\/([^/]+)$/, (m) => ({ act: 'revoke', key: `grant:${m[1]}/${m[2]}` })],
  ['POST', /^\/people$/, (m, b) => ({ act: 'people-confirm', key: `offer:${b && b.namespace}/${b && b.tracker}/${b && b.login}` })],
  ['DELETE', /^\/people\/([^/]+)\/([^/]+)$/, (m) => ({ act: 'people-revoke', key: `people:${m[1]}/${m[2]}` })],
  ['POST', /^\/held\/([^/]+)\/([^/]+)\/([^/]+)$/, (m, b) => ({ act: b && b.happened ? 'confirm-write' : 'deny-write', key: `held:${m[1]}/${m[2]}/${m[3]}` })],
  ['POST', /^\/capacity\/([^/]+)\/approve$/, (m) => ({ act: 'approve-capacity', key: `burst:${m[1]}` })],
];
export function writeOf(method, path, body) {
  for (const [verb, re, fn] of WRITES) {
    const m = verb === method && re.exec(path);
    if (m) return fn(m.map((x) => (x === undefined ? x : decodeURIComponent(x))), body);
  }
  return null;
}

// ------------------------------------------------------------ bytes

const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

export function b64(bytes) {
  let s = '';
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}
export const b64url = (bytes) => b64(bytes).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
export const unb64 = (s) => Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/')), (c) => c.charCodeAt(0));

// canonical: JSON in its canonical form (RFC 8785) for the values the
// HUD signs: objects' keys sorted, no whitespace, strings and numbers as
// JSON.stringify writes them, which is what RFC 8785 asks of them
export function canonical(v) {
  if (Array.isArray(v)) return '[' + v.map(canonical).join(',') + ']';
  if (v && typeof v === 'object') return '{' + Object.keys(v).sort().map((k) => JSON.stringify(k) + ':' + canonical(v[k])).join(',') + '}';
  return JSON.stringify(v);
}

// signRaw: the device key's signature over bytes in its one form, raw r||s
// with a low S
export async function signRaw(key, bytes) {
  return lowS(new Uint8Array(await subtle().sign({ name: 'ECDSA', hash: 'SHA-256' }, key, bytes)));
}
export const hex = (bytes) => [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('');

// c4id: the C4 ID of bytes (SMPTE ST 2114): "c4" and the SHA-512 in
// base58, padded with "1" to 90 characters
export async function c4id(bytes) {
  const h = new Uint8Array(await subtle().digest('SHA-512', bytes));
  let n = 0n;
  for (const b of h) n = (n << 8n) | BigInt(b);
  let s = '';
  while (n > 0n) { s = B58[Number(n % 58n)] + s; n /= 58n; }
  return 'c4' + s.padStart(88, '1');
}

// shortID: a device ID's short form, as the CLI prints it for a person to
// compare across two screens: twelve characters in three groups of four,
// skipping c4 and the character after it, which takes only a few values
// (K4 item 5)
export const shortID = (id) => `${id.slice(3, 7)} ${id.slice(7, 11)} ${id.slice(11, 15)}`;

// pae: DSSE's pre-authentication encoding of a payload, the bytes signed
export function pae(payloadType, payload) {
  const head = enc.encode(`DSSEv1 ${enc.encode(payloadType).length} ${payloadType} ${payload.length} `);
  const out = new Uint8Array(head.length + payload.length);
  out.set(head, 0);
  out.set(payload, head.length);
  return out;
}

// P-256's group order, and the largest low S
const N = 0xffffffff00000000ffffffffffffffffbce6faada7179e84f3b9cac2fc632551n;
const HALF = N >> 1n;

// lowS: a raw r||s signature with S folded into the lower half
export function lowS(sig) {
  let s = 0n;
  for (const b of sig.slice(32)) s = (s << 8n) | BigInt(b);
  if (s <= HALF) return sig;
  s = N - s;
  const out = new Uint8Array(sig);
  for (let i = 63; i >= 32; i--) { out[i] = Number(s & 0xffn); s >>= 8n; }
  return out;
}

// ------------------------------------------------------------ the act

// statement: an act/v1 statement as the signer states it. A person's act
// is by them and for them (the chain's by and for), signed, or confirmed
// when a touch signs it too (K9); the leaf's identity is its signer,
// copied from what the issuer certified, never computed here.
export function statement({ farm, subject, act, params, user, identity, device, as, signedAt, nonce, grade = 'signed' }) {
  if (!identity) throw new Error('an act names the identity its key was certified as: enrol the key first');
  const predicate = { farm, act, params };
  if (as) predicate.as = as;
  if (device) predicate.device = device;
  Object.assign(predicate, {
    nonce,
    // the leaf's identity exactly, as the issuer certified it
    signer: identity,
    chain: { by: { person: user, grade }, for: { party: user, grade } },
    signedAt,
  });
  return { _type: STATEMENT_TYPE, subject: [subject], predicateType: ACT_TYPE, predicate };
}

// signAct: an act's bundle, the base64 of which X-C4-Act carries. signer
// is { key, chain, farm, user, identity, device, as? }: the device's
// private CryptoKey, its certificate chain (DER, leaf first), the farm's
// ID, the person, the identity its leaf names, the device ID, and on a
// one-person farm the role the person acts in.
//
// touch confirms the act (K9, touch.js): given the bytes signed, it
// answers the touch key's signature { keyid, sig, chain, webauthn? }: its
// key ID, the signature (base64), the touch key's chain (DER, leaf first)
// and, for a passkey, the authenticator data and client data it signed
// (bytes). The act is then confirmed rather than signed.
export async function signAct(signer, { subject, act, params, as }, now = new Date(), { touch } = {}) {
  const nonce = globalThis.crypto.getRandomValues(new Uint8Array(16));
  const st = statement({ farm: signer.farm, subject, act, params: params === undefined ? {} : params, user: signer.user, identity: signer.identity,
    device: signer.device, as: as || signer.as, signedAt: now.toISOString(), nonce: b64url(nonce), grade: touch ? 'confirmed' : 'signed' });
  const payload = enc.encode(JSON.stringify(st));
  const signed = pae(PAYLOAD_TYPE, payload);
  const sig = await signRaw(signer.key, signed);
  const keyid = hex(new Uint8Array(await subtle().digest('SHA-256', signer.chain[0])));
  const signatures = [{ keyid, sig: b64(sig) }];
  const material = { x509CertificateChain: { certificates: signer.chain.map((c) => ({ rawBytes: b64(c) })) } };
  if (touch) {
    const t = await touch(signed);
    signatures.push({ keyid: t.keyid, sig: t.sig });
    if (t.chain) material.touchCertificateChain = { certificates: t.chain.map((c) => ({ rawBytes: b64(c) })) };
    if (t.webauthn) material.touchWebAuthn = { authenticatorData: b64(t.webauthn.authenticatorData), clientDataJSON: b64(t.webauthn.clientDataJSON) };
  }
  const bundle = {
    mediaType: BUNDLE_TYPE,
    dsseEnvelope: { payloadType: PAYLOAD_TYPE, payload: b64(payload), signatures },
    verificationMaterial: material,
  };
  return enc.encode(JSON.stringify(bundle));
}

// ------------------------------------------------------------ the signer

// The device key, one per farm, made the first time the HUD loads there and
// never readable by the page: it signs, and its public half is all that
// leaves the browser.
const DB = 'c4-hud-keys';
function idb() {
  return new Promise((ok, no) => {
    const r = indexedDB.open(DB, 1);
    r.onupgradeneeded = () => r.result.createObjectStore('keys');
    r.onsuccess = () => ok(r.result);
    r.onerror = () => no(r.error);
  });
}
async function idbDo(mode, fn) {
  const db = await idb();
  return new Promise((ok, no) => {
    const tx = db.transaction('keys', mode);
    const r = fn(tx.objectStore('keys'));
    tx.oncomplete = () => ok(r && r.result);
    tx.onerror = () => no(tx.error);
  });
}

// deviceKey: this browser's key for a farm, made if it has none:
// { key, spki, device } (device: the C4 ID of its SubjectPublicKeyInfo)
export async function deviceKey(farm) {
  let rec = await idbDo('readonly', (s) => s.get(farm)).catch(() => null);
  if (!rec) {
    const pair = await subtle().generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
    const spki = new Uint8Array(await subtle().exportKey('spki', pair.publicKey));
    rec = { key: pair.privateKey, spki, device: await c4id(spki) };
    await idbDo('readwrite', (s) => s.put(rec, farm));
  }
  return rec;
}

// saveCertified: keep what the issuer certified beside the key it is for
export async function saveCertified(farm, certified) {
  const rec = await idbDo('readonly', (s) => s.get(farm));
  if (!rec) throw new Error('no device key for this farm');
  rec.certified = certified;
  await idbDo('readwrite', (s) => s.put(rec, farm));
  return rec;
}

// savePasskey: keep this browser's passkey for a farm (touch.js) beside
// the device key it confirms for: its credential, its public key and
// device ID, and once the issuer has certified it, its chain
export async function savePasskey(farm, passkey) {
  const rec = await idbDo('readonly', (s) => s.get(farm));
  if (!rec) throw new Error('no device key for this farm');
  rec.passkey = passkey;
  await idbDo('readwrite', (s) => s.put(rec, farm));
  return rec;
}

// forgetKey: drop this browser's key for a farm, once removed or lapsed:
// the next load makes a new one
export async function forgetKey(farm) { await idbDo('readwrite', (s) => s.delete(farm)); }

// The signer this page signs with once the issuer has certified its key
// (K2), and the jobs it can name by UID and spec digest.
let signer = null;
const jobs = new Map();
// the subjects of every other act, by key (writeOf): { name, digest }
const subjects = new Map();

// The touch (K9, touch.js). The farm says which acts need one (GET
// /api/v1/acts/needs, shared piece S); the HUD keeps no list of its own,
// which would drift from the farm's and from a project's minGrade.
// toucher answers signAct's touch for this browser, or null while it has
// no certified passkey. An act that needs a touch this browser can't give
// goes with no act at all, as before personal keys: a signed act the farm
// would refuse for want of a touch must never break what works today.
const needs = new Set();
let toucher = () => null;
export function useToucher(fn) { toucher = fn || (() => null); }

// learnNeeds: the acts the farm says need a touch, as GET
// /api/v1/acts/needs serves them ({ acts: { <kind>: { person?, touch?,
// relayed? } } }), or in the simpler forms a farm may: a list of names, a
// list of { act, needs }, or an object of act to grade. A kind whose need
// turns on its params (the farm's params list) is left to the farm's 403.
export function learnNeeds(x) {
  needs.clear();
  const v = x && !Array.isArray(x) && typeof x === 'object' ? (x.acts || x.needs || x) : x;
  if (Array.isArray(v)) for (const e of v) {
    if (typeof e === 'string') needs.add(e);
    else if (e && e.act && (e.needs || e.minGrade || 'confirmed') === 'confirmed') needs.add(e.act);
  }
  else if (v && typeof v === 'object') for (const [k, g] of Object.entries(v)) if (g === 'confirmed' || (g && g.touch)) needs.add(k);
}
export const needsTouch = (act) => needs.has(act);

// useSigner: sign every act from now on as signer, or stop (null)
export function useSigner(s) { signer = s; }
export const signing = () => !!signer;

// learnJobs: the UID and spec digest of each job in a snapshot
export function learnJobs(list) {
  jobs.clear();
  for (const j of list || []) if (j.uid && j.specDigest) jobs.set(`${j.namespace}/${j.name}`, j);
}

const learn = (key, on) => { if (on && on.name && on.digest) subjects.set(key, on); };

// learnSnapshot: what a snapshot names for acts: its jobs, each project
// (what a grant is made in), each pool's burst waiting for approval, and
// the farm
export function learnSnapshot(snap) {
  learnJobs(snap && snap.jobs);
  if (!snap) return;
  for (const p of snap.projects || []) learn(`project:${p.name}`, p.actOn);
  for (const p of snap.pools || []) learn(`burst:${p.name}`, p.burst && p.burst.approve);
  learn('farm', snap.farm && snap.farm.actOn);
}

// learnRead: what a read names for acts: grants (revoked on themselves),
// tracker logins (confirmed on the offer, revoked on the confirmation) and
// held writes
export function learnRead(path, data) {
  if (!data) return;
  if (path.startsWith('/grants')) for (const g of data.grants || []) learn(`grant:${g.namespace}/${g.name}`, g.actOn);
  if (path.startsWith('/people')) for (const e of data.entries || []) {
    learn(`offer:${e.namespace}/${e.tracker}/${e.login}`, e.confirmOn);
    learn(`people:${e.namespace}/${e.name}`, e.revokeOn);
  }
  if (path.startsWith('/held')) for (const w of data.held || []) learn(`held:${w.namespace}/${w.link}/${w.field}`, w.actOn);
}

// actHeaders: the headers a write carries for its act, {} when this page
// cannot sign it (no certified key yet, a write that takes no act, or a
// job whose uid and specDigest it has not been told), or when the act
// needs a touch this browser can't give. confirm: the farm said this act
// needs a touch (a 403's needs: confirmed), whatever the list said.
export async function actHeaders(path, body, { confirm = false, method = 'POST' } = {}) {
  if (!signer) return {};
  let act, subject;
  const a = method === 'POST' && actOf(path, body);
  if (a) {
    const j = jobs.get(`${a.ns}/${a.name}`);
    if (!j) return {};
    act = a.act;
    subject = subjectOf(a.ns, a.name, j.uid, j.specDigest);
  } else {
    const w = writeOf(method, path, body);
    subject = w && subjects.get(w.key);
    if (!subject) return {};
    act = w.act;
  }
  let touch = null;
  if (confirm || needsTouch(act)) {
    touch = toucher();
    if (!touch) return {};
  }
  const bundle = await signAct(signer, { subject, act, params: body === undefined || body === null ? {} : body }, new Date(), { touch });
  return { [ACT_HEADER]: b64(bundle) };
}
