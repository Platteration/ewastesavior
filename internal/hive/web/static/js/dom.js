// DOM helpers. Every string that reaches the page goes through h() or
// setText(), which only ever create Text nodes; there is no markup parsing
// anywhere in the dashboard.

// Properties h() assigns directly on the element.
const PROPS = new Set(['id', 'type', 'name', 'value', 'checked', 'disabled', 'title', 'placeholder',
  'min', 'max', 'step', 'maxLength', 'rows', 'multiple', 'accept', 'selected', 'required', 'pattern',
  'spellcheck', 'readOnly', 'tabIndex', 'colSpan', 'htmlFor', 'alt', 'open', 'hidden', 'download',
  'width', 'height', 'size', 'lang']);
// Attributes h() sets with setAttribute.
const ATTRS = /^(aria-[a-z]+|data-[a-z-]+|role|scope|list|autocomplete|autocapitalize|inputmode|for)$/;

let idSeq = 0;

// uid returns a document-unique element id.
export function uid(prefix) {
  idSeq++;
  return (prefix || 'el') + '-' + idSeq;
}

// safeURL returns u when it is a same-origin path, a hash route, a blob: URL
// or an http(s) URL, and '' otherwise, so script and data URLs never become
// a link or image source.
export function safeURL(u) {
  u = String(u == null ? '' : u);
  if (/^#/.test(u) || /^\/(?!\/)/.test(u) || /^blob:/.test(u)) return u;
  try {
    const p = new URL(u);
    if (p.protocol === 'http:' || p.protocol === 'https:') return p.href;
  } catch (e) { /* not a URL */ }
  return '';
}

function append(el, c) {
  if (c == null || c === false) return;
  if (Array.isArray(c)) {
    for (const x of c) append(el, x);
  } else if (typeof c === 'object' && c.nodeType) {
    el.appendChild(c);
  } else {
    el.appendChild(document.createTextNode(String(c)));
  }
}

// h creates an element. props may contain: class, text, on (event map),
// style (CSSOM object), dataset, href/src (checked by safeURL), known
// properties (PROPS) and attributes (ATTRS). Children are nodes, strings
// (inserted as text), arrays, or null/false (skipped).
export function h(tag, props) {
  const el = document.createElement(tag);
  if (props) {
    for (const k of Object.keys(props)) {
      const v = props[k];
      if (v == null) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = String(v);
      else if (k === 'on') for (const ev of Object.keys(v)) el.addEventListener(ev, v[ev]);
      else if (k === 'style') {
        if (typeof v !== 'object') throw new Error('h: style must be an object');
        for (const s of Object.keys(v)) el.style[s] = v[s];
      }
      else if (k === 'dataset') for (const d of Object.keys(v)) el.dataset[d] = v[d];
      else if (k === 'href' || k === 'src') {
        const u = safeURL(v);
        if (u) el[k] = u;
      } else if (PROPS.has(k)) el[k] = v;
      else if (ATTRS.test(k)) el.setAttribute(k, String(v));
      else throw new Error('h: unsupported property ' + k);
    }
  }
  for (let i = 2; i < arguments.length; i++) append(el, arguments[i]);
  return el;
}

// clear removes all children of el.
export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

// mount replaces the children of el.
export function mount(el) {
  clear(el);
  for (let i = 1; i < arguments.length; i++) append(el, arguments[i]);
  return el;
}

// setText sets an element's text.
export function setText(el, s) {
  el.textContent = s == null ? '' : String(s);
  return el;
}

// toast shows a transient message. kind: info, ok or error.
export function toast(msg, kind) {
  kind = kind || 'info';
  const box = document.getElementById('toasts');
  if (!box) return;
  const t = h('div', {class: 'toast toast-' + kind, role: kind === 'error' ? 'alert' : 'status'},
    h('span', null, msg));
  const close = function () { if (t.parentNode) t.parentNode.removeChild(t); };
  t.appendChild(h('button', {type: 'button', class: 'toast-close', 'aria-label': 'Dismiss', on: {click: close}}, '×'));
  box.appendChild(t);
  while (box.children.length > 5) box.removeChild(box.firstChild);
  setTimeout(close, kind === 'error' ? 12000 : 5000);
}

// isAbort reports whether e is a fetch/XHR cancellation.
export function isAbort(e) {
  return !!e && e.name === 'AbortError';
}

// reportError toasts an error unless it was a cancellation or already handled
// (a 401 that sent the user back to the sign-in screen).
export function reportError(e) {
  if (!e || isAbort(e) || e.handled) return;
  toast(e.message || String(e), 'error');
}

// btn makes a button. If fn returns a promise, the button is disabled until
// it settles and a rejection is reported.
export function btn(label, fn, cls, props) {
  const b = h('button', Object.assign({type: 'button', class: 'btn' + (cls ? ' ' + cls : '')}, props || {}), label);
  if (fn) {
    b.addEventListener('click', function (ev) {
      if (b.dataset.busy) return;
      let p;
      try {
        p = fn(ev);
      } catch (e) {
        reportError(e);
        return;
      }
      if (p && typeof p.then === 'function') {
        b.dataset.busy = '1';
        b.disabled = true;
        p.catch(reportError).then(function () {
          delete b.dataset.busy;
          b.disabled = false;
        });
      }
    });
  }
  return b;
}

// field wraps a control with a label and an optional hint.
export function field(label, control, hint, cls) {
  if (!control.id) control.id = uid('f');
  const kids = [h('label', {htmlFor: control.id}, label), control];
  if (hint) {
    const hid = uid('hint');
    control.setAttribute('aria-describedby', hid);
    kids.push(h('p', {class: 'hint', id: hid}, hint));
  }
  return h('div', {class: 'field' + (cls ? ' ' + cls : '')}, kids);
}

// check makes a labeled checkbox.
export function check(label, checked, props) {
  const box = h('input', Object.assign({type: 'checkbox', checked: !!checked}, props || {}));
  return {input: box, el: h('label', {class: 'check'}, box, ' ', label)};
}

// select makes a <select> from [value, label] pairs.
export function select(options, value, props) {
  const s = h('select', props || null);
  for (const o of options) s.appendChild(h('option', {value: o[0], selected: String(o[0]) === String(value)}, o[1]));
  return s;
}

// kv renders [label, value] pairs as a description list, skipping empty values.
export function kv(pairs) {
  const dl = h('dl', {class: 'kv'});
  for (const p of pairs) {
    if (!p || p[1] == null || p[1] === '') continue;
    dl.appendChild(h('div', null, h('dt', null, p[0]), h('dd', null, p[1])));
  }
  return dl;
}

// badge renders a small status label. kind: ok, warn, bad, info or neutral.
export function badge(text, kind, title) {
  return h('span', {class: 'badge badge-' + (kind || 'neutral'), title: title || null}, text);
}

// table renders a table inside a horizontally scrollable wrapper. heads are
// strings or nodes; rows are arrays of cells or {cells, cls, onClick}.
export function table(heads, rows, empty) {
  const tbody = h('tbody');
  for (const r of rows) {
    const cells = Array.isArray(r) ? r : r.cells;
    const tr = h('tr', {class: r.cls || null});
    if (r.onClick) {
      tr.classList.add('clickable');
      tr.addEventListener('click', function (ev) {
        if (ev.target.closest('a, button, input, select, label')) return;
        r.onClick(ev);
      });
    }
    for (const c of cells) tr.appendChild(c && c.nodeType && c.tagName === 'TD' ? c : h('td', null, c));
    tbody.appendChild(tr);
  }
  if (!rows.length && empty) {
    tbody.appendChild(h('tr', null, h('td', {colSpan: heads.length, class: 'empty'}, empty)));
  }
  return h('div', {class: 'table-wrap'},
    h('table', null, h('thead', null, h('tr', null, heads.map(function (x) {
      return x && x.nodeType ? x : h('th', {scope: 'col'}, x);
    }))), tbody));
}

// panel renders a titled section.
export function panel(title) {
  const kids = Array.prototype.slice.call(arguments, 1);
  return h('section', {class: 'panel'}, title ? h('h2', null, title) : null, kids);
}

// banner renders a notice. kind: info, warn or bad.
export function banner(kind) {
  return h('div', {class: 'banner banner-' + kind, role: kind === 'bad' ? 'alert' : null},
    Array.prototype.slice.call(arguments, 1));
}

// copyText copies text to the clipboard, falling back to execCommand where
// the async clipboard API is unavailable (for example after accepting the
// hive's self-signed certificate in some browsers).
export async function copyText(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      toast('Copied to the clipboard.', 'ok');
      return;
    }
  } catch (e) { /* fall back below */ }
  const ta = h('textarea', {value: text, readOnly: true, class: 'offscreen'});
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand('copy');
  } catch (e) { /* unsupported */ }
  document.body.removeChild(ta);
  toast(ok ? 'Copied to the clipboard.' : 'Copying failed: select the text and copy it by hand.', ok ? 'ok' : 'error');
}

// saveFile offers a Blob to the user as a download.
export function saveFile(blob, filename) {
  const url = URL.createObjectURL(blob);
  const a = h('a', {href: url, download: filename, class: 'offscreen'});
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  setTimeout(function () { URL.revokeObjectURL(url); }, 10000);
}

// sleep resolves after ms, or early when signal aborts.
export function sleep(ms, signal) {
  return new Promise(function (resolve) {
    const t = setTimeout(resolve, ms);
    if (signal) signal.addEventListener('abort', function () { clearTimeout(t); resolve(); });
  });
}

// copyable renders a value in <code> with a copy button.
export function copyable(text, label) {
  return h('span', {class: 'copyable'}, h('code', null, text),
    btn('Copy', function () { return copyText(text); }, 'btn-small', {'aria-label': 'Copy ' + (label || 'value')}));
}

// memo returns a renderer that replaces el's children only when the data it
// shows has changed, so periodic refreshes keep focus, scroll position,
// selection and <details> state.
export function memo(el) {
  let last = null;
  return function (data, render) {
    const s = JSON.stringify(data);
    if (s !== last) {
      last = s;
      mount(el, render(data));
    }
  };
}
