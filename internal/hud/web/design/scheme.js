// The color scheme: dark (the default), light, or the system's. tokens.css
// defines both schemes (from design/scheme-rules.js); this module chooses
// one by setting data-scheme on the root element. Without it, the CSS
// follows prefers-color-scheme on its own.
//
//   initScheme(stored)  ?scheme=light|dark in the URL wins for the page, else
//                       the stored preference, else 'system'; returns the scheme
//   setScheme(pref)     'light' | 'dark' | 'system'; under 'system' it keeps
//                       following the OS
//   resolveScheme(pref) the scheme a preference means now
//
// A change dispatches `c4:scheme` on window, { detail: { scheme } }; views
// that draw on canvas redraw on it.

import { tok } from './components/core.js';

const PREFS = ['light', 'dark', 'system'];
const mq = typeof matchMedia === 'function' ? matchMedia('(prefers-color-scheme: light)') : null;
let pref = 'system';
let following = false;

export function resolveScheme(p) {
  if (p === 'light' || p === 'dark') return p;
  return mq && mq.matches ? 'light' : 'dark';
}

function apply() {
  const scheme = resolveScheme(pref);
  const root = document.documentElement;
  if (root.dataset.scheme === scheme) return scheme;
  root.dataset.scheme = scheme;
  tok(true);
  window.dispatchEvent(new CustomEvent('c4:scheme', { detail: { scheme } }));
  return scheme;
}

export function setScheme(p) {
  pref = PREFS.includes(p) ? p : 'system';
  if (mq && !following) {
    following = true;
    mq.addEventListener('change', () => { if (pref === 'system') apply(); });
  }
  return apply();
}

export function initScheme(stored) {
  const q = new URLSearchParams(location.search).get('scheme');
  return setScheme(q === 'light' || q === 'dark' ? q : stored || 'system');
}
