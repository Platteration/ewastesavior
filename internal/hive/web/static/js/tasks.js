// Task detail: state, history, outputs and a live log viewer.

import {h, mount, table, kv, panel, banner, btn, check, copyable, sleep, isAbort} from './dom.js';
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
function logViewer(id, ctx, isDone) {
  const url = '/admin/tasks/' + api.enc(id) + '/log';
  const pre = h('pre', {class: 'log', tabIndex: 0, 'aria-label': 'Task log', 'aria-live': 'off'});
  const follow = check('Follow output', true);
  const status = h('span', {class: 'hint', role: 'status'});
  const dec = new TextDecoder('utf-8');
  let offset = 0;
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
  follow.input.addEventListener('change', () => {
    if (follow.input.checked) pre.scrollTop = pre.scrollHeight;
  });
  const run = async () => {
    while (!ctx.signal.aborted) {
      let r;
      try {
        r = await api.get(url, {query: {offset: offset, wait_s: LOG_WAIT_S}, as: 'raw', signal: ctx.signal, timeout: (LOG_WAIT_S + 20) * 1000});
      } catch (e) {
        if (isAbort(e) || e.handled) return;
        status.textContent = e.status === 404 ? 'No log yet.' : 'Log unavailable (' + e.message + '). Retrying…';
        await sleep(5000, ctx.signal);
        continue;
      }
      const n = r.buf.byteLength;
      append(dec.decode(new Uint8Array(r.buf), {stream: true}));
      const next = parseInt(r.headers.get('X-Savior-Log-Offset') || '', 10);
      offset = next >= 0 ? next : offset + n;
      if (n > 0) {
        if (!pre.dataset.trimmed) status.textContent = '';
        continue;
      }
      if (isDone()) {
        append(dec.decode());
        if (!pre.firstChild) status.textContent = 'This task wrote no output.';
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
  const nodeLink = (nid) => (nid ? h('a', {href: '#/nodes/' + api.enc(nid)}, names[nid] || nid) : '');
  const jobName = job ? job.name || job.id : t.job_id;

  const head = h('div');
  const outBox = h('div');
  const histBox = h('div');
  const draw = () => {
    ctx.title('Task ' + t.index + ' · ' + jobName);
    mount(head,
      h('div', {class: 'page-head'}, h('h1', null, 'Task ' + t.index + (job ? ' of ' + job.count : '')), h('span', {class: 'badges'}, stateBadge(t.state))),
      t.wait_reason && !terminal(t.state) ? banner('info', 'Waiting: ' + t.wait_reason) : null,
      t.error ? banner('bad', h('strong', null, (t.error_kind || 'error') + ': '), h('span', {class: 'pre'}, t.error)) : null,
      kv([
        ['Task ID', copyable(t.id, 'task ID')], ['Job', h('a', {href: '#/jobs/' + api.enc(t.job_id)}, jobName)],
        ['Node', nodeLink(t.node)], ['Attempt', attemptText(t)],
        ['Exit code', t.exit_code == null ? '' : String(t.exit_code)], ['Error kind', t.error_kind],
        ['Assigned', fmt.zeroTime(t.assigned_at) ? '' : fmt.time(t.assigned_at)],
        ['Started', fmt.zeroTime(t.started_at) ? '' : fmt.time(t.started_at)],
        ['Finished', fmt.zeroTime(t.finished_at) ? '' : fmt.time(t.finished_at)],
        ['Run time', t.run_s ? fmt.dur(t.run_s) : ''], ['CPU time', t.cpu_seconds ? fmt.num(t.cpu_seconds, 1) + ' s' : ''],
        ['Peak memory', t.max_mem_mb ? fmt.mb(t.max_mem_mb) : ''],
        ['Failed on', (t.failed_nodes || []).length ? t.failed_nodes.map((n, i) => [i ? ', ' : '', nodeLink(n)]) : ''],
      ]));
    mount(histBox, table(['Attempt', 'Node', 'Assigned', 'Finished', 'Outcome', 'Exit', 'Error'],
      (t.history || []).map((a) => [String(a.attempt), nodeLink(a.node), fmt.time(a.assigned_at), fmt.time(a.finished_at),
        a.outcome ? h('span', {class: 'badge badge-' + (OUTCOME[a.outcome] || 'neutral')}, a.outcome) : 'in progress',
        a.outcome ? String(a.exit_code) : '', h('span', {class: 'pre'}, a.error || '')]), 'Not dispatched yet.'));
    mount(outBox, table(['File', 'Size', 'Blob'], (t.outputs || []).map((o) => [
      h('a', {href: api.BASE + path + '/outputs/' + api.enc(o.name), download: fileName(o.name)}, o.name),
      fmt.bytes(o.size), h('code', {title: o.blob}, fmt.shortHash(o.blob))]),
    terminal(t.state) ? 'No outputs.' : 'Outputs appear when the task finishes.'));
  };
  const log = logViewer(id, ctx, () => terminal(t.state));
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
