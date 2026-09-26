// uPlot wrapper with the house style: 2px lines, hairline grid, a crosshair
// tooltip listing every series, outage shading and drag-to-zoom.
import { h, cssVar, fmt, fill } from './util.js';

const FONT = '12px system-ui, -apple-system, "Segoe UI", sans-serif';

export function alpha(color, a) {
  const m = /^#([0-9a-f]{6})$/i.exec(color);
  if (!m) return color;
  const n = parseInt(m[1], 16);
  return `rgba(${n >> 16}, ${(n >> 8) & 255}, ${n & 255}, ${a})`;
}

function tooltipPlugin(yFmt) {
  let tip;
  return {
    hooks: {
      init(u) {
        tip = h('div', { class: 'tip', hidden: true });
        u.over.append(tip);
        u.over.addEventListener('mouseleave', () => { tip.hidden = true; });
      },
      setCursor(u) {
        const i = u.cursor.idx;
        if (i == null || u.cursor.left < 0) { tip.hidden = true; return; }
        const rows = [h('div', { class: 'when', text: fmt.datetime(u.data[0][i]) })];
        u.series.forEach((s, si) => {
          if (si === 0 || !s.show || s.tipHide) return;
          const v = u.data[si][i];
          rows.push(h('div', { class: 'row' },
            h('span', { class: 'key-line', style: { background: s._color } }),
            h('strong', { text: v == null ? '–' : yFmt(v) }),
            h('span', { text: s.label })));
        });
        fill(tip, ...rows);
        tip.hidden = false;
        const w = tip.offsetWidth, W = u.over.clientWidth;
        let left = u.cursor.left + 14;
        if (left + w > W) left = Math.max(0, u.cursor.left - w - 14);
        tip.style.left = left + 'px';
        tip.style.top = '4px';
      },
    },
  };
}

// Shades [start, end) intervals behind the series (outages, degradations).
function shadePlugin(getIntervals) {
  return {
    hooks: {
      drawClear(u) {
        const ivs = getIntervals();
        if (!ivs.length) return;
        const { ctx } = u;
        const { left, top, width, height } = u.bbox;
        const minW = 3 * devicePixelRatio; // short events stay visible on long ranges
        ctx.save();
        for (const iv of ivs) {
          let x0 = u.valToPos(iv.start, 'x', true);
          let x1 = u.valToPos(iv.end, 'x', true);
          x0 = Math.max(left, x0);
          x1 = Math.min(left + width, x1);
          if (x1 < left || x0 > left + width) continue;
          ctx.fillStyle = iv.color;
          ctx.fillRect(x0, top, Math.max(x1 - x0, minW), height);
        }
        ctx.restore();
      },
    },
  };
}

// timeChart draws a time-series chart into el.
//   series: [{ label, color, width?, bars?, fillAlpha?, tipHide? }]
//   data:   [xs, ...ys] (unix seconds; null = gap)
//   yFmt:   value formatter for axis and tooltip
//   yMin:   lower bound of the y range (default 0)
//   yMaxAtLeast: keeps tiny values from filling the whole height
//   bands:  [[upperIdx, lowerIdx, color]] (1-based series indexes)
//   shade:  () => [{ start, end, color }]
//   sync:   cursor sync key; onZoom(from, to) enables drag-to-zoom
export function timeChart(el, opts) {
  const height = opts.height || 220;
  const surface = cssVar('--surface-1');
  const muted = cssVar('--muted');
  const grid = cssVar('--grid');
  const yFmt = opts.yFmt || (v => String(v));

  const series = [{}];
  for (const s of opts.series) {
    const def = {
      label: s.label,
      stroke: s.color,
      width: s.width || 2,
      points: { show: false },
      spanGaps: false,
      _color: s.color,
      tipHide: s.tipHide,
    };
    if (s.bars) {
      def.paths = uPlot.paths.bars({ size: [0.7, 24], align: 1 }); // a bucket starts at its timestamp
      def.fill = s.color;
      def.width = 0;
    } else if (s.fillAlpha) {
      def.fill = alpha(s.color, s.fillAlpha);
    }
    series.push(def);
  }

  const plugins = [tooltipPlugin(yFmt)];
  if (opts.shade) plugins.push(shadePlugin(opts.shade));

  const hooks = {};
  if (opts.onZoom) {
    hooks.setSelect = [u => {
      if (u.select.width > 4) {
        const from = Math.floor(u.posToVal(u.select.left, 'x'));
        const to = Math.ceil(u.posToVal(u.select.left + u.select.width, 'x'));
        opts.onZoom(from, to);
      }
      u.setSelect({ left: 0, top: 0, width: 0, height: 0 }, false);
    }];
  }

  const yMin = opts.yMin ?? 0;
  const uopts = {
    width: el.clientWidth || 600,
    height,
    series,
    bands: (opts.bands || []).map(([hi, lo, color]) => ({ series: [hi, lo], fill: color })),
    legend: { show: false },
    padding: [8, 8, 0, 0],
    scales: {
      x: { time: true },
      y: {
        range: (u, dmin, dmax) => {
          const top = Math.max(dmax ?? 0, opts.yMaxAtLeast ?? 1);
          return [yMin, Math.min(top * 1.15, opts.yMax ?? Infinity)];
        },
      },
    },
    axes: [
      { stroke: muted, font: FONT, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1, size: 4 }, space: 70 },
      {
        stroke: muted, font: FONT, grid: { stroke: grid, width: 1 }, ticks: { show: false },
        size: 56, space: 36, values: (u, vals) => vals.map(yFmt),
      },
    ],
    cursor: {
      y: false, // vertical crosshair only
      drag: { x: !!opts.onZoom, y: false, setScale: false },
      sync: opts.sync ? { key: opts.sync } : undefined,
      points: {
        size: 9, width: 2,
        fill: (u, si) => u.series[si]._color,
        stroke: () => surface, // surface ring keeps the dot legible over lines
      },
    },
    hooks,
    plugins,
  };

  const u = new uPlot(uopts, opts.data, el);
  const ro = new ResizeObserver(() => {
    const w = el.clientWidth;
    if (w && Math.abs(w - u.width) > 1) u.setSize({ width: w, height });
  });
  ro.observe(el);

  return {
    u,
    setData(data) { u.setData(data); },
    redraw() { u.redraw(false, true); },
    destroy() { ro.disconnect(); u.destroy(); },
  };
}

// legend renders a legend row; clicking an item toggles that series.
export function legend(items, chart) {
  const row = h('div', { class: 'legend' });
  items.forEach((it, i) => {
    const key = it.kind === 'rect'
      ? h('span', { class: 'key-rect', style: { background: it.color } })
      : h('span', { class: 'key-line', style: { background: it.color } });
    if (!chart || it.static) {
      row.append(h('span', { class: 'name-cell' }, key, it.label));
      return;
    }
    const btn = h('button', { type: 'button', 'aria-pressed': 'true' }, key, it.label);
    btn.addEventListener('click', () => {
      const c = chart();
      if (!c) return;
      const show = btn.getAttribute('aria-pressed') !== 'true';
      btn.setAttribute('aria-pressed', String(show));
      c.u.setSeries(i + 1, { show });
    });
    row.append(btn);
  });
  return row;
}

// sparkline returns a small SVG trend line; null values break the line.
export function sparkline(values, color, width = 160, height = 32) {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('width', '100%');
  svg.setAttribute('height', height);
  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);
  svg.setAttribute('preserveAspectRatio', 'none'); // stretch to the tile's width
  svg.setAttribute('aria-hidden', 'true');
  const vals = values.filter(v => v != null);
  if (vals.length < 2) return svg;
  const max = Math.max(...vals), min = Math.min(...vals);
  const span = max - min || 1;
  const pad = 3;
  const x = i => pad + (i / (values.length - 1)) * (width - 2 * pad);
  const y = v => height - pad - ((v - min) / span) * (height - 2 * pad);
  let d = '', pen = false;
  values.forEach((v, i) => {
    if (v == null) { pen = false; return; }
    d += (pen ? 'L' : 'M') + x(i).toFixed(1) + ' ' + y(v).toFixed(1);
    pen = true;
  });
  const path = document.createElementNS(ns, 'path');
  path.setAttribute('d', d);
  path.setAttribute('fill', 'none');
  path.setAttribute('stroke', color);
  path.setAttribute('stroke-width', '1.5');
  path.setAttribute('stroke-linejoin', 'round');
  path.setAttribute('stroke-linecap', 'round');
  path.setAttribute('vector-effect', 'non-scaling-stroke');
  svg.append(path);
  return svg;
}
