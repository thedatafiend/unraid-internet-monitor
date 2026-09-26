// Shared helpers: API access, DOM building, formatting, theme and colours.

export async function getJSON(path) {
  const r = await fetch(path, { cache: 'no-store' });
  if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
  return r.json();
}

export async function postJSON(path) {
  const r = await fetch(path, { method: 'POST' });
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(body.error || r.statusText);
  return body;
}

// h builds an element. Strings become text nodes, so data never becomes markup.
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

// fill replaces el's children, skipping null/false (replaceChildren would
// render them as the text "null").
export function fill(el, ...kids) {
  el.replaceChildren(...kids.flat().filter(k => k != null && k !== false));
  return el;
}

// Brand icon and tab title follow the connection state on every page.
export function setBrandState(state) {
  const s = STATUS[state] || STATUS.unknown;
  const icon = document.getElementById('brandIcon');
  if (icon) { icon.className = 'status-icon ' + s.cls; icon.textContent = s.icon; }
  document.title = (state === 'online' || !STATUS[state] ? '' : s.label + ' · ') + 'Internet Monitor';
}

// ---- Formatting ----
export const fmt = {
  ms(v) { return v == null ? '–' : (v < 100 ? v.toFixed(1) : Math.round(v).toString()) + ' ms'; },
  msShort(v) { return v == null ? '–' : (v < 100 ? v.toFixed(1) : Math.round(v).toString()); },
  pct(v) {
    if (v == null) return '–';
    if (v === 0) return '0%';
    if (v > 0 && v < 0.1) return '<0.1%';
    return (v < 10 ? v.toFixed(1) : Math.round(v).toString()) + '%';
  },
  uptime(v) {
    if (v == null) return '–';
    if (v >= 100) return '100%';
    return (Math.floor(v * 100) / 100).toFixed(2) + '%'; // never round 99.996 up to 100
  },
  dur(sec) {
    sec = Math.max(0, Math.round(sec));
    if (sec < 60) return sec + 's';
    const m = Math.floor(sec / 60), s = sec % 60;
    if (m < 60) return s ? `${m}m ${s}s` : `${m}m`;
    const hr = Math.floor(m / 60), mm = m % 60;
    if (hr < 24) return mm ? `${hr}h ${mm}m` : `${hr}h`;
    const d = Math.floor(hr / 24), hh = hr % 24;
    return hh ? `${d}d ${hh}h` : `${d}d`;
  },
  time(ts) { return new Date(ts * 1000).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit', second: '2-digit' }); },
  datetime(ts) {
    return new Date(ts * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit', second: '2-digit' });
  },
  bytes(n) {
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(0) + ' KB';
    return (n / 1048576).toFixed(1) + ' MB';
  },
};

export const now = () => Math.floor(Date.now() / 1000);

// ---- Domain labels ----
export const STATUS = {
  online: { label: 'Online', icon: '●', cls: 'good' },
  degraded: { label: 'Degraded', icon: '▲', cls: 'warning' },
  outage: { label: 'Outage', icon: '■', cls: 'critical' },
  unknown: { label: 'Starting…', icon: '○', cls: 'muted' },
};
export const KIND = {
  outage: { label: 'Outage', icon: '■', cls: 'critical' },
  degraded: { label: 'Degraded', icon: '▲', cls: 'warning' },
  partial: { label: 'Target unreachable', icon: '▲', cls: 'warning' },
  isp_hop_change: { label: 'ISP route changed', icon: '●', cls: 'muted' },
};
export const CAUSE = {
  local: 'Your router or LAN',
  isp_edge: "Your ISP's connection",
  upstream: 'ISP network or beyond',
};
export const ROLE = { gateway: 'Router', isp: 'ISP edge', internet: 'Internet', custom: 'Custom' };
const ROLE_ORDER = { gateway: 0, isp: 1, internet: 2, custom: 3 };

export function statusIcon(s) {
  return h('span', { class: 'status-icon ' + s.cls, 'aria-hidden': 'true', text: s.icon });
}

export function mosLabel(m) {
  if (m == null) return '';
  if (m >= 4.3) return 'Excellent';
  if (m >= 4.0) return 'Good';
  if (m >= 3.6) return 'Fair';
  if (m >= 3.1) return 'Poor';
  return 'Bad';
}

// ---- Theme ----
const THEME_KEY = 'im-theme';
const THEMES = ['auto', 'light', 'dark'];

export function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export function initTheme(btn) {
  let mode = 'auto';
  try { mode = localStorage.getItem(THEME_KEY) || 'auto'; } catch { /* storage unavailable */ }
  const apply = () => {
    if (mode === 'auto') document.documentElement.removeAttribute('data-theme');
    else document.documentElement.setAttribute('data-theme', mode);
    btn.textContent = 'Theme: ' + mode;
    window.dispatchEvent(new Event('themechange'));
  };
  btn.addEventListener('click', () => {
    mode = THEMES[(THEMES.indexOf(mode) + 1) % THEMES.length];
    try { localStorage.setItem(THEME_KEY, mode); } catch { /* ignore */ }
    apply();
  });
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
    if (mode === 'auto') window.dispatchEvent(new Event('themechange'));
  });
  apply();
}

// ---- Targets and their colours ----
// Colour follows the target, never its position in a chart: slots are
// assigned by target ID (stable across restarts), so filtering never repaints.
export async function loadTargets() {
  const all = await getJSON('/api/targets');
  all.sort((a, b) => a.id - b.id);
  all.forEach((t, i) => { t.slot = i < 8 ? i + 1 : 0; });
  return all;
}

export function colorOf(t) {
  return t && t.slot ? cssVar(`--series-${t.slot}`) : cssVar('--muted');
}

export function byRole(targets) {
  return [...targets].sort((a, b) => (ROLE_ORDER[a.role] - ROLE_ORDER[b.role]) || a.id - b.id);
}

export function lineKey(color) {
  return h('span', { class: 'key-line', style: { background: color }, 'aria-hidden': 'true' });
}

// A filter row of preset buttons; onPick receives the chosen value.
export function segmented(options, current, onPick, label) {
  const wrap = h('div', { class: 'seg', role: 'group', 'aria-label': label });
  for (const [value, text] of options) {
    wrap.append(h('button', {
      type: 'button', 'aria-pressed': String(value === current), text,
      onclick: () => onPick(value),
    }));
  }
  return wrap;
}
