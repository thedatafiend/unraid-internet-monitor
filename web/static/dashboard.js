// Dashboard: live state, headline numbers, the last 15 minutes of latency,
// per-target details and recent events.
import {
  h, getJSON, fmt, now, STATUS, CAUSE, ROLE, statusIcon, mosLabel,
  loadTargets, colorOf, byRole, lineKey, cssVar, fill, setBrandState, targetAddress, kindOf,
} from './util.js';
import { timeChart, legend, sparkline } from './charts.js';
import { gradeStatus, mbps } from './speed.js';

const WINDOW = 900; // seconds of live data

export async function mount(root, ctx) {
  let targets = [];
  let status = null;
  let chart = null;
  let es = null;
  let lastTick = 0;
  let timers = [];
  let xs = [], ys = [], order = [];

  // ---- Layout (built once, filled in place) ----
  let heroIcon = h('span', {});
  const heroLabel = h('span', { text: '…' });
  const heroSub = h('p', { class: 'hero-sub' });
  const liveBadge = h('span', { class: 'live', text: 'Connecting…' });
  const publicIP = h('span', { class: 'secondary' });
  const hero = h('section', { class: 'card hero', 'aria-live': 'polite' },
    h('div', {}, h('h1', { class: 'hero-state' }, heroIcon, heroLabel), heroSub),
    h('div', { class: 'hero-side' }, liveBadge, publicIP));

  const tiles = {
    uptime: tile('Uptime, last 24 hours'),
    latency: tile('Latency to the internet'),
    loss: tile('Packet loss'),
    mos: tile('Call quality (MOS)'),
    speed: tile('Last speed test'),
  };
  const kpis = h('section', { class: 'kpis' }, ...Object.values(tiles).map(t => t.el));

  const warnBox = h('section', { class: 'callout', hidden: true });
  const chartEl = h('div', { class: 'chart' });
  const legendSlot = h('div');
  const chartCard = h('section', { class: 'card' },
    h('div', { class: 'card-head' },
      h('h2', { text: 'Latency, last 15 minutes' }),
      h('p', { text: 'Round-trip time to each target, every second. Gaps are lost packets.' })),
    legendSlot, chartEl);

  const targetRows = h('tbody');
  const targetsCard = h('section', { class: 'card' },
    h('div', { class: 'card-head' }, h('h2', { text: 'Targets' }), h('p', { text: 'Last 60 seconds' })),
    h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {},
        h('th', { text: 'Target' }), h('th', { text: 'Role' }), h('th', { text: 'Address' }),
        h('th', { class: 'num', text: 'Now' }), h('th', { class: 'num', text: 'Average' }),
        h('th', { class: 'num', text: 'p95' }), h('th', { class: 'num', text: 'Jitter' }),
        h('th', { class: 'num', text: 'Loss' }))),
      targetRows)));

  const eventsList = h('ul', { class: 'list' });
  const eventsCard = h('section', { class: 'card' },
    h('div', { class: 'card-head' }, h('h2', { text: 'Recent events' }),
      h('a', { href: '#/events', class: 'secondary', text: 'All events →' })),
    eventsList);

  const serviceRows = h('tbody');
  const servicesCard = h('section', { class: 'card' },
    h('div', { class: 'card-head' }, h('h2', { text: 'DNS & web' }),
      h('p', { text: 'Name lookups and a fresh HTTPS request, checked every 30–60 s · last 15 minutes' })),
    h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {},
        h('th', { text: 'Check' }), h('th', { text: 'Server' }), h('th', { text: 'Last result' }),
        h('th', { class: 'num', text: 'Average' }), h('th', { class: 'num', text: 'p95' }), h('th', { class: 'num', text: 'Success' }))),
      serviceRows)));

  fill(root, hero, warnBox, kpis, chartCard,
    h('div', { class: 'grid-wide' }, targetsCard, eventsCard),
    servicesCard);

  // ---- Data ----
  targets = await loadTargets();
  const [st, live] = await Promise.all([getJSON('/api/status'), getJSON(`/api/live?seconds=${WINDOW}`)]);
  if (ctx.gone()) return () => {};
  status = st;
  order = byRole(targets.filter(t => st.targets.some(s => s.id === t.id)));
  seed(live);
  buildChart();
  renderStatus();
  refreshEvents();
  connect();
  timers.push(setInterval(refreshStatus, 5000));
  timers.push(setInterval(renderHeroSub, 1000));
  timers.push(setInterval(refreshEvents, 30000));
  window.addEventListener('themechange', rebuild);

  return () => {
    timers.forEach(clearInterval);
    if (es) es.close();
    if (chart) chart.destroy();
    window.removeEventListener('themechange', rebuild);
  };

  // ---- Live chart ----
  function seed(live) {
    xs = [];
    ys = order.map(() => []);
    const byId = new Map(live.series.map(s => [s.id, s]));
    for (let i = 0, ts = live.from; ts <= live.to; i++, ts++) {
      xs.push(ts);
      order.forEach((t, j) => {
        const s = byId.get(t.id);
        ys[j].push(s && s.state[i] === 1 ? s.rtt[i] : null);
      });
    }
  }

  function buildChart() {
    if (chart) chart.destroy();
    fill(chartEl, );
    const series = order.map(t => ({ label: t.name, color: colorOf(t) }));
    fill(legendSlot, legend(series.map(s => ({ label: s.label, color: s.color })), () => chart));
    chart = timeChart(chartEl, {
      height: 240, series, data: [xs, ...ys], yFmt: v => fmt.msShort(v) + ' ms', yMaxAtLeast: 5,
    });
  }

  function rebuild() {
    buildChart();
    renderStatus();
  }

  function addTick(t) {
    lastTick = Date.now();
    let i = xs.length;
    while (i > 0 && xs[i - 1] > t.ts) i--;
    const exists = i > 0 && xs[i - 1] === t.ts;
    const vals = order.map(tg => t.rtt[tg.id] ?? null);
    if (exists) {
      vals.forEach((v, j) => { ys[j][i - 1] = v; });
    } else {
      xs.splice(i, 0, t.ts);
      vals.forEach((v, j) => ys[j].splice(i, 0, v));
    }
    let drop = 0;
    while (drop < xs.length && xs[drop] < t.ts - WINDOW) drop++;
    if (drop) { xs.splice(0, drop); ys.forEach(y => y.splice(0, drop)); }
    chart.setData([xs, ...ys]);
    if (status && (t.state !== status.state || t.since !== status.since)) {
      status.state = t.state;
      status.since = t.since;
      renderHero();
      refreshEvents(); // show a new or finished event right away
      refreshStatus();
    }
  }

  function connect() {
    es = new EventSource('/api/stream');
    es.onmessage = e => addTick(JSON.parse(e.data));
    es.onerror = () => { liveBadge.className = 'live'; liveBadge.textContent = 'Reconnecting…'; };
    es.onopen = () => { liveBadge.className = 'live on'; liveBadge.textContent = 'Live'; };
  }

  // ---- Status ----
  async function refreshStatus() {
    try {
      status = await getJSON('/api/status');
      renderStatus();
    } catch { /* keep the last render; the live badge shows connectivity */ }
  }

  function renderStatus() {
    renderHero();
    const internet = status.targets.filter(t => t.role === 'internet');

    const up = status.uptime_24h;
    setTile(tiles.uptime, fmt.uptime(up.uptime_pct),
      up.uptime_pct == null ? 'Collecting data…'
        : up.outages === 0 ? 'No outages' : `${up.outages} outage${up.outages > 1 ? 's' : ''}, longest ${fmt.dur(up.longest_s)}`);

    const avgs = internet.map(t => t.avg_60s).filter(v => v != null);
    const avg = avgs.length ? avgs.reduce((a, b) => a + b, 0) / avgs.length : null;
    setTile(tiles.latency, fmt.ms(avg), `Average of ${internet.length} servers, last minute`,
      sparkline(internetTrend(), cssVar('--aggregate')));

    const sent = internet.reduce((a, t) => a + t.sent_60s, 0);
    const lost = internet.reduce((a, t) => a + t.sent_60s * t.loss_pct_60s / 100, 0);
    setTile(tiles.loss, fmt.pct(sent ? 100 * lost / sent : null), 'Internet servers, last minute');

    setTile(tiles.mos, status.mos == null ? '–' : status.mos.toFixed(2),
      status.mos == null ? '' : `${mosLabel(status.mos)} · 1 (bad) to 4.5 (best)`);

    const sp = status.speedtest || {};
    if (sp.progress && sp.progress.running) {
      setTile(tiles.speed, 'Running…', sp.progress.phase === 'latency' ? 'measuring idle latency' : `${sp.progress.phase} · ${mbps(sp.progress.mbps)}`);
    } else if (sp.last && sp.last.down_mbps != null) {
      const g = gradeStatus(sp.last.grade);
      setTile(tiles.speed, `${Math.round(sp.last.down_mbps)} / ${Math.round(sp.last.up_mbps ?? 0)}`,
        `Mbps down / up · ${fmt.datetime(sp.last.ts)}`,
        g ? h('a', { href: '#/speed', class: 'name-cell secondary', style: { fontSize: '12px', marginTop: '6px' } }, statusIcon(g), `Bufferbloat ${sp.last.grade}`) : null);
    } else {
      setTile(tiles.speed, '–', sp.next_at ? 'first test ' + fmt.datetime(sp.next_at) : 'no tests yet',
        h('a', { href: '#/speed', class: 'secondary', style: { fontSize: '12px' }, text: 'Run one now →' }));
    }

    const warnings = status.info.warnings || [];
    warnBox.hidden = warnings.length === 0;
    fill(warnBox, h('strong', { text: 'Check your setup' }),
      h('ul', {}, ...warnings.map(w => h('li', { text: w }))));

    const ip = status.public_ip || {};
    publicIP.textContent = ip.ipv4 || ip.ipv6 ? 'Public IP ' + [ip.ipv4, ip.ipv6].filter(Boolean).join(' · ') : '';

    const services = byRole(status.services || []);
    servicesCard.hidden = services.length === 0;
    fill(serviceRows, services.map(s => {
      const tg = targets.find(t => t.id === s.id) || s;
      let last = '–';
      if (s.last_at) last = s.last_ok ? fmt.ms(s.last_ms) : 'failed: ' + (s.last_error || 'no reply');
      const phases = s.last_http && s.last_ok ? phaseText(s.last_http) : '';
      return h('tr', {},
        h('td', {}, h('span', { class: 'name-cell' }, lineKey(colorOf(tg)), s.name)),
        h('td', { class: 'secondary', text: targetAddress(s) }),
        h('td', {}, h('div', { text: last }), phases ? h('div', { class: 'muted', style: { fontSize: '12px' }, text: phases }) : null),
        h('td', { class: 'num', text: fmt.msShort(s.avg_ms) }),
        h('td', { class: 'num', text: fmt.msShort(s.p95_ms) }),
        h('td', { class: 'num', text: s.success_pct == null ? '–' : fmt.pct(s.success_pct) }));
    }));

    const byId = new Map(status.targets.map(t => [t.id, t]));
    fill(targetRows, ...order.map(tg => {
      const t = byId.get(tg.id);
      if (!t) return null;
      return h('tr', {},
        h('td', {}, h('span', { class: 'name-cell' }, lineKey(colorOf(tg)), t.name)),
        h('td', { class: 'secondary', text: ROLE[t.role] || t.role }),
        h('td', { class: 'secondary', text: t.address }),
        h('td', { class: 'num', text: t.last_ok ? fmt.msShort(t.last_rtt) : 'lost' }),
        h('td', { class: 'num', text: fmt.msShort(t.avg_60s) }),
        h('td', { class: 'num', text: fmt.msShort(t.p95_60s) }),
        h('td', { class: 'num', text: fmt.msShort(t.jitter_60s) }),
        h('td', { class: 'num', text: fmt.pct(t.loss_pct_60s) }));
    }));
  }

  function renderHero() {
    const s = STATUS[status.state] || STATUS.unknown;
    heroIcon.replaceWith(heroIcon = statusIcon(s));
    heroLabel.textContent = s.label;
    setBrandState(status.state);
    renderHeroSub();
  }

  function renderHeroSub() {
    if (!status) return;
    const parts = [];
    if (status.since) parts.push(`since ${fmt.datetime(status.since)} (${fmt.dur(now() - status.since)})`);
    const open = (status.open_events || []).find(e => e.kind === 'outage');
    if (open && open.class) parts.push('likely cause: ' + (CAUSE[open.class] || open.class));
    heroSub.textContent = parts.join(' · ');
    if (lastTick && Date.now() - lastTick > 10000 && es && es.readyState === EventSource.OPEN) {
      liveBadge.className = 'live';
      liveBadge.textContent = 'No data for ' + fmt.dur((Date.now() - lastTick) / 1000);
    }
  }

  // Mean latency of the internet targets in 15-second buckets.
  function internetTrend() {
    const idx = order.map((t, j) => (t.role === 'internet' ? j : -1)).filter(j => j >= 0);
    const out = [];
    for (let i = 0; i < xs.length; i += 15) {
      let sum = 0, n = 0;
      for (let k = i; k < Math.min(i + 15, xs.length); k++) {
        for (const j of idx) if (ys[j][k] != null) { sum += ys[j][k]; n++; }
      }
      out.push(n ? sum / n : null);
    }
    return out;
  }

  async function refreshEvents() {
    try {
      const evs = await getJSON(`/api/events?from=${now() - 7 * 86400}`);
      const recent = evs.reverse().slice(0, 6);
      if (!recent.length) {
        fill(eventsList, h('li', { class: 'muted', text: 'No events in the last 7 days.' }));
        return;
      }
      fill(eventsList, ...recent.map(e => {
        const k = kindOf(e);
        const what = e.kind === 'outage' ? CAUSE[e.class] || ''
          : e.kind === 'partial' ? e.scope
          : e.kind === 'ip_change' ? `${e.details.old_ipv4 || e.details.old_ipv6} → ${e.details.new_ipv4 || e.details.new_ipv6}` : '';
        return h('li', {},
          h('span', { class: 'name-cell' }, statusIcon(k), h('strong', { text: k.label })),
          what ? h('span', { class: 'secondary', text: what }) : null,
          h('span', { class: 'muted', text: fmt.datetime(e.started_at) }),
          h('span', { class: 'muted', text: e.ended_at == null ? 'ongoing' : fmt.dur(e.ended_at - e.started_at) }));
      }));
    } catch { /* keep last */ }
  }
}

// phaseText describes where a web request's time went.
function phaseText(hs) {
  const parts = [['DNS', hs.dns_ms], ['connect', hs.connect_ms], ['TLS', hs.tls_ms], ['server', hs.ttfb_ms]]
    .filter(([, v]) => v != null).map(([k, v]) => `${k} ${fmt.msShort(v)}`);
  return parts.length ? parts.join(' · ') + ' ms' : '';
}

function tile(label) {
  const value = h('div', { class: 'value', text: '–' });
  const sub = h('div', { class: 'sub' });
  const trend = h('div');
  const el = h('section', { class: 'card tile' }, h('div', { class: 'label', text: label }), value, sub, trend);
  return { el, value, sub, trend };
}

function setTile(t, value, sub, trend) {
  t.value.textContent = value;
  t.sub.textContent = sub || '';
  fill(t.trend, ...(trend ? [trend] : []));
}
