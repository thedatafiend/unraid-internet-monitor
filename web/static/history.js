// History: stored per-minute data for a time range, with outages shaded.
import { h, getJSON, fmt, now, loadTargets, colorOf, byRole, cssVar, segmented, lineKey, fill, isPing, overran } from './util.js';
import { timeChart, legend, alpha } from './charts.js';

const PRESETS = [['1h', '1 h', 3600], ['6h', '6 h', 6 * 3600], ['24h', '24 h', 86400], ['7d', '7 d', 7 * 86400], ['30d', '30 d', 30 * 86400]];
const MAX_POINTS = 1000;

// Same rule as the server: at most MAX_POINTS buckets, whole minutes.
function pickStep(span) {
  const step = Math.max(60, Math.ceil(span / MAX_POINTS));
  return Math.ceil(step / 60) * 60;
}

export async function mount(root, ctx) {
  const state = { preset: '24h', from: null, to: null, target: 'all', table: false };
  let charts = [];
  let targets = [];
  let loadSeq = 0;

  const filterRow = h('div', { class: 'filters' });
  const shadeLegend = h('div', { class: 'legend' });
  const body = h('div', { class: 'charts', style: { display: 'grid', gap: '16px' } });
  fill(root, filterRow, body);

  targets = byRole((await loadTargets()).filter(t => t.enabled));
  if (ctx.gone()) return () => {};
  renderFilters();
  await load();
  window.addEventListener('themechange', load);
  return () => {
    charts.forEach(c => c.destroy());
    window.removeEventListener('themechange', load);
  };

  function range() {
    if (state.from != null) return [state.from, state.to];
    const span = PRESETS.find(p => p[0] === state.preset)[2];
    const to = now();
    return [to - span, to];
  }

  function renderFilters() {
    const pick = v => { state.preset = v; state.from = state.to = null; renderFilters(); load(); };
    const opt = t => h('option', { value: String(t.id), text: t.name });
    const svc = targets.filter(t => !isPing(t));
    const sel = h('select', { class: 'btn', 'aria-label': 'Target' },
      h('option', { value: 'all', text: 'All targets' }),
      h('optgroup', { label: 'Ping' }, ...targets.filter(isPing).map(opt)),
      svc.length ? h('optgroup', { label: 'DNS & web' }, ...svc.map(opt)) : null);
    sel.value = state.target;
    sel.addEventListener('change', () => { state.target = sel.value; load(); });
    const tableToggle = h('input', { type: 'checkbox' });
    tableToggle.checked = state.table;
    tableToggle.addEventListener('change', () => { state.table = tableToggle.checked; load(); });
    fill(filterRow, 
      segmented(PRESETS.map(p => [p[0], p[1]]), state.from == null ? state.preset : null, pick, 'Time range'),
      sel,
      state.from != null
        ? h('button', { class: 'btn', type: 'button', text: 'Reset zoom', onclick: () => pick(state.preset) })
        : null,
      h('label', { class: 'check' }, tableToggle, 'Show table'),
      h('span', { class: 'muted', text: 'Drag across a chart to zoom in.' }));
  }

  async function load() {
    const seq = ++loadSeq;
    const [from, to] = range();
    const step = pickStep(to - from);
    const shown = state.target === 'all' ? targets : targets.filter(t => String(t.id) === state.target);
    body.classList.add('loading');
    let metrics, events, tests;
    try {
      [metrics, events, tests] = await Promise.all([
        Promise.all(shown.map(t => getJSON(`/api/metrics?target=${t.id}&from=${from}&to=${to}&step=${step}`))),
        getJSON(`/api/events?from=${from}&to=${to}`),
        getJSON(`/api/speedtests?from=${from}&to=${to}`).catch(() => []),
      ]);
    } catch (err) {
      body.classList.remove('loading');
      fill(body, h('section', { class: 'card empty', text: 'Could not load history: ' + err.message }));
      return;
    }
    if (seq !== loadSeq || ctx.gone()) return;
    render(from, to, step, shown, metrics, events, tests);
    body.classList.remove('loading');
  }

  function render(from, to, step, shown, metrics, events, tests) {
    charts.forEach(c => c.destroy());
    charts = [];

    // Align every target on one set of bucket timestamps.
    const tsSet = new Set();
    metrics.forEach(m => m.ts.forEach(t => tsSet.add(t)));
    const xs = [...tsSet].sort((a, b) => a - b);
    const col = (m, key) => { const idx = new Map(m.ts.map((t, i) => [t, i])); return xs.map(t => (idx.has(t) ? m[key][idx.get(t)] : null)); };

    const critical = cssVar('--critical'), warning = cssVar('--warning');
    const allOutages = events.filter(e => e.kind === 'outage' && e.scope === 'ip4');
    // The part of an outage inside its scheduled reboot window is expected.
    const reboots = allOutages.filter(e => e.planned);
    const outages = allOutages.filter(e => !e.planned || overran(e));
    const outageStart = e => (e.planned ? Math.max(e.started_at, e.planned.end) : e.started_at);
    const degraded = events.filter(e => e.kind === 'degraded');
    // Speed tests saturate the line on purpose; mark them so their latency
    // spike isn't mistaken for a problem.
    const muted = cssVar('--muted');
    const shadeIvs = [
      ...tests.map(t => ({ start: t.ts, end: t.ts + Math.max(t.duration_s, 1), color: alpha(muted, 0.22) })),
      ...degraded.map(e => ({ start: e.started_at, end: e.ended_at ?? now(), color: alpha(warning, 0.18) })),
      ...reboots.map(e => ({ start: e.started_at, end: Math.min(e.ended_at ?? now(), e.planned.end), color: alpha(muted, 0.3) })),
      ...outages.map(e => ({ start: outageStart(e), end: e.ended_at ?? now(), color: alpha(critical, 0.16) })),
    ];
    fill(shadeLegend,
      ...(tests.length ? [h('span', { class: 'name-cell' }, h('span', { class: 'key-rect', style: { background: alpha(muted, 0.4) } }), `Speed test (${tests.length})`)] : []),
      ...(outages.length ? [h('span', { class: 'name-cell' }, h('span', { class: 'key-rect', style: { background: alpha(critical, 0.35) } }), `Outage (${outages.length})`)] : []),
      ...(reboots.length ? [h('span', { class: 'name-cell' }, h('span', { class: 'key-rect', style: { background: alpha(muted, 0.5) } }), `Scheduled reboot (${reboots.length})`)] : []),
      ...(degraded.length ? [h('span', { class: 'name-cell' }, h('span', { class: 'key-rect', style: { background: alpha(warning, 0.45) } }), `Degraded (${degraded.length})`)] : []));

    if (!xs.length) {
      fill(body, h('section', { class: 'card empty', text: 'No data in this range yet. History is saved once a minute.' }));
      return;
    }

    const common = { shade: () => shadeIvs, sync: 'history', onZoom: (a, b) => { state.from = a; state.to = b; renderFilters(); load(); } };
    const single = shown.length === 1 ? shown[0] : null;
    const cards = [];
    // Ping targets and DNS/web checks are charted separately: different
    // cadences, and web requests are an order of magnitude slower.
    const pick = pred => { const idx = shown.map((t, i) => (pred(t) ? i : -1)).filter(i => i >= 0); return [idx.map(i => shown[i]), idx.map(i => metrics[i])]; };
    const [pingShown, pingMetrics] = pick(isPing);
    const [svcShown, svcMetrics] = pick(t => !isPing(t));
    const singleIsPing = single && isPing(single);

    // Latency
    let latSeries, latData, latBands = [];
    if (single) {
      const c = colorOf(single);
      latSeries = [{ label: '95th percentile', color: alpha(c, 0.55), width: 1 }, { label: 'Median', color: c }];
      latData = [col(metrics[0], 'p95'), col(metrics[0], 'p50')];
      latBands = [[1, 2, alpha(c, 0.12)]];
    } else {
      latSeries = pingShown.map(t => ({ label: t.name, color: colorOf(t) }));
      latData = pingMetrics.map(m => col(m, 'avg'));
    }
    cards.push(chartCard(
      single && !singleIsPing ? 'Response time' : 'Latency',
      single ? `${single.name}: median with the 95th-percentile band, per ${fmt.dur(step)}` : `Average round-trip time per target, per ${fmt.dur(step)}`,
      latSeries, latData, { yFmt: v => fmt.msShort(v) + ' ms', yMaxAtLeast: 5, bands: latBands }, true));

    // Packet loss
    let lossLabel, lossColor, lossData;
    if (single) {
      lossLabel = single.name; lossColor = colorOf(single); lossData = col(metrics[0], 'loss_pct');
    } else {
      // Pool the internet targets: lost / sent across all of them.
      lossLabel = 'Internet servers combined'; lossColor = cssVar('--aggregate');
      const inet = metrics.filter((m, i) => shown[i].role === 'internet').map(m => [col(m, 'sent'), col(m, 'recv')]);
      lossData = xs.map((_, k) => {
        let sent = 0, recv = 0;
        for (const [S, R] of inet) if (S[k] != null) { sent += S[k]; recv += R[k]; }
        return sent ? 100 * (sent - recv) / sent : null;
      });
    }
    const anyLoss = lossData.some(v => v > 0);
    const lossTitle = single && !singleIsPing ? 'Failed checks' : 'Packet loss';
    cards.push(chartCard(lossTitle, anyLoss ? `${lossLabel}, per ${fmt.dur(step)}` : `${lossLabel}: no ${lossTitle.toLowerCase()} in this range`,
      [{ label: lossLabel, color: lossColor, bars: true }], [lossData], { yFmt: v => fmt.pct(v), yMaxAtLeast: 1, yMax: 100, height: 160 }, false));

    // Jitter (ping only: two DNS lookups a minute say little about jitter)
    if (!single || singleIsPing) {
      const jitSeries = single ? [{ label: single.name, color: colorOf(single) }] : pingShown.map(t => ({ label: t.name, color: colorOf(t) }));
      const jitData = (single ? [metrics[0]] : pingMetrics).map(m => col(m, 'jitter'));
      cards.push(chartCard('Jitter', `Average change between consecutive round trips, per ${fmt.dur(step)}`,
        jitSeries, jitData, { yFmt: v => fmt.msShort(v) + ' ms', yMaxAtLeast: 1, height: 180 }, !single));
    }

    // DNS lookups and web requests
    if (!single && svcShown.length) {
      cards.push(chartCard('DNS & web response time',
        `Name lookups and a fresh HTTPS request (DNS, connect, TLS and server time), per ${fmt.dur(step)}. Gaps are failed checks.`,
        svcShown.map(t => ({ label: t.name, color: colorOf(t) })), svcMetrics.map(m => col(m, 'avg')),
        { yFmt: v => fmt.msShort(v) + ' ms', yMaxAtLeast: 10, height: 200 }, true));
    }

    const tableCard = state.table
      ? (single ? dataTable(xs, shown, metrics, col, single, lossData) : dataTable(xs, pingShown, pingMetrics, col, null, lossData))
      : null;
    fill(body, shadeLegend, ...cards, ...(tableCard ? [tableCard] : []));
    // Charts need their final width, so build them after insertion.
    for (const c of cards) c.build();

    function chartCard(title, subtitle, series, data, opts, withLegend) {
      const el = h('div', { class: 'chart' });
      let ch = null;
      const card = h('section', { class: 'card' },
        h('div', { class: 'card-head' }, h('h2', { text: title }), h('p', { text: subtitle })),
        withLegend && series.length > 1 ? legend(series.map(s => ({ label: s.label, color: s.color })), () => ch) : null,
        el);
      card.build = () => {
        ch = timeChart(el, { ...common, ...opts, series, data: [xs, ...data] });
        charts.push(ch);
      };
      return card;
    }
  }
}

// dataTable is the table view of the charts (newest first).
function dataTable(xs, shown, metrics, col, single, lossData) {
  const LIMIT = 500;
  let headers, rows;
  if (single) {
    const m = metrics[0];
    const cols = ['p50', 'p95', 'max', 'jitter'].map(k => col(m, k));
    headers = ['Median', 'p95', 'Max', 'Jitter', 'Loss'];
    rows = xs.map((t, k) => [t, ...cols.map(c => fmt.msShort(c[k])), fmt.pct(lossData[k])]);
  } else {
    const avgs = metrics.map(m => col(m, 'avg'));
    headers = [...shown.map(t => t.name), 'Internet loss'];
    rows = xs.map((t, k) => [t, ...avgs.map(a => fmt.msShort(a[k])), fmt.pct(lossData[k])]);
  }
  rows.reverse();
  const shownRows = rows.slice(0, LIMIT);
  return h('section', { class: 'card' },
    h('div', { class: 'card-head' }, h('h2', { text: 'Table' }),
      h('p', { text: (single ? 'Latency in ms' : 'Average latency in ms') + (rows.length > LIMIT ? ` · newest ${LIMIT} of ${rows.length} rows` : '') })),
    h('div', { class: 'table-wrap scroll' }, h('table', {},
      h('thead', {}, h('tr', {}, h('th', { text: 'Time' }),
        ...headers.map((name, i) => h('th', { class: 'num' },
          !single && i < shown.length ? h('span', { class: 'name-cell' }, lineKey(colorOf(shown[i])), name) : name)))),
      h('tbody', {}, ...shownRows.map(r => h('tr', {},
        h('td', { text: fmt.datetime(r[0]) }), ...r.slice(1).map(v => h('td', { class: 'num', text: v }))))))));
}
