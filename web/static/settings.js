// Settings: what discovery found, the effective configuration and alerts.
import { h, getJSON, postJSON, fmt, loadTargets, colorOf, byRole, lineKey, ROLE, fill, targetAddress, routeTable } from './util.js';

export async function mount(root, ctx) {
  const [status, cfg, targets, ipHistory, traces] = await Promise.all([
    getJSON('/api/status'), getJSON('/api/config'), loadTargets(),
    getJSON('/api/public-ip').catch(() => []), getJSON('/api/traces').catch(() => [])]);
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
      card('Route to the internet', 'Each router between this server and the internet', routeView(info, traces))),

    h('div', { class: 'grid-2' },
      card('Targets', 'Colours match the charts', h('div', { class: 'table-wrap' }, h('table', {},
        h('thead', {}, h('tr', {}, h('th', { text: 'Target' }), h('th', { text: 'Role' }), h('th', { text: 'Address' }), h('th', { text: 'Status' }))),
        h('tbody', {}, ...byRole(targets).map(t => h('tr', {},
          h('td', {}, h('span', { class: 'name-cell' }, lineKey(colorOf(t)), t.name)),
          h('td', { class: 'secondary', text: ROLE[t.role] || t.role }),
          h('td', { class: 'secondary', text: targetAddress(t) }),
          h('td', { class: 'secondary', text: t.enabled ? 'Monitored' : 'History only' }))))))),
      card('Monitoring', 'Set with environment variables on the container', kv([
        ['Ping interval / timeout', `${cfg.ping_interval_s} s / ${cfg.ping_timeout_s} s`],
        ['Internet targets', cfg.ping_targets.join(', ')],
        ['Custom targets', cfg.custom_targets.length ? cfg.custom_targets.map(c => `${c.name} (${c.host})`).join(', ') : 'None'],
        ['DNS checks', cfg.dns_servers.length
          ? `${cfg.dns_servers.map(d => (d === 'system' ? `system (${info.dns_system || 'unknown'})` : d)).join(', ')} · looking up ${cfg.dns_query} every ${fmt.dur(cfg.dns_interval_s)}`
          : 'Off'],
        ['Web checks', cfg.http_targets.length ? `${cfg.http_targets.join(', ')} every ${fmt.dur(cfg.http_interval_s)}` : 'Off'],
        ['Public IP check', cfg.public_ip_interval_s ? `every ${fmt.dur(cfg.public_ip_interval_s)}` : 'Off'],
        ['Outage after', `${cfg.outage_threshold_s} s with no internet target answering`],
        ['Degraded when', `loss ≥ ${cfg.degraded_loss_pct}% or p95 > ${cfg.degraded_p95_ms} ms for ${fmt.dur(cfg.degraded_min_s)}`],
        ['Retention', `${cfg.retention_days} days`],
        ['Database size', fmt.bytes(status.db_size_bytes)],
        ['Version', status.version],
      ]))),

    card('Public IP address', 'The address the internet sees; a change usually means your ISP reconnected you', publicIPView(status.public_ip, ipHistory)),

    card('Discord alerts', cfg.alerts.discord ? 'Enabled' : 'Off. Set DISCORD_WEBHOOK_URL on the container to turn them on.', h('div', { style: { display: 'grid', gap: '12px' } },
      kv([
        ['Webhook configured', yes(cfg.alerts.discord)],
        ['Alert on outages longer than', fmt.dur(cfg.alerts.min_outage_s)],
        ['Batch repeated alerts within', fmt.dur(cfg.alerts.coalesce_s)],
        ['Alert on ISP route changes', yes(cfg.alerts.isp_hop_change)],
        ['Alert on public IP changes', yes(cfg.alerts.ip_change)],
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

// routeView prefers the latest discovery trace and falls back to the newest
// stored one (e.g. taken at the start of an outage).
function routeView(info, traces) {
  if (info.trace && info.trace.length) {
    const hops = info.trace.map(hp => ({ ttl: hp.ttl, address: hp.address, reached: hp.reached, rtt_ms: hp.rtt / 1e6 }));
    return routeTable(hops, `Traced at ${fmt.datetime(Date.parse(info.discovered_at) / 1000)}`);
  }
  if (traces.length) {
    const t = traces[0];
    return routeTable(t.hops, (t.reason === 'outage' ? 'Traced at the start of an outage, ' : 'Traced ') + fmt.datetime(t.ts));
  }
  return h('p', { class: 'muted', text: 'No traceroute yet. Routes are traced when the ISP edge router is auto-detected and at the start of each outage.' });
}

function publicIPView(cur, history) {
  if (!cur || (!cur.ipv4 && !cur.ipv6)) return h('p', { class: 'muted', text: 'Not checked yet (the first check runs shortly after start-up).' });
  const rows = [['IPv4', cur.ipv4 || '–']];
  if (cur.ipv6) rows.push(['IPv6', cur.ipv6]);
  rows.push(['Since', fmt.datetime(cur.ts)]);
  const older = history.filter(p => p.ts !== cur.ts).slice(0, 10);
  return h('div', { style: { display: 'grid', gap: '12px' } }, kv(rows),
    older.length ? h('div', {},
      h('div', { class: 'secondary', text: 'Previous addresses' }),
      h('ul', { class: 'list' }, ...older.map(p => h('li', {},
        h('span', { text: [p.ipv4, p.ipv6].filter(Boolean).join(' · ') }),
        h('span', { class: 'muted', text: 'from ' + fmt.datetime(p.ts) }))))) : null);
}
