// HUD API client. The mock takes the viewer as ?as=; the real backend uses
// a bearer token, or on a local farm the installer's cookie, which
// `c4 farm open` sets. Every write says it comes from the HUD's own page
// (X-C4-HUD): a page on another localhost port cannot send that header
// without a preflight the farm refuses, so it cannot post to a control
// with the cookie.

const params = new URLSearchParams(location.search);
export const as = params.get('as') || '';
export const staticMode = params.get('static') === '1';

function withViewer(path) {
  if (!as) return path;
  return path + (path.includes('?') ? '&' : '?') + 'as=' + encodeURIComponent(as);
}

// failed: the server's words for a refusal, and its status
async function failed(r) {
  const body = await r.json().catch(() => ({}));
  const e = new Error(body.error || `${r.status} ${r.statusText}`);
  e.status = r.status;
  return e;
}

export async function get(path) {
  const r = await fetch(withViewer('/api/v1' + path), { headers: { Accept: 'application/json' } });
  if (!r.ok) throw await failed(r);
  return r.json();
}

const WRITE = { 'X-C4-HUD': '1' };

export async function post(path, body) {
  const r = await fetch(withViewer('/api/v1' + path), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...WRITE },
    body: JSON.stringify(body || {}),
  });
  if (!r.ok) throw await failed(r);
  return r.json().catch(() => ({}));
}

export async function del(path) {
  const r = await fetch(withViewer('/api/v1' + path), { method: 'DELETE', headers: { Accept: 'application/json', ...WRITE } });
  if (!r.ok) throw await failed(r);
  return r.json().catch(() => ({}));
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
