// Display content editor shared by the node detail view and the wall editor.
// It edits a proto.DisplaySpec (without rev); images are uploaded as hive
// blobs first and referenced by hash.

import {h, field, select, btn, mount, toast, reportError, uid} from './dom.js';
import * as api from './api.js';
import {checkDisplaySpec, validHTTPURL, validSHA256, ZONES} from './model.js';
import {bytes, shortHash} from './fmt.js';

export const MODE_LABELS = {
  status: 'Status screen', off: 'Off (blank)', color: 'Solid color', text: 'Text', clock: 'Clock',
  image: 'Image', slideshow: 'Slideshow', dashboard: 'Swarm dashboard', wall: 'Video wall', test: 'Test pattern',
};

const MODE_HELP = {
  status: 'The default: name, short code, addresses, link state and load bars. Handy for telling machines apart.',
  off: 'Blanks the screen.',
  color: 'Fills the screen with one color.',
  text: 'Large, centered, word-wrapped text with an optional title.',
  clock: 'The time and date in the chosen format and time zone.',
  image: 'One image fitted to the screen.',
  slideshow: 'Images change every interval, in step on every screen.',
  dashboard: 'Live swarm statistics, refreshed every 5 seconds.',
  test: 'Color bars, gradients, a 1-pixel border, the resolution and the pixel format.',
};

const CLOCK_FORMATS = [['15:04', '24-hour (15:04)'], ['15:04:05', '24-hour with seconds'], ['3:04 PM', '12-hour (3:04 PM)'],
  ['3:04:05 PM', '12-hour with seconds'], ['custom', 'Custom Go layout']];

const MAX_MEDIA_BYTES = 64 << 20;

// thumbURL is a small PNG rendering of an image blob (hive render endpoint).
export function thumbURL(sha, w, ht) {
  return api.BASE + '/blobs/' + api.enc(sha) + '/render' +
    api.query({cw: w, ch: ht, x: 0, y: 0, w: w, h: ht, pw: w, ph: ht, fit: 'contain'});
}

function thumb(item) {
  const none = h('span', {class: 'thumb thumb-none'}, item.media.url ? 'URL' : 'image');
  const src = item.local || (item.media.blob ? thumbURL(item.media.blob, 160, 90) : '');
  if (!src) return none;
  const img = h('img', {class: 'thumb', src: src, alt: ''});
  img.addEventListener('error', function () {
    if (img.parentNode) img.parentNode.replaceChild(none, img);
  });
  return img;
}

function itemLabel(it) {
  if (it.label) return it.label;
  const m = it.media;
  if (m.url) return m.url;
  return 'blob ' + shortHash(m.blob) + (m.width ? ' · ' + m.width + '×' + m.height : '');
}

function colorValue(c, dflt) {
  if (/^#[0-9a-fA-F]{6}$/.test(c || '')) return c.toLowerCase();
  if (/^#[0-9a-fA-F]{3}$/.test(c || '')) return ('#' + c[1] + c[1] + c[2] + c[2] + c[3] + c[3]).toLowerCase();
  return dflt;
}

function compact(o) {
  for (const k of Object.keys(o)) {
    if (o[k] === undefined || o[k] === '') delete o[k];
  }
  return o;
}

// displayEditor builds the form. opts.modes lists the selectable modes and
// opts.signal ends it (revoking preview URLs and aborting uploads). Returns
// {el, get}; get() returns the spec or throws an Error to show the user.
export function displayEditor(spec, opts) {
  spec = spec || {mode: opts.modes[0]};
  const modes = opts.modes;
  const signal = opts.signal;
  const objectURLs = [];
  signal.addEventListener('abort', function () { objectURLs.forEach(function (u) { URL.revokeObjectURL(u); }); });
  let pending = 0;

  const modeSel = select(modes.map(function (m) { return [m, MODE_LABELS[m]]; }),
    modes.indexOf(spec.mode) >= 0 ? spec.mode : modes[0]);
  const help = h('p', {class: 'hint'});

  const fg = h('input', {type: 'color', value: colorValue(spec.fg, '#ffffff')});
  const bg = h('input', {type: 'color', value: colorValue(spec.bg, '#000000')});
  const fgField = field('Text color', fg);
  const bgField = field('Background color', bg);

  const title = h('input', {type: 'text', value: spec.title || '', maxLength: 256});
  const text = h('textarea', {rows: 4, value: spec.text || ''});
  const textGroup = h('div', {class: 'form-grid'}, field('Title (optional)', title), field('Text', text, null, 'wide'));

  const preset = CLOCK_FORMATS.some(function (f) { return f[0] === (spec.clock_format || '15:04'); });
  const fmtSel = select(CLOCK_FORMATS, preset ? (spec.clock_format || '15:04') : 'custom');
  const fmtCustom = h('input', {type: 'text', value: preset ? '' : spec.clock_format, maxLength: 64, placeholder: 'Mon 2 Jan 15:04', spellcheck: false});
  const fmtCustomField = field('Custom format', fmtCustom, 'Go time layout: 15 = hour, 04 = minute, 05 = second, Mon = weekday, 2 = day, Jan = month, 2006 = year.');
  const zones = typeof Intl.supportedValuesOf === 'function' ? Intl.supportedValuesOf('timeZone') : ZONES;
  const zoneList = h('datalist', {id: uid('zones')},
    zones.map(function (z) { return h('option', {value: z}); }));
  const tz = h('input', {type: 'text', value: spec.timezone || '', list: zoneList.id, placeholder: 'the node\'s own time zone', autocomplete: 'off', spellcheck: false});
  const clockGroup = h('div', {class: 'form-grid'}, field('Format', fmtSel), fmtCustomField,
    field('Time zone', tz, 'IANA name such as Europe/Berlin. Empty uses the node\'s timezone setting.'), zoneList);
  const syncClock = function () { fmtCustomField.hidden = fmtSel.value !== 'custom'; };
  fmtSel.addEventListener('change', syncClock);

  const fit = select([['contain', 'Contain (whole image, letterboxed)'], ['cover', 'Cover (fill, cropped)'], ['stretch', 'Stretch']], spec.fit || 'contain');
  const fitField = field('Fit', fit);

  // Image (single) and slideshow (list) media.
  let single = spec.image ? {media: spec.image} : null;
  const items = (spec.images || []).map(function (m) { return {media: m}; });
  const singleBox = h('div', {class: 'media-list'});
  const listBox = h('div', {class: 'media-list'});
  const status = h('p', {class: 'hint', role: 'status'});

  const renderSingle = function () {
    mount(singleBox, single
      ? h('div', {class: 'media-item'}, thumb(single), h('span', {class: 'media-label'}, itemLabel(single)))
      : h('p', {class: 'muted'}, 'No image chosen yet.'));
  };
  const renderList = function () {
    mount(listBox, items.length ? items.map(function (it, i) {
      const move = function (d) {
        return function () {
          const j = i + d;
          const t = items[i];
          items[i] = items[j];
          items[j] = t;
          renderList();
        };
      };
      return h('div', {class: 'media-item'}, thumb(it), h('span', {class: 'media-label'}, (i + 1) + '. ' + itemLabel(it)),
        h('span', {class: 'btn-row'},
          btn('↑', move(-1), 'btn-small', {disabled: i === 0, 'aria-label': 'Move image ' + (i + 1) + ' up'}),
          btn('↓', move(1), 'btn-small', {disabled: i === items.length - 1, 'aria-label': 'Move image ' + (i + 1) + ' down'}),
          btn('Remove', function () { items.splice(i, 1); renderList(); }, 'btn-small', {'aria-label': 'Remove image ' + (i + 1)})));
    }) : h('p', {class: 'muted'}, 'No images yet.'));
  };

  const uploadFiles = async function (files, onItem) {
    for (const f of files) {
      if (f.size > MAX_MEDIA_BYTES) {
        toast(f.name + ' is larger than 64 MB, which displays refuse.', 'error');
        continue;
      }
      pending++;
      status.textContent = 'Uploading ' + f.name + '…';
      try {
        const info = await api.upload(f, function (done, total) {
          status.textContent = 'Uploading ' + f.name + ': ' + Math.floor(done * 100 / total) + '%';
        }, signal);
        const local = URL.createObjectURL(f);
        objectURLs.push(local);
        onItem({media: {blob: info.sha256}, local: local, label: f.name + ' (' + bytes(info.size) + ')'});
        status.textContent = 'Uploaded ' + f.name + '.';
      } catch (e) {
        status.textContent = '';
        reportError(e);
      } finally {
        pending--;
      }
    }
  };

  const urlMedia = function (input, shaInput) {
    const u = input.value.trim();
    const sha = shaInput.value.trim().toLowerCase();
    if (!validHTTPURL(u)) throw new Error('Enter an http:// or https:// image URL.');
    if (sha && !validSHA256(sha)) throw new Error('The SHA-256 must be 64 hex characters.');
    input.value = '';
    shaInput.value = '';
    return compact({url: u, sha256: sha});
  };

  const mediaAdder = function (multiple, onItem) {
    const file = h('input', {type: 'file', accept: 'image/*', multiple: multiple});
    file.addEventListener('change', function () {
      const files = Array.prototype.slice.call(file.files || []);
      file.value = '';
      uploadFiles(files, onItem);
    });
    const url = h('input', {type: 'url', placeholder: 'https://example.com/picture.jpg', spellcheck: false});
    const sha = h('input', {type: 'text', placeholder: 'optional', spellcheck: false, maxLength: 64});
    return h('div', {class: 'form-grid'},
      field(multiple ? 'Upload images' : 'Upload an image', file, 'PNG, JPEG, GIF, BMP or WebP, up to 64 MB. Stored on the hive.'),
      h('div', {class: 'field'}, field('Or an image URL', url, 'Fetched by each node; blocked if the node has no internet access.'),
        field('SHA-256 of the URL content', sha),
        btn(multiple ? 'Add URL' : 'Use URL', function () { onItem({media: urlMedia(url, sha)}); }, 'btn-small')));
  };

  const imageGroup = h('div', null, singleBox, mediaAdder(false, function (it) { single = it; renderSingle(); }));
  const interval = h('input', {type: 'number', min: 3, max: 86400, step: 1, value: spec.interval_s || '', placeholder: '10'});
  const slideGroup = h('div', null, listBox, mediaAdder(true, function (it) { items.push(it); renderList(); }),
    field('Seconds per image', interval, 'Default 10, at least 3.'));

  const groups = {text: [textGroup, fgField, bgField], color: [bgField], clock: [clockGroup, fgField, bgField],
    image: [imageGroup, fitField, bgField], slideshow: [slideGroup, fitField, bgField]};
  const all = [textGroup, clockGroup, imageGroup, slideGroup, fitField, fgField, bgField];
  const colors = h('div', {class: 'form-grid'}, fgField, bgField, fitField);
  const sync = function () {
    const m = modeSel.value;
    help.textContent = MODE_HELP[m] || '';
    const show = groups[m] || [];
    for (const g of all) g.hidden = show.indexOf(g) < 0;
    syncClock();
  };
  modeSel.addEventListener('change', sync);
  renderSingle();
  renderList();
  sync();

  const el = h('div', {class: 'display-editor'}, field('Show', modeSel), help, textGroup, clockGroup, imageGroup, slideGroup, colors, status);

  const get = function () {
    if (pending) throw new Error('Wait for the uploads to finish.');
    const m = modeSel.value;
    const s = {mode: m};
    const show = groups[m] || [];
    if (show.indexOf(fgField) >= 0) s.fg = fg.value;
    if (show.indexOf(bgField) >= 0) s.bg = bg.value;
    if (m === 'text') {
      s.title = title.value.trim();
      s.text = text.value;
      if (!s.title && !text.value.trim()) throw new Error('Enter the text to show.');
    } else if (m === 'clock') {
      s.clock_format = fmtSel.value === 'custom' ? fmtCustom.value.trim() : fmtSel.value;
      s.timezone = tz.value.trim();
    } else if (m === 'image') {
      s.image = single ? single.media : undefined;
      s.fit = fit.value;
    } else if (m === 'slideshow') {
      s.images = items.map(function (it) { return it.media; });
      s.fit = fit.value;
      const iv = interval.value.trim();
      s.interval_s = iv ? Number(iv) : 0;
    }
    compact(s);
    const err = checkDisplaySpec(s, modes);
    if (err) throw new Error('Display: ' + err + '.');
    return s;
  };
  return {el: el, get: get};
}
