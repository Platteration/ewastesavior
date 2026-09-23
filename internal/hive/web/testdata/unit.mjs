// Unit tests for the dashboard's pure modules and DOM builder, run by
// TestJSUnit (node testdata/unit.mjs). The DOM is a small stub: enough to
// check that h() only ever creates text nodes and vets URLs.

import assert from 'node:assert/strict';

// --- Minimal DOM stub -----------------------------------------------------
class StubNode {
  constructor(type) {
    this.nodeType = type;
    this.childNodes = [];
    this.parentNode = null;
  }
  appendChild(c) {
    c.parentNode = this;
    this.childNodes.push(c);
    return c;
  }
  removeChild(c) {
    this.childNodes = this.childNodes.filter((x) => x !== c);
    c.parentNode = null;
    return c;
  }
  get firstChild() {
    return this.childNodes[0] || null;
  }
  get textContent() {
    return this.childNodes.map((c) => c.textContent).join('');
  }
}
class StubText extends StubNode {
  constructor(data) {
    super(3);
    this.data = data;
  }
  get textContent() {
    return this.data;
  }
}
class StubElement extends StubNode {
  constructor(tag) {
    super(1);
    this.tagName = tag.toUpperCase();
    this.attributes = {};
    this.listeners = {};
    this.style = {};
    this.dataset = {};
    this.className = '';
    const self = this;
    this.classList = {add(c) { self.className = (self.className + ' ' + c).trim(); }};
  }
  setAttribute(k, v) {
    this.attributes[k] = String(v);
  }
  getAttribute(k) {
    return k in this.attributes ? this.attributes[k] : null;
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  get children() {
    return this.childNodes.filter((c) => c.nodeType === 1);
  }
  get textContent() {
    return super.textContent;
  }
  set textContent(v) {
    this.childNodes = [];
    this.appendChild(new StubText(String(v)));
  }
}
const toastBox = new StubElement('div');
globalThis.document = {
  createElement: (t) => new StubElement(t),
  createTextNode: (d) => new StubText(d),
  getElementById: (id) => (id === 'toasts' ? toastBox : null),
};

const model = await import('../static/js/model.js');
const fmt = await import('../static/js/fmt.js');
const api = await import('../static/js/api.js');
const dom = await import('../static/js/dom.js');

let checks = 0;
const eq = (got, want, msg) => {
  checks++;
  assert.deepStrictEqual(got, want, msg);
};
const ok = (v, msg) => eq(!!v, true, msg);
const no = (v, msg) => eq(!!v, false, msg);

// --- model: validation mirroring internal/proto ---------------------------
for (const s of ['lab-1', 'a', 'pc42', 'a'.repeat(32), 'n0123456789abc', 'n012345678ab']) ok(model.validNodeName(s), 'valid name ' + s);
for (const s of ['', 'Lab', '-a', 'a-', 'a_b', 'a'.repeat(33), 'n0123456789ab', 'a b', 'é']) no(model.validNodeName(s), 'invalid name ' + s);

for (const k of ['room', 'a.b/c-d_e', '0']) ok(model.validLabelKey(k), 'label key ' + k);
for (const k of ['', 'Room', '_x', '-x', 'a'.repeat(64)]) no(model.validLabelKey(k), 'bad label key ' + k);

ok(model.validEnvKey('MODE'), 'env MODE');
ok(model.validEnvKey('_x1'), 'env _x1');
no(model.validEnvKey('SAVIOR_X'), 'env SAVIOR_ reserved');
no(model.validEnvKey('1A'), 'env digit first');
no(model.validEnvKey('A-B'), 'env dash');

for (const p of ['a', 'a/b.txt', 'x/.savior', 'é/ü', 'out/*.png', '.bashrc']) ok(model.validRelPath(p), 'rel path ' + p);
for (const p of ['', '/abs', '../x', 'a/../b', './a', 'a//b', '-x', 'a/-x', 'x.', 'x ', 'a\\b', 'c:x', '.savior-script',
  '.saviorx/a', 'a\u0001b', 'a\u007fb', 'x'.repeat(256), 'é'.repeat(128), '\ud800']) no(model.validRelPath(p), 'bad rel path ' + JSON.stringify(p));
ok(model.validRelPath('é'.repeat(127)), '254 bytes is fine');
for (const [str, n] of [['', 0], ['a', 1], ['é', 2], ['✓', 3], ['😀', 4], ['\ud800', 3], ['aé✓😀', 10]]) {
  eq(model.utf8Len(str), n, 'utf8Len ' + JSON.stringify(str));
  if (!str.includes('\ud800')) eq(model.utf8Len(str), Buffer.byteLength(str), 'utf8Len matches Buffer');
}

// Expected values are Go's: path.Match(pattern, "") == nil.
const globs = {'out/*.png': true, '[abc]': true, '[^a]x': true, '[a-z]x': true, '*': true, '?': true, 'a[b]c': true, '[]a]': false,
  '[]': false, '[a-]': false, '[-a]': false, '[a': false, '[^]': false, 'x[': false, '[z-a]': true, '[a-b-c]': false,
  '[ab-]': false, 'a]b': true, '[^^]': true, '[a]]': true};
for (const p of Object.keys(globs)) eq(model.validGlob(p), globs[p], 'glob ' + p);
ok(model.validOutputPattern('out/*.png'), 'output pattern');
no(model.validOutputPattern('../*.png'), 'output pattern escaping');
no(model.validOutputPattern('out/[x'), 'output pattern bad glob');

ok(model.validSHA256('a'.repeat(64)), 'sha');
no(model.validSHA256('A'.repeat(64)), 'sha uppercase');
no(model.validSHA256('a'.repeat(63)), 'sha short');
ok(model.validColor('#fff') && model.validColor('#A0b1C2'), 'colors');
no(model.validColor('#ff') || model.validColor('fff') || model.validColor('#ggg') || model.validColor('red'), 'bad colors');
ok(model.validHTTPURL('https://x.org/a.png') && model.validHTTPURL('http://10.0.0.1/a'), 'http urls');
no(model.validHTTPURL('ftp://x') || model.validHTTPURL('https:// x') || model.validHTTPURL('https://' + 'a'.repeat(2048)), 'bad urls');

// --- model: parsing -------------------------------------------------------
{
  const r = model.parseKV('a=1\n# comment\n\n b = 2 \r\nc==x', 'Labels', null, true);
  eq(r.count, 3, 'kv count');
  eq(r.map.a, '1');
  eq(r.map.b, '2');
  eq(r.map.c, '=x', 'value keeps later =');
  eq(model.parseKV('a=1\nnovalue', 'Labels').error, 'Labels, line 2: expected key=value');
  eq(model.parseKV('Bad=1', 'Labels', model.validLabelKey, true).error, 'Labels, line 1: invalid key "Bad"');
  eq(model.parseKV('a=1\na=2', 'x').count, 1, 'duplicate keys count once');
  eq(model.parseKV('K= spaced ', 'Env', model.validEnvKey, false).map.K, ' spaced ', 'env values untrimmed');
  const p = model.parseKV('__proto__=x\nconstructor=y', 'Env', model.validEnvKey, false);
  eq(Object.getPrototypeOf(p.map), null, 'prototype-free map');
  eq(JSON.stringify(p.map), '{"__proto__":"x","constructor":"y"}', 'proto key is plain data');
  eq({}.x, undefined, 'Object.prototype untouched');
  eq(model.kvText({b: '2', a: '1'}), 'a=1\nb=2');
  eq(model.kvText(undefined), '');
}

{
  const s = (x) => model.splitArgs(x);
  eq(s('a b  c'), {args: ['a', 'b', 'c']});
  eq(s('  '), {args: []});
  eq(s('\'a b\' "c \\"d\\"" e\\ f'), {args: ['a b', 'c "d"', 'e f']});
  eq(s('""'), {args: ['']});
  eq(s('a\'b\'c'), {args: ['abc']});
  eq(s('"$HOME" \'$x\''), {args: ['$HOME', '$x']}, 'no expansion');
  eq(s('"a\\nb"'), {args: ['a\\nb']}, 'other escapes kept in double quotes');
  eq(s('echo {{index}}'), {args: ['echo', '{{index}}']});
  eq(s('a\\'), {args: ['a']}, 'trailing backslash');
  eq(s('"open'), {error: 'unterminated double quote'});
  eq(s('\'open'), {error: 'unterminated single quote'});
}

eq(model.normalizePairCode(' 7k3m-9qxd '), '7K3M9QXD');
eq(model.normalizePairCode('oil0'), '0110', 'Crockford aliases');
ok(model.validPairCode('7K3M9QXD'), 'pair code');
no(model.validPairCode('ABCDEFGU'), 'U is not Crockford');
no(model.validPairCode('ABC'), 'short code');

eq(model.fileName('a/b/c.txt'), 'c.txt');
eq(model.fileName('..'), 'download');
eq(model.fileName('x<y>:"z".txt'), 'x_y___z_.txt');
eq(model.inputName('../../etc/passwd'), 'passwd');
eq(model.inputName('C:\\x\\y.txt'), 'C__x_y.txt');
eq(model.inputName('-rf'), 'rf');
eq(model.inputName('.savior-script'), '_.savior-script');
eq(model.inputName('.bashrc'), '.bashrc');
eq(model.inputName('name. '), 'name');
eq(model.inputName('..'), '');
eq(model.hostPort('https://192.168.1.20:7700/'), '192.168.1.20:7700');
eq(model.hostPort('https://[fe80::1]:7700'), '[fe80::1]:7700');
eq(model.hostPort('nonsense'), '');

// --- model: display specs -------------------------------------------------
{
  const c = (s, modes) => model.checkDisplaySpec(s, modes);
  const sha = 'b'.repeat(64);
  eq(c({mode: 'text', text: 'hi'}), '');
  eq(c({mode: 'status'}), '');
  ok(c({mode: 'bogus'}), 'unknown mode');
  ok(c({mode: 'status'}, model.WALL_CONTENT_MODES), 'mode not allowed in walls');
  ok(c({mode: 'image'}), 'image needs media');
  ok(c({mode: 'image', image: {blob: 'x'}}), 'bad blob');
  eq(c({mode: 'image', image: {blob: sha}, fit: 'cover'}), '');
  ok(c({mode: 'image', image: {blob: sha, url: 'https://x/'}}), 'both blob and url');
  ok(c({mode: 'image', image: {url: 'ftp://x/'}}), 'non-http url');
  ok(c({mode: 'image', image: {url: 'https://x/', sha256: 'nothex'}}), 'bad url sha');
  ok(c({mode: 'slideshow', images: []}), 'empty slideshow');
  eq(c({mode: 'slideshow', images: [{blob: sha}], interval_s: 3}), '');
  eq(c({mode: 'slideshow', images: [{blob: sha}], interval_s: 0}), '', 'interval 0 = default');
  ok(c({mode: 'slideshow', images: [{blob: sha}], interval_s: 2}), 'interval too short');
  ok(c({mode: 'slideshow', images: [{blob: sha}], interval_s: 86401}), 'interval too long');
  ok(c({mode: 'slideshow', images: new Array(101).fill({blob: sha})}), 'too many images');
  ok(c({mode: 'text', fg: '#12'}), 'bad color');
  ok(c({mode: 'image', image: {blob: sha}, fit: 'fill'}), 'bad fit');
  ok(c({mode: 'text', text: 'é'.repeat(2049)}), 'text over 4096 bytes');
  eq(c({mode: 'text', text: 'é'.repeat(2048)}), '', 'text of 4096 bytes');
  ok(c({mode: 'text', title: 'x'.repeat(257)}), 'title too long');
  ok(c({mode: 'clock', clock_format: 'x'.repeat(65)}), 'clock format too long');
}

// --- model: wall layout ---------------------------------------------------
{
  const conn = (w, h, status) => ({inventory: {connectors: [{status: status || 'connected', width_mm: w, height_mm: h}]}});
  const nodes = {a: conn(400, 250), b: {inventory: {}}, c: Object.assign(conn(510, 290), {display_rotate: 90}), d: conn(300, 200, 'disconnected')};
  eq(model.screenSizeMM(nodes.a), {w: 400, h: 250});
  eq(model.screenSizeMM(nodes.b), null);
  eq(model.screenSizeMM(nodes.c), {w: 290, h: 510}, 'rotated monitor');
  eq(model.screenSizeMM(nodes.d), null, 'disconnected output ignored');
  eq(model.screenSizeMM(undefined), null);

  let L = model.wallLayout({rows: 1, cols: 2, gap_x_mm: 20, cells: [{node: 'a', row: 0, col: 0}, {node: 'b', row: 0, col: 1}]}, nodes);
  eq([L.w, L.h], [820, 300], 'canvas');
  eq(L.tiles.map((t) => [t.x, t.y, t.w, t.h]), [[0, 25, 400, 250], [420, 0, 400, 300]], 'tiles centered in slots');
  eq(L.slots.length, 2);

  L = model.wallLayout({rows: 2, cols: 1, gap_y_mm: 10, cells: [{node: 'c', row: 0, col: 0}, {node: 'a', row: 1, col: 0, width_mm: 500}]}, nodes);
  eq([L.w, L.h], [500, 770], 'rotated + override canvas');
  eq(L.tiles.map((t) => [t.x, t.y, t.w, t.h]), [[105, 0, 290, 510], [0, 520, 500, 250]]);

  L = model.wallLayout({rows: 1, cols: 3, gap_x_mm: 10, cells: [{node: 'b', row: 0, col: 1}]}, nodes);
  eq([L.w, L.h], [1220, 300], 'empty columns keep the default size');
  eq([L.tiles[0].x, L.tiles[0].y], [410, 0]);

  L = model.wallLayout({rows: 1, cols: 1, cells: [{node: 'b', row: 0, col: 0, rect: {x: 1000, y: 0, w: 100, h: 100}}]}, nodes);
  eq([L.w, L.h], [1100, 300], 'rect cells placed exactly');
  eq([L.tiles[0].x, L.tiles[0].w], [1000, 100]);

  L = model.wallLayout({rows: 1, cols: 1, cells: [{node: 'a', row: 3, col: 0}, {node: 'b', row: 0, col: 0}]}, nodes);
  eq(L.tiles.length, 1, 'cells outside the grid are ignored');
  ok(L.tiles.every((t) => [t.x, t.y, t.w, t.h].every(Number.isFinite)), 'finite geometry');
}

// --- fmt ------------------------------------------------------------------
eq(fmt.bytes(0), '0 B');
eq(fmt.bytes(1023), '1023 B');
eq(fmt.bytes(1536), '1.5 KB');
eq(fmt.bytes(1048576), '1 MB');
eq(fmt.bytes(150 * 1024), '150 KB');
eq(fmt.mb(512), '512 MB');
eq(fmt.mb(3072), '3 GB');
eq(fmt.dur(59), '59 s');
eq(fmt.dur(61), '1 m 1 s');
eq(fmt.dur(3700), '1 h 1 m');
eq(fmt.dur(90000), '1 d 1 h');
eq(fmt.dur(-5), '0 s');
ok(fmt.zeroTime('0001-01-01T00:00:00Z') && fmt.zeroTime('') && fmt.zeroTime(undefined), 'zero times');
eq(fmt.time('0001-01-01T00:00:00Z'), '\u2014');
eq(fmt.ago('2026-01-01T00:00:10Z', Date.parse('2026-01-01T00:01:10Z')), '1 m 0 s ago');
eq(fmt.ago('2026-01-01T00:00:10Z', Date.parse('2026-01-01T00:00:12Z')), 'just now');
eq(fmt.ago(''), 'never');
eq(fmt.plural(1, 'node'), '1 node');
eq(fmt.plural(2, 'node'), '2 nodes');
eq(fmt.shortHash('a'.repeat(64)), 'a'.repeat(12) + '\u2026');
eq(fmt.num(1.23456, 2), '1.23');

// --- api helpers ----------------------------------------------------------
eq(api.query({a: 1, b: '', c: null, d: 'x y&z'}), '?a=1&d=x%20y%26z');
eq(api.query({}), '');
eq(api.enc('a/b ?#'), 'a%2Fb%20%3F%23');
eq(api.errorText(400, 'Bad Request', '{"error":"nope"}'), 'nope');
eq(api.errorText(404, 'Not Found', '{"error":""}'), 'HTTP 404 Not Found');
eq(api.errorText(502, 'Bad Gateway', '<html>proxy</html>'), 'HTTP 502 Bad Gateway');
eq(api.errorText(500, '', 'plain message\n'), 'plain message');
eq(api.errorText(500, '', ''), 'HTTP 500');
ok(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/.test(api.clientTime()), 'client time is RFC 3339 without fractions');

{
  const calls = [];
  let reply = () => new Response('{"ok":true}', {status: 200, headers: {'Content-Type': 'application/json'}});
  globalThis.fetch = async (url, init) => {
    calls.push({url, init});
    return reply(init);
  };
  eq(await api.post('/admin/nodes/x/action', {action: 'identify'}, {query: {a: 'b'}}), {ok: true});
  const c = calls[0];
  eq(c.url, '/api/v1/admin/nodes/x/action?a=b');
  eq(c.init.method, 'POST');
  eq(c.init.credentials, 'same-origin');
  eq(c.init.headers['X-Savior'], '1');
  ok(/Z$/.test(c.init.headers['X-Savior-Client-Time']), 'client time header');
  eq(c.init.headers['Content-Type'], 'application/json');
  eq(c.init.body, '{"action":"identify"}');

  let unauth = 0;
  api.setUnauthorizedHandler(() => unauth++);
  reply = () => new Response('{"error":"session expired"}', {status: 401});
  let err = await api.get('/admin/info').catch((e) => e);
  eq([err.name, err.status, err.message, err.handled, unauth], ['ApiError', 401, 'session expired', true, 1]);
  err = await api.get('/admin/info', {auth: false}).catch((e) => e);
  eq([err.status, err.handled, unauth], [401, false, 1], 'auth:false leaves 401 to the caller');

  reply = () => new Response('{"error":"blob is referenced by job j1"}', {status: 409});
  err = await api.del('/admin/blobs/x').catch((e) => e);
  eq([err.status, err.message], [409, 'blob is referenced by job j1']);

  reply = () => new Response('not json', {status: 200});
  err = await api.get('/admin/info').catch((e) => e);
  eq(err.message, 'The hive sent an unexpected response.');

  reply = () => new Response('', {status: 200});
  eq(await api.post('/admin/logout'), null, 'empty body');

  reply = () => new Response('hello', {status: 200, headers: {'X-Savior-Log-Offset': '5'}});
  const raw = await api.get('/admin/tasks/t/log', {as: 'raw'});
  eq([raw.buf.byteLength, raw.headers.get('X-Savior-Log-Offset')], [5, '5']);

  reply = () => Promise.reject(new TypeError('Failed to fetch'));
  err = await api.get('/admin/info').catch((e) => e);
  eq([err.status, err.message], [0, 'Cannot reach the hive (Failed to fetch).']);

  const hang = (init) => new Promise((resolve, reject) => {
    init.signal.addEventListener('abort', () => {
      const e = new Error('aborted');
      e.name = 'AbortError';
      reject(e);
    });
  });
  reply = hang;
  err = await api.get('/admin/info', {timeout: 20}).catch((e) => e);
  eq([err.name, err.status, err.message], ['ApiError', 0, 'The hive did not answer in time.']);
  const ac = new AbortController();
  const p = api.get('/admin/info', {signal: ac.signal}).catch((e) => e);
  ac.abort();
  err = await p;
  eq(err.name, 'AbortError', 'navigation aborts stay aborts');
  ok(dom.isAbort(err), 'isAbort');
}

{
  // upload() uses XHR for progress; check headers and error mapping.
  const sent = [];
  class FakeXHR {
    constructor() {
      this.headers = {};
      this.listeners = {};
      this.upload = {addEventListener: (t, f) => { this.progress = f; }};
    }
    open(m, u) { this.method = m; this.url = u; }
    setRequestHeader(k, v) { this.headers[k] = v; }
    addEventListener(t, f) { this.listeners[t] = f; }
    abort() { this.listeners.abort(); }
    send(body) {
      sent.push(this);
      this.body = body;
      queueMicrotask(() => {
        if (this.progress) this.progress({lengthComputable: true, loaded: 5, total: 10});
        Object.assign(this, FakeXHR.next);
        this.listeners[FakeXHR.event || 'load']();
      });
    }
  }
  globalThis.XMLHttpRequest = FakeXHR;
  FakeXHR.next = {status: 200, responseText: '{"sha256":"' + 'c'.repeat(64) + '","size":10}'};
  const prog = [];
  const info = await api.upload('file-body', (a, b) => prog.push([a, b]));
  eq(info.sha256, 'c'.repeat(64));
  eq(prog, [[5, 10]]);
  const x = sent[0];
  eq([x.method, x.url, x.body], ['POST', '/api/v1/admin/blobs', 'file-body']);
  eq([x.headers['X-Savior'], x.headers['Content-Type']], ['1', 'application/octet-stream']);
  ok(x.headers['X-Savior-Client-Time'], 'upload carries client time');

  FakeXHR.next = {status: 413, statusText: 'Payload Too Large', responseText: '{"error":"blob too large"}'};
  let err = await api.upload('x').catch((e) => e);
  eq([err.status, err.message], [413, 'blob too large']);
  FakeXHR.next = {status: 200, responseText: '{"nope":1}'};
  err = await api.upload('x').catch((e) => e);
  eq(err.message, 'The hive sent an unexpected response.');
  FakeXHR.event = 'error';
  err = await api.upload('x').catch((e) => e);
  eq(err.status, 0);
  FakeXHR.event = null;
  const ac = new AbortController();
  ac.abort();
  err = await api.upload('x', null, ac.signal).catch((e) => e);
  eq(err.name, 'AbortError', 'pre-aborted upload');
}

// --- dom ------------------------------------------------------------------
{
  for (const u of ['javascript:alert(1)', 'JaVaScRiPt:alert(1)', ' javascript:alert(1)', '\tjavascript:x', 'data:text/html,<b>',
    'vbscript:x', '//evil.example/x', 'file:///etc/passwd', '']) eq(dom.safeURL(u), '', 'unsafe URL ' + JSON.stringify(u));
  for (const u of ['#/nodes', '/api/v1/admin/jobs/j1/outputs.zip', 'blob:https://hive/1234', 'https://192.168.1.2:7700/']) {
    eq(dom.safeURL(u), u, 'safe URL ' + u);
  }

  const markup = '<img src=x onerror=alert(1)><script>alert(2)</script>';
  const el = dom.h('div', {class: 'x', title: markup}, markup, ['a', null, false, 1], dom.h('span', null, 'b'));
  eq(el.className, 'x');
  eq(el.title, markup, 'attributes are plain data');
  eq(el.childNodes.map((c) => c.nodeType), [3, 3, 3, 1], 'strings become text nodes');
  eq(el.childNodes[0].data, markup);
  eq(el.textContent, markup + 'a1b');

  const a = dom.h('a', {href: 'javascript:alert(1)'}, 'x');
  eq(a.href, undefined, 'script URL dropped');
  eq(dom.h('a', {href: '#/jobs'}).href, '#/jobs');
  eq(dom.h('img', {src: 'data:image/svg+xml,<svg onload=alert(1)>'}).src, undefined, 'data: src dropped');

  const markupProp = 'inner' + 'HTML';
  assert.throws(() => dom.h('div', {[markupProp]: '<b>x</b>'}), /unsupported property/);
  assert.throws(() => dom.h('div', {onclick: 'alert(1)'}), /unsupported property/);
  assert.throws(() => dom.h('iframe', {srcdoc: '<b>x</b>'}), /unsupported property/);
  assert.throws(() => dom.h('div', {style: 'color:red'}), /style must be an object/);
  checks += 4;

  let clicked = 0;
  const b = dom.h('button', {on: {click: () => clicked++}, 'aria-label': 'go', dataset: {k: 'v'}, style: {width: '5%'}});
  b.listeners.click[0]();
  eq([clicked, b.attributes['aria-label'], b.dataset.k, b.style.width], [1, 'go', 'v', '5%']);

  const t = dom.table(['A', 'B'], [['<i>1</i>', dom.h('td', null, '2')]], 'empty');
  const row = t.childNodes[0].childNodes[1].childNodes[0];
  eq(row.childNodes.map((c) => c.tagName), ['TD', 'TD']);
  eq(row.childNodes[0].textContent, '<i>1</i>');
  const empty = dom.table(['A', 'B'], [], 'Nothing here');
  eq(empty.textContent, 'ABNothing here');

  const list = dom.kv([['Name', '<b>n</b>'], ['Skip', ''], ['Null', null], ['Zero', '0']]);
  eq(list.childNodes.length, 2, 'kv skips empty values');

  dom.toast('<b>careful</b>', 'error');
  eq(toastBox.childNodes.length, 1);
  eq(toastBox.childNodes[0].childNodes[0].textContent, '<b>careful</b>');
  eq(toastBox.childNodes[0].attributes.role, 'alert');

  const box = dom.h('div', null, 'old');
  const render = dom.memo(box);
  let renders = 0;
  const r = (d) => {
    renders++;
    return String(d.v);
  };
  render({v: 1}, r);
  render({v: 1}, r);
  render({v: 2}, r);
  eq([renders, box.textContent], [2, '2'], 'memo re-renders only on change');
}

console.log('dashboard unit tests: ' + checks + ' checks passed');
process.exit(0);
