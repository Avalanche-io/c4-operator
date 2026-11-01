// HUD API client. The mock takes the viewer as ?as=; the real backend uses
// a bearer token, or on a local farm the installer's cookie, which
// `c4 farm open` sets. Every write says it comes from the HUD's own page
// (X-C4-HUD): a page on another localhost port cannot send that header
// without a preflight the farm refuses, so it cannot post to a control
// with the cookie.

import { actHeaders } from './act.js';

const params = new URLSearchParams(location.search);
export const as = params.get('as') || '';
export const staticMode = params.get('static') === '1';

function withViewer(path) {
  if (!as) return path;
  return path + (path.includes('?') ? '&' : '?') + 'as=' + encodeURIComponent(as);
}

// failed: the server's words for a refusal, its status, and what more the
// act needed (needs: confirmed, a touch)
async function failed(r) {
  const body = await r.json().catch(() => ({}));
  const e = new Error(body.error || `${r.status} ${r.statusText}`);
  e.status = r.status;
  e.needs = body.needs;
  return e;
}

export async function get(path) {
  const r = await fetch(withViewer('/api/v1' + path), { headers: { Accept: 'application/json' } });
  if (!r.ok) throw await failed(r);
  return r.json();
}

const WRITE = { 'X-C4-HUD': '1' };

// post: a write; a job control carries its act, signed by this browser's
// key, when the page can sign it (act.js). Refused for want of a touch
// (403, needs: confirmed), it is sent once more with a fresh act: confirmed
// when this browser can touch, otherwise with none, as before personal
// keys. A refused act was never taken, so nothing is spent.
export async function post(path, body) {
  const sent = body || {};
  const send = async (acts) => fetch(withViewer('/api/v1' + path), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...WRITE, ...acts },
    body: JSON.stringify(sent),
  });
  let r = await send(await actHeaders(path, sent));
  if (r.status === 403) {
    const e = await failed(r.clone());
    if (e.needs === 'confirmed') r = await send(await confirmed(path, sent, send));
  }
  if (!r.ok) throw await failed(r);
  return r.json().catch(() => ({}));
}

// confirmed: the confirmed act's headers for a write the farm refused for
// want of a touch. A browser may refuse the passkey's prompt this long
// after the click (Safari wants a fresh press: NotAllowedError); the write
// then waits for one, as an error whose confirm() the person's press
// calls: Touch to confirm (app.js).
async function confirmed(path, sent, send) {
  try {
    return await actHeaders(path, sent, { confirm: true });
  } catch (err) {
    if (!err || err.name !== 'NotAllowedError') throw err;
    const e = new Error('This needs your touch. Confirm with your passkey to send it.');
    e.confirm = async () => {
      const r = await send(await actHeaders(path, sent, { confirm: true }));
      if (!r.ok) throw await failed(r);
      return r.json().catch(() => ({}));
    };
    throw e;
  }
}

export async function del(path) {
  const r = await fetch(withViewer('/api/v1' + path), { method: 'DELETE', headers: { Accept: 'application/json', ...WRITE } });
  if (!r.ok) throw await failed(r);
  return r.json().catch(() => ({}));
}

// issuer: a call to the farm's people's issuer, which the HUD serves at its
// own origin (enrol.js): a read, or with a body a write, marked as the
// HUD's own like every write
export function issuer(path, body) {
  const url = '/issuer/v1/people' + path;
  if (body === undefined) return fetch(url, { headers: { Accept: 'application/json' } });
  return fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json', Accept: 'application/json', ...WRITE }, body });
}

// providerToken: the sign-in provider's token exchange (enrol.js), the one
// write the HUD sends to another origin. It is not the farm's, so it
// carries no write header: a header of the HUD's own would fail the
// provider's CORS.
export function providerToken(url, form) {
  return fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body: form });
}

export function objectURL(url) {
  return withViewer(url);
}

// loadFonts loads every face the stylesheets declare and records how many
// loaded as <html data-fonts="loaded/declared">. The faces use font-display:
// block, so text in a face still loading is invisible; a static page
// (fixtures, screenshots) draws only once they are in, and the capture
// check reads the count.
function loadFonts() {
  const faces = [...document.fonts];
  return Promise.allSettled(faces.map((ff) => ff.load())).then((rs) => {
    document.documentElement.dataset.fonts = `${rs.filter((r) => r.status === 'fulfilled').length}/${faces.length}`;
  });
}

// subscribe delivers snapshots: from the event stream, or once in static mode.
export function subscribe(onSnapshot, onStatus) {
  if (staticMode) {
    loadFonts().then(() => get('/snapshot')).then(onSnapshot, (e) => onStatus('error', e.message, e.status));
    return;
  }
  let es;
  let pollTimer;
  let streamed = false;
  // Paint from one plain request, so the first view never waits on the
  // stream (a proxy that buffers event streams would otherwise hold it).
  get('/snapshot').then((s) => { if (!streamed) onSnapshot(s); }, () => {});
  const poll = () => {
    get('/snapshot').then((s) => { onSnapshot(s); onStatus('polling'); }, (e) => onStatus('error', e.message, e.status));
  };
  const open = () => {
    es = new EventSource(withViewer('/api/v1/events'));
    es.addEventListener('snapshot', (ev) => {
      clearInterval(pollTimer);
      pollTimer = null;
      streamed = true;
      onStatus('live');
      onSnapshot(JSON.parse(ev.data));
    });
    es.onerror = () => {
      onStatus('reconnecting');
      if (!pollTimer) pollTimer = setInterval(poll, 2000);
    };
  };
  open();
}
