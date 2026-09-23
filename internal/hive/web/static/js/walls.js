// Video walls: list and editor with a grid of screens, a proportional
// preview and the shared content editor.

import {h, mount, table, btn, panel, field, select, toast} from './dom.js';
import * as api from './api.js';
import {LIMITS, WALL_CONTENT_MODES, wallLayout, screenSizeMM, DEFAULT_SCREEN, utf8Len} from './model.js';
import {displayEditor, MODE_LABELS} from './display.js';

export async function list(root, ctx) {
  ctx.title('Walls');
  const walls = (await api.get('/admin/walls', {signal: ctx.signal})) || [];
  mount(root, h('div', {class: 'page-head'}, h('h1', null, 'Video walls'), h('a', {class: 'btn btn-primary', href: '#/walls/new'}, 'New wall')),
    h('p', {class: 'hint'}, 'A wall spreads one image, slideshow, text or test pattern over several screens arranged in a grid. Each screen shows its part, sized by the monitors\' real dimensions.'),
    table(['Name', 'Grid', 'Screens', 'Content'], walls.map((w) => ({
      cells: [h('a', {href: '#/walls/' + api.enc(w.id)}, w.name || w.id), w.rows + ' × ' + w.cols, String((w.cells || []).length),
        MODE_LABELS[(w.content || {}).mode] || (w.content || {}).mode || ''],
      onClick: () => ctx.go('#/walls/' + api.enc(w.id)),
    })), 'No walls yet.'));
}

function intIn(input, label, min, max, dflt) {
  const v = input.value.trim();
  if (v === '') return dflt;
  const n = Number(v);
  if (!(n >= min && n <= max) || n !== Math.floor(n)) throw new Error(label + ' must be a whole number from ' + min + ' to ' + max + '.');
  return n;
}

export async function edit(root, ctx, id) {
  const sig = {signal: ctx.signal};
  const r = await Promise.all([api.get('/admin/nodes', sig), id ? api.get('/admin/walls/' + api.enc(id), sig) : null]);
  const nodes = (r[0] || []).slice().sort((a, b) => String(a.name).localeCompare(String(b.name)));
  let wall = r[1] || {id: '', name: '', rows: 1, cols: 2, cells: [], content: {mode: 'test'}};
  const byID = {};
  for (const n of nodes) byID[n.id] = n;
  ctx.title(id ? 'Wall ' + (wall.name || wall.id) : 'New wall');

  const num = (v, min, max, label) => h('input', {type: 'number', min: min, max: max, step: 1, value: v, 'aria-label': label || null});
  const name = h('input', {type: 'text', value: wall.name || '', maxLength: 64, placeholder: 'lobby'});
  const rows = num(wall.rows || 1, 1, LIMITS.wallSize);
  const cols = num(wall.cols || 1, 1, LIMITS.wallSize);
  const gapX = num(wall.gap_x_mm || 0, 0, LIMITS.gapMM);
  const gapY = num(wall.gap_y_mm || 0, 0, LIMITS.gapMM);
  const cellMap = new Map();
  for (const c of wall.cells || []) cellMap.set(c.row + ',' + c.col, Object.assign({}, c));

  const dims = () => ({
    rows: Math.min(LIMITS.wallSize, Math.max(1, parseInt(rows.value, 10) || 1)),
    cols: Math.min(LIMITS.wallSize, Math.max(1, parseInt(cols.value, 10) || 1)),
  });
  const inGrid = (c, d) => c.row < d.rows && c.col < d.cols;
  const eligible = (n) => (n.roles || []).indexOf('display') >= 0 && (!n.wall_id || n.wall_id === wall.id);
  const nodeLabel = (n) => {
    const s = screenSizeMM(n);
    return n.name + ' (' + n.short_code + ')' + (n.liveness === 'online' ? '' : ', offline') + (s ? ', ' + s.w + '×' + s.h + ' mm' : '');
  };

  const grid = h('div', {class: 'wall-grid'});
  // The canvas's percentage padding resolves against the preview's width,
  // which drawPreview caps, so the canvas keeps the wall's aspect ratio.
  const canvas = h('div', {class: 'wall-canvas', role: 'img'});
  const preview = h('div', {class: 'wall-preview'}, canvas);
  const previewNote = h('p', {class: 'hint'});
  let cellEls = [];

  const spec = (withContent) => {
    const d = dims();
    const cells = [];
    cellMap.forEach((c) => {
      if (c.node && inGrid(c, d)) {
        const out = {node: c.node, row: c.row, col: c.col};
        if (c.width_mm) out.width_mm = c.width_mm;
        if (c.height_mm) out.height_mm = c.height_mm;
        if (c.rect) out.rect = c.rect;
        cells.push(out);
      }
    });
    cells.sort((a, b) => a.row - b.row || a.col - b.col);
    return {id: wall.id || '', name: name.value.trim(), rows: d.rows, cols: d.cols,
      gap_x_mm: Math.max(0, parseInt(gapX.value, 10) || 0), gap_y_mm: Math.max(0, parseInt(gapY.value, 10) || 0),
      cells: cells, content: withContent ? editor.get() : wall.content};
  };

  const drawPreview = () => {
    const s = spec(false);
    const L = wallLayout(s, byID);
    // Width follows the aspect ratio so the preview is at most ~420px tall.
    preview.style.maxWidth = Math.max(120, Math.min(720, Math.round(420 * L.w / Math.max(1, L.h)))) + 'px';
    canvas.style.paddingBottom = (L.h / Math.max(1, L.w)) * 100 + '%';
    const place = (el, t) => {
      el.style.left = (t.x * 100 / L.w) + '%';
      el.style.top = (t.y * 100 / L.h) + '%';
      el.style.width = (t.w * 100 / L.w) + '%';
      el.style.height = (t.h * 100 / L.h) + '%';
      return el;
    };
    mount(canvas,
      L.slots.map((t) => place(h('div', {class: 'wall-slot'}), t)),
      L.tiles.map((t) => place(h('div', {class: 'wall-tile'},
        h('span', null, 'R' + (t.row + 1) + 'C' + (t.col + 1)), h('span', null, byID[t.node] ? byID[t.node].name : t.node)), t)));
    canvas.setAttribute('aria-label', 'Preview of the wall layout, ' + s.cells.length + ' screens');
    previewNote.textContent = 'Canvas ' + L.w + ' × ' + L.h + ' mm. Each screen shows the part under it. ' +
      'Sizes come from the overrides, else the monitor\'s reported size, else 400 × 300 mm. This is a preview; the hive computes the final layout.';
  };

  // syncCells updates option availability and size fields without
  // rebuilding the grid, so keyboard focus stays put.
  const syncCells = () => {
    const d = dims();
    const used = new Set();
    cellMap.forEach((c) => { if (c.node && inGrid(c, d)) used.add(c.node); });
    for (const ce of cellEls) {
      const cell = cellMap.get(ce.key);
      const cur = cell ? cell.node : '';
      for (const o of ce.sel.options) o.disabled = !!o.value && o.value !== cur && used.has(o.value);
      const auto = screenSizeMM(byID[cur]) || DEFAULT_SCREEN;
      ce.w.disabled = ce.h.disabled = !cur;
      ce.w.placeholder = String(auto.w);
      ce.h.placeholder = String(auto.h);
    }
  };

  const drawGrid = () => {
    const d = dims();
    grid.style.gridTemplateColumns = 'repeat(' + d.cols + ', minmax(10rem, 1fr))';
    cellEls = [];
    const els = [];
    for (let row = 0; row < d.rows; row++) {
      for (let col = 0; col < d.cols; col++) {
        const key = row + ',' + col;
        const cell = cellMap.get(key) || {};
        const pos = 'row ' + (row + 1) + ', column ' + (col + 1);
        const opts = [['', '— empty —']];
        for (const n of nodes) if (eligible(n) || n.id === cell.node) opts.push([n.id, nodeLabel(n)]);
        if (cell.node && !byID[cell.node]) opts.push([cell.node, 'unknown node ' + cell.node]);
        const sel = select(opts, cell.node || '', {'aria-label': 'Screen at ' + pos});
        const w = num(cell.width_mm || '', 0, LIMITS.cellMM, 'Visible width in mm at ' + pos);
        const ht = num(cell.height_mm || '', 0, LIMITS.cellMM, 'Visible height in mm at ' + pos);
        const setSize = () => {
          const c = cellMap.get(key);
          if (!c) return;
          c.width_mm = Math.max(0, parseInt(w.value, 10) || 0);
          c.height_mm = Math.max(0, parseInt(ht.value, 10) || 0);
        };
        for (const inp of [w, ht]) {
          inp.addEventListener('input', () => {
            setSize();
            drawPreview();
          });
        }
        sel.addEventListener('change', () => {
          if (sel.value) cellMap.set(key, Object.assign(cellMap.get(key) || {}, {node: sel.value, row: row, col: col}));
          else cellMap.delete(key);
          syncCells();
          setSize();
          drawPreview();
        });
        cellEls.push({key: key, sel: sel, w: w, h: ht});
        els.push(h('div', {class: 'wall-cell'}, h('span', {class: 'hint'}, 'Row ' + (row + 1) + ', column ' + (col + 1)), sel,
          h('span', {class: 'size'}, w, '×', ht, 'mm'),
          cell.rect ? h('span', {class: 'hint'}, 'Custom position kept (' + cell.rect.w + '×' + cell.rect.h + ' mm at ' + cell.rect.x + ',' + cell.rect.y + ')') : null));
      }
    }
    mount(grid, els);
    syncCells();
    drawPreview();
  };
  for (const inp of [rows, cols]) inp.addEventListener('input', drawGrid);
  for (const inp of [gapX, gapY]) inp.addEventListener('input', drawPreview);

  const editor = displayEditor(wall.content || {mode: 'test'}, {modes: WALL_CONTENT_MODES, signal: ctx.signal});

  const validate = (s) => {
    intIn(rows, 'Rows', 1, LIMITS.wallSize);
    intIn(cols, 'Columns', 1, LIMITS.wallSize);
    intIn(gapX, 'The horizontal gap', 0, LIMITS.gapMM, 0);
    intIn(gapY, 'The vertical gap', 0, LIMITS.gapMM, 0);
    if (utf8Len(s.name) > 64) throw new Error('The name is too long (at most 64 bytes).');
    if (!s.cells.length) throw new Error('Choose at least one screen.');
    for (const c of s.cells) {
      if ((c.width_mm || 0) > LIMITS.cellMM || (c.height_mm || 0) > LIMITS.cellMM) throw new Error('Screen sizes must be at most ' + LIMITS.cellMM + ' mm.');
    }
  };
  const save = async () => {
    const s = spec(true);
    validate(s);
    const saved = id ? await api.put('/admin/walls/' + api.enc(id), s, sig) : await api.post('/admin/walls', s, sig);
    toast('Wall saved. The screens switch within a few seconds.', 'ok');
    if (!id) {
      ctx.go(saved && saved.id ? '#/walls/' + api.enc(saved.id) : '#/walls');
      return;
    }
    if (saved && saved.id) wall = saved;
  };
  const identify = async () => {
    const ids = spec(false).cells.map((c) => c.node);
    if (!ids.length) throw new Error('Choose some screens first.');
    await api.post('/admin/identify', {nodes: ids, seconds: 30}, sig);
    toast('These screens show their short code and position for 30 seconds.', 'ok');
  };
  const remove = async () => {
    if (!window.confirm('Delete this wall? Its screens go back to the status screen.')) return;
    await api.del('/admin/walls/' + api.enc(id), sig);
    toast('Wall deleted.', 'ok');
    ctx.go('#/walls');
  };

  mount(root, h('p', {class: 'crumbs'}, h('a', {href: '#/walls'}, '← Walls')),
    h('h1', null, id ? 'Wall ' + (wall.name || wall.id) : 'New wall'),
    panel('Layout',
      h('div', {class: 'form-grid'}, field('Name', name), field('Rows', rows), field('Columns', cols),
        field('Horizontal gap (mm)', gapX, 'Bezels between neighboring screens, 0 to 500.'), field('Vertical gap (mm)', gapY)),
      h('p', {class: 'hint'}, 'Pick a screen for each position. Only nodes with the display role that are not in another wall are listed. Width and height are the visible picture size in millimeters; leave them empty to use the monitor\'s own report.'),
      grid),
    panel('Preview', preview, previewNote),
    panel('Content', editor.el),
    h('div', {class: 'btn-row'}, btn('Save wall', save, 'btn-primary'), btn('Identify these screens', identify),
      id ? btn('Delete wall', remove, 'btn-danger') : null));
  drawGrid();
}
