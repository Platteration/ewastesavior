// Settings and help: pairing, certificate check, command line, sessions.

import {h, mount, btn, kv, panel, copyable} from './dom.js';
import * as api from './api.js';
import * as fmt from './fmt.js';

const CTL_HELP = [
  ['savior ctl pair', 'print a pairing code for signing in a browser'],
  ['savior ctl node-config', 'print savior.conf for new nodes (swarm key, hive address and fingerprint)'],
  ['savior ctl genkey', 'make a new random swarm key'],
  ['savior ctl help', 'list every command: nodes, jobs, displays, walls, blobs'],
];

export async function render(root, ctx) {
  ctx.title('Settings');
  const info = (await api.get('/admin/info', {signal: ctx.signal})) || {};
  const pairBox = h('div', {role: 'status'});
  const pair = btn('Create a pairing code', async () => {
    const p = await api.post('/admin/pair', {}, {signal: ctx.signal});
    mount(pairBox, h('p', {class: 'pair-code'}, p.code),
      h('p', {class: 'hint'}, 'Valid until ' + fmt.time(p.expires_at) + ', for one sign-in.'));
  });
  mount(root, h('h1', null, 'Settings and help'),
    panel('Sign in another device',
      h('p', null, 'Open this dashboard on the other device (a phone works) and enter a pairing code. Codes last 10 minutes and work once. The hive\'s own screen always shows a current one.'),
      pair, pairBox),
    panel('Check the hive\'s certificate',
      h('p', null, 'The hive uses its own self-signed certificate, so browsers warn the first time. Before you accept, compare the SHA-256 fingerprint in the browser\'s certificate viewer with the one on the hive\'s screen:'),
      info.fingerprint ? copyable(info.fingerprint, 'fingerprint') : h('p', {class: 'muted'}, 'unknown')),
    panel('Command line',
      h('p', null, 'The same binary controls the hive from a terminal. Run it on any computer on the network:'),
      h('dl', {class: 'cli'}, CTL_HELP.map((c) => [h('dt', null, h('code', null, c[0])), h('dd', null, c[1])])),
      h('p', {class: 'hint'}, 'ctl signs in with a proof of the admin token, so the token itself never crosses the network.')),
    panel('Sessions',
      h('p', null, 'Browser sessions last 12 hours.'),
      h('div', {class: 'btn-row'},
        btn('Sign out', () => ctx.signOut()),
        btn('Sign out everywhere', async () => {
          if (!window.confirm('Sign out every browser and ctl session, including this one?')) return;
          await api.post('/admin/sessions/revoke', {}, {signal: ctx.signal});
          ctx.signedOut('Every session was signed out.');
        }, 'btn-danger'))),
    panel('About this hive',
      kv([['Version', info.version], ['API version', info.api_version != null ? String(info.api_version) : ''], ['Hive ID', info.hive_id],
        ['Running since', fmt.time(info.started_at)], ['Data directory', info.data_dir], ['Listening on', info.listen]])));
}
