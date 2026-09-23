// Overview: swarm statistics, hive information and how to add machines.

import {h, mount, btn, kv, panel, banner, field, select, saveFile, copyable, memo} from './dom.js';
import * as api from './api.js';
import * as fmt from './fmt.js';
import {hostPort} from './model.js';
import {identifyAll} from './nodes.js';

function card(label, value, sub, extra) {
  return h('div', {class: 'card'}, h('div', {class: 'card-label'}, label), h('div', {class: 'card-value'}, value),
    sub ? h('div', {class: 'hint'}, sub) : null, extra || null);
}

function meter(frac, label) {
  const f = Math.max(0, Math.min(1, frac || 0));
  return h('div', {class: 'bar', role: 'meter', 'aria-valuemin': 0, 'aria-valuemax': 100, 'aria-valuenow': Math.round(f * 100), 'aria-label': label},
    h('span', {class: 'seg seg-running', style: {width: (f * 100) + '%'}}));
}

function counts(map, order) {
  map = map || {};
  const keys = order.filter((k) => map[k]).concat(Object.keys(map).filter((k) => order.indexOf(k) < 0 && map[k]));
  return keys.length ? keys.map((k) => map[k] + ' ' + k).join(' · ') : 'none';
}

function statsCards(s) {
  return [
    card('Nodes online', String(s.nodes_online || 0), (s.nodes_offline || 0) + ' offline · ' + counts(s.nodes_by_state, ['idle', 'busy', 'paused', 'draining'])),
    card('Cores in use', fmt.num(s.cores_in_use, 1) + ' / ' + fmt.num(s.cores_allocatable, 1),
      (s.cores || 0) + ' logical CPUs online', meter(s.cores_allocatable ? s.cores_in_use / s.cores_allocatable : 0, 'Cores in use')),
    card('Memory', fmt.mb(s.mem_mb), 'on online nodes'),
    card('Displays', String(s.displays || 0), 'screens being driven'),
    card('Tasks', String(s.tasks_completed || 0) + ' done', counts(s.tasks_by_state, ['pending', 'assigned', 'running', 'succeeded', 'failed', 'canceled'])),
    card('Jobs', counts(s.jobs_by_state, ['queued', 'running', 'succeeded', 'failed', 'canceled'])),
    card('CPU time', fmt.dur(s.cpu_seconds_total), 'used by tasks'),
    card('Speed', (s.bench_total || 0) + ' MB/s', 'sum of single-core SHA-256 scores'),
  ];
}

function infoBanners(i) {
  const out = [];
  if (!i.persistent) {
    out.push(banner('warn', h('strong', null, 'State is kept in memory only. '),
      'Jobs, node names, displays and walls are lost when the hive restarts. On a SaviorOS stick, leave at least 256 MB unpartitioned after the boot partition so the hive can create its data partition.'));
  }
  for (const w of i.warnings || []) out.push(banner('warn', w));
  return out;
}

function infoList(i) {
  return kv([
    ['Version', i.version + (i.api_version ? ' (API ' + i.api_version + ')' : '')],
    ['Fingerprint', i.fingerprint ? copyable(i.fingerprint, 'fingerprint') : ''],
    ['Addresses', (i.urls || []).length ? h('ul', {class: 'plain'}, i.urls.map((u) => h('li', null, h('a', {href: u}, u)))) : ''],
    ['Join policy', i.join_policy === 'approve' ? 'approve: new machines wait for approval' : 'open: machines with the swarm key join directly'],
    ['Clock', (i.time_synced ? 'synced' : 'not verified') + (i.time_source ? ' (' + i.time_source + ')' : '')],
    ['Running since', fmt.time(i.started_at)],
    ['Data', (i.data_dir || '') + (i.persistent ? '' : ' (in memory)')],
    ['Hive ID', i.hive_id],
  ]);
}

function addMachine(i, ctx) {
  const addrs = [['auto', 'Find the hive automatically (LAN broadcast)']].concat(
    (i.urls || []).map(hostPort).filter(Boolean).map((a) => [a, 'Fixed address ' + a]));
  const hiveSel = select(addrs, 'auto', {class: 'auto'});
  const download = btn('Download savior.conf for new nodes', async () => {
    const text = await api.get('/admin/node-config', {query: {hive: hiveSel.value}, as: 'text', signal: ctx.signal});
    saveFile(new Blob([text], {type: 'text/plain'}), 'savior.conf');
  }, 'btn-primary');
  return panel('Add a machine',
    h('ol', {class: 'steps'},
      h('li', null, 'Write the SaviorOS image (savior.img) to a USB stick with a raw image writer such as dd, balenaEtcher or Rufus in DD mode. Use savior.iso for machines that only boot from CD.'),
      h('li', null, 'Download savior.conf below and copy it onto the stick\'s SAVIOR partition, replacing the one there. It holds the swarm key and pins this hive\'s fingerprint.'),
      h('li', null, 'Boot the old machine from the stick (the boot menu key is often F12, F10, F9 or Esc). It shows up under Nodes within a minute.' +
        (i.join_policy === 'approve' ? ' This hive requires approval: approve it on its node page.' : ''))),
    h('p', {class: 'hint'}, 'One stick works for any number of machines, one after another. Nothing is written to their disks.'),
    h('div', {class: 'form-grid'}, field('Hive address in the file', hiveSel,
      'Automatic works on one network segment. Choose a fixed address if nodes are on another subnet.')),
    h('div', {class: 'btn-row'}, download, btn('Identify all screens', () => identifyAll(ctx.signal))),
    h('p', {class: 'hint'}, 'savior.conf contains the swarm key: anyone with it can join machines to your swarm. Keep it like a password.'));
}

export async function render(root, ctx) {
  ctx.title('Overview');
  const sig = {signal: ctx.signal};
  const r = await Promise.all([api.get('/stats', sig), api.get('/admin/info', sig)]);
  const stats = h('div', {class: 'cards'});
  const banners = h('div');
  const info = h('div');
  const drawBanners = memo(banners);
  const drawInfo = memo(info);
  const draw = (s, i) => {
    mount(stats, statsCards(s || {}));
    drawBanners(i || {}, infoBanners);
    drawInfo(i || {}, infoList);
  };
  draw(r[0], r[1]);
  mount(root, h('h1', null, 'Overview'), banners, panel('Swarm', stats), addMachine(r[1] || {}, ctx), panel('Hive', info));
  ctx.poll(async () => {
    const x = await Promise.all([api.get('/stats', sig), api.get('/admin/info', sig)]);
    draw(x[0], x[1]);
  }, 5000);
}
