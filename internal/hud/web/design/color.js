// Color math for the scheme rules (scheme-rules.js), the token generator
// and the contrast test. No DOM; runs in the browser and in node.
//
// sRGB in [0, 1]; CIELAB is D65; OKLab is Ottosson's, with the CSS Color 4
// matrices; contrast is WCAG 2.x; ΔE is CIEDE2000; color-vision simulations
// are Machado, Oliveira and Fernandes (2009) at severity 1, in linear RGB.

export const hex2rgb = (h) => {
  const s = h.replace('#', '');
  return [0, 2, 4].map((i) => parseInt(s.slice(i, i + 2), 16) / 255);
};
const clamp01 = (x) => Math.min(1, Math.max(0, x));
export const rgb2hex = (c) => '#' + c.map((x) => Math.round(clamp01(x) * 255).toString(16).padStart(2, '0')).join('');

const toLin = (x) => (x <= 0.04045 ? x / 12.92 : Math.pow((x + 0.055) / 1.055, 2.4));
const toGam = (x) => (x <= 0.0031308 ? 12.92 * x : 1.055 * Math.pow(x, 1 / 2.4) - 0.055);
export const lin = (c) => c.map(toLin);
export const gam = (c) => c.map(toGam);

// WCAG relative luminance and contrast ratio
export const luminance = (hex) => { const [r, g, b] = lin(hex2rgb(hex)); return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
export function contrast(a, b) {
  const la = luminance(a), lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

// CIELAB (D65)
const WHITE = [0.95047, 1, 1.08883];
function lin2xyz([r, g, b]) {
  return [0.4124564 * r + 0.3575761 * g + 0.1804375 * b, 0.2126729 * r + 0.7151522 * g + 0.072175 * b, 0.0193339 * r + 0.119192 * g + 0.9503041 * b];
}
function xyz2lin([x, y, z]) {
  return [3.2404542 * x - 1.5371385 * y - 0.4985314 * z, -0.969266 * x + 1.8760108 * y + 0.041556 * z, 0.0556434 * x - 0.2040259 * y + 1.0572252 * z];
}
const E = 216 / 24389, K = 24389 / 27;
export function linToLab(c) {
  const f = lin2xyz(c).map((v, i) => { const t = v / WHITE[i]; return t > E ? Math.cbrt(t) : (K * t + 16) / 116; });
  return [116 * f[1] - 16, 500 * (f[0] - f[1]), 200 * (f[1] - f[2])];
}
export const hex2lab = (hex) => linToLab(lin(hex2rgb(hex)));
export function lab2rgb([L, a, b]) {
  const fy = (L + 16) / 116, fx = fy + a / 500, fz = fy - b / 200;
  const inv = (f) => (f * f * f > E ? f * f * f : (116 * f - 16) / K);
  const xyz = [inv(fx) * WHITE[0], (L > K * E ? fy * fy * fy : L / K), inv(fz) * WHITE[2]];
  return gam(xyz2lin(xyz));
}

// OKLab and OKLCH
export function linToOklab([r, g, b]) {
  const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b);
  const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b);
  const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b);
  return [0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s, 1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s, 0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s];
}
export function oklabToLin([L, a, b]) {
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3;
  const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3;
  const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3;
  return [4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s, -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s, -0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s];
}
export const hex2oklab = (hex) => linToOklab(lin(hex2rgb(hex)));
export const oklab2hex = (o) => rgb2hex(gam(oklabToLin(o)));
export function hex2oklch(hex) {
  const [L, a, b] = hex2oklab(hex);
  return [L, Math.hypot(a, b), ((Math.atan2(b, a) * 180) / Math.PI + 360) % 360];
}
const lch2lab = ([L, C, h]) => [L, C * Math.cos((h * Math.PI) / 180), C * Math.sin((h * Math.PI) / 180)];
const inGamut = (c, eps = 1e-6) => c.every((x) => x >= -eps && x <= 1 + eps);
// oklch2hex keeps L and h and lowers chroma until the color is in sRGB
// (CSS Color 4 gamut mapping, without the ΔEOK shortcut).
export function oklch2hex([L, C, h]) {
  let lo = 0, hi = C;
  if (inGamut(oklabToLin(lch2lab([L, C, h])))) return oklab2hex(lch2lab([L, C, h]));
  for (let i = 0; i < 40; i++) {
    const mid = (lo + hi) / 2;
    if (inGamut(oklabToLin(lch2lab([L, mid, h])))) lo = mid; else hi = mid;
  }
  return oklab2hex(lch2lab([L, lo, h]));
}

// mix is CSS color-mix(in oklab, a p, b): p of a over b.
export function mix(a, b, p) {
  const x = hex2oklab(a), y = hex2oklab(b);
  return oklab2hex(x.map((v, i) => p * v + (1 - p) * y[i]));
}

// CIEDE2000
export function de2000(lab1, lab2) {
  const [L1, a1, b1] = lab1, [L2, a2, b2] = lab2;
  const rad = Math.PI / 180;
  const C1 = Math.hypot(a1, b1), C2 = Math.hypot(a2, b2), Cb = (C1 + C2) / 2;
  const G = 0.5 * (1 - Math.sqrt(Cb ** 7 / (Cb ** 7 + 25 ** 7)));
  const a1p = (1 + G) * a1, a2p = (1 + G) * a2;
  const C1p = Math.hypot(a1p, b1), C2p = Math.hypot(a2p, b2);
  const hp = (a, b) => (a === 0 && b === 0 ? 0 : (Math.atan2(b, a) / rad + 360) % 360);
  const h1p = hp(a1p, b1), h2p = hp(a2p, b2);
  const dLp = L2 - L1, dCp = C2p - C1p;
  let dhp = 0;
  if (C1p * C2p !== 0) { dhp = h2p - h1p; if (dhp > 180) dhp -= 360; else if (dhp < -180) dhp += 360; }
  const dHp = 2 * Math.sqrt(C1p * C2p) * Math.sin((dhp / 2) * rad);
  const Lbp = (L1 + L2) / 2, Cbp = (C1p + C2p) / 2;
  let hbp = h1p + h2p;
  if (C1p * C2p !== 0) { if (Math.abs(h1p - h2p) <= 180) hbp /= 2; else hbp = h1p + h2p < 360 ? (hbp + 360) / 2 : (hbp - 360) / 2; }
  const T = 1 - 0.17 * Math.cos((hbp - 30) * rad) + 0.24 * Math.cos(2 * hbp * rad) + 0.32 * Math.cos((3 * hbp + 6) * rad) - 0.2 * Math.cos((4 * hbp - 63) * rad);
  const dTheta = 30 * Math.exp(-(((hbp - 275) / 25) ** 2));
  const Rc = 2 * Math.sqrt(Cbp ** 7 / (Cbp ** 7 + 25 ** 7));
  const Sl = 1 + (0.015 * (Lbp - 50) ** 2) / Math.sqrt(20 + (Lbp - 50) ** 2);
  const Sc = 1 + 0.045 * Cbp, Sh = 1 + 0.015 * Cbp * T;
  const Rt = -Math.sin(2 * dTheta * rad) * Rc;
  return Math.sqrt((dLp / Sl) ** 2 + (dCp / Sc) ** 2 + (dHp / Sh) ** 2 + Rt * (dCp / Sc) * (dHp / Sh));
}
export const deltaE = (x, y) => de2000(hex2lab(x), hex2lab(y));

// Color-vision simulation, linear RGB
export const CVD = {
  normal: null,
  protan: [[0.152286, 1.052583, -0.204868], [0.114503, 0.786281, 0.099216], [-0.003882, -0.048116, 1.051998]],
  deutan: [[0.367322, 0.860646, -0.227968], [0.280085, 0.672501, 0.047413], [-0.01182, 0.04294, 0.968881]],
  tritan: [[1.255528, -0.076749, -0.178779], [-0.078411, 0.930809, 0.147602], [0.004733, 0.691367, 0.3039]],
};
export function simulate(hex, kind) {
  const M = CVD[kind];
  if (!M) return hex;
  const c = lin(hex2rgb(hex));
  return rgb2hex(gam(M.map((row) => clamp01(row[0] * c[0] + row[1] * c[1] + row[2] * c[2]))));
}
