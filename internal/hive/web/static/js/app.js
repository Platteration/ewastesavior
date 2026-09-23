// Dashboard entry point: sign-in, hash router and view lifecycle.

import {h, mount, clear, toast, reportError, isAbort, field, btn, sleep} from './dom.js';
import * as api from './api.js';
import {normalizePairCode, validPairCode} from './model.js';
import * as overview from './overview.js';
import * as nodes from './nodes.js';
import * as jobs from './jobs.js';
import * as tasks from './tasks.js';
import * as walls from './walls.js';
import * as blobs from './blobs.js';
import * as settings from './settings.js';

const ROUTES = [
  [/^\/?$/, overview.render, '#/'],
  [/^\/nodes$/, nodes.list, '#/nodes'],
  [/^\/nodes\/([^/]+)$/, nodes.detail, '#/nodes'],
  [/^\/jobs$/, jobs.list, '#/jobs'],
  [/^\/jobs\/new$/, jobs.submit, '#/jobs'],
  [/^\/jobs\/([^/]+)$/, jobs.detail, '#/jobs'],
  [/^\/tasks\/([^/]+)$/, tasks.detail, '#/jobs'],
  [/^\/walls$/, walls.list, '#/walls'],
  [/^\/walls\/new$/, walls.edit, '#/walls'],
  [/^\/walls\/([^/]+)$/, walls.edit, '#/walls'],
  [/^\/blobs$/, blobs.render, '#/blobs'],
  [/^\/settings$/, settings.render, '#/settings'],
];

const $ = (id) => document.getElementById(id);
let authed = false;
let view = null; // AbortController of the current view
let firstRoute = true;

// poll runs fn every ms until signal aborts, pausing while the tab is
// hidden. Repeated identical errors are reported once.
function poll(signal, fn, ms) {
  let lastErr = '';
  (async () => {
    for (;;) {
      await sleep(ms, signal);
      while (!signal.aborted && document.hidden) await sleep(1000, signal);
      if (signal.aborted) return;
      try {
        await fn();
        lastErr = '';
      } catch (e) {
        if (isAbort(e) || e.handled) continue;
        if (e.message !== lastErr) reportError(e);
        lastErr = e.message;
      }
    }
  })();
}

function stopView() {
  if (view) view.abort();
  view = null;
}

function route() {
  if (!authed) return;
  stopView();
  const ctl = new AbortController();
  view = ctl;
  const main = $('main');
  clear(main);
  window.scrollTo(0, 0);
  if (!firstRoute) main.focus({preventScroll: true});
  firstRoute = false;
  const path = location.hash.replace(/^#/, '') || '/';
  const ctx = {
    signal: ctl.signal,
    title: (t) => { document.title = t + ' · SaviorOS hive'; },
    poll: (fn, ms) => poll(ctl.signal, fn, ms),
    go: (hash) => { location.hash = hash; },
    signOut: signOut,
    signedOut: (msg) => showLogin(msg),
  };
  for (const r of ROUTES) {
    const m = r[0].exec(path);
    if (!m) continue;
    let params;
    try {
      params = m.slice(1).map(decodeURIComponent);
    } catch (e) {
      break;
    }
    for (const a of document.querySelectorAll('#nav a')) {
      if (a.getAttribute('href') === r[2]) a.setAttribute('aria-current', 'page');
      else a.removeAttribute('aria-current');
    }
    Promise.resolve().then(() => r[1].apply(null, [main, ctx].concat(params))).catch((e) => {
      if (isAbort(e) || e.handled || ctl.signal.aborted) return;
      mount(main, h('section', {class: 'panel'}, h('h1', null, 'Could not load this page'),
        h('p', {role: 'alert'}, e.message || String(e)),
        h('div', {class: 'btn-row'}, btn('Try again', route, 'btn-primary'), h('a', {class: 'btn', href: '#/'}, 'Overview'))));
    });
    return;
  }
  ctx.title('Not found');
  mount(main, h('section', {class: 'panel'}, h('h1', null, 'Page not found'), h('p', null, h('a', {href: '#/'}, 'Go to the overview'))));
}

function showLogin(msg) {
  authed = false;
  stopView();
  $('topbar').hidden = true;
  document.title = 'Sign in · SaviorOS hive';
  const code = h('input', {type: 'text', maxLength: 12, autocomplete: 'off', autocapitalize: 'characters', spellcheck: false,
    inputmode: 'text', placeholder: 'e.g. 7K3M9QXD', class: 'pair-input', required: true});
  const token = h('input', {type: 'password', autocomplete: 'off', spellcheck: false});
  const status = h('p', {class: 'login-status', role: 'alert'});
  if (msg) status.textContent = msg;
  const submit = async (body, button) => {
    status.textContent = '';
    button.disabled = true;
    try {
      await api.post('/admin/session', body, {auth: false});
      await start(true);
    } catch (e) {
      if (e.status === 401 || e.status === 403) {
        status.textContent = body.pair_code ? 'That code is not valid. Codes expire after 10 minutes and work only once.' : 'That token is not valid.';
      } else if (e.status === 429) {
        status.textContent = 'Too many attempts from this address. Wait a minute, then try again.';
      } else {
        status.textContent = e.message || String(e);
      }
    } finally {
      button.disabled = false;
    }
  };
  const codeBtn = h('button', {type: 'submit', class: 'btn btn-primary'}, 'Sign in');
  const codeForm = h('form', {class: 'login-form', on: {submit: (ev) => {
    ev.preventDefault();
    const c = normalizePairCode(code.value);
    if (!validPairCode(c)) {
      status.textContent = 'A pairing code has 8 letters and digits.';
      code.focus();
      return;
    }
    submit({pair_code: c}, codeBtn);
  }}}, field('Pairing code', code, 'Shown on the hive\'s screen, or run "savior ctl pair".'), codeBtn);
  const tokenBtn = h('button', {type: 'submit', class: 'btn'}, 'Sign in with token');
  const tokenForm = h('form', {class: 'login-form', on: {submit: (ev) => {
    ev.preventDefault();
    const t = token.value.trim();
    if (!t) {
      token.focus();
      return;
    }
    submit({token: t}, tokenBtn);
  }}}, field('Admin token', token, 'From admin_token in savior.conf, or the admin_token file in the hive\'s data directory.'), tokenBtn);
  mount($('main'), h('section', {class: 'panel login'},
    h('h1', null, 'Sign in to the hive'),
    h('p', null, 'Enter the pairing code shown on the hive\'s screen. Each code works once and lasts 10 minutes.'),
    codeForm,
    h('details', null, h('summary', null, 'Advanced: use the admin token'), tokenForm),
    status));
  code.focus();
}

async function signOut() {
  try {
    await api.post('/admin/logout', {}, {auth: false});
  } catch (e) {
    if (e.status !== 401) {
      reportError(e);
      return;
    }
  }
  showLogin('Signed out.');
}

async function start(afterLogin) {
  let info;
  try {
    info = await api.get('/admin/info', {auth: false});
  } catch (e) {
    if (e.status === 401) {
      showLogin(afterLogin ? 'The hive accepted the sign-in but this browser did not keep the session cookie. Use the https:// address and allow cookies for it.' : '');
      return;
    }
    stopView();
    mount($('main'), h('section', {class: 'panel'}, h('h1', null, 'Cannot reach the hive'), h('p', {role: 'alert'}, e.message),
      btn('Try again', () => start(false), 'btn-primary')));
    return;
  }
  authed = true;
  $('topbar').hidden = false;
  $('hive-version').textContent = info && info.version ? info.version : '';
  route();
}

api.setUnauthorizedHandler(() => {
  if (!authed) return;
  toast('Your session ended. Sign in again.', 'error');
  showLogin();
});
window.addEventListener('hashchange', route);
$('logout').addEventListener('click', signOut);
const boot = $('boot');
if (boot) boot.parentNode.removeChild(boot);
start(false);
