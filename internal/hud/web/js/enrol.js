// Enrolment (PEOPLE-KEYS-2 3, slice K3): this browser's key certified by
// the farm's people's issuer (K2), and kept certified. The HUD serves the
// issuer at its own origin under /issuer/v1/people/, so the key enrols
// without leaving the page's origin; the HUD adds nothing to what it is
// sent and is trusted for nothing.
//
// Once the page knows who is viewing, it:
//   - reads the issuer's config; a farm that runs none (404) leaves every
//     act unsigned, as before, and the page says nothing of it
//   - makes the device key the first time (act.js deviceKey)
//   - enrols it, on evidence the issuer can check without the HUD's word:
//       a one-person farm: the one-time code `c4 farm open` puts in the
//         page's link (#/<route>?browser=<code>), read once and taken out
//         of the address bar at once; it works for a minute, on loopback
//       a studio farm: an ID token from the farm's sign-in provider, whose
//         nonce is the device ID, fetched on a silent round trip
//         (prompt=none, an authorization code with PKCE, answered in the
//         fragment so the code never reaches a server or a log); a
//         provider that wants the person to sign in first leaves a Sign in
//         button on their page, never a loop of redirects
//   - signs every act with it (act.js useSigner), as the person: on a
//     one-person farm the farm's owner, with the role they view in (a
//     role's sign-in, artist or wrangler) as the act's `as`
//   - tells the person when a device they haven't seen signs in as them,
//     or one of theirs is seen from two places at once (notices)
//   - renews it before its leaf ends: on its own proof on a one-person
//     farm; on a studio farm by the same round trip, which leaves the
//     page, so only while the page is out of sight and no form on it holds
//     input the person hasn't sent
//   - forgets a key the issuer says was removed or has lapsed, and makes a
//     new one: a removed key never comes back

import { b64url, unb64, canonical, signRaw, deviceKey, saveCertified, forgetKey, useSigner, signAct, shortID } from './act.js';

export const ISSUER = '/issuer/v1/people';
// renew this long before the leaf ends
export const RENEW_BEFORE = 60 * 60 * 1000;
const enc = new TextEncoder();
const RETURN = 'c4-hud-sign-in';

// What the person's page shows of this browser's key:
//   state: off (the farm runs no issuer) | starting | signing | enrolling
//     | sign-in (the provider wants the person to sign in) | open (a
//     one-person farm, opened without a code) | refused (the issuer's words
//     in message)
//   touch: null, or the step-up's answer for this browser's passkey
//     (touch.js): finish (signed in again: the passkey signs its
//     enrolment at the person's press) | refused (touchMessage says why)
export const status = { state: 'starting', device: null, expires: null, person: null, message: null, notices: null, touch: null, touchMessage: null };

// deps: what the flow touches outside itself, so a test can stand in for
// each (the issuer, the provider, the key store, the address bar, the
// clock). Writes go through api.js, which marks the farm's; it is loaded
// when first used, so a test that stands in for it never loads it.
const api = () => import('./api.js');
const real = {
  issuer: (path, body) => api().then((m) => m.issuer(path, body)),
  token: (url, form) => api().then((m) => m.providerToken(url, form)),
  fetch: (...a) => globalThis.fetch(...a),
  deviceKey, saveCertified, forgetKey,
  location: () => globalThis.location,
  // the address bar as the person left it; the router reads a new route
  replaceURL: (u) => {
    const was = location.hash;
    history.replaceState(null, '', u);
    if (location.hash !== was) window.dispatchEvent(new HashChangeEvent('hashchange'));
  },
  go: (u) => location.assign(u),
  session: () => globalThis.sessionStorage,
  now: () => Date.now(),
  hidden: () => document.hidden,
  // a field whose value isn't what the page drew: input not sent yet
  dirty: () => [...document.querySelectorAll('input, textarea, select')].some((el) => (el.tagName === 'SELECT'
    ? [...el.options].some((o) => o.selected !== o.defaultSelected)
    : el.type === 'checkbox' || el.type === 'radio' ? el.checked !== el.defaultChecked : el.value !== el.defaultValue)),
  onVisibility: (fn) => document.addEventListener('visibilitychange', fn),
  offVisibility: (fn) => document.removeEventListener('visibilitychange', fn),
  local: () => globalThis.localStorage,
  // the farm book after a position (book.js reads it too), and a pause
  book: (since) => api().then((m) => m.get(`/book/farm?since=${since}`)),
  pause: () => new Promise((ok) => setTimeout(ok, 3000)),
  changed: () => document.dispatchEvent(new CustomEvent('hud:refresh')),
};
let deps = real;
export function stubs(d) { deps = d ? { ...real, ...d } : real; }

function set(s) {
  Object.assign(status, s);
  deps.changed();
}

// ------------------------------------------------------------ the request

// request: an enrolment request, the issuer's Request, for a key
export function request(farm, person, spki, now = new Date(deps.now())) {
  return { farm, person, key: b64url(spki), nonce: b64url(globalThis.crypto.getRandomValues(new Uint8Array(16))), requestedAt: now.toISOString().replace(/\.\d+Z$/, 'Z') };
}

// enrolment: the request with the key's proof over its canonical JSON
// (schema.DevicePredicate.ProofMessage), and what the device is
export async function enrolment(key, req, evidence) {
  const proof = b64url(await signRaw(key, enc.encode(canonical(req))));
  return { request: req, proof, protection: 'software-browser', kind: 'browser', label: browserLabel(), ...evidence };
}

// browserLabel: what the person's device list calls this browser
export function browserLabel(ua = globalThis.navigator ? navigator.userAgent : '') {
  const b = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : 'A browser';
  const os = /Mac OS X/.test(ua) ? 'macOS' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : '';
  return os ? `${b} on ${os}` : b;
}

// call: the issuer's answer to a read, or with a body a write
async function call(path, body) {
  const r = await deps.issuer(path, body === undefined || typeof body === 'string' || body instanceof Uint8Array ? body : JSON.stringify(body));
  const out = await r.json().catch(() => ({}));
  if (!r.ok) {
    const e = new Error(out.error || `${r.status}`);
    e.status = r.status;
    throw e;
  }
  return out;
}

// ------------------------------------------------------------ secrets in the address

// The secrets an address can carry, taken out of the address bar before
// anything else reads it (app.js calls capture first, before its router):
//   - the one-time browser code (#/<route>?browser=<code>)
//   - the sign-in provider's answer (#code=…&state=…, response_mode
//     fragment; in the query too, should a provider answer there)
// The provider's answer is taken out whether or not this page asked for
// it: a code from a sign-in someone else started never stays in the
// address bar or its history, and the HUD sends no Referer
// (Referrer-Policy), so it leaves with no link either.
const ANSWER = ['code', 'state', 'iss', 'error', 'error_description', 'session_state'];
const answerIn = (p) => (ANSWER.some((k) => p.has(k)) ? Object.fromEntries(ANSWER.filter((k) => p.has(k)).map((k) => [k, p.get(k)])) : null);
let caught = null;
export function capture() {
  if (caught) return caught;
  const loc = deps.location();
  let hash = loc.hash;
  let answer = null;
  if (!hash.startsWith('#/')) {
    answer = answerIn(new URLSearchParams(hash.replace(/^#/, '')));
    if (answer) hash = '';
  }
  const q = new URLSearchParams(loc.search);
  const inQuery = answerIn(q);
  if (inQuery) for (const k of ANSWER) q.delete(k);
  const b = codeIn(hash);
  // the address is rewritten only when a secret came out of it (the query
  // as it was otherwise: URLSearchParams would re-encode it)
  const search = inQuery ? (q.toString() ? '?' + q : '') : loc.search;
  if (answer || inQuery || b.code) deps.replaceURL(loc.pathname + search + b.hash);
  caught = { browser: b.code, answer: answer || inQuery };
  return caught;
}

// ------------------------------------------------------------ the round trip

// codeIn: the one-time code a link carries in its route's query
// (#/<route>?browser=<code>), and the hash without it
export function codeIn(hash) {
  const [path, query] = hash.split('?');
  const q = new URLSearchParams(query || '');
  const code = q.get('browser');
  if (!code) return { code: null, hash };
  q.delete('browser');
  const rest = q.toString();
  return { code, hash: rest ? `${path}?${rest}` : path };
}

// authorizeURL: the provider's authorization request for a device: its
// nonce the device ID, an authorization code with PKCE answered in the
// fragment, silent unless the person asked to sign in
export function authorizeURL(endpoint, { clientID, redirect, device, state, challenge, silent, extra }) {
  const q = new URLSearchParams({ response_type: 'code', response_mode: 'fragment', client_id: clientID, redirect_uri: redirect, scope: 'openid', nonce: device, state, code_challenge: challenge, code_challenge_method: 'S256' });
  if (silent) q.set('prompt', 'none');
  for (const [k, v] of Object.entries(extra || {})) q.set(k, v);
  return `${endpoint}${endpoint.includes('?') ? '&' : '?'}${q}`;
}

async function discover(issuer) {
  const r = await deps.fetch(issuer.replace(/\/$/, '') + '/.well-known/openid-configuration', { headers: { Accept: 'application/json' } });
  if (!r.ok) throw new Error(`the sign-in provider ${issuer} did not say where to sign in (${r.status})`);
  return r.json();
}

const redirectOf = (loc) => loc.origin + loc.pathname;

// stepUpParams: what a touch key's sign-in asks of the provider, from the
// issuer's step-up rule (config.stepUp): a fresh sign-in (max_age, in
// seconds) with multi-factor (acr_values)
export function stepUpParams(stepUp) {
  const out = {};
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/.exec((stepUp && stepUp.maxAge) || '');
  if (m && m[0]) out.max_age = String(Math.floor((+m[1] || 0) * 3600 + (+m[2] || 0) * 60 + (+m[3] || 0)));
  if (stepUp && (stepUp.acr || []).length) out.acr_values = stepUp.acr.join(' ');
  return out;
}

// signIn: send the person to the provider for a token naming a device;
// the page comes back with the route it left from. touch: the sign-in is
// the step-up for this browser's passkey (device is its device ID), never
// silent.
export async function signIn(cfg, device, { silent = true, renew = false, touch = false } = {}) {
  if (touch) silent = false;
  const loc = deps.location();
  const meta = await discover(cfg.provider.issuer);
  const verifier = b64url(globalThis.crypto.getRandomValues(new Uint8Array(32)));
  const challenge = b64url(new Uint8Array(await globalThis.crypto.subtle.digest('SHA-256', enc.encode(verifier))));
  const state = b64url(globalThis.crypto.getRandomValues(new Uint8Array(16)));
  deps.session().setItem(RETURN, JSON.stringify({ state, verifier, farm: cfg.farm, back: loc.hash, renew, silent, touch, token: meta.token_endpoint }));
  deps.go(authorizeURL(meta.authorization_endpoint, { clientID: cfg.provider.clientID, redirect: redirectOf(loc), device, state, challenge, silent,
    extra: touch ? stepUpParams(cfg.stepUp) : null }));
}

// returned: the provider's answer, when this load is the round trip's end
// and the round trip is this page's own (its state): { idToken } or
// { error }, or null; capture() has already taken it out of the address,
// and the route the person left from is put back
async function returned(cfg) {
  const a = capture().answer;
  if (!a || !a.state) return null;
  const saved = JSON.parse(deps.session().getItem(RETURN) || 'null');
  if (!saved || saved.state !== a.state || saved.farm !== cfg.farm) return null;
  deps.session().removeItem(RETURN);
  const loc = deps.location();
  const code = a.code, err = a.error, redirect = redirectOf(loc);
  deps.replaceURL(loc.pathname + loc.search + saved.back);
  if (err) return { error: err, silent: saved.silent, renew: saved.renew, touch: saved.touch };
  const r = await deps.token(saved.token, new URLSearchParams({ grant_type: 'authorization_code', code, redirect_uri: redirect, client_id: cfg.provider.clientID, code_verifier: saved.verifier }).toString());
  const t = await r.json().catch(() => ({}));
  if (!r.ok || !t.id_token) return { error: t.error || `the sign-in provider answered ${r.status}`, renew: saved.renew, touch: saved.touch };
  return { idToken: t.id_token, renew: saved.renew, touch: saved.touch };
}

// the provider's answers to a silent request that mean "ask the person"
const ASK = new Set(['login_required', 'interaction_required', 'consent_required', 'account_selection_required']);

// ------------------------------------------------------------ the flow

let cfg = null, rec = null, person = null, as = null, timer = null, lapse = null, stepUp = null;

// current: what touch.js builds on: the issuer's config, this browser's
// key record, the person, and the step-up's ID token while it is fresh
export const current = () => ({ cfg, rec, person, stepUp });

// stepUpFor: send the person to the provider's step-up for a passkey
export const stepUpFor = (device) => signIn(cfg, device, { touch: true });

// post: a write to the issuer, its answer or its refusal (touch.js); read:
// a read of it
export const post = (path, body) => call(path, body);
export const read = (path) => call(path);

// refreshed: the key record changed under us (touch.js saved a passkey)
export function refreshed(r) { rec = r; deps.changed(); }

// use: sign with a certified key from now on, and renew it in time; a key
// just certified waits a minute at least, so a browser clock hours off the
// issuer's never renews back to back
function use(r, fresh) {
  const c = r.certified;
  useSigner({ key: r.key, chain: c.chain.map(unb64), farm: c.farm, user: c.person, identity: c.identity, device: c.device, as: as || undefined });
  // provisional: the issuer booked it, and the farm's book hasn't sealed
  // it yet: until then nothing outside the farm counts its acts
  set({ state: 'signing', device: c.device, expires: c.expires, person: c.person, message: null, provisional: !!c.provisional, seal: c.provisional ? 'pending' : null });
  if (c.provisional) watchSeal(c);
  notices();
  clearTimeout(timer);
  clearTimeout(lapse);
  const left = Date.parse(c.expires) - deps.now();
  timer = setTimeout(renewNow, Math.max(fresh ? 60 * 1000 : 0, left - RENEW_BEFORE));
}

// certified: keep what the issuer answered beside the key, and sign with it
async function certified(c, renews) {
  // a renewal of a key a code enrolled is still that key: it keeps viaCode
  const viaCode = c.viaCode || !!(rec.certified && rec.certified.viaCode && rec.certified.device === c.device);
  rec = await deps.saveCertified(cfg.farm, { ...c, person, renews, viaCode });
  use(rec, true);
}

// SEAL_FOR: how long the page watches for the seal, about twice the
// longest a farm takes to seal; SEAL_BACK: how far before the position
// the issuer gave it reads, since a refusal ahead of it moves it earlier
const SEAL_FOR = 60 * 1000;
// refusedIn: the entries a farm book segment refused at the seal (its
// book/v1 predicate, read, not verified)
const refusedIn = (seg) => {
  try { return JSON.parse(atob(seg.dsseEnvelope.payload)).predicate.refused || []; } catch (e) { return []; }
};
const SEAL_BACK = 64;

// watchSeal: the enrolment's entry in the farm book, matched by its
// record (a provisional position can still move): sealed (seal null), or
// refused at the seal (seal refused), or not seen sealed within a minute
// (seal unknown), so the page never spins
let sealing = null;
function watchSeal(c) {
  if (sealing) return sealing;
  const since = Math.max(0, (c.position || 0) - SEAL_BACK);
  const until = deps.now() + SEAL_FOR;
  sealing = (async () => {
    let id = null;
    try {
      while (deps.now() < until) {
        const page = await deps.book(since).catch((e) => (e.status === 404 ? 'none' : null));
        if (page === 'none') return set({ provisional: false, seal: null });
        if (page) {
          const sealed = (page.entries || []).find((pe) => pe.entry && pe.entry.record === c.record);
          if (sealed) return set({ provisional: false, seal: null });
          const pending = (page.pending || []).find((pe) => pe.entry && pe.entry.record === c.record);
          if (pending) id = pending.id;
          const refused = id && (page.segments || []).some((seg) => refusedIn(seg).some((r) => r.submission === id));
          if (refused) return set({ seal: 'refused' });
        }
        await deps.pause();
      }
      set({ seal: 'unknown' });
    } finally {
      sealing = null;
    }
  })();
  return sealing;
}

// again: forget a key whose enrolment the book refused, and enrol anew
export const again = () => gone(new Error('The farm’s book refused this browser’s enrolment: enrolling a new key.'));

// gone: the issuer will not renew this key (removed, lapsed or unknown):
// forget it and enrol a new one
async function gone(e) {
  useSigner(null);
  await deps.forgetKey(cfg.farm);
  rec = await deps.deviceKey(cfg.farm);
  set({ state: 'enrolling', device: rec.device, expires: null, message: e.message });
  return enrol();
}

function refused(e) {
  set({ state: 'refused', message: e.message });
}

// renewNow: renew before the leaf ends; on a studio farm the round trip
// leaves the page, so it waits for the page to be out of sight
async function renewNow() {
  if (!rec || !rec.certified) return;
  // a farm with a sign-in provider renews every device on a fresh ID
  // token, however it enrolled; only a one-person farm's on its own proof
  if (!cfg.provider) {
    try {
      await certified(await call('/renew', await enrolment(rec.key, request(cfg.farm, person, rec.spki), {})), 'proof');
    } catch (e) {
      if (e.status === 403 || e.status === 404) return gone(e);
      refused(e);
    }
    return;
  }
  // the round trip leaves the page: only while it is out of sight, and
  // never while a form on it holds input the person hasn't sent
  const may = () => deps.hidden() && !deps.dirty();
  if (may()) return signIn(cfg, rec.device, { renew: true });
  deps.onVisibility(function away() {
    if (!may()) return;
    deps.offVisibility(away);
    signIn(cfg, rec.device, { renew: true });
  });
  // past the leaf's end the page signs nothing until it renews
  clearTimeout(lapse);
  lapse = setTimeout(() => { useSigner(null); set({ state: 'enrolling', message: 'This browser’s key renews when you next leave or reload the page.' }); },
    Math.max(0, Date.parse(rec.certified.expires) - deps.now()));
}

// enrol: certify the key on whatever evidence this load has
async function enrol(code) {
  try {
    // a code from c4 farm open: the farm's machine countersigns this key
    // once the person sees the same short ID here as there (viaCode)
    if (code) return await certified({ ...(await call('/browser', await enrolment(rec.key, request(cfg.farm, person, rec.spki), { code }))), viaCode: true }, 'proof');
    if (!cfg.provider) return set({ state: 'open', device: rec.device });
    // try the silent round trip once a session: a provider that wants the
    // person to sign in leaves the Sign in button, not a loop
    if (deps.session().getItem(RETURN + ':tried:' + cfg.farm)) return set({ state: 'sign-in', device: rec.device });
    deps.session().setItem(RETURN + ':tried:' + cfg.farm, '1');
    set({ state: 'enrolling', device: rec.device });
    return await signIn(cfg, rec.device);
  } catch (e) {
    if (e.status === 409) return gone(e);
    refused(e);
  }
}

// ------------------------------------------------------------ notices

// A device the person hasn't seen yet, signed in as them, and any device
// of theirs seen from two places at once (PEOPLE-KEYS-2 7). This browser
// remembers the devices it has shown them, per farm and person; the first
// time it reads the list it takes it as seen, so a new browser doesn't
// announce the devices that were there before it.
const SEEN = 'c4-hud-seen-devices';
const seenKey = () => `${SEEN}:${cfg.farm}:${person}`;

async function notices() {
  try {
    const list = (await devices(person)).devices || [];
    const raw = deps.local().getItem(seenKey());
    const seen = new Set(raw ? JSON.parse(raw) : list.map((d) => d.device));
    if (!raw) deps.local().setItem(seenKey(), JSON.stringify([...seen]));
    const fresh = list.filter((d) => !seen.has(d.device) && d.device !== status.device);
    const twice = list.filter((d) => (d.flags || []).length);
    // a key a c4 farm open code enrolled, not yet countersigned: the page
    // shows its short ID, computed here from its own key, to compare
    const mine = list.find((d) => d.device === status.device);
    const check = rec && rec.certified && rec.certified.viaCode && mine && !mine.countersigned;
    set({ notices: fresh.length || twice.length ? { fresh, twice } : null, countersign: check ? shortID(rec.device) : null });
  } catch (e) {
    // the list is a courtesy here; the person's page reads it again
  }
}

// seen: the person has seen their devices (their page shows them): no
// device in the list is new to them any more
export function seen(list) {
  if (!cfg || !person) return;
  deps.local().setItem(seenKey(), JSON.stringify(list.map((d) => d.device)));
  if (status.notices) set({ notices: status.notices.twice.length ? { fresh: [], twice: status.notices.twice } : null });
}

// start: enrol or renew this browser's key for the person viewing (the
// snapshot's viewer), once per page
let started = null;
export function start(viewer) {
  if (!viewer || !viewer.user || started) return started;
  started = run(viewer).catch((e) => refused(e));
  return started;
}

async function run(viewer) {
  person = viewer.user;
  if (!globalThis.indexedDB || !globalThis.crypto || !globalThis.crypto.subtle) return set({ state: 'off' });
  try {
    cfg = await call('/config');
  } catch (e) {
    // a farm that runs no issuer: acts go unsigned, as before
    if (e.status === 404) return set({ state: 'off' });
    throw e;
  }
  // a one-person farm's acts are its owner's, whichever role's sign-in
  // views the HUD; the farm takes them as the owner's in that role
  if (cfg.owner && cfg.owner !== viewer.user) {
    person = cfg.owner;
    as = viewer.role || viewer.user;
  }
  rec = await deps.deviceKey(cfg.farm);
  set({ device: rec.device });
  const code = capture().browser;

  let back = cfg.provider ? await returned(cfg) : null;
  // the step-up for this browser's passkey: hold its token for the
  // passkey's own enrolment, which needs the person's press (touch.js),
  // and go on with the device key as on any load
  if (back && back.touch) {
    if (back.idToken) {
      stepUp = { idToken: back.idToken, at: deps.now() };
      set({ touch: 'finish', touchMessage: null });
    } else set({ touch: 'refused', touchMessage: ASK.has(back.error) ? 'The sign-in was not finished.' : `The sign-in provider answered ${back.error}.` });
    back = null;
  }
  if (back && back.idToken) {
    try {
      if (back.renew && rec.certified) return await certified(await call('/renew', await enrolment(rec.key, request(cfg.farm, person, rec.spki), { idToken: back.idToken })), 'token');
      return await certified(await call('/enroll', await enrolment(rec.key, request(cfg.farm, person, rec.spki), { idToken: back.idToken })), 'token');
    } catch (e) {
      if (e.status === 403 && back.renew) return gone(e);
      if (e.status === 409) return gone(e);
      return refused(e);
    }
  }
  if (back && back.error) {
    if (rec.certified && Date.parse(rec.certified.expires) > deps.now()) use(rec);
    return set(ASK.has(back.error) ? { state: 'sign-in' } : { state: 'refused', message: `The sign-in provider answered ${back.error}.` });
  }

  const c = rec.certified;
  // a key certified for someone else (another person signed in here) is
  // not theirs to sign with: this person enrols their own
  if (c && c.person !== person) return gone(new Error(`This browser’s key was ${c.person}’s.`));
  // in force: sign with it; use() renews it in time, at once if due
  if (c && Date.parse(c.expires) > deps.now()) return use(rec);
  // ended: the page has only just loaded, so a round trip loses nothing
  if (c && cfg.provider) return signIn(cfg, rec.device, { renew: true });
  if (c) return renewNow();
  return enrol(code);
}

// renew: renew this browser's key now, as its timer does before the leaf
// ends (the farm's embedded test calls it; nothing on the page does yet)
export const renew = () => renewNow();

// signInNow: the person asked to sign in (their page's button)
export function signInNow() {
  if (cfg && cfg.provider && rec) return signIn(cfg, rec.device, { silent: false });
}

// ------------------------------------------------------------ devices

// devices: the person's devices in force, from the issuer
export function devices(who) {
  return call('/devices?person=' + encodeURIComponent(who));
}

// remove: a signed act removing one device (its ID), or every device of
// the person's (all: true); removing this browser's own key forgets it
export async function remove({ device, all }) {
  if (!rec || !rec.certified) throw new Error('This browser’s key is not certified yet, so it cannot sign a removal.');
  const c = rec.certified;
  const signer = { key: rec.key, chain: c.chain.map(unb64), farm: c.farm, user: c.person, identity: c.identity, device: c.device };
  const act = all
    ? { subject: { name: `farm:${c.farm}`, digest: { c4: c.farm } }, act: 'remove-device', params: { person: c.person } }
    : { subject: { name: `device:${device}`, digest: { c4: device } }, act: 'remove-device', params: {} };
  const out = await call('/acts', await signAct(signer, act));
  if (all || device === c.device) {
    clearTimeout(timer);
    useSigner(null);
    await deps.forgetKey(cfg.farm);
    rec = await deps.deviceKey(cfg.farm);
    set({ state: cfg.provider ? 'sign-in' : 'open', device: rec.device, expires: null });
  }
  return out;
}

// reset: forget this page's flow (tests)
export function reset() {
  clearTimeout(timer);
  clearTimeout(lapse);
  started = cfg = rec = person = as = caught = stepUp = null;
  Object.assign(status, { state: 'starting', device: null, expires: null, person: null, message: null, notices: null, touch: null, touchMessage: null });
  useSigner(null);
}
