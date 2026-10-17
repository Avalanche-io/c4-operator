// Live components. A view renders a host element that it does not morph
// (data-own); the component inside keeps its own drawing and animates from
// one snapshot to the next. Views call own() while rendering and hydrate()
// after the DOM is in place.
//
//   view:   h(..., own('pylon', 'pylon:' + key, { glance, mode }))
//   after:  hydrate(main)

const REG = new Map();
const PROPS = new Map();

export function register(name, Cls) {
  REG.set(name, Cls);
}

// own returns a host element for a component. The element keeps its
// identity across renders through data-key, so its component survives.
export function own(name, key, props, attrs = {}) {
  PROPS.set(key, props);
  const el = document.createElement(attrs.tag || 'div');
  el.setAttribute('data-own', '');
  el.setAttribute('data-comp', name);
  el.setAttribute('data-key', key);
  for (const [k, v] of Object.entries(attrs)) if (k !== 'tag' && v !== undefined && v !== null && v !== false) el.setAttribute(k, v === true ? '' : String(v));
  return el;
}

export function hydrate(root) {
  for (const el of root.querySelectorAll('[data-comp]')) {
    const Cls = REG.get(el.dataset.comp);
    if (!Cls) continue;
    const key = el.getAttribute('data-key');
    if (!el.__comp) el.__comp = new Cls(el);
    if (PROPS.has(key)) {
      el.__comp.update(PROPS.get(key));
      PROPS.delete(key);
    }
  }
}

// tween runs fn(t) for t in [0, 1] over ms, eased; ms 0 jumps.
export function tween(ms, fn, done) {
  if (!ms) { fn(1); if (done) done(); return () => {}; }
  const t0 = performance.now();
  let raf = 0;
  const ease = (t) => 1 - Math.pow(1 - t, 3);
  const step = (now) => {
    const t = Math.max(0, Math.min(1, (now - t0) / ms));
    fn(ease(t));
    if (t < 1) raf = requestAnimationFrame(step);
    else if (done) done();
  };
  raf = requestAnimationFrame(step);
  return () => cancelAnimationFrame(raf);
}

export const lerp = (a, b, t) => a + (b - a) * t;

export function ms(name, fallback) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  const n = parseFloat(v);
  return Number.isFinite(n) ? n : fallback;
}
