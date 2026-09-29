// Router: one view at a time, chosen by the URL hash.
import { h, initTheme, fill, getJSON, setBrandState, displayState } from './util.js';
import * as dashboard from './dashboard.js';
import * as history from './history.js';
import * as events from './events.js';
import * as settings from './settings.js';
import * as speed from './speed.js';

const routes = { '': dashboard, history, events, speed, settings };
const view = document.getElementById('view');
let unmount = null;
let generation = 0;

async function route() {
  const name = location.hash.replace(/^#\/?/, '').split('?')[0];
  const mod = routes[name] || dashboard;
  document.querySelectorAll('#nav a').forEach(a => {
    if (a.dataset.route === (routes[name] ? name : '')) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
  if (unmount) { unmount(); unmount = null; }
  const gen = ++generation;
  // ctx.gone() lets a view stop work if the user navigated away mid-load.
  const ctx = { gone: () => gen !== generation };
  try {
    const cleanup = await mod.mount(view, ctx);
    if (ctx.gone()) { if (cleanup) cleanup(); return; }
    unmount = cleanup;
  } catch (err) {
    if (!ctx.gone()) fill(view, h('section', { class: 'card empty', text: 'Could not load this page: ' + err.message }));
  }
}

async function pollState() {
  try { setBrandState(displayState(await getJSON('/api/status'))); } catch { setBrandState('unknown'); }
}

initTheme(document.getElementById('themeBtn'));
window.addEventListener('hashchange', route);
route();
pollState();
setInterval(pollState, 10000);
