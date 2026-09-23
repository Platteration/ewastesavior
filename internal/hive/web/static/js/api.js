// HTTP layer for the hive API (docs/DESIGN.md 6.3, 7.2). Every request is
// same-origin with the session cookie and carries X-Savior (the CSRF guard)
// and X-Savior-Client-Time (lets a hive without a trusted clock set it).

export const BASE = '/api/v1';

let unauthorized = null;

// setUnauthorizedHandler registers the callback run on any 401.
export function setUnauthorizedHandler(fn) {
  unauthorized = fn;
}

// ApiError is a failed API call. status is 0 for network errors.
export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.handled = false;
  }
}

// clientTime is now in RFC 3339 (UTC, whole seconds).
export function clientTime() {
  return new Date().toISOString().replace(/\.\d+Z$/, 'Z');
}

function baseHeaders() {
  return {'X-Savior': '1', 'X-Savior-Client-Time': clientTime()};
}

// enc escapes one path segment.
export function enc(s) {
  return encodeURIComponent(String(s));
}

// query renders params as a query string, skipping empty values.
export function query(params) {
  if (!params) return '';
  const parts = [];
  for (const k of Object.keys(params)) {
    const v = params[k];
    if (v == null || v === '') continue;
    parts.push(encodeURIComponent(k) + '=' + encodeURIComponent(String(v)));
  }
  return parts.length ? '?' + parts.join('&') : '';
}

// errorText extracts the message of an error response: the API's
// {"error": "..."} when present, else short plain text, else the status.
export function errorText(status, statusText, body) {
  const fallback = 'HTTP ' + status + (statusText ? ' ' + statusText : '');
  let j;
  try {
    j = JSON.parse(body);
  } catch (e) {
    const t = String(body || '').trim();
    return t && t.length <= 300 && t.charAt(0) !== '<' ? t : fallback;
  }
  return j && typeof j.error === 'string' && j.error ? j.error : fallback;
}

function fail(status, message, opts) {
  const err = new ApiError(status, message);
  if (status === 401 && opts.auth !== false && unauthorized) {
    err.handled = true;
    unauthorized();
  }
  return err;
}

// request calls the API. path is relative to /api/v1. opts:
//   body     JSON-encoded request body
//   query    query parameters
//   signal   AbortSignal (navigation)
//   timeout  ms before giving up (default 30000, 0 = none)
//   as       'json' (default), 'text', 'blob' or 'raw' ({buf, headers})
//   auth     false: a 401 is an ordinary error, not "signed out"
export async function request(method, path, opts) {
  opts = opts || {};
  const headers = baseHeaders();
  headers.Accept = !opts.as || opts.as === 'json' ? 'application/json' : '*/*';
  let body;
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(opts.body);
  }
  const ctl = new AbortController();
  const outer = opts.signal;
  const onAbort = function () { ctl.abort(); };
  if (outer) {
    if (outer.aborted) ctl.abort();
    else outer.addEventListener('abort', onAbort);
  }
  let timedOut = false;
  const ms = opts.timeout === undefined ? 30000 : opts.timeout;
  const timer = ms ? setTimeout(function () { timedOut = true; ctl.abort(); }, ms) : 0;
  try {
    const res = await fetch(BASE + path + query(opts.query), {
      method: method, headers: headers, body: body, credentials: 'same-origin', cache: 'no-store', signal: ctl.signal,
    });
    if (!res.ok) throw fail(res.status, errorText(res.status, res.statusText, await res.text()), opts);
    switch (opts.as) {
    case 'text': return await res.text();
    case 'blob': return await res.blob();
    case 'raw': return {buf: await res.arrayBuffer(), headers: res.headers};
    }
    const text = await res.text();
    if (!text) return null;
    try {
      return JSON.parse(text);
    } catch (e) {
      throw new ApiError(res.status, 'The hive sent an unexpected response.');
    }
  } catch (e) {
    if (e instanceof ApiError) throw e;
    if (e && e.name === 'AbortError' && !timedOut) throw e;
    throw new ApiError(0, timedOut ? 'The hive did not answer in time.'
      : 'Cannot reach the hive (' + ((e && e.message) || 'network error') + ').');
  } finally {
    clearTimeout(timer);
    if (outer) outer.removeEventListener('abort', onAbort);
  }
}

export function get(path, opts) { return request('GET', path, opts); }
export function post(path, body, opts) { return request('POST', path, Object.assign({body: body === undefined ? {} : body}, opts)); }
export function put(path, body, opts) { return request('PUT', path, Object.assign({body: body}, opts)); }
export function patch(path, body, opts) { return request('PATCH', path, Object.assign({body: body}, opts)); }
export function del(path, opts) { return request('DELETE', path, opts); }

function abortError() {
  const e = new Error('Aborted');
  e.name = 'AbortError';
  return e;
}

// upload streams a file to POST /admin/blobs and resolves to its BlobInfo.
// XHR rather than fetch because only XHR reports upload progress.
export function upload(file, onProgress, signal) {
  return new Promise(function (resolve, reject) {
    if (signal && signal.aborted) {
      reject(abortError());
      return;
    }
    const xhr = new XMLHttpRequest();
    xhr.open('POST', BASE + '/admin/blobs');
    const hd = baseHeaders();
    for (const k of Object.keys(hd)) xhr.setRequestHeader(k, hd[k]);
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.setRequestHeader('Accept', 'application/json');
    if (onProgress) {
      xhr.upload.addEventListener('progress', function (e) {
        if (e.lengthComputable) onProgress(e.loaded, e.total);
      });
    }
    xhr.addEventListener('load', function () {
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          const info = JSON.parse(xhr.responseText);
          if (info && typeof info.sha256 === 'string') {
            resolve(info);
            return;
          }
        } catch (e) { /* fall through */ }
        reject(new ApiError(xhr.status, 'The hive sent an unexpected response.'));
        return;
      }
      reject(fail(xhr.status, errorText(xhr.status, xhr.statusText, xhr.responseText), {}));
    });
    xhr.addEventListener('error', function () { reject(new ApiError(0, 'Upload failed: cannot reach the hive.')); });
    xhr.addEventListener('abort', function () { reject(abortError()); });
    if (signal) signal.addEventListener('abort', function () { xhr.abort(); });
    xhr.send(file);
  });
}
