// Speed: daily throughput tests and how much latency rises under load.
import { h, getJSON, postJSON, fmt, now, cssVar, segmented, fill, statusIcon } from './util.js';
import { timeChart, legend } from './charts.js';

const RANGES = [['7d', '7 d', 7 * 86400], ['30d', '30 d', 30 * 86400]];

// gradeStatus maps a bufferbloat grade to a status (icon + label, never colour alone).
export function gradeStatus(g) {
  if (!g) return null;
  if (g === 'A+' || g === 'A') return { icon: '●', cls: 'good', text: 'little or no bufferbloat' };
  if (g === 'B' || g === 'C') return { icon: '▲', cls: 'warning', text: 'noticeable bufferbloat: calls and games may lag during big downloads' };
  return { icon: '■', cls: 'critical', text: 'severe bufferbloat: the connection becomes sluggish under load' };
}

const rate = v => (v >= 1000 ? (v / 1000).toFixed(v % 1000 ? 1 : 0) + ' Gbps' : Math.round(v) + ' Mbps');

export const mbps = v => (v == null ? '–' : (v < 100 ? v.toFixed(1) : Math.round(v).toString()) + ' Mbps');

export async function mount(root, ctx) {
  const state = { range: '30d' };
  let charts = [];
  let poll = null;

  const runBtn = h('button', { class: 'btn', type: 'button', text: 'Run speed test now' });
  const runStatus = h('span', { class: 'secondary' });
  const nextRun = h('p', { class: 'muted' });
  const tiles = h('section', { class: 'kpis' });
  const filterRow = h('div', { class: 'filters' });
  const body = h('div', { class: 'charts', style: { display: 'grid', gap: '16px' } });
  root.replaceChildren(
    h('section', { class: 'card' },
      h('div', { class: 'card-head' }, h('h2', { text: 'Speed test' }),
        h('p', { text: 'Download and upload throughput from Cloudflare, plus the latency increase while the line is full (bufferbloat)' })),
      h('div', { class: 'filters' }, runBtn, runStatus), nextRun),
    tiles, filterRow, body);

  runBtn.addEventListener('click', async () => {
    runBtn.disabled = true;
    try { await postJSON('/api/speedtest'); watch(); }
    catch (err) { runStatus.textContent = err.message; runBtn.disabled = false; }
  });

  renderFilters();
  await load();
  const status = await getJSON('/api/status');
  if (ctx.gone()) return () => {};
  if (status.speedtest.progress.running) watch();
  nextRun.textContent = status.speedtest.next_at ? 'Next scheduled test: ' + fmt.datetime(status.speedtest.next_at) : 'Scheduled tests are off (SPEEDTEST_SCHEDULE=off).';
  window.addEventListener('themechange', load);
  return () => {
    clearInterval(poll);
    charts.forEach(c => c.destroy());
    window.removeEventListener('themechange', load);
  };

  // watch polls progress while a test runs, then reloads the results.
  function watch() {
    clearInterval(poll);
    runBtn.disabled = true;
    const phases = { latency: 'Measuring idle latency…', download: 'Downloading', upload: 'Uploading' };
    poll = setInterval(async () => {
      let p;
      try { p = await getJSON('/api/speedtest/progress'); } catch { return; }
      if (p.running) {
        runStatus.textContent = (phases[p.phase] || p.phase) + (p.phase === 'latency' ? '' : ` · ${mbps(p.mbps)}`);
        return;
      }
      clearInterval(poll);
      runBtn.disabled = false;
      runStatus.textContent = 'Done.';
      load();
    }, 500);
  }

  function renderFilters() {
    fill(filterRow, segmented(RANGES.map(r => [r[0], r[1]]), state.range, v => { state.range = v; renderFilters(); load(); }, 'Time range'));
  }

  async function load() {
    const to = now();
    const from = to - RANGES.find(r => r[0] === state.range)[2];
    body.classList.add('loading');
    let results;
    try { results = await getJSON(`/api/speedtests?from=${from}&to=${to}`); }
    catch (err) { fill(body, h('section', { class: 'card empty', text: 'Could not load results: ' + err.message })); return; }
    if (ctx.gone()) return;
    body.classList.remove('loading');
    renderTiles(results[0]);
    renderCharts(results.slice().reverse().filter(r => !r.error || r.down_mbps != null));
    body.append(resultsTable(results));
  }

  function renderTiles(last) {
    if (!last) {
      fill(tiles, h('section', { class: 'card empty', style: { gridColumn: '1 / -1' }, text: 'No speed tests yet. Run one now, or wait for the scheduled test.' }));
      return;
    }
    const g = gradeStatus(last.grade);
    const tile = (label, value, sub, extra) => h('section', { class: 'card tile' },
      h('div', { class: 'label', text: label }), h('div', { class: 'value' }, extra, value), h('div', { class: 'sub', text: sub }));
    fill(tiles,
      tile('Download', mbps(last.down_mbps), `${fmt.datetime(last.ts)}${last.server ? ' · via ' + last.server : ''}`),
      tile('Upload', mbps(last.up_mbps), `${fmt.bytes(last.bytes_down + last.bytes_up)} of data used`),
      tile('Latency idle → loaded', `${fmt.msShort(last.idle_ms)} → ${fmt.msShort(Math.max(last.loaded_down_ms ?? 0, last.loaded_up_ms ?? 0) || null)} ms`,
        `download ${fmt.ms(last.loaded_down_ms)} · upload ${fmt.ms(last.loaded_up_ms)}`),
      tile('Bufferbloat grade', last.grade || '–', g ? g.text : '', g ? [statusIcon(g), ' '] : null));
  }

  function renderCharts(results) {
    charts.forEach(c => c.destroy());
    charts = [];
    if (!results.length) { fill(body); return; }
    const xs = results.map(r => r.ts);
    const down = cssVar('--series-1'), up = cssVar('--series-2'), idle = cssVar('--aggregate');
    const cards = [
      card('Throughput', 'Each point is one test', [
        { label: 'Download', color: down, points: true }, { label: 'Upload', color: up, points: true }],
      [results.map(r => r.down_mbps), results.map(r => r.up_mbps)], { yFmt: rate, yMaxAtLeast: 10 }),
      card('Latency under load', 'Median round trip to 1.1.1.1 while idle and during each direction of the test', [
        { label: 'Idle', color: idle, points: true }, { label: 'During download', color: down, points: true }, { label: 'During upload', color: up, points: true }],
      [results.map(r => r.idle_ms), results.map(r => r.loaded_down_ms), results.map(r => r.loaded_up_ms)],
      { yFmt: v => fmt.msShort(v) + ' ms', yMaxAtLeast: 10 }),
    ];
    fill(body, ...cards);
    for (const c of cards) c.build();

    function card(title, subtitle, series, data, opts) {
      const el = h('div', { class: 'chart' });
      let ch = null;
      const c = h('section', { class: 'card' },
        h('div', { class: 'card-head' }, h('h2', { text: title }), h('p', { text: subtitle })),
        legend(series.map(s => ({ label: s.label, color: s.color })), () => ch), el);
      c.build = () => { ch = timeChart(el, { ...opts, series, data: [xs, ...data], height: 220 }); charts.push(ch); };
      return c;
    }
  }
}

function resultsTable(results) {
  if (!results.length) return h('div');
  return h('section', { class: 'card' },
    h('div', { class: 'card-head' }, h('h2', { text: 'Results' }), h('p', { text: `${results.length} test${results.length > 1 ? 's' : ''} in this range` })),
    h('div', { class: 'table-wrap scroll' }, h('table', {},
      h('thead', {}, h('tr', {}, ...['When', 'Download', 'Upload', 'Idle', 'Loaded ↓', 'Loaded ↑', 'Grade', 'Data', 'Server'].map((t, i) =>
        h('th', { class: i > 0 && i < 8 ? 'num' : null, text: t })))),
      h('tbody', {}, ...results.map(r => h('tr', {},
        h('td', {}, fmt.datetime(r.ts), r.trigger === 'manual' ? h('span', { class: 'muted', text: ' · manual' }) : null),
        ...(r.error && r.down_mbps == null
          ? [h('td', { colspan: 8, class: 'secondary', text: 'Failed: ' + r.error })]
          : [
            h('td', { class: 'num', text: mbps(r.down_mbps) }),
            h('td', { class: 'num', text: mbps(r.up_mbps) }),
            h('td', { class: 'num', text: fmt.msShort(r.idle_ms) }),
            h('td', { class: 'num', text: fmt.msShort(r.loaded_down_ms) }),
            h('td', { class: 'num', text: fmt.msShort(r.loaded_up_ms) }),
            h('td', { class: 'num', text: r.grade || '–' }),
            h('td', { class: 'num', text: fmt.bytes(r.bytes_down + r.bytes_up) }),
            h('td', { class: 'secondary', text: r.server || '' })]),
      ))))));
}
