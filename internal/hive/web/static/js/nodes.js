// Nodes: sortable list and per-node detail with actions and display editor.

import {h, mount, table, badge, btn, kv, panel, banner, field, select, toast, copyable, memo} from './dom.js';
import * as api from './api.js';
import * as fmt from './fmt.js';
import {validNodeName, validLabelKey, parseKV, kvText, LIMITS, DISPLAY_MODES} from './model.js';
import {displayEditor, MODE_LABELS} from './display.js';

const NODE_MODES = DISPLAY_MODES.filter((m) => m !== 'wall');
const sortState = {key: 'status', dir: 1};
let filterText = '';

const m = (n) => (n.status && n.status.metrics) || {};
const inv = (n) => n.inventory || {};
const online = (n) => n.liveness === 'online';

export function liveBadge(n) {
  return online(n) ? badge('online', 'ok') : badge('offline', 'neutral', 'Last seen ' + fmt.ago(n.last_seen));
}

function stateBadge(n) {
  if (!online(n)) return null;
  const s = (n.status && n.status.state) || 'idle';
  const kind = {idle: 'ok', busy: 'info', paused: 'warn', draining: 'warn'}[s] || 'neutral';
  return badge(s, kind, n.status.reason || null);
}

// statusBadge is "offline", or the online node's own state.
function statusBadge(n) {
  return online(n) ? stateBadge(n) : liveBadge(n);
}

// cpuName drops trademark noise from a CPU model string.
function cpuName(s) {
  return String(s || 'unknown').replace(/\((?:R|TM|tm|r)\)/g, '').replace(/\s+(?:CPU|processor)\s+/g, ' ').replace(/\s+/g, ' ').trim();
}

function tempText(n) {
  const t = m(n).cpu_temp_c;
  return t ? Math.round(t) + ' °C' : '—';
}

function batteryText(n) {
  const x = m(n);
  if (!(x.battery_percent >= 0) || !inv(n).has_battery) return '—';
  return x.battery_percent + '%' + (x.on_battery ? ' (on battery)' : '');
}

function displayText(n) {
  if (n.wall_id) return 'wall';
  const d = n.display ? n.display.mode : (n.status && n.status.display && n.status.display.mode) || '';
  return (d ? MODE_LABELS[d] || d : '—') + (n.display_rotate ? ' ↻' + n.display_rotate + '°' : '');
}

function flags(n) {
  const out = [];
  if (!n.approved) out.push(badge('pending', 'warn', 'Waiting for admin approval'));
  if (n.quarantine) out.push(badge('quarantined', 'bad', n.quarantine));
  if (n.drain) out.push(badge('draining', 'warn'));
  if (n.reserved_for) out.push(badge('reserved', 'info', 'Reserved for task ' + n.reserved_for));
  const w = n.warnings || [];
  if (w.length) out.push(badge(fmt.plural(w.length, 'warning'), 'warn', w.join('\n')));
  const d = n.status && n.status.display;
  if (d && d.error) out.push(badge('display error', 'bad', d.error));
  return out;
}

const COLS = [
  {key: 'code', label: 'Code', val: (n) => n.short_code, cell: (n) => h('code', null, n.short_code)},
  {key: 'name', label: 'Name', cls: 'nowrap', val: (n) => n.name, cell: (n) => h('a', {href: '#/nodes/' + api.enc(n.id)}, n.name)},
  {key: 'status', label: 'Status', val: (n) => (online(n) ? '0' + ((n.status && n.status.state) || '') : '1'), cell: statusBadge},
  {key: 'roles', label: 'Roles', opt: true, val: (n) => (n.roles || []).join(', '), cell: (n) => (n.roles || []).join(', ')},
  {key: 'cpu', label: 'CPU', opt: true, val: (n) => inv(n).cores || 0,
    cell: (n) => [String(inv(n).cores || '?') + ' × ', h('span', {class: 'muted clip', title: inv(n).cpu_model || null}, cpuName(inv(n).cpu_model))]},
  {key: 'mem', label: 'Memory', cls: 'nowrap', val: (n) => inv(n).mem_total_mb || 0, cell: (n) => fmt.mb(inv(n).mem_total_mb)},
  {key: 'temp', label: 'Temp', cls: 'nowrap', val: (n) => m(n).cpu_temp_c || -1, cell: tempText},
  {key: 'battery', label: 'Battery', opt: true, cls: 'nowrap', val: (n) => (inv(n).has_battery ? m(n).battery_percent : -1), cell: batteryText},
  {key: 'tasks', label: 'Tasks', val: (n) => (n.running_tasks || []).length, cell: (n) => String((n.running_tasks || []).length)},
  {key: 'display', label: 'Display', opt: true, val: displayText, cell: displayText},
  {key: 'flags', label: 'Notes', val: (n) => flags(n).length, cell: flags},
];

function searchText(n) {
  const labels = Object.keys(n.labels || {}).map((k) => k + '=' + n.labels[k]);
  return [n.name, n.id, n.short_code, n.addr, inv(n).cpu_model, inv(n).product, (n.roles || []).join(' ')].concat(labels).join(' ').toLowerCase();
}

// identifyAll flashes the short code on every online screen.
export async function identifyAll(signal) {
  await api.post('/admin/identify', {}, {signal: signal});
  toast('Every online screen shows its short code for 30 seconds.', 'ok');
}

export async function list(root, ctx) {
  ctx.title('Nodes');
  let nodes = [];
  const summary = h('p', {class: 'muted', role: 'status'});
  const filter = h('input', {type: 'search', value: filterText, placeholder: 'Filter: name, code, label, address…', 'aria-label': 'Filter nodes'});
  const box = h('div');
  const heads = () => COLS.map((c) => {
    const active = sortState.key === c.key;
    return h('th', {scope: 'col', class: c.opt ? 'opt' : null, 'aria-sort': active ? (sortState.dir > 0 ? 'ascending' : 'descending') : null},
      h('button', {type: 'button', class: 'sort', on: {click: () => {
        sortState.dir = active ? -sortState.dir : 1;
        sortState.key = c.key;
        draw();
      }}}, c.label, active ? (sortState.dir > 0 ? ' ▲' : ' ▼') : ''));
  });
  const draw = () => {
    const q = filterText.trim().toLowerCase();
    const col = COLS.filter((c) => c.key === sortState.key)[0] || COLS[2];
    const cls = (c) => [c.opt ? 'opt' : '', c.cls || ''].join(' ').trim() || null;
    const shown = nodes.filter((n) => !q || searchText(n).indexOf(q) >= 0);
    shown.sort((a, b) => {
      const va = col.val(a);
      const vb = col.val(b);
      const r = typeof va === 'number' && typeof vb === 'number' ? va - vb : String(va).localeCompare(String(vb));
      return (r || String(a.name).localeCompare(String(b.name))) * sortState.dir;
    });
    const on = nodes.filter(online).length;
    summary.textContent = fmt.plural(nodes.length, 'node') + ' · ' + on + ' online' + (q ? ' · ' + shown.length + ' shown' : '');
    mount(box, table(heads(), shown.map((n) => ({
      cells: COLS.map((c) => h('td', {class: cls(c)}, c.cell(n))),
      cls: online(n) ? null : 'dim',
      onClick: () => ctx.go('#/nodes/' + api.enc(n.id)),
    })), q ? 'No nodes match the filter.' : 'No nodes yet. See "Add a machine" on the Overview page.'));
  };
  filter.addEventListener('input', () => {
    filterText = filter.value;
    draw();
  });
  mount(root, h('div', {class: 'page-head'}, h('h1', null, 'Nodes'), btn('Identify all screens', () => identifyAll(ctx.signal))),
    h('div', {class: 'toolbar'}, filter, summary), box);
  const load = async () => {
    nodes = (await api.get('/admin/nodes', {signal: ctx.signal})) || [];
    draw();
  };
  await load();
  ctx.poll(load, 5000);
}

function res(r) {
  r = r || {};
  return fmt.num(r.cores, 2) + (r.cores === 1 ? ' core · ' : ' cores · ') + fmt.mb(r.mem_mb) + ' RAM' + (r.disk_mb ? ' · ' + fmt.mb(r.disk_mb) + ' disk' : '');
}

function headPanel(n) {
  const labels = kvText(n.labels);
  return [
    h('div', {class: 'page-head'}, h('h1', null, n.name, ' ', h('code', {class: 'code-big'}, n.short_code)),
      h('span', {class: 'badges'}, liveBadge(n), stateBadge(n), flags(n))),
    !n.approved ? banner('warn', 'This machine is waiting for approval and gets no tasks until you approve it.') : null,
    n.quarantine ? banner('bad', 'Quarantined: ' + n.quarantine + '. No tasks are sent to it until you clear the quarantine.') : null,
    n.drain ? banner('info', 'Draining: running tasks finish, no new tasks start.') : null,
    (n.warnings || []).map((w) => banner('warn', w)),
    kv([
      ['Node ID', copyable(n.id, 'node ID')], ['Address', n.addr], ['Roles', (n.roles || []).join(', ')],
      ['Labels', labels ? h('code', {class: 'pre'}, labels) : ''], ['Version', n.version],
      ['First seen', fmt.time(n.first_seen)], ['Last seen', n.last_seen ? fmt.ago(n.last_seen) : ''],
      ['Reserved for', n.reserved_for ? h('a', {href: '#/tasks/' + api.enc(n.reserved_for)}, n.reserved_for) : ''],
      ['Wall', n.wall_id ? h('a', {href: '#/walls/' + api.enc(n.wall_id)}, n.wall_id) : ''],
      ['Boot ID', n.boot_id],
    ]),
  ];
}

// headData drops what changes with every heartbeat, so the header (with its
// copy button) re-renders only on real changes.
function headData(n) {
  const st = n.status || {};
  return Object.assign({}, n, {inventory: null, allocated: null, running_tasks: null, last_seen: online(n) ? '' : n.last_seen,
    status: {state: st.state, reason: st.reason, display: {error: st.display && st.display.error}}});
}

function statusPanel(n) {
  const st = n.status || {};
  const x = st.metrics || {};
  const i = inv(n);
  const battery = i.has_battery && x.battery_percent >= 0
    ? x.battery_percent + '% · ' + (x.on_battery ? 'on battery' : 'on AC') + (x.battery_status ? ' · ' + x.battery_status : '') +
      (x.battery_health_percent ? ' · health ' + x.battery_health_percent + '%' : '') : '';
  return panel('Status',
    online(n) ? null : h('p', {class: 'muted'}, 'Offline. These are the last values it reported.'),
    kv([
      ['State', (st.state || '—') + (st.reason ? ' — ' + st.reason : '')],
      ['CPU', fmt.pct(x.cpu_percent) + ' · load ' + fmt.num(x.load1, 2)],
      ['Memory available', fmt.mb(x.mem_available_mb) + ' of ' + fmt.mb(i.mem_total_mb)],
      ['Swap used', x.swap_used_mb ? fmt.mb(x.swap_used_mb) : ''],
      ['Memory pressure', x.mem_pressure ? fmt.num(x.mem_pressure, 1) + '%' : ''],
      ['CPU temperature', x.cpu_temp_c ? Math.round(x.cpu_temp_c) + ' °C' + (x.cpu_temp_limit_c ? ' (pauses at ' + Math.round(x.cpu_temp_limit_c) + ' °C)' : '') : 'unknown'],
      ['Throttle events', x.throttle_events ? String(x.throttle_events) : ''],
      ['Battery', battery], ['Lid', i.is_laptop ? (x.lid_closed ? 'closed' : 'open') : ''],
      ['Uptime', x.uptime_s ? fmt.dur(x.uptime_s) : ''],
      ['Network', 'received ' + fmt.bytes(x.net_rx_bytes) + ' · sent ' + fmt.bytes(x.net_tx_bytes)],
      ['Addresses', (st.addrs || []).join(', ')],
      ['Offered', res(st.total)], ['Free', res(st.free)], ['Assigned by hive', res(n.allocated)],
    ]));
}

function tasksPanel(n) {
  const rt = (n.status && n.status.running_tasks) || [];
  const link = (id) => h('a', {href: '#/tasks/' + api.enc(id)}, id);
  return panel('Tasks on this node',
    table(['Task', 'Phase', 'Run time', 'Transferred'],
      rt.map((t) => [link(t.id), t.phase, fmt.dur(t.run_s), fmt.bytes(t.xfer_bytes)]), 'No running tasks.'),
    (n.running_tasks || []).length ? h('p', {class: 'muted'}, 'Assigned by the hive: ', (n.running_tasks || []).map((id, k) => [k ? ', ' : '', link(id)])) : null);
}

function displayStatePanel(n) {
  const d = (n.status && n.status.display) || {};
  return panel('Screen (as reported)',
    d.error ? banner('bad', 'Display error: ' + d.error) : null,
    d.warning ? banner('warn', d.warning) : null,
    kv([
      ['Framebuffer', d.active ? [d.device, d.driver, d.format].filter(Boolean).join(' · ') : 'none active'],
      ['Resolution', d.fb_width ? d.fb_width + '×' + d.fb_height + (d.rotate ? ' rotated ' + d.rotate + '° to ' + d.width + '×' + d.height : '') : ''],
      ['Showing', d.mode ? (MODE_LABELS[d.mode] || d.mode) + ' (rev ' + (d.rev || 0) + ')' + (d.ready ? '' : ' · loading ' + (d.loaded || 0) + '/' + (d.total || 0)) : ''],
      ['Blanked', d.blanked ? 'yes' + (d.blank_reason ? ' (' + d.blank_reason + ')' : '') + (d.blank_method ? ' via ' + d.blank_method : '') : 'no'],
      ['On screen', d.active ? (d.foreground ? 'yes' : 'no (another console is active)') : ''],
      ['Render time', d.render_ms ? d.render_ms + ' ms' : ''],
      ['Media errors', (d.media_errors || []).length ? h('ul', null, d.media_errors.map((e) => h('li', null, e))) : ''],
    ]));
}

function sandboxPanel(n) {
  return panel('Sandbox',
    kv([
      ['Mode', n.sandbox || 'unknown'],
      ['Full isolation', n.full_isolation ? badge('yes', 'ok') : badge('no', 'warn', 'Only jobs with isolation=any run here')],
      ['Available', (n.sandbox_caps || []).join(' ') || 'none'],
      ['Scratch space', n.scratch_in_ram ? 'RAM (disk use counts against memory)' : 'disk'],
    ]));
}

function inventoryPanel(i) {
  const yn = (b) => (b ? 'yes' : 'no');
  const nicCells = (c) => [c.name, h('code', null, c.mac || ''), c.wireless ? 'Wi-Fi' : 'wired', [c.bus, c.driver].filter(Boolean).join(' / '),
    (c.up ? 'up' : 'down') + (c.carrier ? ', link' : '') + (c.speed_mbps ? ', ' + c.speed_mbps + ' Mb/s' : ''),
    (c.addrs || []).join(' '), c.error ? h('span', {class: 'text-bad'}, c.error) : ''];
  return panel('Hardware',
    kv([
      ['Machine', [i.vendor, i.product].filter(Boolean).join(' ') + (i.is_laptop ? ' (laptop)' : '') + (i.virtualized ? ' (virtual)' : '')],
      ['BIOS date', i.bios_date], ['Hostname', i.hostname],
      ['CPU', (i.cpu_model || 'unknown') + (i.cpu_vendor ? ' · ' + i.cpu_vendor : '')],
      ['Cores', (i.cores || 0) + ' logical' + (i.physical_cores ? ', ' + i.physical_cores + ' physical' : '') + (i.cpu_mhz ? ' · ' + i.cpu_mhz + ' MHz' : '')],
      ['CPU flags', (i.cpu_flags || []).join(' ')],
      ['Speed', i.bench_score ? i.bench_score + ' MB/s SHA-256 (single core)' : ''],
      ['Memory', fmt.mb(i.mem_total_mb) + (i.swap_total_mb ? ' + ' + fmt.mb(i.swap_total_mb) + ' swap' : '')],
      ['Architecture', [i.arch, i.machine].filter(Boolean).join(' / ')],
      ['Kernel', i.kernel], ['SaviorOS', i.os_version], ['Temperature sensor', i.temp_sensor || 'none'],
      ['Battery', yn(i.has_battery)],
    ]),
    h('h3', null, 'Disks'),
    table(['Name', 'Size', 'Model', 'Type', 'Removable', 'Bus'], (i.disks || []).map((d) =>
      [d.name, fmt.mb(d.size_mb), d.model || '', d.rotational ? 'spinning' : 'solid state', yn(d.removable), d.transport || '']), 'No disks found.'),
    h('h3', null, 'Network interfaces'),
    table(['Name', 'MAC', 'Type', 'Bus / driver', 'Link', 'Addresses', 'Problem'], (i.nics || []).map(nicCells), 'No network interfaces reported.'),
    h('h3', null, 'Graphics'),
    table(['Card', 'Driver', 'PCI ID'], (i.gpus || []).map((g) => [g.card, g.driver, [g.vendor, g.device].filter(Boolean).join(':')]), 'No GPUs found.'),
    table(['Framebuffer', 'Driver', 'Size', 'Depth'], (i.framebuffers || []).map((f) => [f.name, f.driver, f.width + '×' + f.height, f.bpp + ' bpp']), 'No framebuffers.'),
    table(['Output', 'Card', 'Status', 'Enabled', 'Preferred mode', 'Size'], (i.connectors || []).map((c) =>
      [c.name, c.card, c.status, yn(c.enabled), c.preferred || '', c.width_mm ? c.width_mm + '×' + c.height_mm + ' mm' : '']), 'No display outputs.'));
}

export async function detail(root, ctx, id) {
  const path = '/admin/nodes/' + api.enc(id);
  const sig = {signal: ctx.signal};
  let node = await api.get(path, sig);
  const refresh = async () => {
    node = await api.get(path, sig);
    draw();
  };
  const act = async (p, msg) => {
    await p;
    if (msg) toast(msg, 'ok');
    await refresh();
  };
  const patchNode = (body, msg) => act(api.patch(path, body, sig), msg);
  const action = (a, extra, msg) => act(api.post(path + '/action', Object.assign({action: a}, extra), sig), msg);
  const setDis = (b, v) => {
    if (!b.dataset.busy) b.disabled = v;
  };

  // Actions.
  const secs = select([[10, '10 s'], [30, '30 s'], [60, '1 min'], [300, '5 min']], 30, {'aria-label': 'How long to identify'});
  const bIdent = btn('Identify', () => action('identify', {seconds: Number(secs.value)}, 'The screen shows its short code now.'));
  const rot = select([[0, '0°'], [90, '90°'], [180, '180°'], [270, '270°']], node.display_rotate, {'aria-label': 'Screen rotation'});
  let rotDirty = false;
  rot.addEventListener('change', () => { rotDirty = true; });
  const bRot = btn('Rotate screen', () => patchNode({display_rotate: Number(rot.value)}, 'Rotation saved.').then(() => { rotDirty = false; }));
  const bDrain = btn('Drain', () => patchNode({drain: !node.drain}, node.drain ? 'The node takes tasks again.' : 'Draining: running tasks finish, no new ones start.'));
  const bApprove = btn('Approve', () => patchNode({approved: true}, 'Approved.'), 'btn-primary');
  const bClearQ = btn('Clear quarantine', () => patchNode({clear_quarantine: true}, 'Quarantine cleared.'));
  const bReboot = btn('Reboot', () => window.confirm('Reboot ' + node.name + '? Running tasks are requeued.') &&
    action('reboot', {}, 'Reboot requested.'), 'btn-danger');
  const bPower = btn('Power off', () => window.confirm('Power off ' + node.name + '? Someone has to press its power button to bring it back.') &&
    action('poweroff', {}, 'Power off requested.'), 'btn-danger');
  const bForget = btn('Forget', async () => {
    if (!window.confirm('Forget ' + node.name + '? Its name, labels and display settings are deleted. It joins as new if it comes back.')) return;
    await api.del(path, sig);
    toast('Forgot ' + node.name + '.', 'ok');
    ctx.go('#/nodes');
  }, 'btn-danger');

  // Rename and labels.
  const nameIn = h('input', {type: 'text', value: node.name, maxLength: 32, spellcheck: false, autocapitalize: 'none', autocomplete: 'off'});
  nameIn.addEventListener('input', () => {
    nameIn.setAttribute('aria-invalid', String(!validNodeName(nameIn.value.trim().toLowerCase())));
  });
  const bName = btn('Rename', () => {
    const v = nameIn.value.trim().toLowerCase();
    if (!validNodeName(v)) throw new Error('Use 1 to 32 lowercase letters, digits and inner dashes; names can\'t look like a node ID (n + 12 hex digits).');
    return patchNode({name: v}, 'Renamed to ' + v + '.');
  });
  const labelsIn = h('textarea', {rows: 3, value: kvText(node.admin_labels), spellcheck: false, placeholder: 'room=lab-2'});
  const bLabels = btn('Save labels', () => {
    const r = parseKV(labelsIn.value, 'Labels', validLabelKey, true);
    if (r.error) throw new Error(r.error);
    if (r.count > LIMITS.labels) throw new Error('At most ' + LIMITS.labels + ' labels.');
    return patchNode({labels: r.map}, 'Labels saved.');
  });
  const cfgLabels = h('p', {class: 'hint'});

  // Display editor, rebuilt on reset and when wall membership changes.
  const dispBox = h('div');
  let editor = null;
  let editorWall = null;
  const buildEditor = () => {
    editorWall = node.wall_id || '';
    if (editorWall) {
      editor = null;
      mount(dispBox, banner('info', 'This screen is part of a video wall. ', h('a', {href: '#/walls/' + api.enc(editorWall)}, 'Edit the wall'), ' to change what it shows.'));
      return;
    }
    editor = displayEditor(node.display, {modes: NODE_MODES, signal: ctx.signal});
    mount(dispBox, editor.el, h('div', {class: 'btn-row'},
      btn('Save display', () => patchNode({display: editor.get()}, 'Display saved. The screen changes within a few seconds.'), 'btn-primary'),
      btn('Reset form', buildEditor)),
    (node.roles || []).indexOf('display') < 0 ? h('p', {class: 'hint'}, 'This node doesn\'t have the display role right now; the setting applies once it does.') : null);
  };
  buildEditor();

  const head = h('div');
  const secs2 = {status: h('div'), tasks: h('div'), screen: h('div'), sandbox: h('div'), hw: h('div')};
  const memos = {};
  for (const k of Object.keys(secs2)) memos[k] = memo(secs2[k]);
  const headMemo = memo(head);

  const draw = () => {
    const on = online(node);
    ctx.title(node.name);
    headMemo(headData(node), headPanel);
    memos.status(node, statusPanel);
    memos.tasks(node, tasksPanel);
    memos.screen(node, displayStatePanel);
    memos.sandbox(node, sandboxPanel);
    memos.hw(inv(node), inventoryPanel);
    bDrain.textContent = node.drain ? 'Stop draining' : 'Drain';
    bApprove.hidden = !!node.approved;
    bClearQ.hidden = !node.quarantine;
    bForget.hidden = on;
    for (const b of [bIdent, bReboot, bPower]) setDis(b, !on);
    if (!rotDirty) rot.value = String(node.display_rotate || 0);
    const cl = kvText(node.config_labels);
    cfgLabels.textContent = cl ? 'From the node\'s savior.conf (admin labels override these): ' + cl.replace(/\n/g, ', ') : '';
    if ((node.wall_id || '') !== editorWall) buildEditor();
  };

  mount(root, h('p', {class: 'crumbs'}, h('a', {href: '#/nodes'}, '← All nodes')), head,
    panel('Actions',
      h('div', {class: 'btn-row'}, bApprove, bClearQ, h('span', {class: 'group'}, bIdent, secs), h('span', {class: 'group'}, bRot, rot), bDrain, bReboot, bPower, bForget),
      h('p', {class: 'hint'}, 'Identify flashes the short code on the screen and beeps. Reboot and power off happen after the node\'s next heartbeat.')),
    panel('Name and labels',
      h('div', {class: 'form-grid'},
        field('Name', nameIn, 'Lowercase letters, digits and dashes, up to 32 characters.'),
        field('Admin labels', labelsIn, 'One key=value per line. Jobs can require labels.')),
      cfgLabels, h('div', {class: 'btn-row'}, bName, bLabels)),
    panel('What the screen shows', dispBox),
    secs2.status, secs2.tasks, secs2.screen, secs2.sandbox, secs2.hw);
  draw();
  ctx.poll(refresh, 5000);
}
