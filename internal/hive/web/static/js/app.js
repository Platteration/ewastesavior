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
let viewReady = false; // the current view has rendered
let firstRoute = true;
let routeHash = ''; // location.hash of the current view
// suspended is the view an ended session left, detached while the sign-in
// form is up ({frag, view, hash, title, scrollY}); signing in restores it.
let suspended = null;

// poll runs fn every ms until signal aborts, pausing while the tab is
// hidden. Repeated identical errors are reported once.
function poll(signal, fn, ms) {
  let lastErr = '';
  (async () => {
    for (;;) {
      await sleep(ms, signal);
      // Pause while the tab is hidden or the sign-in form is up.
      while (!signal.aborted && (document.hidden || !authed)) await sleep(1000, signal);
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
  if (suspended) suspended.view.abort();
  suspended = null;
}

function route() {
  if (!authed) return;
  if (location.hash === '#main') {
    // Only the skip link points here: keep the page, focus its content.
    window.history.replaceState(null, '', routeHash || '#/');
    if (view) {
      $('main').focus();
      return;
    }
  }
  routeHash = location.hash;
  stopView();
  const ctl = new AbortController();
  view = ctl;
  viewReady = false;
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
    Promise.resolve().then(() => r[1].apply(null, [main, ctx].concat(params))).then(() => {
      if (view === ctl) viewReady = true;
    }).catch((e) => {
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

// showLogin shows the sign-in form. With keep, the current view is kept
// for after the next sign-in.
function showLogin(msg, keep) {
  authed = false;
  const main = $('main');
  if (keep && view && viewReady && !suspended) {
    const frag = document.createDocumentFragment();
    while (main.firstChild) frag.appendChild(main.firstChild);
    suspended = {frag: frag, view: view, hash: location.hash, title: document.title, scrollY: window.pageYOffset};
    view = null;
  } else if (!keep || !suspended) {
    // Signed out on purpose, or the page hadn't finished loading.
    stopView();
  }
  $('topbar').hidden = true;
  document.title = 'Sign in · SaviorOS hive';
  // Pairing codes only: the admin token never goes into a browser (DESIGN 6.3).
  const code = h('input', {type: 'text', maxLength: 12, autocomplete: 'off', autocapitalize: 'characters', spellcheck: false,
    inputmode: 'text', placeholder: 'e.g. 7K3M9QXD', class: 'pair-input', required: true});
  const status = h('p', {class: 'login-status', role: 'alert'});
  if (msg) status.textContent = msg;
  const codeBtn = h('button', {type: 'submit', class: 'btn btn-primary'}, 'Sign in');
  const codeForm = h('form', {class: 'login-form', on: {submit: async (ev) => {
    ev.preventDefault();
    const c = normalizePairCode(code.value);
    if (!validPairCode(c)) {
      status.textContent = 'A pairing code has 8 letters and digits.';
      code.focus();
      return;
    }
    status.textContent = '';
    codeBtn.disabled = true;
    try {
      await api.post('/admin/session', {pair_code: c}, {auth: false});
      await start(true);
    } catch (e) {
      if (e.status === 401 || e.status === 403) {
        status.textContent = 'That code is not valid. Codes expire after 10 minutes and work only once.';
      } else if (e.status === 429) {
        status.textContent = 'Too many attempts from this address. Wait a minute, then try again.';
      } else {
        status.textContent = e.message || String(e);
      }
    } finally {
      codeBtn.disabled = false;
    }
  }}}, field('Pairing code', code, 'Shown on the hive\'s screen. On a hive without a screen, run "savior ctl pair" on a computer where savior ctl is logged in.'), codeBtn);
  mount(main, h('section', {class: 'panel login'},
    h('h1', null, 'Sign in to the hive'),
    h('p', null, 'Enter the pairing code shown on the hive\'s screen. Each code works once and lasts 10 minutes.'),
    suspended ? h('p', null, 'The page you were on is kept: after signing in you can carry on where you left off.') : null,
    codeForm,
    status));
  code.focus();
}

// resume restores the suspended view if the address is unchanged.
function resume() {
  const s = suspended;
  suspended = null;
  if (!s) return false;
  if (s.view.signal.aborted || s.hash !== location.hash) {
    s.view.abort();
    return false;
  }
  view = s.view;
  viewReady = true;
  routeHash = s.hash;
  const main = $('main');
  clear(main);
  main.appendChild(s.frag);
  document.title = s.title;
  window.scrollTo(0, s.scrollY || 0);
  main.focus({preventScroll: true});
  return true;
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
      showLogin(afterLogin ? 'The hive accepted the sign-in but this browser did not keep the session cookie. Use the https:// address and allow cookies for it.' : '', true);
      return;
    }
    if (view) view.abort();
    view = null;
    mount($('main'), h('section', {class: 'panel'}, h('h1', null, 'Cannot reach the hive'), h('p', {role: 'alert'}, e.message),
      btn('Try again', () => start(false), 'btn-primary')));
    return;
  }
  authed = true;
  $('topbar').hidden = false;
  $('hive-version').textContent = info && info.version ? info.version : '';
  if (!resume()) route();
}

// checkDownload: a download link to the API fails silently once the session
// has ended, so an API call goes first; a 401 there shows the sign-in form.
async function checkDownload(ev) {
  if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
  const a = ev.target && ev.target.closest ? ev.target.closest('a[download]') : null;
  if (!a || (a.getAttribute('href') || '').indexOf(api.BASE + '/') !== 0) return;
  if (a.dataset.checked) {
    delete a.dataset.checked; // checked just now: let the browser download
    return;
  }
  ev.preventDefault();
  try {
    await api.get('/admin/info', {timeout: 15000});
  } catch (e) {
    reportError(e);
    return;
  }
  a.dataset.checked = '1';
  a.click();
}

api.setUnauthorizedHandler(() => {
  if (!authed) return;
  toast('Your session ended. Sign in again to carry on.', 'error');
  showLogin('', true);
});
window.addEventListener('hashchange', route);
document.addEventListener('click', checkDownload);
// The skip link targets #main, which would otherwise be taken for a route.
const skip = document.querySelector('.skip');
if (skip) {
  skip.addEventListener('click', (ev) => {
    ev.preventDefault();
    $('main').focus();
  });
}
$('logout').addEventListener('click', signOut);
const boot = $('boot');
if (boot) boot.parentNode.removeChild(boot);
start(false);
