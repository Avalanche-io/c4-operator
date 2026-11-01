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
// (c4-operator internal/provenance): its subject the job by UID and spec
// digest, its act the control's action, its params the request's body
// exactly as sent (the farm compares them in canonical form, RFC 8785).
// The signature is raw r||s, 64 bytes, over the SHA-256 of the DSSE
// pre-authentication encoding, with S folded into the lower half of the
// group, the one form the farm accepts.

export const ACT_HEADER = 'X-C4-Act';
export const STATEMENT_TYPE = 'https://in-toto.io/Statement/v1';
export const ACT_TYPE = 'https://cccc.io/provenance/act/v1';
export const PAYLOAD_TYPE = 'application/vnd.in-toto+json';
export const BUNDLE_TYPE = 'application/vnd.cccc.attestation.v1+json';
export const IDENTITY_BASE = 'https://cccc.io/id/';

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

// ------------------------------------------------------------ bytes

const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

export function b64(bytes) {
  let s = '';
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}
const b64url = (bytes) => b64(bytes).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
const hex = (bytes) => [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('');

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
// is by them and for them (the chain's by and for), signed; the leaf's
// identity is its signer.
export function statement({ farm, subject, act, params, user, identity, device, as, signedAt, nonce }) {
  const predicate = { farm, act, params };
  if (as) predicate.as = as;
  if (device) predicate.device = device;
  Object.assign(predicate, {
    nonce,
    // the leaf's identity exactly, as the issuer certified it
    signer: identity || IDENTITY_BASE + 'user/' + encodeURIComponent(user),
    chain: { by: { person: user, grade: 'signed' }, for: { party: user, grade: 'signed' } },
    signedAt,
  });
  return { _type: STATEMENT_TYPE, subject: [subject], predicateType: ACT_TYPE, predicate };
}

// signAct: an act's bundle, the base64 of which X-C4-Act carries. signer
// is { key, chain, farm, user, identity, device }: the device's private
// CryptoKey, its certificate chain (DER, leaf first), the farm's ID, the
// person, the identity its leaf names and the device ID.
export async function signAct(signer, { subject, act, params, as }, now = new Date()) {
  const nonce = globalThis.crypto.getRandomValues(new Uint8Array(16));
  const st = statement({ farm: signer.farm, subject, act, params: params === undefined ? {} : params, user: signer.user, identity: signer.identity,
    device: signer.device, as, signedAt: now.toISOString(), nonce: b64url(nonce) });
  const payload = enc.encode(JSON.stringify(st));
  const raw = new Uint8Array(await subtle().sign({ name: 'ECDSA', hash: 'SHA-256' }, signer.key, pae(PAYLOAD_TYPE, payload)));
  const keyid = hex(new Uint8Array(await subtle().digest('SHA-256', signer.chain[0])));
  const bundle = {
    mediaType: BUNDLE_TYPE,
    dsseEnvelope: { payloadType: PAYLOAD_TYPE, payload: b64(payload), signatures: [{ keyid, sig: b64(lowS(raw)) }] },
    verificationMaterial: { x509CertificateChain: { certificates: signer.chain.map((c) => ({ rawBytes: b64(c) })) } },
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

// The signer this page signs with once the issuer has certified its key
// (K2), and the jobs it can name by UID and spec digest.
let signer = null;
const jobs = new Map();

// useSigner: sign every act from now on as signer, or stop (null)
export function useSigner(s) { signer = s; }
export const signing = () => !!signer;

// learnJobs: the UID and spec digest of each job in a snapshot
export function learnJobs(list) {
  jobs.clear();
  for (const j of list || []) if (j.uid && j.specDigest) jobs.set(`${j.namespace}/${j.name}`, j);
}

// actHeaders: the headers a write carries for its act, {} when this page
// cannot sign it (no certified key yet, a write that takes no act, or a
// job whose uid and specDigest it has not been told)
export async function actHeaders(path, body) {
  if (!signer) return {};
  const a = actOf(path, body);
  const j = a && jobs.get(`${a.ns}/${a.name}`);
  if (!j) return {};
  const bundle = await signAct(signer, { subject: subjectOf(a.ns, a.name, j.uid, j.specDigest), act: a.act, params: body === undefined ? {} : body });
  return { [ACT_HEADER]: b64(bundle) };
}
