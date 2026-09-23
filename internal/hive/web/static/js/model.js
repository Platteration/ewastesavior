// Pure helpers: validation mirroring internal/proto (the hive re-validates
// everything; these only give early, specific feedback), input parsing, and
// the wall layout preview. No DOM access, so testdata/unit.mjs runs them
// under Node.

export const LIMITS = {
  labels: 32, text: 4096, title: 256, clockFormat: 64, wallSize: 16, images: 100,
  minInterval: 3, maxInterval: 86400, count: 100000, cores: 256, memMB: 1 << 20, diskMB: 1 << 20,
  timeoutS: 7 * 24 * 3600, retries: 10, priority: 1000, outputs: 64, inputs: 256, gapMM: 500, cellMM: 10000,
};

export const DISPLAY_MODES = ['status', 'off', 'color', 'text', 'clock', 'image', 'slideshow', 'dashboard', 'wall', 'test'];
export const WALL_CONTENT_MODES = ['image', 'slideshow', 'text', 'color', 'test'];

// Default visible screen size (mm) when neither the cell nor EDID has one.
export const DEFAULT_SCREEN = {w: 400, h: 300};

const NODE_NAME = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/;
const ID_LIKE = /^n[0-9a-f]{12}$/;
const LABEL_KEY = /^[a-z0-9][a-z0-9_.\-/]{0,62}$/;
const ENV_KEY = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;

// utf8Len is the UTF-8 byte length of s (Go measures strings in bytes). A
// lone surrogate counts 3 bytes, like the replacement character Go uses.
export function utf8Len(s) {
  let n = 0;
  for (const ch of String(s)) {
    const c = ch.codePointAt(0);
    n += c < 0x80 ? 1 : c < 0x800 ? 2 : c < 0x10000 ? 3 : 4;
  }
  return n;
}

export function validNodeName(s) {
  return NODE_NAME.test(s) && !ID_LIKE.test(s);
}

export function validLabelKey(k) {
  return LABEL_KEY.test(k);
}

export function validEnvKey(k) {
  return ENV_KEY.test(k) && k.indexOf('SAVIOR_') !== 0;
}

export function validSHA256(s) {
  return /^[0-9a-f]{64}$/.test(s);
}

export function validColor(s) {
  return /^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$/.test(s);
}

export function validHTTPURL(s) {
  return typeof s === 'string' && s.length <= 2048 && /^https?:\/\/[^\s]/.test(s);
}

// validRelPath mirrors proto.ValidRelPath.
export function validRelPath(p) {
  if (typeof p !== 'string' || p === '' || p.charAt(0) === '/' || utf8Len(p) > 255) return false;
  if (/[\u0000-\u001f\u007f\\:]/.test(p) || /[\ud800-\udfff]/.test(p.replace(/[\ud800-\udbff][\udc00-\udfff]/g, ''))) return false;
  const segs = p.split('/');
  if (segs[0].indexOf('.savior') === 0) return false;
  for (const s of segs) {
    if (s === '' || s === '.' || s === '..' || s.charAt(0) === '-' || /[. ]$/.test(s)) return false;
  }
  return true;
}

// validGlob reports whether pattern has valid Go path.Match syntax. Backslash
// escapes are not handled because validRelPath already rejects them.
export function validGlob(p) {
  let i = 0;
  const n = p.length;
  const classChar = function () {
    if (i >= n || p[i] === '-' || p[i] === ']') return false;
    i++;
    return i < n;
  };
  while (i < n) {
    if (p[i] !== '[') {
      i++;
      continue;
    }
    i++;
    if (i < n && p[i] === '^') i++;
    for (let nr = 0; ; nr++) {
      if (i < n && p[i] === ']' && nr > 0) {
        i++;
        break;
      }
      if (!classChar()) return false;
      if (p[i] === '-') {
        i++;
        if (!classChar()) return false;
      }
    }
  }
  return true;
}

// validOutputPattern mirrors proto.ValidOutputPattern.
export function validOutputPattern(p) {
  return validRelPath(p) && validGlob(p);
}

// parseKV parses "key=value" lines (blank lines and # comments skipped) into
// a prototype-free map. keyOK validates keys; trimValues trims values.
// Returns {map, count} or {error}.
export function parseKV(text, what, keyOK, trimValues) {
  const map = Object.create(null);
  let count = 0;
  const lines = String(text || '').split(/\r?\n/);
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const t = line.trim();
    if (!t || t.charAt(0) === '#') continue;
    const eq = line.indexOf('=');
    if (eq < 0) return {error: what + ', line ' + (i + 1) + ': expected key=value'};
    const k = line.slice(0, eq).trim();
    const v = trimValues ? line.slice(eq + 1).trim() : line.slice(eq + 1);
    if (!k || (keyOK && !keyOK(k))) return {error: what + ', line ' + (i + 1) + ': invalid key "' + k + '"'};
    if (!Object.prototype.hasOwnProperty.call(map, k)) count++;
    map[k] = v;
  }
  return {map: map, count: count};
}

// kvText renders a map as sorted "key=value" lines.
export function kvText(map) {
  return Object.keys(map || {}).sort().map(function (k) { return k + '=' + map[k]; }).join('\n');
}

// splitArgs splits a command line into argv with POSIX-shell-like quoting:
// '...' is literal, "..." allows \" \\ \$ \` escapes, and a backslash outside
// quotes escapes the next character. Nothing is expanded. Returns {args} or
// {error}.
export function splitArgs(s) {
  const args = [];
  let cur = '';
  let has = false;
  let q = '';
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (q === '\'') {
      if (c === '\'') q = '';
      else cur += c;
    } else if (q === '"') {
      if (c === '"') q = '';
      else if (c === '\\' && i + 1 < s.length && '"\\$`'.indexOf(s[i + 1]) >= 0) cur += s[++i];
      else cur += c;
    } else if (c === '\'' || c === '"') {
      q = c;
      has = true;
    } else if (c === '\\') {
      if (i + 1 < s.length) cur += s[++i];
      has = true;
    } else if (/\s/.test(c)) {
      if (has) args.push(cur);
      cur = '';
      has = false;
    } else {
      cur += c;
      has = true;
    }
  }
  if (q) return {error: 'unterminated ' + (q === '"' ? 'double' : 'single') + ' quote'};
  if (has) args.push(cur);
  return {args: args};
}

// normalizePairCode uppercases a pairing code, drops spaces and dashes, and
// applies Crockford base32's decoding aliases (I and L are 1, O is 0).
export function normalizePairCode(s) {
  return String(s || '').toUpperCase().replace(/[\s-]/g, '').replace(/[IL]/g, '1').replace(/O/g, '0');
}

// validPairCode reports whether a normalized code is 8 Crockford base32 chars.
export function validPairCode(s) {
  return /^[0-9A-HJKMNP-TV-Z]{8}$/.test(s);
}

// fileName reduces a path to a harmless download file name.
export function fileName(p, fallback) {
  const base = String(p || '').split('/').pop().replace(/[\u0000-\u001f\u007f"*:<>?\\|]/g, '_');
  return base && base !== '.' && base !== '..' ? base : (fallback === undefined ? 'download' : fallback);
}

// inputName derives a valid task input name from an uploaded file's name,
// or '' when none can be made.
export function inputName(name) {
  let s = fileName(name, '').replace(/^-+/, '').replace(/[. ]+$/, '');
  if (s.indexOf('.savior') === 0) s = '_' + s;
  return validRelPath(s) ? s : '';
}

// hostPort returns "host:port" of an http(s) URL, or ''.
export function hostPort(u) {
  try {
    return new URL(u).host;
  } catch (e) {
    return '';
  }
}

function checkMedia(m) {
  if (!m || (!m.blob) === (!m.url)) return 'each image needs exactly one of an upload or a URL';
  if (m.blob && !validSHA256(m.blob)) return 'invalid blob hash';
  if (m.url && !validHTTPURL(m.url)) return 'image URLs must start with http:// or https:// (at most 2048 characters)';
  if (m.url && m.sha256 && !validSHA256(m.sha256)) return 'the SHA-256 must be 64 lowercase hex characters';
  return '';
}

// checkDisplaySpec mirrors proto.ValidateDisplaySpec (except timezone names,
// which only the hive can check). It returns an error message or ''.
export function checkDisplaySpec(s, modes) {
  if (!s || (modes || DISPLAY_MODES).indexOf(s.mode) < 0) return 'choose a display mode';
  for (const c of [s.fg, s.bg]) {
    if (c && !validColor(c)) return 'colors must look like #rrggbb';
  }
  if (s.fit && ['contain', 'cover', 'stretch'].indexOf(s.fit) < 0) return 'fit must be contain, cover or stretch';
  if (utf8Len(s.text || '') > LIMITS.text) return 'the text is too long (at most ' + LIMITS.text + ' bytes)';
  if (utf8Len(s.title || '') > LIMITS.title) return 'the title is too long (at most ' + LIMITS.title + ' bytes)';
  if (utf8Len(s.clock_format || '') > LIMITS.clockFormat) return 'the clock format is too long';
  const iv = s.interval_s || 0;
  if (iv !== 0 && (!(iv >= LIMITS.minInterval) || iv > LIMITS.maxInterval || iv !== Math.floor(iv))) {
    return 'the interval must be ' + LIMITS.minInterval + ' to ' + LIMITS.maxInterval + ' seconds';
  }
  if (s.mode === 'image' && !s.image) return 'choose an image';
  if (s.mode === 'slideshow' && !(s.images && s.images.length)) return 'add at least one image';
  if (s.images && s.images.length > LIMITS.images) return 'at most ' + LIMITS.images + ' images';
  for (const m of (s.image ? [s.image] : []).concat(s.images || [])) {
    const e = checkMedia(m);
    if (e) return e;
  }
  return '';
}

// screenSizeMM returns the visible size of a node's connected display from
// EDID, swapped for 90/270 degree rotation, or null.
export function screenSizeMM(node) {
  const conns = (node && node.inventory && node.inventory.connectors) || [];
  for (const c of conns) {
    if (c.status === 'connected' && c.width_mm > 0 && c.height_mm > 0) {
      const r = node.display_rotate;
      return r === 90 || r === 270 ? {w: c.height_mm, h: c.width_mm} : {w: c.width_mm, h: c.height_mm};
    }
  }
  return null;
}

// wallLayout previews the WallSpec layout rules (see proto.WallSpec): a
// cell's size comes from width_mm/height_mm, else its node's EDID size, else
// 400x300 mm; each column and row is as large as its largest cell (empty ones
// use the default); gaps separate neighbors; cells are centered in their slot;
// cells with rect are placed exactly. Returns {w, h, tiles} in mm with the
// canvas normalized to its bounding box. The hive computes the real geometry.
export function wallLayout(w, nodeByID) {
  const rows = Math.max(1, w.rows | 0);
  const cols = Math.max(1, w.cols | 0);
  const gx = Math.max(0, w.gap_x_mm | 0);
  const gy = Math.max(0, w.gap_y_mm | 0);
  const cells = (w.cells || []).filter(function (c) {
    return c.rect || (c.row >= 0 && c.col >= 0 && c.row < rows && c.col < cols);
  });
  const colW = [];
  const rowH = [];
  const sizes = cells.map(function (c) {
    const auto = screenSizeMM(nodeByID && nodeByID[c.node]) || DEFAULT_SCREEN;
    const s = {w: c.width_mm || auto.w, h: c.height_mm || auto.h};
    if (!c.rect) {
      colW[c.col] = Math.max(colW[c.col] || 0, s.w);
      rowH[c.row] = Math.max(rowH[c.row] || 0, s.h);
    }
    return s;
  });
  const xs = [];
  const ys = [];
  let x = 0;
  for (let c = 0; c < cols; c++) {
    colW[c] = colW[c] || DEFAULT_SCREEN.w;
    xs[c] = x;
    x += colW[c] + gx;
  }
  let y = 0;
  for (let r = 0; r < rows; r++) {
    rowH[r] = rowH[r] || DEFAULT_SCREEN.h;
    ys[r] = y;
    y += rowH[r] + gy;
  }
  const tiles = cells.map(function (c, i) {
    const t = {row: c.row, col: c.col, node: c.node};
    if (c.rect) return Object.assign(t, {x: c.rect.x, y: c.rect.y, w: c.rect.w, h: c.rect.h});
    const s = sizes[i];
    return Object.assign(t, {
      x: xs[c.col] + Math.round((colW[c.col] - s.w) / 2),
      y: ys[c.row] + Math.round((rowH[c.row] - s.h) / 2),
      w: s.w, h: s.h,
    });
  });
  // The empty grid itself spans every slot, so the preview keeps its shape.
  let minX = 0;
  let minY = 0;
  let maxX = x - gx;
  let maxY = y - gy;
  for (const t of tiles) {
    minX = Math.min(minX, t.x);
    minY = Math.min(minY, t.y);
    maxX = Math.max(maxX, t.x + t.w);
    maxY = Math.max(maxY, t.y + t.h);
  }
  for (const t of tiles) {
    t.x -= minX;
    t.y -= minY;
  }
  const slots = [];
  for (let r = 0; r < rows; r++) {
    for (let c = 0; c < cols; c++) slots.push({row: r, col: c, x: xs[c] - minX, y: ys[r] - minY, w: colW[c], h: rowH[r]});
  }
  return {w: maxX - minX, h: maxY - minY, tiles: tiles, slots: slots};
}

// Common IANA zones, used when the browser can't list its own.
export const ZONES = ['UTC', 'Europe/London', 'Europe/Berlin', 'Europe/Paris', 'Europe/Madrid', 'Europe/Rome',
  'Europe/Warsaw', 'Europe/Moscow', 'Africa/Johannesburg', 'Africa/Lagos', 'Africa/Nairobi', 'Asia/Dubai',
  'Asia/Kolkata', 'Asia/Shanghai', 'Asia/Singapore', 'Asia/Tokyo', 'Australia/Sydney', 'Pacific/Auckland',
  'America/Sao_Paulo', 'America/New_York', 'America/Chicago', 'America/Denver', 'America/Los_Angeles',
  'America/Mexico_City', 'America/Toronto'];
