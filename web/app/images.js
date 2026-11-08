// Image catalogue, download probe and streaming save.
//
// The page has to answer three questions before it can offer "下载":
//
//   1. may the browser read this URL at all? A cross-origin file without
//      `Access-Control-Allow-Origin` cannot be fetched by script. Rather than
//      failing silently (or showing a progress bar that never moves), the page
//      probes first and degrades to "open it in a new tab", with the reason.
//   2. can this browser stream to disk? showSaveFilePicker + a WritableStream
//      is what keeps a 8 GiB image out of memory; Firefox/Safari cannot do it
//      and are told so instead of getting a dead button.
//   3. how far along is it? bytes/total, speed and ETA come from the same read
//      loop that writes, never from a timer.
//
// Everything here takes its browser surface as an argument (fetch, picker), so
// the logic is testable in node without a DOM. app.js supplies the real ones.
// User-visible text comes from ./i18n.js; each entry point takes a `t` so a
// test can pin the language.

import { t as translate } from './i18n.js';

/** CATALOG_URL is relative to the page (/app/). */
export const CATALOG_URL = './images.json';

/** probeSampleMiB is how much a ranged GET asks for when HEAD is useless. */
const PROBE_BYTES = 1;

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

/**
 * capabilityReport says what this browser can do. `save` is the one that
 * matters: without it a multi-GiB download would have to be buffered.
 */
export function capabilityReport(env = globalThis, { t = translate } = {}) {
  const save = typeof env.showSaveFilePicker === 'function';
  const open = typeof env.showOpenFilePicker === 'function';
  const streams = typeof env.ReadableStream === 'function' && typeof env.WritableStream === 'function';
  return {
    save,
    open,
    streams,
    // Chromium-only surfaces; the page must say so rather than disable a
    // button without a word.
    chromiumOnly: !save || !streams,
    advice: save && streams ? '' : t('images.advice.chromium'),
  };
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

/**
 * progressSnapshot turns the bytes written so far into the three numbers the
 * UI shows. Speed is the average since the transfer started: for a single
 * long download that is stable, while an instant rate jumps around with
 * buffering.
 */
export function progressSnapshot({ received = 0, total = 0, elapsedMs = 0 } = {}) {
  const seconds = elapsedMs / 1000;
  const speed = seconds > 0 ? received / seconds : 0;
  const remainingBytes = total > 0 ? Math.max(0, total - received) : null;
  const etaSeconds = speed > 0 && remainingBytes !== null ? remainingBytes / speed : null;
  return {
    received,
    total,
    percent: total > 0 ? Math.min(100, (received / total) * 100) : null,
    speed,
    etaSeconds,
    elapsedMs,
  };
}

function absoluteURL(url, base) {
  try {
    return new URL(url, base).toString();
  } catch {
    return url;
  }
}

function sameOrigin(a, b) {
  try {
    return new URL(a).origin === new URL(b).origin;
  } catch {
    return false;
  }
}

async function attemptFetch(fetchImpl, url, init) {
  try {
    const response = await fetchImpl(url, init);
    return { response };
  } catch (err) {
    return { error: err };
  }
}

/**
 * probeCors decides whether the page may fetch `url` itself.
 *
 * The check is a real request, not a guess: a HEAD first (cheap), then a
 * one-byte ranged GET when the server does not answer HEAD. A cross-origin
 * response without `Access-Control-Allow-Origin` is not readable by script
 * even when the status is 200, so the header is checked explicitly instead of
 * trusting `response.ok`.
 */
export async function probeCors(
  url,
  { fetchImpl = globalThis.fetch, base = globalThis.location?.href ?? 'http://localhost/', signal, t = translate } = {},
) {
  const absolute = absoluteURL(url, base);
  if (sameOrigin(absolute, base)) {
    return { direct: true, url: absolute, sameOrigin: true, reason: t('images.probe.sameOrigin') };
  }
  if (typeof fetchImpl !== 'function') {
    return { direct: false, url: absolute, sameOrigin: false, reason: t('images.probe.noFetch') };
  }

  const head = await attemptFetch(fetchImpl, absolute, { method: 'HEAD', mode: 'cors', signal });
  let response = head.response;
  let method = 'HEAD';
  if (!response || !response.ok) {
    // 405/403 on HEAD is common; a one-byte ranged GET answers the same
    // question without downloading the image.
    const got = await attemptFetch(fetchImpl, absolute, {
      method: 'GET',
      mode: 'cors',
      headers: { Range: `bytes=0-${PROBE_BYTES - 1}` },
      signal,
    });
    if (got.response) {
      response = got.response;
      method = 'GET';
      // Do not leave the body of a ranged request hanging.
      try {
        await got.response.body?.cancel?.();
      } catch {
        // already closed
      }
    } else if (head.error) {
      return {
        direct: false,
        url: absolute,
        sameOrigin: false,
        reason: t('images.probe.cors', { error: head.error?.message ?? head.error }),
      };
    } else {
      return {
        direct: false,
        url: absolute,
        sameOrigin: false,
        reason: t('images.probe.rejected', {
          head: head.response?.status ?? '?',
          get: got.response?.status ?? '?',
        }),
      };
    }
  }

  const allow = response.headers?.get?.('access-control-allow-origin') ?? null;
  const contentLength = Number(response.headers?.get?.('content-length') ?? NaN);
  const contentType = response.headers?.get?.('content-type') ?? null;
  if (!allow) {
    return {
      direct: false,
      url: absolute,
      sameOrigin: false,
      status: response.status,
      method,
      contentLength: Number.isFinite(contentLength) ? contentLength : null,
      contentType,
      reason: t('images.probe.noAcao'),
    };
  }
  return {
    direct: true,
    url: absolute,
    sameOrigin: false,
    status: response.status,
    method,
    allowOrigin: allow,
    contentLength: Number.isFinite(contentLength) ? contentLength : null,
    contentType,
    reason: t('images.probe.allowed', { origin: allow }),
  };
}

/**
 * downloadEntry streams one catalogue entry to a file the user picks.
 *
 * Returns a result object instead of throwing, because every failure mode has
 * a different next step for the user (pick Chrome, open the link in a tab,
 * retry, free disk space). `kind` is what app.js switches on.
 */
export async function downloadEntry(
  entry,
  {
    fetchImpl = globalThis.fetch,
    savePicker = globalThis.showSaveFilePicker,
    base = globalThis.location?.href ?? 'http://localhost/',
    onProgress,
    onProbe,
    signal,
    now = () => performance.now(),
    progressIntervalMs = 250,
    t = translate,
  } = {},
) {
  if (!entry || typeof entry !== 'object') return { ok: false, kind: 'bad-entry', reason: t('images.badEntry') };
  if (!entry.url) {
    return {
      ok: false,
      kind: 'no-url',
      reason: t(entry.page ? 'images.noUrl.withPage' : 'images.noUrl.plain'),
      page: entry.page ?? null,
    };
  }

  const caps = capabilityReport(
    { showSaveFilePicker: savePicker, ReadableStream: globalThis.ReadableStream, WritableStream: globalThis.WritableStream },
    { t },
  );
  if (!caps.save) {
    return { ok: false, kind: 'unsupported', reason: caps.advice, url: absoluteURL(entry.url, base) };
  }

  const probe = await probeCors(entry.url, { fetchImpl, base, signal, t });
  onProbe?.(probe);
  if (!probe.direct) {
    return { ok: false, kind: 'cors', reason: probe.reason, url: probe.url, probe };
  }

  let handle;
  try {
    handle = await savePicker({ suggestedName: entry.file ?? 'update.img' });
  } catch (err) {
    return {
      ok: false,
      kind: err?.name === 'AbortError' ? 'cancelled' : 'picker',
      reason:
        err?.name === 'AbortError'
          ? t('images.picker.cancelled')
          : t('images.picker.failed', { error: err?.message ?? err }),
      url: probe.url,
    };
  }

  const writable = await handle.createWritable();
  const started = now();
  let received = 0;
  let total = probe.contentLength ?? entry.size ?? 0;
  let lastTick = -Infinity;

  try {
    const response = await fetchImpl(probe.url, { signal });
    if (!response.ok) throw new Error(`HTTP ${response.status} ${response.statusText ?? ''}`.trim());
    const declared = Number(response.headers?.get?.('content-length') ?? NaN);
    if (Number.isFinite(declared) && declared > 0) total = declared;
    if (!response.body) throw new Error(t('images.transfer.noBody'));

    const reader = response.body.getReader();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (!value || value.length === 0) continue;
      // One chunk in, one chunk out: the image never accumulates in memory.
      await writable.write(value);
      received += value.length;
      const elapsedMs = now() - started;
      if (elapsedMs - lastTick >= progressIntervalMs) {
        lastTick = elapsedMs;
        onProgress?.(progressSnapshot({ received, total, elapsedMs }));
      }
    }
    await writable.close();
  } catch (err) {
    try {
      await writable.abort?.(err);
    } catch {
      // The writable may already be closed by the fetch failure.
    }
    return {
      ok: false,
      kind: 'transfer',
      reason: t('images.transfer.failed', { error: err?.message ?? err }),
      url: probe.url,
      received,
      total,
    };
  }

  const elapsedMs = now() - started;
  const snapshot = progressSnapshot({ received, total, elapsedMs });
  onProgress?.(snapshot);

  let file = null;
  let name = handle.name ?? entry.file ?? 'update.img';
  try {
    if (typeof handle.getFile === 'function') {
      file = await handle.getFile();
      name = file.name ?? name;
    }
  } catch {
    // A handle from a picker we do not own may not expose getFile; the user
    // can still pick the file with "选择本地镜像".
  }

  return { ok: true, kind: 'saved', file, name, ...snapshot, url: probe.url, probe };
}
