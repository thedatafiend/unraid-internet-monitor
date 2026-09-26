// Settings: what discovery found, the effective configuration and alerts.
import { h, getJSON, postJSON, fmt, loadTargets, colorOf, byRole, lineKey, ROLE, fill } from './util.js';

export async function mount(root, ctx) {
  const [status, cfg, targets] = await Promise.all([getJSON('/api/status'), getJSON('/api/config'), loadTargets()]);
  if (ctx.gone()) return () => {};
  const info = status.info;
  const yes = b => (b ? 'Yes' : 'No');
  const src = (v, s) => (v ? `${v} (${s === 'auto' ? 'detected' : 'configured'})` : 'Not monitored');

  const testResult = h('span', { class: 'secondary' });
  const testBtn = h('button', { class: 'btn', type: 'button', text: 'Send test alert', disabled: !cfg.alerts.discord });
  testBtn.addEventListener('click', async () => {
    testBtn.disabled = true;
    try { await postJSON('/api/alerts/test'); testResult.textContent = 'Queued. Check your Discord channel.'; }
    catch (err) { testResult.textContent = 'Failed: ' + err.message; }
    finally { testBtn.disabled = false; }
  });

  const warnings = info.warnings || [];
  fill(root, 
    warnings.length ? h('section', { class: 'callout' }, h('strong', { text: 'Check your setup' }),
      h('ul', {}, ...warnings.map(w => h('li', { text: w })))) : null,

    h('div', { class: 'grid-2' },
      card('Connection path', 'How this server reaches the internet', kv([
        ['Outgoing interface', info.egress_iface || '–'],
        ['Source address', info.egress_src || '–'],
        ['Routing table', info.egress_table || '–'],
        ['Tunnel in the path', info.tunnel || 'None'],
        ['Router', src(info.gateway, info.gateway_source)],
        ['ISP edge router', src(info.isp_hop, info.isp_hop_source)],
        ['IPv6', info.ipv6_available ? 'Available' : `Not available (mode: ${info.ipv6_mode})`],
        ['ICMP sockets', `IPv4 ${info.socket_v4}, IPv6 ${info.socket_v6}`],
        ['Last discovery', fmt.datetime(Date.parse(info.discovered_at) / 1000)],
      ])),
      card('Route to the internet', 'Traceroute used to find your ISP edge router', traceTable(info.trace || []))),

    h('div', { class: 'grid-2' },
      card('Targets', 'Colours match the charts', h('div', { class: 'table-wrap' }, h('table', {},
        h('thead', {}, h('tr', {}, h('th', { text: 'Target' }), h('th', { text: 'Role' }), h('th', { text: 'Address' }), h('th', { text: 'Status' }))),
        h('tbody', {}, ...byRole(targets).map(t => h('tr', {},
          h('td', {}, h('span', { class: 'name-cell' }, lineKey(colorOf(t)), t.name)),
          h('td', { class: 'secondary', text: ROLE[t.role] || t.role }),
          h('td', { class: 'secondary', text: t.address }),
          h('td', { class: 'secondary', text: t.enabled ? 'Monitored' : 'History only' }))))))),
      card('Monitoring', 'Set with environment variables on the container', kv([
        ['Ping interval / timeout', `${cfg.ping_interval_s} s / ${cfg.ping_timeout_s} s`],
        ['Internet targets', cfg.ping_targets.join(', ')],
        ['Custom targets', cfg.custom_targets.length ? cfg.custom_targets.map(c => `${c.name} (${c.host})`).join(', ') : 'None'],
        ['Outage after', `${cfg.outage_threshold_s} s with no internet target answering`],
        ['Degraded when', `loss ≥ ${cfg.degraded_loss_pct}% or p95 > ${cfg.degraded_p95_ms} ms for ${fmt.dur(cfg.degraded_min_s)}`],
        ['Retention', `${cfg.retention_days} days`],
        ['Database size', fmt.bytes(status.db_size_bytes)],
        ['Version', status.version],
      ]))),

    card('Discord alerts', cfg.alerts.discord ? 'Enabled' : 'Off. Set DISCORD_WEBHOOK_URL on the container to turn them on.', h('div', { style: { display: 'grid', gap: '12px' } },
      kv([
        ['Webhook configured', yes(cfg.alerts.discord)],
        ['Alert on outages longer than', fmt.dur(cfg.alerts.min_outage_s)],
        ['Batch repeated alerts within', fmt.dur(cfg.alerts.coalesce_s)],
        ['Alert on ISP route changes', yes(cfg.alerts.isp_hop_change)],
      ]),
      h('div', { class: 'filters' }, testBtn, testResult))),
  );
  return () => {};
}

function card(title, subtitle, content) {
  return h('section', { class: 'card' },
    h('div', { class: 'card-head' }, h('h2', { text: title }), subtitle ? h('p', { text: subtitle }) : null),
    content);
}

function kv(pairs) {
  return h('dl', { class: 'kv' }, ...pairs.flatMap(([k, v]) => [h('dt', { text: k }), h('dd', { text: v })]));
}

function traceTable(hops) {
  if (!hops.length) return h('p', { class: 'muted', text: 'No traceroute yet (ISP hop detection is off or unsupported).' });
  return h('div', { class: 'table-wrap' }, h('table', {},
    h('thead', {}, h('tr', {}, h('th', { class: 'num', text: 'Hop' }), h('th', { text: 'Address' }), h('th', { class: 'num', text: 'Round trip' }))),
    h('tbody', {}, ...hops.map(hp => h('tr', {},
      h('td', { class: 'num', text: String(hp.ttl) }),
      h('td', { text: hp.address === 'invalid IP' || !hp.address ? 'no reply' : hp.address + (hp.reached ? ' (destination)' : '') }),
      h('td', { class: 'num', text: hp.rtt ? fmt.ms(hp.rtt / 1e6) : '–' }))))));
}
