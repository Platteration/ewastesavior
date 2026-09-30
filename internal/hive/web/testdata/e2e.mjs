// Browser test of the dashboard, run by TestDashboardE2E (e2e_test.go)
// against a real hive with real node agents. It uses playwright-core and
// headless Chromium. Exit status 77 means a prerequisite is missing.
//
// Environment (set by the Go test): E2E_HIVE (https://127.0.0.1:port),
// E2E_ADMIN_TOKEN (for API checks and pairing codes, never typed into the
// page), E2E_NODES (two approved node IDs), E2E_PENDING (a node waiting for
// approval), E2E_OUT (screenshots of a failed step, and of every route
// with E2E_SHOTS=1). Optional:
// SAVIOR_PLAYWRIGHT_DIR (a directory whose node_modules has playwright-core)
// and SAVIOR_E2E_CHROMIUM (a Chromium or Chrome executable).
//
// It checks, with no console errors, page errors, CSP violations or
// unexpected API errors along the way:
//   - sign-in with a pairing code; the page never asks for the admin token
//   - the skip link keeps the route
//   - keyboard focus on the nodes list survives a live refresh and a sort
//   - a pending node shows "pending" in Status; at 800x600 and 1024x768
//     the nodes table fits and no page scrolls sideways
//   - a node page opened by name keeps working after a rename; labels,
//     drain and display settings round-trip to the API
//   - Overview's savior.conf download
//   - a job submitted with the form runs; a session revoked while the form
//     is filled in asks to sign in again and keeps the form
//   - zip, output and log downloads; after a revoke a download asks to sign
//     in again instead of failing silently
//   - the log viewer shows the final attempt of a retried task
//   - a wall is created, edited and deleted
//   - every route renders

import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
import fs from 'node:fs';
import path from 'node:path';

const SKIP = 77;

function loadPlaywright() {
  const bases = [process.env.SAVIOR_PLAYWRIGHT_DIR, process.cwd(), path.join(path.dirname(process.execPath), '..', 'lib')]
    .concat((process.env.NODE_PATH || '').split(path.delimiter).map((p) => p && path.dirname(p)))
    .filter(Boolean);
  for (const b of bases) {
    const req = createRequire(path.join(path.resolve(b), 'noop.js'));
    for (const name of ['playwright-core', 'playwright']) {
      try {
        return req(name);
      } catch (e) { /* try the next one */ }
    }
  }
  return null;
}

const pw = loadPlaywright();
if (!pw) {
  console.log('playwright-core not found (npm install playwright-core, or set SAVIOR_PLAYWRIGHT_DIR)');
  process.exit(SKIP);
}

const HIVE = process.env.E2E_HIVE;
const TOKEN = process.env.E2E_ADMIN_TOKEN;
const NODES = (process.env.E2E_NODES || '').split(',').filter(Boolean);
const PENDING = process.env.E2E_PENDING;
const OUT = process.env.E2E_OUT || '.';
assert.ok(HIVE && TOKEN && NODES.length === 2 && PENDING, 'E2E_HIVE, E2E_ADMIN_TOKEN, E2E_NODES and E2E_PENDING must be set');

let browser;
try {
  browser = await pw.chromium.launch({executablePath: process.env.SAVIOR_E2E_CHROMIUM || undefined, args: ['--no-sandbox']});
} catch (e) {
  console.log('cannot launch Chromium (npx playwright install chromium, or set SAVIOR_E2E_CHROMIUM): ' + String(e.message).split('\n')[0]);
  process.exit(SKIP);
}

// --- API side (bearer admin token, like a script) ---------------------------
const apiCtx = await pw.request.newContext({baseURL: HIVE, ignoreHTTPSErrors: true, extraHTTPHeaders: {Authorization: 'Bearer ' + TOKEN}});
async function api(method, p, body) {
  const r = await apiCtx.fetch('/api/v1/admin/' + p, {method: method, data: body});
  const text = await r.text();
  if (!r.ok()) throw new Error(method + ' ' + p + ': ' + r.status() + ' ' + text);
  return text ? JSON.parse(text) : null;
}
const pairCode = async () => (await api('POST', 'pair', {})).code;
const nodeView = (id) => api('GET', 'nodes/' + encodeURIComponent(id));

async function until(what, fn, ms) {
  const deadline = Date.now() + (ms || 30000);
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() > deadline) throw new Error('timed out waiting for ' + what);
    await new Promise((r) => setTimeout(r, 250));
  }
}

// --- Browser side -----------------------------------------------------------
const context = await browser.newContext({ignoreHTTPSErrors: true, viewport: {width: 1024, height: 768}, acceptDownloads: true});
await context.addInitScript(() => {
  document.addEventListener('securitypolicyviolation', (e) => console.error('CSP violation: ' + e.violatedDirective + ' ' + e.blockedURI));
});
const page = await context.newPage();
page.setDefaultTimeout(15000);
page.on('dialog', (d) => d.accept());

const problems = [];
let allowed = []; // regexps for API errors a step expects, e.g. /^401 /
page.on('console', (m) => {
  // Failed requests are also logged to the console; the response hook
  // below decides whether those were expected.
  if (m.type() === 'error' && !/^Failed to load resource/.test(m.text())) problems.push('console error: ' + m.text());
});
page.on('pageerror', (e) => problems.push('page error: ' + e.message));
page.on('response', (r) => {
  const u = new URL(r.url());
  if (!u.pathname.startsWith('/api/') || r.status() < 400) return;
  const s = r.status() + ' ' + r.request().method() + ' ' + u.pathname;
  if (!allowed.some((re) => re.test(s))) problems.push('unexpected API error: ' + s);
});

// E2E_KEEP_GOING=1 runs the later steps after a failure (to see everything
// an old dashboard gets wrong); the run still fails.
const keepGoing = process.env.E2E_KEEP_GOING === '1';
let stepNo = 0;
const failed = [];
async function step(name, fn) {
  stepNo++;
  const t0 = Date.now();
  console.log('--- ' + name);
  try {
    await fn();
    if (problems.length) throw new Error(problems.join('\n'));
  } catch (e) {
    const shot = path.join(OUT, 'step-' + stepNo + '.png');
    await page.screenshot({path: shot, fullPage: true}).catch(() => {});
    console.log('FAILED: ' + name + ' (screenshot ' + shot + ', URL ' + page.url() + ')');
    if (!keepGoing) throw e;
    console.log('    ' + String(e && e.message).split('\n').slice(0, 3).join('\n    '));
    failed.push(name);
    problems.length = 0;
    return;
  } finally {
    allowed = [];
  }
  console.log('    ok (' + (Date.now() - t0) + ' ms)');
}

const h1 = () => page.locator('main h1').first();
const SIGN_IN = 'Sign in to the hive';
// waitH1 waits for a page by its heading. With E2E_KEEP_GOING, a sign-in
// form that a failed step left behind is answered on the way.
async function waitH1(text, ms) {
  const deadline = Date.now() + (ms || 15000);
  for (;;) {
    const cur = await page.evaluate(() => {
      const e = document.querySelector('main h1');
      return e ? e.textContent : '';
    });
    if (cur.indexOf(text) >= 0) return;
    if (keepGoing && cur === SIGN_IN && text !== SIGN_IN) {
      await signIn();
      continue;
    }
    if (Date.now() > deadline) throw new Error('timed out waiting for the page "' + text + '"; showing "' + cur + '"');
    await page.waitForTimeout(100);
  }
}
async function go(hash, title) {
  await page.evaluate((x) => { location.hash = x; }, hash);
  if (title) await waitH1(title);
}
const toast = (text) => page.locator('.toast', {hasText: text}).first().waitFor();
// focused describes the focused element: its href, else its text.
const focused = () => page.evaluate(() => {
  const e = document.activeElement;
  return !e || e === document.body ? 'BODY' : e.getAttribute('href') || e.textContent.trim();
});
const sortButton = (label) => page.locator('main th button', {hasText: label}).first();
const sortOf = (label) => page.evaluate((l) => {
  const b = Array.prototype.filter.call(document.querySelectorAll('main th button'), (x) => x.textContent.indexOf(l) === 0)[0];
  return b ? b.closest('th').getAttribute('aria-sort') : null;
}, label);
async function signIn() {
  await waitH1(SIGN_IN);
  await page.locator('.pair-input').fill(await pairCode());
  await page.keyboard.press('Enter');
}
async function download(click) {
  const [dl] = await Promise.all([page.waitForEvent('download'), click()]);
  const failure = await dl.failure();
  assert.equal(failure, null, 'download of ' + dl.suggestedFilename() + ' failed');
  return {name: dl.suggestedFilename(), body: fs.readFileSync(await dl.path())};
}
// noHorizontalOverflow checks the page and the nodes table at this size.
async function noHorizontalOverflow(where) {
  const r = await page.evaluate(() => ({doc: document.documentElement.scrollWidth, win: window.innerWidth}));
  assert.ok(r.doc <= r.win, where + ': the page scrolls sideways (' + r.doc + ' > ' + r.win + ')');
}

try {
  await step('sign in with a pairing code; no admin token field (WEB-5)', async () => {
    allowed = [/^401 GET \/api\/v1\/admin\/info$/];
    await page.goto(HIVE + '/');
    await waitH1(SIGN_IN);
    const passwords = await page.locator('input[type=password]').count();
    const text = (await page.locator('main').textContent()).toLowerCase();
    await signIn();
    await waitH1('Overview');
    assert.equal(await page.locator('#topbar').isVisible(), true);
    assert.equal(passwords, 0, 'the sign-in page has a password field');
    assert.ok(text.indexOf('admin token') < 0, 'the sign-in page mentions the admin token');
  });

  await step('the skip link keeps the route (WEB-4)', async () => {
    await go('#/nodes', 'Nodes');
    await page.reload();
    await waitH1('Nodes');
    await page.keyboard.press('Tab');
    assert.equal(await page.evaluate(() => document.activeElement.className), 'skip');
    await page.keyboard.press('Enter');
    await page.waitForTimeout(300);
    const url = page.url();
    const title = await h1().textContent();
    const active = await page.evaluate(() => document.activeElement.id);
    if (!url.endsWith('#/nodes')) await go('#/nodes', 'Nodes');
    assert.ok(url.endsWith('#/nodes'), 'URL changed to ' + url);
    assert.equal(title, 'Nodes');
    assert.equal(active, 'main');
  });

  await step('focus on the nodes list survives a refresh and a sort (WEB-2)', async () => {
    const href = '#/nodes/' + NODES[1];
    await page.locator('main a[href="' + href + '"]').focus();
    await page.evaluate(() => { window.e2eWrap = document.querySelector('main .table-wrap'); });
    // Change what the list shows so that the next poll redraws it.
    await api('PATCH', 'nodes/' + NODES[0], {name: 'e2e-node-1b'});
    await page.waitForFunction(() => document.querySelector('main .table-wrap') !== window.e2eWrap, null, {timeout: 12000});
    assert.equal(await focused(), href, 'focus lost by the refresh');
    await sortButton('Name').focus();
    await page.keyboard.press('Enter');
    await until('the list sorted by name', async () => (await sortOf('Name')) === 'ascending', 5000);
    assert.equal(await focused(), 'Name ▲', 'focus lost by sorting');
    await page.keyboard.press('Enter');
    await until('the list sorted by name, descending', async () => (await sortOf('Name')) === 'descending', 5000);
    assert.equal(await focused(), 'Name ▼', 'focus lost by sorting again');
    await sortButton('Status').click();
  });

  await step('pending status and table fit at 800x600 and 1024x768 (WEB-3)', async () => {
    const row = page.locator('tr', {hasText: 'e2e-new-box'});
    assert.equal((await row.locator('td').nth(2).textContent()).trim(), 'pending', 'Status of a node waiting for approval');
    assert.equal((await page.locator('tr', {hasText: 'e2e-node-2'}).locator('td').nth(2).textContent()).trim(), 'idle', 'an online node');
    assert.ok((await page.locator('main p[role=status]').textContent()).indexOf('3 nodes · 3 online') === 0, 'nodes summary');
    const wrap = page.locator('main .table-wrap');
    assert.equal(await wrap.getAttribute('role'), 'region');
    assert.equal(await wrap.getAttribute('tabindex'), '0');
    for (const vp of [{width: 800, height: 600}, {width: 1024, height: 768}]) {
      await page.setViewportSize(vp);
      await page.waitForTimeout(200);
      const fit = await page.evaluate(() => {
        const w = document.querySelector('main .table-wrap');
        const notes = Array.prototype.filter.call(w.querySelectorAll('th'), (th) => th.textContent.indexOf('Notes') === 0)[0];
        return {table: w.firstChild.scrollWidth, wrap: w.clientWidth, notesRight: notes.getBoundingClientRect().right, wrapRight: w.getBoundingClientRect().right};
      });
      assert.ok(fit.table <= fit.wrap, vp.width + 'x' + vp.height + ': the nodes table is ' + fit.table + ' px wide in ' + fit.wrap);
      assert.ok(fit.notesRight <= fit.wrapRight, vp.width + 'x' + vp.height + ': Notes is cut off');
      await noHorizontalOverflow('#/nodes at ' + vp.width);
    }
  });

  await step('node page by name: rename, labels, drain, display (WEB-8)', async () => {
    await go('#/nodes/e2e-node-1b', 'e2e-node-1b');
    await page.getByLabel('Name', {exact: true}).fill('e2e-renamed');
    await page.getByRole('button', {name: 'Rename'}).click();
    await toast('Renamed to e2e-renamed.');
    await waitH1('e2e-renamed');
    assert.equal((await nodeView(NODES[0])).name, 'e2e-renamed');
    assert.equal(await page.evaluate(() => location.hash), '#/nodes/' + NODES[0], 'the address bar keeps the old name');
    await page.getByLabel('Admin labels').fill('room=lab-2');
    await page.getByRole('button', {name: 'Save labels'}).click();
    await toast('Labels saved.');
    assert.deepEqual((await nodeView(NODES[0])).admin_labels, {room: 'lab-2'});
    await page.getByRole('button', {name: 'Drain', exact: true}).click();
    await page.getByRole('button', {name: 'Stop draining'}).waitFor();
    assert.equal((await nodeView(NODES[0])).drain, true);
    await page.getByRole('button', {name: 'Stop draining'}).click();
    await page.getByRole('button', {name: 'Drain', exact: true}).waitFor();
    assert.equal((await nodeView(NODES[0])).drain, false);
    await page.getByLabel('Show').selectOption('text');
    await page.getByLabel('Text', {exact: true}).fill('hello from e2e');
    await page.getByRole('button', {name: 'Save display'}).click();
    await toast('Display saved.');
    const d = (await nodeView(NODES[0])).display;
    assert.equal(d.mode, 'text');
    assert.equal(d.text, 'hello from e2e');
  });

  await step('Overview: download savior.conf', async () => {
    await go('#/', 'Overview');
    const f = await download(() => page.getByRole('button', {name: 'Download savior.conf for new nodes'}).click());
    const conf = f.body.toString();
    for (const want of ['swarm_key =', 'hive_fingerprint = sha256:', 'hive =']) assert.ok(conf.indexOf(want) >= 0, 'savior.conf lacks ' + want);
  });

  let jobID = '';
  let retryTask = '';
  const fillJob = async () => {
    await page.getByLabel('Name (optional)').fill('e2e-form');
    await page.getByLabel('Type').selectOption('script');
    await page.getByLabel('Script', {exact: true}).fill('echo "log line"\necho hi > o.txt\n');
    await page.getByLabel('Cores per task').fill('0.5');
    await page.getByLabel('Memory per task (MB)').fill('48');
    await page.getByLabel('Disk per task (MB)').fill('16');
    await page.getByLabel('Also run on nodes without full isolation (isolation=any)').check();
    await page.getByLabel('Outputs to collect').fill('o.txt');
  };
  await step('job form survives a revoked session and the job runs (WEB-6)', async () => {
    await go('#/jobs/new', 'Submit a job');
    await fillJob();
    allowed = [/^401 /];
    await api('POST', 'sessions/revoke', {});
    await page.getByRole('button', {name: 'Submit job'}).click();
    await toast('Your session ended');
    await signIn();
    await waitH1('Submit a job');
    const kept = [await page.getByLabel('Name (optional)').inputValue(), await page.getByLabel('Outputs to collect').inputValue()];
    if (keepGoing && kept[0] !== 'e2e-form') await fillJob();
    await page.getByRole('button', {name: 'Submit job'}).click();
    await page.waitForFunction(() => /^#\/jobs\/j/.test(location.hash));
    jobID = decodeURIComponent(page.url().split('#/jobs/')[1]);
    await page.locator('main .page-head .badge', {hasText: 'succeeded'}).waitFor({timeout: 60000});
    assert.deepEqual(kept, ['e2e-form', 'o.txt'], 'the job form lost its input');
  });

  await step('downloads, and a download after a revoke asks to sign in (WEB-7)', async () => {
    const zip = await download(() => page.getByText('Download all outputs (zip)').click());
    assert.equal(zip.body.subarray(0, 2).toString(), 'PK', 'zip');
    if (zip.body.indexOf('task-0/o.txt') < 0) {
      const tid = (await api('GET', 'jobs/' + jobID + '/tasks')).tasks[0].id;
      const lr = await apiCtx.fetch('/api/v1/admin/tasks/' + tid + '/log?offset=0');
      assert.fail('the zip lacks task-0/o.txt: ' + zip.body.length + ' bytes; task log: ' + JSON.stringify(await lr.text()));
    }

    allowed = [/^401 /];
    await api('POST', 'sessions/revoke', {});
    let downloads = 0;
    const count = () => downloads++;
    page.on('download', count);
    await page.getByText('Download all outputs (zip)').click();
    let asked = true;
    try {
      await waitH1(SIGN_IN, 5000);
    } catch (e) {
      asked = false;
      // The next API call shows the sign-in form.
      await page.evaluate(() => { location.hash = '#/jobs'; });
      await page.waitForTimeout(300);
      await page.evaluate((id) => { location.hash = '#/jobs/' + id; }, jobID);
      await waitH1(SIGN_IN);
    }
    await page.waitForTimeout(500);
    page.off('download', count);
    await signIn();
    await waitH1('e2e-form');
    assert.ok(asked, 'a download after the session ended did not ask to sign in');
    assert.equal(downloads, 0, 'a download started without a session');
    const again = await download(() => page.getByText('Download all outputs (zip)').click());
    assert.equal(again.body.subarray(0, 2).toString(), 'PK');

    await page.locator('main a[href^="#/tasks/"]').first().click();
    await page.waitForFunction(() => /^#\/tasks\//.test(location.hash));
    await page.locator('.log-status, main [role=status]', {hasText: 'End of log.'}).first().waitFor({timeout: 30000});
    const log = await download(() => page.getByText('Download log').click());
    assert.ok(log.body.toString().indexOf('log line') >= 0, 'the log download');
    const out = await download(() => page.getByRole('link', {name: 'o.txt'}).click());
    assert.equal(out.body.toString(), 'hi\n');
  });

  await step('the log viewer shows the final attempt of a retried task (WEB-1)', async () => {
    const script = [
      'if [ "$SAVIOR_ATTEMPT" = 1 ]; then',
      '  for i in 1 2 3 4 5 6; do echo "FIRST-ATTEMPT-LINE-$i-padding-padding"; done',
      '  sleep 3',
      '  exit 3',
      'fi',
      'for i in 1 2 3 4 5 6; do echo "SECOND-$i"; done',
      'sleep 1',
    ].join('\n') + '\n';
    const job = await api('POST', 'jobs', {name: 'e2e-retry', kind: 'script', script: script, count: 1, retries: 1,
      resources: {cores: 0.5, mem_mb: 48, disk_mb: 16}, requirements: {isolation: 'any'}});
    const task = await until('the retry task to run', async () => {
      const p = await api('GET', 'jobs/' + job.id + '/tasks');
      const t = p && p.tasks && p.tasks[0];
      return t && t.state === 'running' ? t : null;
    });
    retryTask = task.id;
    await go('#/tasks/' + task.id, 'Task 0');
    await page.locator('main [role=status]', {hasText: 'End of log.'}).first().waitFor({timeout: 60000});
    const text = await page.locator('pre.log').textContent();
    assert.ok(text.indexOf('--- attempt 2 ---') >= 0, 'no attempt marker in ' + JSON.stringify(text));
    const second = text.slice(text.indexOf('--- attempt 2 ---'));
    for (let i = 1; i <= 6; i++) assert.ok(second.indexOf('SECOND-' + i + '\n') >= 0, 'SECOND-' + i + ' missing from ' + JSON.stringify(text));
    assert.ok(second.indexOf('FIRST') < 0, 'attempt 1 output after the attempt 2 marker: ' + JSON.stringify(text));
    const t = await api('GET', 'tasks/' + task.id);
    assert.equal(t.attempt, 2);
    assert.equal(t.state, 'succeeded');
  });

  await step('walls: create, edit, delete', async () => {
    await go('#/walls/new', 'New wall');
    await page.getByLabel('Name', {exact: true}).fill('e2e-wall');
    await page.getByLabel('Screen at row 1, column 1').selectOption(NODES[0]);
    await page.getByLabel('Screen at row 1, column 2').selectOption(NODES[1]);
    await page.getByRole('button', {name: 'Save wall'}).click();
    await page.waitForFunction(() => /^#\/walls\/./.test(location.hash) && location.hash !== '#/walls/new');
    let walls = await api('GET', 'walls');
    assert.equal(walls.length, 1);
    assert.equal(walls[0].name, 'e2e-wall');
    assert.equal(walls[0].cells.length, 2);
    await waitH1('Wall e2e-wall');
    await page.getByLabel('Name', {exact: true}).fill('e2e-wall-2');
    await page.getByRole('button', {name: 'Save wall'}).click();
    await toast('Wall saved.');
    await until('the rename to reach the API', async () => (await api('GET', 'walls'))[0].name === 'e2e-wall-2');
    await page.getByRole('button', {name: 'Delete wall'}).click();
    await page.waitForFunction(() => location.hash === '#/walls');
    walls = await api('GET', 'walls');
    assert.equal((walls || []).length, 0);
  });

  await step('every route renders, also at 800x600', async () => {
    if (!jobID) jobID = (await api('GET', 'jobs'))[0].id;
    const jobName = (await api('GET', 'jobs/' + jobID)).name;
    const task = (await api('GET', 'jobs/' + jobID + '/tasks')).tasks[0].id;
    const routes = [['#/', 'Overview'], ['#/nodes', 'Nodes'], ['#/nodes/' + NODES[1], 'e2e-node-2'], ['#/nodes/' + PENDING, 'e2e-new-box'],
      ['#/jobs', 'Jobs'], ['#/jobs/new', 'Submit a job'], ['#/jobs/' + jobID, jobName], ['#/tasks/' + task, 'Task 0'],
      ['#/walls', 'Video walls'], ['#/walls/new', 'New wall'], ['#/blobs', 'Blobs'], ['#/settings', 'Settings and help'],
      ['#/no/such/page', 'Page not found']];
    if (retryTask) routes.push(['#/tasks/' + retryTask, 'Task 0']);
    for (const vp of [{width: 1024, height: 768}, {width: 800, height: 600}]) {
      await page.setViewportSize(vp);
      for (const r of routes) {
        await go(r[0], r[1]);
        await page.waitForTimeout(150);
        await noHorizontalOverflow(r[0] + ' at ' + vp.width + 'x' + vp.height);
        if (process.env.E2E_SHOTS === '1') {
          await page.screenshot({path: path.join(OUT, vp.width + '-' + r[0].replace(/[^a-z0-9]+/gi, '_') + '.png')});
        }
      }
    }
  });
} catch (e) {
  console.log(e && e.stack ? e.stack : String(e));
  await browser.close();
  process.exit(1);
}
await browser.close();
if (failed.length) {
  console.log('dashboard e2e: ' + failed.length + ' of ' + stepNo + ' steps failed:\n  ' + failed.join('\n  '));
  process.exit(1);
}
console.log('dashboard e2e: ' + stepNo + ' steps passed');
