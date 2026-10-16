// Small DOM toolkit: element builders and a morph that updates the live
// tree in place, so focus, scroll and canvases survive once-a-second updates.

const SVGNS = 'http://www.w3.org/2000/svg';

function build(ns, tag, attrs, kids) {
  const el = ns ? document.createElementNS(ns, tag) : document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === false || v === null || v === undefined) continue;
      if (k === 'style' && typeof v === 'object') {
        el.setAttribute('style', Object.entries(v).map(([a, b]) => `${a.replace(/[A-Z]/g, (m) => '-' + m.toLowerCase())}:${b}`).join(';'));
        continue;
      }
      el.setAttribute(k === 'className' ? 'class' : k, v === true ? '' : String(v));
    }
  }
  append(el, kids);
  return el;
}

function append(el, kids) {
  for (const k of kids) {
    if (k === null || k === undefined || k === false) continue;
    if (Array.isArray(k)) { append(el, k); continue; }
    el.appendChild(k instanceof Node ? k : document.createTextNode(String(k)));
  }
}

export const h = (tag, attrs, ...kids) => build(null, tag, attrs, kids);
export const s = (tag, attrs, ...kids) => build(SVGNS, tag, attrs, kids);

function keyOf(n) {
  return n.nodeType === 1 ? n.getAttribute('data-key') : null;
}

function same(a, b) {
  if (a.nodeType !== b.nodeType) return false;
  if (a.nodeType !== 1) return true;
  return a.tagName === b.tagName && keyOf(a) === keyOf(b);
}

// morph makes `from` look like `to`, reusing nodes.
export function morph(from, to) {
  if (from.nodeType !== 1) {
    if (from.nodeValue !== to.nodeValue) from.nodeValue = to.nodeValue;
    return;
  }
  for (const a of [...to.attributes]) {
    if (from.getAttribute(a.name) !== a.value) from.setAttribute(a.name, a.value);
  }
  for (const a of [...from.attributes]) {
    if (!to.hasAttribute(a.name)) from.removeAttribute(a.name);
  }
  if (from.tagName === 'INPUT' || from.tagName === 'SELECT' || from.tagName === 'TEXTAREA') {
    if (document.activeElement !== from && 'value' in to && from.value !== to.getAttribute('value') && to.hasAttribute('value')) {
      from.value = to.getAttribute('value');
    }
  }
  // Canvases and owned regions are drawn by components, not by the view.
  if (from.tagName === 'CANVAS' || from.hasAttribute('data-own')) return;
  morphChildren(from, to);
}

function morphChildren(from, to) {
  const keyed = new Map();
  for (const c of from.childNodes) {
    const k = keyOf(c);
    if (k) keyed.set(k, c);
  }
  const newKids = [...to.childNodes];
  let i = 0;
  for (const nk of newKids) {
    const k = keyOf(nk);
    let cur = from.childNodes[i];
    let match = null;
    if (k) {
      match = keyed.get(k) || null;
      if (match && !same(match, nk)) match = null;
    } else if (cur && !keyOf(cur) && same(cur, nk)) {
      match = cur;
    }
    if (match) {
      if (match !== cur) from.insertBefore(match, cur || null);
      morph(match, nk);
    } else {
      from.insertBefore(nk, cur || null);
    }
    i++;
  }
  while (from.childNodes.length > newKids.length) from.removeChild(from.lastChild);
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
}
