// Task detail: state, history, outputs and a live log viewer.

import {h, mount, remount, table, kv, panel, banner, btn, check, copyable, sleep, isAbort} from './dom.js';
import * as api from './api.js';
import * as fmt from './fmt.js';
import {fileName} from './model.js';
import {stateBadge, attemptText} from './jobs.js';

const LOG_WAIT_S = 20;
const LOG_KEEP = 2000000; // characters kept in the viewer
const OUTCOME = {succeeded: 'ok', failed: 'bad', timeout: 'bad', node_error: 'warn', lost: 'warn', preempted: 'warn', canceled: 'neutral'};

const terminal = (s) => s === 'succeeded' || s === 'failed' || s === 'canceled';

// logViewer follows GET /admin/tasks/{id}/log with long polls, appending
// text nodes. Byte offsets come from X-Savior-Log-Offset; a streaming
// TextDecoder keeps multi-byte characters intact across chunk boundaries.
// Every attempt's log starts at offset 0: when X-Savior-Log-Attempt (or,
// from an older hive, getAttempt, the task view's attempt) changes, the
// viewer marks the new attempt and reads it from the start.
export function logViewer(id, ctx, isDone, getAttempt) {
  const url = '/admin/tasks/' + api.enc(id) + '/log';
  const pre = h('pre', {class: 'log', tabIndex: 0, 'aria-label': 'Task log', 'aria-live': 'off'});
  const follow = check('Follow output', true);
  const status = h('span', {class: 'hint', role: 'status'});
  let dec = new TextDecoder('utf-8');
  let offset = 0;
  let attempt = null; // attempt whose log is shown, once known
  let wrote = false; // the shown attempt's log has bytes
  let kept = 0;
  const append = (s) => {
    if (!s) return;
    pre.appendChild(document.createTextNode(s));
    kept += s.length;
    while (kept > LOG_KEEP && pre.firstChild) {
      kept -= pre.firstChild.textContent.length;
      pre.removeChild(pre.firstChild);
      if (!pre.dataset.trimmed) {
        pre.dataset.trimmed = '1';
        status.textContent = 'Older output was trimmed from this view; download the log for all of it.';
      }
    }
    if (follow.input.checked) pre.scrollTop = pre.scrollHeight;
  };
  // newAttempt shows attempt a's log from its start, under a marker line.
  const newAttempt = (a) => {
    append(dec.decode());
    dec = new TextDecoder('utf-8');
    offset = 0;
    attempt = a;
    wrote = false;
    const last = pre.lastChild;
    append((last && !/\n$/.test(last.textContent) ? '\n' : '') + '--- attempt ' + a + ' ---\n');
  };
  follow.input.addEventListener('change', () => {
    if (follow.input.checked) pre.scrollTop = pre.scrollHeight;
  });
  const run = async () => {
    while (!ctx.signal.aborted) {
      let r;
      try {
        r = await api.get(url, {query: {offset: offset, wait_s: LOG_WAIT_S}, as: 'raw', signal: ctx.signal, timeout: (LOG_WAIT_S + 20) * 1000});
      } catch (e) {
        if (isAbort(e)) return;
        // After a 401 (e.handled) the sign-in form is up: retry later.
        if (!e.handled) status.textContent = e.status === 404 ? 'No log yet.' : 'Log unavailable (' + e.message + '). Retrying…';
        await sleep(5000, ctx.signal);
        continue;
      }
      const next = parseInt(r.headers.get('X-Savior-Log-Offset') || '', 10);
      const said = parseInt(r.headers.get('X-Savior-Log-Attempt') || '', 10);
      const cur = said >= 0 ? said : getAttempt();
      if (attempt === null && cur > 1) {
        newAttempt(cur);
      } else if (attempt === null) {
        attempt = cur;
      } else if (cur !== attempt || (next < offset && !(said >= 0))) {
        // A new attempt, or a shorter stream from an older hive: unless
        // read from offset 0, drop the response and start over at 0.
        const reread = offset !== 0;
        newAttempt(cur);
        if (reread) continue;
      }
      // Same attempt, fewer bytes (the hive restarted): wait for more.
      const n = next < offset ? 0 : r.buf.byteLength;
      if (n > 0) {
        wrote = true;
        append(dec.decode(new Uint8Array(r.buf), {stream: true}));
        offset = next >= 0 ? next : offset + n;
        if (!pre.dataset.trimmed) status.textContent = '';
        continue;
      }
      if (next > offset) offset = next;
      // Done only once the log of the task's final attempt has been read.
      if (isDone() && attempt === getAttempt()) {
        append(dec.decode());
        if (!wrote) status.textContent = attempt > 1 ? 'This attempt wrote no output.' : 'This task wrote no output.';
        else if (!pre.dataset.trimmed) status.textContent = 'End of log.';
        return;
      }
      // The hive long-polls; this pause only guards against a hive that doesn't.
      await sleep(1000, ctx.signal);
    }
  };
  const el = h('div', null,
    h('div', {class: 'toolbar'}, follow.el,
      h('a', {class: 'btn btn-small', href: api.BASE + url + '?offset=0', download: fileName(id + '.log')}, 'Download log'), status),
    pre);
  return {el: el, run: run};
}

export async function detail(root, ctx, id) {
  const path = '/admin/tasks/' + api.enc(id);
  const sig = {signal: ctx.signal};
  let t = await api.get(path, sig);
  let job = null;
  const names = {};
  try {
    const r = await Promise.all([api.get('/admin/jobs/' + api.enc(t.job_id), sig), api.get('/admin/nodes', sig)]);
    job = r[0];
    for (const n of r[1] || []) names[n.id] = n.name;
  } catch (e) {
    if (isAbort(e) || e.handled) throw e;
  }
  // key tells refreshes which link had focus.
  const nodeLink = (nid, key) => (nid ? h('a', {href: '#/nodes/' + api.enc(nid), 'data-key': key}, names[nid] || nid) : '');
  const jobName = job ? job.name || job.id : t.job_id;

  const head = h('div');
  const outBox = h('div');
  const histBox = h('div');
  const draw = () => {
    ctx.title('Task ' + t.index + ' · ' + jobName);
    remount(head,
      h('div', {class: 'page-head'}, h('h1', null, 'Task ' + t.index + (job ? ' of ' + job.count : '')), h('span', {class: 'badges'}, stateBadge(t.state))),
      t.wait_reason && !terminal(t.state) ? banner('info', 'Waiting: ' + t.wait_reason) : null,
      t.error ? banner('bad', h('strong', null, (t.error_kind || 'error') + ': '), h('span', {class: 'pre'}, t.error)) : null,
      kv([
        ['Task ID', copyable(t.id, 'task ID')], ['Job', h('a', {href: '#/jobs/' + api.enc(t.job_id)}, jobName)],
        ['Node', nodeLink(t.node, 'node')], ['Attempt', attemptText(t)],
        ['Exit code', t.exit_code == null ? '' : String(t.exit_code)], ['Error kind', t.error_kind],
        ['Assigned', fmt.zeroTime(t.assigned_at) ? '' : fmt.time(t.assigned_at)],
        ['Started', fmt.zeroTime(t.started_at) ? '' : fmt.time(t.started_at)],
        ['Finished', fmt.zeroTime(t.finished_at) ? '' : fmt.time(t.finished_at)],
        ['Run time', t.run_s ? fmt.dur(t.run_s) : ''], ['CPU time', t.cpu_seconds ? fmt.num(t.cpu_seconds, 1) + ' s' : ''],
        ['Peak memory', t.max_mem_mb ? fmt.mb(t.max_mem_mb) : ''],
        ['Failed on', (t.failed_nodes || []).length ? t.failed_nodes.map((n, i) => [i ? ', ' : '', nodeLink(n, 'failed:' + n)]) : ''],
      ]));
    remount(histBox, table(['Attempt', 'Node', 'Assigned', 'Finished', 'Outcome', 'Exit', 'Error'],
      (t.history || []).map((a) => [String(a.attempt), nodeLink(a.node, 'attempt:' + a.attempt), fmt.time(a.assigned_at), fmt.time(a.finished_at),
        a.outcome ? h('span', {class: 'badge badge-' + (OUTCOME[a.outcome] || 'neutral')}, a.outcome) : 'in progress',
        a.outcome ? String(a.exit_code) : '', h('span', {class: 'pre'}, a.error || '')]), 'Not dispatched yet.'));
    remount(outBox, table(['File', 'Size', 'Blob'], (t.outputs || []).map((o) => [
      h('a', {href: api.BASE + path + '/outputs/' + api.enc(o.name), download: fileName(o.name), 'data-key': 'out:' + o.name}, o.name),
      fmt.bytes(o.size), h('code', {title: o.blob}, fmt.shortHash(o.blob))]),
    terminal(t.state) ? 'No outputs.' : 'Outputs appear when the task finishes.'));
  };
  const log = logViewer(id, ctx, () => terminal(t.state), () => t.attempt || 0);
  mount(root, h('p', {class: 'crumbs'}, h('a', {href: '#/jobs/' + api.enc(t.job_id)}, '← ' + jobName)), head,
    panel('Log', log.el), panel('Outputs', outBox), panel('Attempts', histBox),
    h('div', {class: 'btn-row'}, btn('Refresh', async () => {
      t = await api.get(path, sig);
      draw();
    }, 'btn-small')));
  draw();
  log.run();
  ctx.poll(async () => {
    if (terminal(t.state)) return;
    t = await api.get(path, sig);
    draw();
  }, 5000);
}
