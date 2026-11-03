// Compare: mark a change (a new ONT, modem or router) and compare the
// connection before and after it. Rates are per day of monitoring, so a
// short "after" window can still be compared with a longer "before".
import { h, getJSON, postJSON, deleteJSON, fmt, now, CAUSE, CAUSE_ORDER, fill, segmented, statusIcon, onAccessLink } from './util.js';
import { mbps } from './speed.js';

const WINDOWS = [['7', '7 d'], ['14', '14 d'], ['30', '30 d']];

export async function mount(root, ctx) {
  const state = { marker: null, days: '14' };
  let markers = [];

  const when = h('input', { type: 'datetime-local', class: 'btn', 'aria-label': 'When it happened' });
  when.value = localInput(now());
  const label = h('input', { type: 'text', class: 'btn', placeholder: 'e.g. ONT replaced', maxlength: '80', 'aria-label': 'What changed', style: { minWidth: '220px' } });
  const addBtn = h('button', { class: 'btn', type: 'submit', text: 'Add marker' });
  const formMsg = h('span', { class: 'secondary' });
  const form = h('form', { class: 'filters' }, when, label, addBtn, formMsg);
  form.addEventListener('submit', async ev => {
    ev.preventDefault();
    const ts = Math.floor(new Date(when.value).getTime() / 1000);
    if (!label.value.trim() || !Number.isFinite(ts)) { formMsg.textContent = 'Enter a time and a short description.'; return; }
    addBtn.disabled = true;
    try {
      const m = await postJSON('/api/markers', { ts, label: label.value });
      label.value = '';
      formMsg.textContent = '';
      state.marker = m.id;
      await loadMarkers();
    } catch (err) {
      formMsg.textContent = 'Could not add it: ' + err.message;
    } finally {
      addBtn.disabled = false;
    }
  });

  const markerList = h('ul', { class: 'list' });
  const filterRow = h('div', { class: 'filters' });
  const result = h('section', { class: 'card loading-frame' });
  fill(root,
    h('section', { class: 'card' },
      h('div', { class: 'card-head' }, h('h2', { text: 'Markers' }),
        h('p', { text: 'Mark a change, such as a new ONT or router, to compare the connection before and after it. Markers also appear on the History charts.' })),
      h('div', { style: { display: 'grid', gap: '12px' } }, form, markerList)),
    filterRow, result);

  await loadMarkers();
  return () => {};

  async function loadMarkers() {
    try {
      markers = await getJSON('/api/markers');
    } catch (err) {
      fill(markerList, h('li', { class: 'muted', text: 'Could not load markers: ' + err.message }));
      return;
    }
    if (ctx.gone()) return;
    if (!markers.some(m => m.id === state.marker)) {
      const past = markers.filter(m => m.ts <= now());
      state.marker = past.length ? past[past.length - 1].id : null; // the latest one that has happened
    }
    renderMarkers();
    renderFilters();
    load();
  }

  function renderMarkers() {
    if (!markers.length) {
      fill(markerList, h('li', { class: 'muted', text: 'No markers yet.' }));
      return;
    }
    fill(markerList, ...[...markers].reverse().map(m => {
      const del = h('button', { class: 'btn ghost', type: 'button', text: 'Delete', 'aria-label': `Delete marker ${m.label}` });
      del.addEventListener('click', async () => {
        if (!confirm(`Delete the marker “${m.label}”?`)) return;
        try { await deleteJSON(`/api/markers/${m.id}`); await loadMarkers(); }
        catch (err) { formMsg.textContent = 'Could not delete it: ' + err.message; }
      });
      return h('li', {},
        h('span', { class: 'name-cell' }, h('span', { class: 'key-dash' }), h('strong', { text: m.label })),
        h('span', { class: 'secondary', text: fmt.datetime(m.ts) }),
        m.ts > now() ? h('span', { class: 'muted', text: 'not yet' }) : null,
        del);
    }));
  }

  function renderFilters() {
    const usable = markers.filter(m => m.ts <= now());
    if (!usable.length) { fill(filterRow); return; }
    const sel = h('select', { class: 'btn', 'aria-label': 'Compare around' },
      ...[...usable].reverse().map(m => h('option', { value: String(m.id), text: `${m.label} · ${fmt.datetime(m.ts)}` })));
    sel.value = String(state.marker);
    sel.addEventListener('change', () => { state.marker = Number(sel.value); load(); });
    fill(filterRow, h('span', { class: 'secondary', text: 'Compare around' }), sel,
      segmented(WINDOWS, state.days, v => { state.days = v; renderFilters(); load(); }, 'Days on each side'),
      h('span', { class: 'muted', text: 'on each side' }));
  }

  async function load() {
    const m = markers.find(x => x.id === state.marker);
    if (!m) {
      fill(result, h('p', { class: 'muted', text: markers.length ? 'The comparison starts once a marker’s time has passed.' : 'Add a marker to compare before and after it.' }));
      return;
    }
    result.classList.add('loading');
    let c;
    try {
      c = await getJSON(`/api/compare?at=${m.ts}&days=${state.days}`);
    } catch (err) {
      result.classList.remove('loading');
      fill(result, h('p', { class: 'muted', text: 'Could not compare: ' + err.message }));
      return;
    }
    if (ctx.gone() || m.id !== state.marker) return;
    result.classList.remove('loading');
    render(m, c);
  }

  function render(m, c) {
    const b = c.before, a = c.after;
    const days = w => w.uptime.monitored_s / 86400;
    const perDay = (w, n) => (days(w) > 0 ? n / days(w) : null);
    const rate = v => (v == null ? '–' : v === 0 ? '0' : v < 10 ? v.toFixed(2) : v.toFixed(1));
    const span = w => `${fmt.date(w.from)} – ${w.to >= now() - 60 ? 'now' : fmt.date(w.to)}` +
      (w.uptime.monitored_s ? ` · ${fmt.dur(w.uptime.monitored_s)} of data` : ' · no data');

    // [label, before, after, format, lowerIsBetter, points]; points compares
    // percentages by their difference (99.8% → 99.9% is not "+0%").
    const rows = [
      ['Uptime', b.uptime.uptime_pct, a.uptime.uptime_pct, fmt.uptime, false, true],
      ['Outages per day', perDay(b, b.uptime.outages), perDay(a, a.uptime.outages), rate, true],
    ];
    for (const cls of CAUSE_ORDER) {
      const nb = (b.uptime.by_class[cls] || {}).outages || 0, na = (a.uptime.by_class[cls] || {}).outages || 0;
      if (nb || na) rows.push([`  …cause: ${CAUSE[cls]}`, perDay(b, nb), perDay(a, na), rate, true]);
    }
    rows.push(
      ['Downtime per day', perDay(b, b.uptime.downtime_s), perDay(a, a.uptime.downtime_s), v => (v == null ? '–' : fmt.dur(v)), true],
      ['Longest outage', b.uptime.outages ? b.uptime.longest_s : 0, a.uptime.outages ? a.uptime.longest_s : 0, fmt.dur, true],
      ['Brief drops per day', perDay(b, b.blips.count), perDay(a, a.blips.count), rate, true],
      ['  …on your ISP connection', perDay(b, onAccessLink(b.blips.by_class)), perDay(a, onAccessLink(a.blips.by_class)), rate, true],
      ['Packet loss outside outages', b.internet.loss_pct, a.internet.loss_pct, fmt.pct, true],
      ['Average latency', b.internet.avg_ms, a.internet.avg_ms, fmt.ms, true],
      [`Median download (${b.speed.tests} / ${a.speed.tests} tests)`, b.speed.down_mbps, a.speed.down_mbps, mbps, false],
      ['Median upload', b.speed.up_mbps, a.speed.up_mbps, mbps, false],
    );

    const shortAfter = a.uptime.monitored_s < 3 * 86400;
    fill(result,
      h('div', { class: 'card-head' }, h('h2', { text: `Before and after “${m.label}”` }),
        h('p', { text: fmt.datetime(m.ts) })),
      shortAfter && a.uptime.monitored_s > 0 ? h('p', { class: 'callout', text: `Only ${fmt.dur(a.uptime.monitored_s)} since the change. Rates per day are shown, but give it a few days before trusting the difference.` }) : null,
      h('div', { class: 'table-wrap' }, h('table', { class: 'compare' },
        h('thead', {}, h('tr', {}, h('th', { text: '' }),
          h('th', { class: 'num' }, h('div', { text: 'Before' }), h('div', { class: 'muted', text: span(b) })),
          h('th', { class: 'num' }, h('div', { text: 'After' }), h('div', { class: 'muted', text: span(a) })),
          h('th', { class: 'num', text: 'Change' }))),
        h('tbody', {}, ...rows.map(([name, vb, va, f, lower, points]) => h('tr', {},
          h('td', { class: name.startsWith('  ') ? 'secondary' : '', style: name.startsWith('  ') ? { paddingLeft: '22px' } : null, text: name.trim() }),
          h('td', { class: 'num', text: f(vb) }),
          h('td', { class: 'num', text: f(va) }),
          h('td', { class: 'num' }, change(vb, va, lower, points))))))));
  }
}

// change shows the direction and size of a difference, with an icon so it
// never relies on colour alone.
function change(before, after, lowerIsBetter, points) {
  if (before == null || after == null) return '–';
  if (before === after) return h('span', { class: 'muted', text: 'same' });
  const better = lowerIsBetter ? after < before : after > before;
  const st = better ? { icon: '●', cls: 'good' } : { icon: '▲', cls: 'critical' };
  const sign = after > before ? '+' : '−';
  let text, word = better ? ', better' : ', worse';
  if (points) text = `${sign}${Math.abs(after - before).toFixed(2)} pts`;
  else if (before === 0) [text, word] = ['new', ''];
  else if (after === 0 && lowerIsBetter) [text, word] = ['none now', ''];
  else text = `${sign}${Math.round(Math.abs(after - before) / before * 100)}%`;
  // The word is dropped on small screens; the icon still says which way.
  return h('span', { class: 'name-cell' }, statusIcon(st), h('span', {}, text, h('span', { class: 'change-word', text: word })));
}

// localInput formats unix seconds for a datetime-local input (local time).
function localInput(ts) {
  const d = new Date(ts * 1000);
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}
