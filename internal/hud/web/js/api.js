// HUD API client. The mock takes the viewer as ?as=; the real backend uses
// a bearer token and ignores it.

const params = new URLSearchParams(location.search);
export const as = params.get('as') || '';
export const staticMode = params.get('static') === '1';

function withViewer(path) {
  if (!as) return path;
  return path + (path.includes('?') ? '&' : '?') + 'as=' + encodeURIComponent(as);
}

export async function get(path) {
  const r = await fetch(withViewer('/api/v1' + path), { headers: { Accept: 'application/json' } });
  if (!r.ok) {
    const body = await r.json().catch(() => ({}));
    throw new Error(body.error || `${r.status} ${r.statusText}`);
  }
  return r.json();
}

export async function post(path, body) {
  const r = await fetch(withViewer('/api/v1' + path), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
  const out = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(out.error || `${r.status} ${r.statusText}`);
  return out;
}

export async function del(path) {
  const r = await fetch(withViewer('/api/v1' + path), { method: 'DELETE', headers: { Accept: 'application/json' } });
  const out = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(out.error || `${r.status} ${r.statusText}`);
  return out;
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
    loadFonts().then(() => get('/snapshot')).then(onSnapshot, (e) => onStatus('error', e.message));
    return;
  }
  let es;
  let pollTimer;
  let streamed = false;
  // Paint from one plain request, so the first view never waits on the
  // stream (a proxy that buffers event streams would otherwise hold it).
  get('/snapshot').then((s) => { if (!streamed) onSnapshot(s); }, () => {});
  const poll = () => {
    get('/snapshot').then((s) => { onSnapshot(s); onStatus('polling'); }, (e) => onStatus('error', e.message));
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
