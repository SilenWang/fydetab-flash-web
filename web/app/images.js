// Image catalogue and the plain (navigation) download.
//
// The page and the vendor files live on different origins, and neither
// download.fydeos.io nor the GitHub release assets send
// `Access-Control-Allow-Origin` (SIL-890). No amount of cleverness lets script
// read those bytes: `fetch` is refused, `no-cors` yields an opaque body, a
// directory grant buys local read permission, not network read permission.
//
// So a download here is a navigation, not a transfer: the page hands the URL to
// the browser's own downloader with an `<a download>` click and keeps its hands
// off the bytes. The file lands in the browser's download folder; the user
// picks it back up with the local file picker. That is why this module has no
// fetch, no probe and no progress: there is nothing to read.
//
// Everything here takes its browser surface as an argument, so the logic is
// testable in node without a DOM. app.js supplies the real one.
// User-visible text comes from ./i18n.js; each entry point that produces copy
// takes a `t` so a test can pin the language.

import { t as translate } from './i18n.js';

/** CATALOG_URL is relative to the page (/app/). */
export const CATALOG_URL = './images.json';

function isPlainObject(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function nullableString(value) {
  return typeof value === 'string' && value.trim() !== '' ? value.trim() : null;
}

function nullableSize(value) {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? Math.round(value) : null;
}

/** normaliseEntry validates one catalogue row; null entries are dropped. */
export function normaliseEntry(raw, index) {
  if (!isPlainObject(raw)) return null;
  const id = nullableString(raw.id);
  const label = nullableString(raw.label);
  if (!id || !label) return null;
  const sha256 = nullableString(raw.sha256);
  if (sha256 !== null && !/^[0-9a-f]{64}$/i.test(sha256)) return null;
  return {
    id,
    label,
    brand: nullableString(raw.brand),
    released: nullableString(raw.released),
    device: nullableString(raw.device),
    version: nullableString(raw.version),
    file: nullableString(raw.file),
    size: nullableSize(raw.size),
    sha256: sha256 === null ? null : sha256.toLowerCase(),
    url: nullableString(raw.url),
    page: nullableString(raw.page),
    demo: raw.demo === true,
    notes: nullableString(raw.notes),
    index,
    // A catalogue row without a URL is still useful: it tells the user which
    // image to fetch and where the vendor publishes it.
    hasDirectURL: nullableString(raw.url) !== null,
    hasChecksum: sha256 !== null,
  };
}

/**
 * parseCatalog accepts the JSON text (or the already parsed object) and
 * returns the usable entries plus what was dropped, so the page can log a
 * broken catalogue instead of silently showing nothing.
 */
export function parseCatalog(input, { t = translate } = {}) {
  let raw = input;
  if (typeof input === 'string') {
    try {
      raw = JSON.parse(input);
    } catch (err) {
      return { ok: false, error: t('images.catalog.badJson', { error: err.message }), entries: [], dropped: [] };
    }
  }
  if (!isPlainObject(raw) || !Array.isArray(raw.images)) {
    return { ok: false, error: t('images.catalog.missingImages'), entries: [], dropped: [] };
  }
  const entries = [];
  const dropped = [];
  raw.images.forEach((item, index) => {
    const entry = normaliseEntry(item, index);
    if (entry) entries.push(entry);
    else dropped.push({ index, id: isPlainObject(item) ? (item.id ?? null) : null });
  });
  return {
    ok: true,
    schemaVersion: raw.schemaVersion ?? null,
    updated: nullableString(raw.updated),
    note: nullableString(raw.note),
    entries,
    dropped,
  };
}

/** loadCatalog fetches and parses images.json (precached by sw.js). */
export async function loadCatalog({ fetchImpl = globalThis.fetch, url = CATALOG_URL, signal, t = translate } = {}) {
  if (typeof fetchImpl !== 'function') return { ok: false, error: t('images.catalog.noFetch'), entries: [], dropped: [] };
  try {
    const response = await fetchImpl(url, { signal });
    if (!response.ok) {
      return { ok: false, error: t('images.catalog.http', { status: response.status }), entries: [], dropped: [] };
    }
    return parseCatalog(await response.text(), { t });
  } catch (err) {
    return { ok: false, error: t('images.catalog.fetchFailed', { error: err?.message ?? err }), entries: [], dropped: [] };
  }
}

/** formatBytes renders binary units the same way the rest of the page does. */
export function formatBytes(n) {
  if (!Number.isFinite(n) || n < 0) return '?';
  const unit = 1024;
  if (n < unit) return `${n} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let value = n / unit;
  let index = 0;
  while (value >= unit && index < units.length - 1) {
    value /= unit;
    index += 1;
  }
  return `${value.toFixed(value >= 10 ? 1 : 2)} ${units[index]}`;
}

/** formatSpeed renders a transfer rate. */
export function formatSpeed(bytesPerSec) {
  if (!Number.isFinite(bytesPerSec) || bytesPerSec <= 0) return '—';
  return `${formatBytes(bytesPerSec)}/s`;
}

/** formatDuration renders seconds in the active language ("1m 05s" / "1 分 05 秒"). */
export function formatDuration(seconds, { t = translate } = {}) {
  if (!Number.isFinite(seconds) || seconds < 0) return '—';
  if (seconds < 1) return t('time.lessThanSecond');
  const total = Math.round(seconds);
  if (total < 60) return t('time.seconds', { count: total });
  const minutes = Math.floor(total / 60);
  const rest = total % 60;
  if (minutes < 60) return t('time.minutes', { minutes, seconds: String(rest).padStart(2, '0') });
  const hours = Math.floor(minutes / 60);
  return t('time.hours', { hours, minutes: String(minutes % 60).padStart(2, '0') });
}

function absoluteURL(url, base) {
  try {
    return new URL(url, base).toString();
  } catch {
    return url;
  }
}

/**
 * downloadTarget turns a catalogue row into the two things the navigation
 * needs: an absolute URL and the file name the browser should use when the
 * server does not name it. Returns null when the row has no direct link.
 */
export function downloadTarget(entry, base = globalThis.location?.href ?? 'http://localhost/') {
  if (!isPlainObject(entry)) return null;
  const url = nullableString(entry.url);
  if (!url) return null;
  return {
    url: absoluteURL(url, base),
    name: nullableString(entry.file) ?? 'update.img',
    label: nullableString(entry.label) ?? '',
  };
}

/**
 * matchCatalogFile decides whether a file the user picked is the catalogue row
 * they just downloaded. A cross-origin download keeps the vendor name (or the
 * one in GitHub's `Content-Disposition`), so the name is tried first; the size
 * is the fallback for a browser that renamed it to `update.img`.
 */
export function matchCatalogFile(entry, file) {
  if (!isPlainObject(entry) || !file) return false;
  const name = (file.name ?? '').toLowerCase();
  const expected = (entry.file ?? '').toLowerCase();
  if (expected && name === expected) return true;
  return Boolean(entry.size) && Boolean(file.size) && entry.size === file.size;
}

/**
 * matchCatalogEntry finds the catalogue row a picked file belongs to, if any.
 * `preferred` is checked first so the row the user just asked the browser to
 * download wins over an older version that happens to have the same size.
 */
export function matchCatalogEntry(entries, file, preferred = null) {
  if (!Array.isArray(entries) || !file) return null;
  if (preferred && matchCatalogFile(preferred, file)) return preferred;
  return entries.find((entry) => matchCatalogFile(entry, file)) ?? null;
}

/**
 * triggerDownload hands the URL to the browser's downloader with an
 * `<a download>` click and returns immediately.
 *
 * `download` is a hint the browser only honours same-origin; for these
 * cross-origin files it still works out as a download because the vendors
 * serve a file the browser will not render (zip / octet-stream, and GitHub
 * sends `Content-Disposition: attachment`). Either way the page stays put and
 * never sees a byte — which is the point.
 */
export function triggerDownload(entry, { document: doc = globalThis.document, base } = {}) {
  const target = downloadTarget(entry, base);
  if (!target || typeof doc?.createElement !== 'function' || !doc.body) return null;
  const link = doc.createElement('a');
  link.href = target.url;
  link.download = target.name;
  link.rel = 'noreferrer';
  link.referrerPolicy = 'no-referrer';
  link.hidden = true;
  doc.body.appendChild(link);
  link.click();
  link.remove();
  return target;
}
