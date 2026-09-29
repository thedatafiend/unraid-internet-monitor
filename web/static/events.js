// Events: outages, degradations and other changes, with summary numbers.
import { h, getJSON, fmt, now, CAUSE, statusIcon, segmented, fill, kindOf, overran, routeTable } from './util.js';

const RANGES = [['24h', '24 h', 86400], ['7d', '7 d', 7 * 86400], ['30d', '30 d', 30 * 86400]];
const KINDS = [['', 'All events'], ['outage', 'Outages'], ['degraded', 'Degraded'], ['partial', 'Target unreachable'], ['ip_change', 'Public IP changes'], ['isp_hop_change', 'ISP route changes']];

const DETAIL_LABELS = {
  down_ticks: ['Seconds without internet', v => String(v)],
  gateway_down_pct: ['Router unreachable', v => fmt.pct(v) + ' of the time'],
  isp_down_pct: ['ISP edge unreachable', v => fmt.pct(v) + ' of the time'],
  loss_pct: ['Packet loss when it started', v => fmt.pct(v)],
  p95_ms: ['p95 latency when it started', v => fmt.ms(v)],
  peak_loss_pct: ['Worst packet loss', v => fmt.pct(v)],
  peak_p95_ms: ['Worst p95 latency', v => fmt.ms(v)],
  superseded: ['Ended because', () => 'a wider failure took over'],
  interrupted: ['Note', () => 'monitoring stopped during this event; the end time is approximate'],
  old: ['Previous ISP edge', v => String(v)],
  new: ['New ISP edge', v => String(v)],
  old_ipv4: ['Previous IPv4', v => v || '–'],
  new_ipv4: ['New IPv4', v => v || '–'],
  old_ipv6: ['Previous IPv6', v => v || '–'],
  new_ipv6: ['New IPv6', v => v || '–'],
  error: ['Error', v => String(v)],
  trace_last_hop: ['Last router that answered', v => String(v)],
};

export async function mount(root, ctx) {
  const state = { range: '7d', kind: '' };
  const filterRow = h('div', { class: 'filters' });
  const summary = h('section', { class: 'kpis' });
  const tbody = h('tbody');
  const table = h('section', { class: 'card' },
    h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {},
        h('th', { text: 'Started' }), h('th', { text: 'Event' }), h('th', { text: 'Where' }),
        h('th', { text: 'Likely cause' }), h('th', { class: 'num', text: 'Duration' }), h('th', {}))),
      tbody)));
  const frame = h('div', { class: 'loading-frame', style: { display: 'grid', gap: '16px' } }, summary, table);
  fill(root, filterRow, frame);
  renderFilters();
  await load();
  const timer = setInterval(load, 30000);
  return () => clearInterval(timer);

  function renderFilters() {
    const sel = h('select', { class: 'btn', 'aria-label': 'Event type' }, ...KINDS.map(([v, t]) => h('option', { value: v, text: t })));
    sel.value = state.kind;
    sel.addEventListener('change', () => { state.kind = sel.value; load(); });
    fill(filterRow, 
      segmented(RANGES.map(r => [r[0], r[1]]), state.range, v => { state.range = v; renderFilters(); load(); }, 'Time range'),
      sel);
  }

  async function load() {
    const to = now();
    const from = to - RANGES.find(r => r[0] === state.range)[2];
    frame.classList.add('loading');
    let evs, up;
    try {
      [evs, up] = await Promise.all([
        getJSON(`/api/events?from=${from}&to=${to}${state.kind ? '&kind=' + state.kind : ''}`),
        getJSON(`/api/uptime?from=${from}&to=${to}`),
      ]);
    } catch (err) {
      frame.classList.remove('loading');
      fill(tbody, h('tr', {}, h('td', { colspan: 6, class: 'empty', text: 'Could not load events: ' + err.message })));
      return;
    }
    if (ctx.gone()) return;
    frame.classList.remove('loading');

    fill(summary, 
      stat('Uptime', fmt.uptime(up.uptime_pct), up.monitored_s
        ? `over ${fmt.dur(up.monitored_s)} of monitoring${up.planned_reboots ? ', not counting scheduled reboots' : ''}` : 'Collecting data…'),
      stat('Outages', String(up.outages), up.last_outage_at ? 'last ' + fmt.datetime(up.last_outage_at) : 'none in this range'),
      stat('Total downtime', fmt.dur(up.downtime_s), up.planned_reboots
        ? `plus ${fmt.dur(up.planned_s)} in ${up.planned_reboots} scheduled reboot${up.planned_reboots === 1 ? '' : 's'}` : ''),
      stat('Longest outage', up.outages ? fmt.dur(up.longest_s) : '–', ''));

    evs.reverse();
    if (!evs.length) {
      fill(tbody, h('tr', {}, h('td', { colspan: 6, class: 'empty', text: 'No events in this range.' })));
      return;
    }
    fill(tbody, ...evs.flatMap(row));
  }
}

function stat(label, value, sub) {
  return h('section', { class: 'card tile' },
    h('div', { class: 'label', text: label }), h('div', { class: 'value', text: value }), h('div', { class: 'sub', text: sub }));
}

function where(e) {
  if (e.kind === 'partial') return e.scope === 'ip6' ? 'IPv6' : e.scope;
  if (e.kind === 'isp_hop_change') return `${e.details.old} → ${e.details.new}`;
  if (e.kind === 'ip_change') {
    const d = e.details;
    return [d.old_ipv4 !== d.new_ipv4 && `${d.old_ipv4 || '–'} → ${d.new_ipv4}`, d.old_ipv6 !== d.new_ipv6 && `${d.old_ipv6 || '–'} → ${d.new_ipv6}`]
      .filter(Boolean).join(' · ');
  }
  return e.scope === 'ip6' ? 'IPv6' : 'Internet (IPv4)';
}

function plannedText(e) {
  const p = e.planned;
  const win = `${p.label} (${fmt.clock(p.start)}–${fmt.clock(p.end)})`;
  return overran(e) ? `began during ${win} but was still down after it` : `during ${win}; not alerted or counted as downtime`;
}

function row(e) {
  const k = kindOf(e);
  const details = Object.entries(e.details || {}).filter(([key]) => DETAIL_LABELS[key]);
  const routeSlot = h('div');
  const detailRow = h('tr', { class: 'details', hidden: true },
    h('td', { colspan: 6 }, h('dl', { class: 'kv' },
      ...(e.planned ? [h('dt', { text: 'Scheduled reboot' }), h('dd', { text: plannedText(e) })] : []),
      ...details.flatMap(([key, v]) => [h('dt', { text: DETAIL_LABELS[key][0] }), h('dd', { text: DETAIL_LABELS[key][1](v) })]),
      h('dt', { text: 'Ended' }), h('dd', { text: e.ended_at == null ? 'still ongoing' : fmt.datetime(e.ended_at) })),
    routeSlot));
  const toggle = h('button', { class: 'btn ghost', type: 'button', 'aria-expanded': 'false', text: 'Details' });
  let routeLoaded = false;
  toggle.addEventListener('click', async () => {
    detailRow.hidden = !detailRow.hidden;
    toggle.setAttribute('aria-expanded', String(!detailRow.hidden));
    if (!detailRow.hidden && !routeLoaded && e.kind === 'outage') {
      routeLoaded = true;
      try {
        const [tr] = await getJSON(`/api/traces?event_id=${e.id}`);
        if (tr) fill(routeSlot, h('div', { style: { marginTop: '12px' } }, routeTable(tr.hops, `Route to ${tr.dst} when it started (${fmt.time(tr.ts)})`)));
      } catch { /* the route is optional detail */ }
    }
  });
  const instant = e.kind === 'isp_hop_change' || e.kind === 'ip_change';
  const duration = instant ? '–' : e.ended_at == null ? 'ongoing' : fmt.dur(e.ended_at - e.started_at);
  return [
    h('tr', {},
      h('td', { text: fmt.datetime(e.started_at) }),
      h('td', {}, h('span', { class: 'name-cell' }, statusIcon(k), k.label)),
      h('td', { class: 'secondary', text: where(e) }),
      h('td', { class: 'secondary', text: e.kind !== 'outage' ? '–' : e.planned && !overran(e) ? 'Scheduled reboot' : CAUSE[e.class] || '–' }),
      h('td', { class: 'num', text: duration }),
      h('td', { class: 'num' }, toggle)),
    detailRow,
  ];
}
