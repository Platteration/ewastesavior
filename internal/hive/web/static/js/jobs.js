// Jobs: list, submit form (simple or raw JSON) and job detail with tasks.

import {h, mount, table, badge, btn, kv, panel, banner, field, select, check, toast, reportError, copyable} from './dom.js';
import * as api from './api.js';
import * as fmt from './fmt.js';
import {LIMITS, splitArgs, parseKV, validLabelKey, validEnvKey, validOutputPattern, validRelPath, inputName, fileName} from './model.js';

export const TASK_STATES = ['pending', 'assigned', 'running', 'succeeded', 'failed', 'canceled'];
const JOB_STATES = ['queued', 'running', 'succeeded', 'failed', 'canceled'];
const KIND = {queued: 'neutral', pending: 'neutral', assigned: 'info', running: 'info', succeeded: 'ok', failed: 'bad', canceled: 'neutral'};
const PAGE = 50;
const TASK_PAGE = 100;

let listFilter = '';
let cloneSpec = null;

export function stateBadge(s) {
  return badge(s, KIND[s] || 'neutral');
}

const terminal = (s) => s === 'succeeded' || s === 'failed' || s === 'canceled';

// countsBar is a stacked progress bar of task states.
export function countsBar(c, total) {
  c = c || {};
  total = Math.max(1, total || 0);
  const done = (c.succeeded || 0) + (c.failed || 0) + (c.canceled || 0);
  const bar = h('div', {class: 'bar', role: 'progressbar', 'aria-valuemin': 0, 'aria-valuemax': total, 'aria-valuenow': done,
    'aria-valuetext': done + ' of ' + total + ' tasks finished'});
  for (const k of ['succeeded', 'failed', 'canceled', 'running', 'assigned']) {
    if (c[k] > 0) bar.appendChild(h('span', {class: 'seg seg-' + k, title: c[k] + ' ' + k, style: {width: (c[k] * 100 / total) + '%'}}));
  }
  return bar;
}

export function countsText(c, total) {
  c = c || {};
  const parts = [(c.succeeded || 0) + '/' + total + ' succeeded'];
  for (const k of ['failed', 'canceled', 'running', 'assigned', 'pending']) {
    if (c[k]) parts.push(c[k] + ' ' + k);
  }
  return parts.join(' · ');
}

function jobRow(j, ctx) {
  return {
    cells: [
      String(j.seq),
      h('a', {href: '#/jobs/' + api.enc(j.id)}, j.name || j.id),
      stateBadge(j.state),
      h('td', {class: 'progress-cell'}, countsBar(j.counts, j.count), h('span', {class: 'hint'}, countsText(j.counts, j.count))),
      h('td', {class: 'opt'}, String(j.priority || 0)),
      h('td', {class: 'opt'}, fmt.ago(j.created_at)),
      j.warning ? badge('warning', 'warn', j.warning) : '',
    ],
    onClick: () => ctx.go('#/jobs/' + api.enc(j.id)),
  };
}

export async function list(root, ctx) {
  ctx.title('Jobs');
  const sig = {signal: ctx.signal};
  let jobs = [];
  let more = false;
  const stateSel = select([['', 'All states']].concat(JOB_STATES.map((s) => [s, s])), listFilter, {'aria-label': 'Show jobs in state'});
  const box = h('div');
  const moreBtn = btn('Load older jobs', async () => {
    const last = jobs[jobs.length - 1];
    const older = (await api.get('/admin/jobs', Object.assign({query: {state: listFilter, limit: PAGE, before: last.seq}}, sig))) || [];
    const seen = new Set(jobs.map((j) => j.id));
    jobs = jobs.concat(older.filter((j) => !seen.has(j.id)));
    more = older.length === PAGE;
    draw();
  });
  const heads = ['#', 'Name', 'State', 'Progress', h('th', {scope: 'col', class: 'opt'}, 'Priority'), h('th', {scope: 'col', class: 'opt'}, 'Submitted'), ''];
  const draw = () => {
    mount(box, table(heads, jobs.map((j) => jobRow(j, ctx)), listFilter ? 'No jobs in this state.' : 'No jobs yet.'));
    moreBtn.hidden = !more;
  };
  // A refresh re-reads the newest page and merges it, keeping older pages.
  const load = async (reset) => {
    const fresh = (await api.get('/admin/jobs', Object.assign({query: {state: listFilter, limit: PAGE}}, sig))) || [];
    if (reset) {
      jobs = fresh;
      more = fresh.length === PAGE;
    } else {
      // Jobs in the fresh page's range that vanished were deleted or left
      // the filtered state; older pages are kept as they were.
      const ids = new Set(fresh.map((j) => j.id));
      const oldest = fresh.length === PAGE ? fresh[fresh.length - 1].seq : -Infinity;
      jobs = fresh.concat(jobs.filter((j) => !ids.has(j.id) && j.seq < oldest));
    }
    draw();
  };
  stateSel.addEventListener('change', () => {
    listFilter = stateSel.value;
    load(true).catch(reportError);
  });
  mount(root, h('div', {class: 'page-head'}, h('h1', null, 'Jobs'), h('a', {class: 'btn btn-primary', href: '#/jobs/new'}, 'Submit a job')),
    h('div', {class: 'toolbar'}, stateSel), box, h('div', {class: 'btn-row'}, moreBtn));
  await load(true);
  ctx.poll(() => load(false), 5000);
}

function numIn(input, label, min, max, integer) {
  const v = input.value.trim();
  if (v === '') return undefined;
  const n = Number(v);
  if (!isFinite(n) || n < min || n > max || (integer && n !== Math.floor(n))) {
    throw new Error(label + ' must be ' + (integer ? 'a whole number ' : 'a number ') + 'from ' + min + ' to ' + max + '.');
  }
  return n;
}

function words(s) {
  return s.split(/[\s,]+/).filter(Boolean);
}

export async function submit(root, ctx) {
  ctx.title('Submit a job');
  const t = (props) => h('input', Object.assign({type: 'text', spellcheck: false, autocomplete: 'off'}, props));
  const n = (props) => h('input', Object.assign({type: 'number'}, props));
  const ta = (rows, ph) => h('textarea', {rows: rows, spellcheck: false, placeholder: ph || ''});

  const name = t({maxLength: 128, placeholder: 'render-frames'});
  const kind = select([['exec', 'Run a command'], ['script', 'Run a shell script']], 'exec');
  const cmd = t({placeholder: 'python3 work.py --part {{index}} --of {{count}}'});
  const argv = h('p', {class: 'hint mono', 'aria-live': 'polite'});
  const script = ta(8, '#!/bin/sh\necho "task $SAVIOR_TASK_INDEX of $SAVIOR_TASK_COUNT"');
  const cmdField = field('Command', cmd, 'Quoted like a shell command line, but nothing is expanded except {{index}} and {{count}}.', 'wide');
  const scriptField = field('Script', script, 'Runs with /bin/sh (BusyBox). SAVIOR_TASK_INDEX, SAVIOR_TASK_COUNT, SAVIOR_JOB_ID are set.', 'wide');
  const count = n({min: 1, max: LIMITS.count, step: 1, value: 1});
  const priority = n({min: -LIMITS.priority, max: LIMITS.priority, step: 1, placeholder: '0'});
  const cores = n({min: 0.05, max: LIMITS.cores, step: 0.05, placeholder: '1'});
  const mem = n({min: 1, max: LIMITS.memMB, step: 1, placeholder: '128'});
  const disk = n({min: 0, max: LIMITS.diskMB, step: 1, placeholder: '64'});
  const timeout = n({min: 1, max: LIMITS.timeoutS, step: 1, placeholder: '3600'});
  const retries = n({min: 0, max: LIMITS.retries, step: 1, placeholder: '1'});
  const network = check('Allow network access');
  const any = check('Also run on nodes without full isolation (isolation=any)');
  const amd64 = check('amd64 (64-bit)');
  const i386 = check('386 (32-bit)');
  const labels = ta(2, 'room=lab-2');
  const minMem = n({min: 0, step: 1, placeholder: 'any'});
  const flags = t({placeholder: 'sse2 avx'});
  const nodesIn = t({placeholder: 'lab-pc-1, lab-pc-2'});
  const env = ta(2, 'MODE=fast');
  const outputs = ta(2, 'out/*.png\nresult.txt');

  // Inputs are uploaded as blobs as soon as they're picked.
  const inputs = [];
  const inputBox = h('div', {class: 'media-list'});
  let queue = Promise.resolve();
  const drawInputs = () => mount(inputBox, inputs.length ? inputs.map((it) => it.row) : h('p', {class: 'muted'}, 'No input files.'));
  const addFile = (file) => {
    const it = {blob: '', busy: true, failed: false};
    it.name = t({value: inputName(file.name), 'aria-label': 'Input name for ' + file.name});
    it.exec = check('executable');
    it.status = h('span', {class: 'hint'}, 'waiting…');
    it.row = h('div', {class: 'media-item'}, it.name, h('span', {class: 'hint'}, fmt.bytes(file.size)), it.exec.el, it.status,
      btn('Remove', () => {
        it.removed = true;
        inputs.splice(inputs.indexOf(it), 1);
        drawInputs();
      }, 'btn-small'));
    inputs.push(it);
    queue = queue.then(async () => {
      if (it.removed) return;
      try {
        const info = await api.upload(file, (done, total) => { it.status.textContent = Math.floor(done * 100 / total) + '%'; }, ctx.signal);
        it.blob = info.sha256;
        it.status.textContent = 'uploaded';
      } catch (e) {
        it.failed = true;
        it.status.textContent = 'failed';
        reportError(e);
      }
      it.busy = false;
    });
  };
  const filePick = h('input', {type: 'file', multiple: true});
  filePick.addEventListener('change', () => {
    Array.prototype.forEach.call(filePick.files || [], addFile);
    filePick.value = '';
    drawInputs();
  });
  drawInputs();

  const syncKind = () => {
    cmdField.hidden = kind.value !== 'exec';
    scriptField.hidden = kind.value !== 'script';
  };
  kind.addEventListener('change', syncKind);
  cmd.addEventListener('input', () => {
    const r = splitArgs(cmd.value);
    argv.textContent = r.error ? 'Problem: ' + r.error : r.args.length ? 'Runs: ' + JSON.stringify(r.args) : '';
  });
  syncKind();

  const build = () => {
    const spec = {kind: kind.value};
    if (name.value.trim()) spec.name = name.value.trim();
    if (kind.value === 'exec') {
      const r = splitArgs(cmd.value);
      if (r.error) throw new Error('Command: ' + r.error + '.');
      if (!r.args.length) throw new Error('Enter a command.');
      spec.command = r.args;
    } else {
      if (!script.value.trim()) throw new Error('Enter a script.');
      spec.script = script.value.replace(/\r\n/g, '\n');
    }
    spec.count = numIn(count, 'Count', 1, LIMITS.count, true) || 1;
    const pr = numIn(priority, 'Priority', -LIMITS.priority, LIMITS.priority, true);
    if (pr) spec.priority = pr;
    spec.resources = {};
    const c = numIn(cores, 'Cores', 0.01, LIMITS.cores, false);
    const mm = numIn(mem, 'Memory', 1, LIMITS.memMB, true);
    const dm = numIn(disk, 'Disk', 0, LIMITS.diskMB, true);
    if (c) spec.resources.cores = c;
    if (mm) spec.resources.mem_mb = mm;
    if (dm) spec.resources.disk_mb = dm;
    const to = numIn(timeout, 'Timeout', 1, LIMITS.timeoutS, true);
    if (to) spec.timeout_s = to;
    const rt = numIn(retries, 'Retries', 0, LIMITS.retries, true);
    if (rt !== undefined) spec.retries = rt;
    if (network.input.checked) spec.network = true;
    const req = {};
    const arch = [];
    if (amd64.input.checked) arch.push('amd64');
    if (i386.input.checked) arch.push('386');
    if (arch.length) req.arch = arch;
    const lab = parseKV(labels.value, 'Required labels', validLabelKey, true);
    if (lab.error) throw new Error(lab.error);
    if (lab.count) req.labels = lab.map;
    const mn = numIn(minMem, 'Minimum node memory', 0, LIMITS.memMB, true);
    if (mn) req.min_mem_mb = mn;
    if (words(flags.value).length) req.cpu_flags = words(flags.value.toLowerCase());
    if (words(nodesIn.value).length) req.nodes = words(nodesIn.value);
    if (any.input.checked) req.isolation = 'any';
    spec.requirements = req;
    const e = parseKV(env.value, 'Environment', validEnvKey, false);
    if (e.error) throw new Error(e.error + ' (names are letters, digits and _, not starting with SAVIOR_)');
    if (e.count) spec.env = e.map;
    const outs = outputs.value.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);
    for (const o of outs) {
      if (!validOutputPattern(o)) throw new Error('Output pattern "' + o + '" must be a relative path or glob inside the work directory.');
    }
    if (outs.length > LIMITS.outputs) throw new Error('At most ' + LIMITS.outputs + ' output patterns.');
    if (outs.length) spec.outputs = outs;
    const seen = {};
    const ins = [];
    for (const it of inputs) {
      if (it.busy) throw new Error('Wait for the input uploads to finish.');
      if (it.failed) throw new Error('Remove the inputs whose upload failed.');
      const nm = it.name.value.trim();
      if (!validRelPath(nm)) throw new Error('Input name "' + nm + '" must be a plain relative path (no .., no leading dash or dot-savior).');
      if (seen[nm]) throw new Error('Two inputs are named ' + nm + '.');
      seen[nm] = true;
      ins.push(it.exec.input.checked ? {name: nm, blob: it.blob, executable: true} : {name: nm, blob: it.blob});
    }
    if (ins.length) spec.inputs = ins;
    return spec;
  };

  // Raw JSON mode.
  const raw = h('textarea', {rows: 20, spellcheck: false, class: 'mono', 'aria-label': 'Job spec as JSON'});
  let rawDirty = false;
  raw.addEventListener('input', () => { rawDirty = true; });
  const formBox = h('div');
  const rawBox = h('div', {hidden: true}, raw,
    h('p', {class: 'hint'}, 'A JobSpec as described in docs/DESIGN.md. Inputs can reference blobs by hash or URLs with sha256 and size.'));
  const bForm = btn('Form', () => setMode(false), 'btn-small', {'aria-pressed': 'true'});
  const bRaw = btn('JSON', () => setMode(true), 'btn-small', {'aria-pressed': 'false'});
  let rawMode = false;
  const setMode = (on) => {
    if (on && !rawDirty) {
      try {
        raw.value = JSON.stringify(build(), null, 2);
      } catch (err) {
        if (!raw.value) raw.value = JSON.stringify({name: '', command: ['echo', 'hello'], count: 1}, null, 2);
        toast(err.message, 'info');
      }
    }
    rawMode = on;
    formBox.hidden = on;
    rawBox.hidden = !on;
    bForm.setAttribute('aria-pressed', String(!on));
    bRaw.setAttribute('aria-pressed', String(on));
  };

  const go = async () => {
    let spec;
    if (rawMode) {
      try {
        spec = JSON.parse(raw.value);
      } catch (err) {
        throw new Error('The JSON is not valid: ' + err.message);
      }
      if (!spec || typeof spec !== 'object' || Array.isArray(spec)) throw new Error('The JSON must be an object (a JobSpec).');
    } else {
      spec = build();
    }
    const job = await api.post('/admin/jobs', spec, {signal: ctx.signal});
    toast('Job submitted.', 'ok');
    ctx.go('#/jobs/' + api.enc(job.id));
  };

  mount(formBox,
    panel('What to run',
      h('div', {class: 'form-grid'}, field('Name (optional)', name), field('Type', kind), cmdField), argv, scriptField),
    panel('How many and how big',
      h('div', {class: 'form-grid'},
        field('Tasks', count, 'Each task gets its own index.'), field('Cores per task', cores, 'Default 1; fractions allowed.'),
        field('Memory per task (MB)', mem, 'Default 128.'), field('Disk per task (MB)', disk, 'Default 64. On RAM scratch it counts as memory.'),
        field('Timeout (seconds)', timeout, 'Default 3600.'), field('Retries', retries, 'Default 1: a failed task runs once more.'),
        field('Priority', priority, 'Higher runs first; -1000 to 1000.')),
      h('div', {class: 'checks'}, network.el)),
    panel('Where it may run',
      h('div', {class: 'checks'}, amd64.el, i386.el),
      h('p', {class: 'hint'}, 'Leave both architectures unchecked to run on any machine.'),
      h('div', {class: 'form-grid'},
        field('Required node labels', labels, 'key=value per line; all must match.'),
        field('Minimum node memory (MB)', minMem), field('Required CPU flags', flags, 'Space separated, e.g. sse2 avx.'),
        field('Only these nodes', nodesIn, 'Names or IDs, separated by commas or spaces.')),
      h('div', {class: 'checks'}, any.el),
      h('p', {class: 'hint'}, 'Without full isolation a task can see more of the machine. Only allow it for code you trust.')),
    panel('Files and environment',
      field('Input files', filePick, 'Uploaded to the hive now and copied into each task\'s working directory.'), inputBox,
      h('div', {class: 'form-grid'},
        field('Outputs to collect', outputs, 'One path or glob per line, relative to the working directory.'),
        field('Environment variables', env, 'NAME=value per line.'))));

  mount(root, h('p', {class: 'crumbs'}, h('a', {href: '#/jobs'}, '← Jobs')),
    h('div', {class: 'page-head'}, h('h1', null, 'Submit a job'), h('span', {class: 'btn-row', role: 'group', 'aria-label': 'Editor'}, bForm, bRaw)),
    formBox, rawBox, h('div', {class: 'btn-row'}, btn('Submit job', go, 'btn-primary')));

  if (cloneSpec) {
    raw.value = JSON.stringify(cloneSpec, null, 2);
    rawDirty = true;
    cloneSpec = null;
    setMode(true);
  }
}

function taskInfo(t) {
  return t.error || t.wait_reason || '';
}

// attemptText is "2 (1 failed, 1 interrupted)".
export function attemptText(t) {
  const p = [];
  if (t.failures) p.push(t.failures + ' failed');
  if (t.node_errors) p.push(fmt.plural(t.node_errors, 'node error'));
  if (t.interruptions) p.push(t.interruptions + ' interrupted');
  return String(t.attempt || 0) + (p.length ? ' (' + p.join(', ') + ')' : '');
}

export async function detail(root, ctx, id) {
  const path = '/admin/jobs/' + api.enc(id);
  const sig = {signal: ctx.signal};
  let job = await api.get(path, sig);
  const nodeNames = {};
  try {
    for (const nd of (await api.get('/admin/nodes', sig)) || []) nodeNames[nd.id] = nd.name;
  } catch (e) {
    if (e.name === 'AbortError' || e.handled) throw e;
  }
  const nodeLink = (nid) => (nid ? h('a', {href: '#/nodes/' + api.enc(nid)}, nodeNames[nid] || nid) : '—');

  const head = h('div');
  const bCancel = btn('Cancel job', async () => {
    if (!window.confirm('Cancel this job? Pending tasks are dropped and running tasks are stopped.')) return;
    await api.post(path + '/cancel', {}, sig);
    toast('Job canceled.', 'ok');
    await refresh(true);
  }, 'btn-danger');
  const bDelete = btn('Delete job', async () => {
    if (!window.confirm('Delete this job with its task records, logs and outputs?')) return;
    await api.del(path, sig);
    toast('Job deleted.', 'ok');
    ctx.go('#/jobs');
  }, 'btn-danger');
  const bClone = btn('Clone as new job', () => {
    cloneSpec = job.spec;
    ctx.go('#/jobs/new');
  });
  const zip = h('a', {class: 'btn', href: api.BASE + path + '/outputs.zip', download: fileName((job.name || job.id) + '-outputs.zip')}, 'Download all outputs (zip)');

  // Tasks table.
  let tState = '';
  let offset = 0;
  let nextOffset = 0;
  const tBox = h('div');
  const tInfo = h('p', {class: 'muted'});
  const stSel = select([['', 'All states']].concat(TASK_STATES.map((s) => [s, s])), '', {'aria-label': 'Show tasks in state'});
  const bPrev = btn('← Previous', () => { offset = Math.max(0, offset - TASK_PAGE); return loadTasks(); }, 'btn-small');
  const bNext = btn('Next →', () => { offset = nextOffset; return loadTasks(); }, 'btn-small');
  const loadTasks = async () => {
    const page = (await api.get(path + '/tasks', Object.assign({query: {state: tState, offset: offset, limit: TASK_PAGE}}, sig))) || {};
    const tasks = page.tasks || [];
    nextOffset = page.next_offset || 0;
    bPrev.hidden = offset === 0;
    bNext.hidden = !nextOffset;
    tInfo.textContent = (tasks.length ? 'Showing ' + (offset + 1) + '–' + (offset + tasks.length) + ' of ' + page.total + '. ' : '') +
      (page.undispatched ? fmt.plural(page.undispatched, 'task') + ' not dispatched yet.' : '');
    mount(tBox, table(['#', 'State', 'Node', h('th', {scope: 'col', class: 'opt'}, 'Attempt'), 'Exit', h('th', {scope: 'col', class: 'opt'}, 'Run time'), 'Details', h('th', {scope: 'col', class: 'opt'}, 'Outputs')],
      tasks.map((t) => ({
        cells: [h('a', {href: '#/tasks/' + api.enc(t.id)}, String(t.index)), stateBadge(t.state), nodeLink(t.node),
          h('td', {class: 'opt'}, attemptText(t)),
          t.exit_code == null ? '' : String(t.exit_code), h('td', {class: 'opt'}, t.run_s ? fmt.dur(t.run_s) : ''),
          h('span', {class: 'clip', title: taskInfo(t) || null}, taskInfo(t)), h('td', {class: 'opt'}, String((t.outputs || []).length))],
        onClick: () => ctx.go('#/tasks/' + api.enc(t.id)),
      })), tState ? 'No tasks in this state.' : 'No tasks have been dispatched yet.'));
  };
  stSel.addEventListener('change', () => {
    tState = stSel.value;
    offset = 0;
    loadTasks().catch(reportError);
  });

  // Outputs list, loaded on request (it can be long).
  const outBox = h('div');
  const bOutputs = btn('List output files', async () => {
    const outs = (await api.get(path + '/outputs', sig)) || [];
    const total = outs.reduce((a, o) => a + (o.size || 0), 0);
    mount(outBox, h('p', {class: 'muted'}, fmt.plural(outs.length, 'file') + ', ' + fmt.bytes(total) + (outs.length > 200 ? '. Showing the first 200.' : '.')),
      table(['Task', 'File', 'Size'], outs.slice(0, 200).map((o) => [String(o.index),
        h('a', {href: api.BASE + '/admin/tasks/' + api.enc(o.task_id) + '/outputs/' + api.enc(o.name), download: fileName(o.name)}, o.name),
        fmt.bytes(o.size)]), 'No outputs.'));
  }, 'btn-small');

  const drawHead = () => {
    ctx.title(job.name || job.id);
    const dur = !fmt.zeroTime(job.started_at) ? ((fmt.zeroTime(job.finished_at) ? Date.now() : new Date(job.finished_at).getTime()) - new Date(job.started_at).getTime()) / 1000 : 0;
    mount(head,
      h('div', {class: 'page-head'}, h('h1', null, job.name || 'Job ' + job.id), h('span', {class: 'badges'}, stateBadge(job.state))),
      job.warning ? banner('warn', job.warning) : null,
      countsBar(job.counts, job.count), h('p', {class: 'hint'}, countsText(job.counts, job.count)),
      kv([['Job ID', copyable(job.id, 'job ID')], ['Queue position', '#' + job.seq], ['Tasks', String(job.count)], ['Priority', String(job.priority || 0)],
        ['Submitted', fmt.time(job.created_at)], ['Started', fmt.zeroTime(job.started_at) ? '' : fmt.time(job.started_at)],
        ['Finished', fmt.zeroTime(job.finished_at) ? '' : fmt.time(job.finished_at)], ['Duration', dur ? fmt.dur(dur) : '']]));
    bCancel.hidden = terminal(job.state);
    bDelete.hidden = !terminal(job.state);
  };
  const specPre = h('pre', {class: 'code'});
  let seenTerminal = false;
  const refresh = async (force) => {
    if (!force && seenTerminal) return;
    job = await api.get(path, sig);
    drawHead();
    specPre.textContent = JSON.stringify(job.spec, null, 2);
    await loadTasks();
    seenTerminal = terminal(job.state);
  };

  mount(root, h('p', {class: 'crumbs'}, h('a', {href: '#/jobs'}, '← Jobs')), head,
    h('div', {class: 'btn-row'}, bCancel, bDelete, bClone, zip),
    panel('Tasks', h('div', {class: 'toolbar'}, stSel, tInfo), tBox, h('div', {class: 'btn-row'}, bPrev, bNext)),
    panel('Outputs', bOutputs, outBox),
    h('details', {class: 'panel'}, h('summary', null, 'Job spec (with defaults applied)'), specPre));
  await refresh(true);
  ctx.poll(() => refresh(false), 5000);
}
