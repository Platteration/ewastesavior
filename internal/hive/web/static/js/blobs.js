// Blobs: the hive's content-addressed file store (inputs, outputs, images).

import {h, mount, table, btn, badge, toast, reportError, copyText} from './dom.js';
import * as api from './api.js';
import * as fmt from './fmt.js';

const SHOW = 200;

export async function render(root, ctx) {
  ctx.title('Blobs');
  const sig = {signal: ctx.signal};
  let blobs = [];
  let limit = SHOW;
  const summary = h('p', {class: 'muted', role: 'status'});
  const box = h('div');
  const more = btn('Show more', () => {
    limit += SHOW;
    draw();
  });
  const status = h('span', {class: 'hint', role: 'status'});
  const file = h('input', {type: 'file', multiple: true, 'aria-label': 'Upload files'});

  const draw = () => {
    const total = blobs.reduce((a, b) => a + (b.size || 0), 0);
    const unref = blobs.filter((b) => !b.referenced).length;
    summary.textContent = fmt.plural(blobs.length, 'blob') + ', ' + fmt.bytes(total) + ' · ' + unref + ' not referenced';
    mount(box, table(['Hash', 'Size', 'In use', 'Last used', h('th', {scope: 'col', class: 'opt'}, 'Stored'), ''],
      blobs.slice(0, limit).map((b) => [
        h('span', {class: 'copyable'}, h('code', {title: b.sha256}, fmt.shortHash(b.sha256)),
          btn('Copy', () => copyText(b.sha256), 'btn-small', {'aria-label': 'Copy hash ' + fmt.shortHash(b.sha256)})),
        fmt.bytes(b.size),
        b.referenced ? badge('referenced', 'info') : badge('unused', 'neutral'),
        fmt.ago(b.last_touched),
        h('td', {class: 'opt'}, fmt.time(b.created_at)),
        h('span', {class: 'btn-row'},
          h('a', {class: 'btn btn-small', href: api.BASE + '/blobs/' + api.enc(b.sha256), download: b.sha256 + '.bin'}, 'Download'),
          btn('Delete', async () => {
            if (!window.confirm('Delete blob ' + fmt.shortHash(b.sha256) + '?')) return;
            await api.del('/admin/blobs/' + api.enc(b.sha256), sig);
            toast('Blob deleted.', 'ok');
            await load();
          }, 'btn-small btn-danger')),
      ]), 'The blob store is empty.'));
    more.hidden = blobs.length <= limit;
  };
  const load = async () => {
    blobs = ((await api.get('/admin/blobs', sig)) || []).slice();
    blobs.sort((a, b) => String(b.last_touched).localeCompare(String(a.last_touched)));
    draw();
  };
  file.addEventListener('change', async () => {
    const files = Array.prototype.slice.call(file.files || []);
    file.value = '';
    file.disabled = true;
    try {
      for (const f of files) {
        status.textContent = 'Uploading ' + f.name + '…';
        const info = await api.upload(f, (done, total) => {
          status.textContent = 'Uploading ' + f.name + ': ' + Math.floor(done * 100 / total) + '%';
        }, ctx.signal);
        toast('Uploaded ' + f.name + ' as ' + fmt.shortHash(info.sha256) + '.', 'ok');
      }
      status.textContent = '';
      await load();
    } catch (e) {
      status.textContent = '';
      reportError(e);
    } finally {
      file.disabled = false;
    }
  });
  const gc = btn('Collect garbage', async () => {
    const r = (await api.post('/admin/blobs/gc', {}, sig)) || {};
    toast('Deleted ' + fmt.plural(r.deleted || 0, 'blob') + ', freed ' + fmt.bytes(r.freed_bytes || 0) + '.', 'ok');
    await load();
  });

  mount(root, h('div', {class: 'page-head'}, h('h1', null, 'Blobs'), h('span', {class: 'btn-row'}, gc, btn('Refresh', load))),
    h('p', {class: 'hint'}, 'Files stored on the hive by SHA-256: job inputs and outputs and display images. ' +
      'Garbage collection removes blobs that nothing references and that were not used in the last hour. A referenced blob can\'t be deleted.'),
    h('div', {class: 'toolbar'}, h('label', {class: 'btn'}, 'Upload files…', file), status),
    summary, box, h('div', {class: 'btn-row'}, more));
  await load();
}
