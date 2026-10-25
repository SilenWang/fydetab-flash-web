// The download path, exercised without a browser.
//
// Three things here decide whether a user can get an 8 GiB image onto disk:
// whether the URL is readable by script at all (CORS), whether the browser can
// stream to a file (File System Access API), and whether the read loop really
// streams instead of buffering. The fakes below implement only the surface
// images.js touches, so the test stays honest about the logic.

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  capabilityReport,
  downloadEntry,
  formatBytes,
  formatDuration,
  formatSpeed,
  loadCatalog,
  parseCatalog,
  probeCors,
  progressSnapshot,
} from '../images.js';

function fakeHeaders(headers = {}) {
  const map = new Map(Object.entries(headers).map(([key, value]) => [key.toLowerCase(), String(value)]));
  return { get: (key) => map.get(String(key).toLowerCase()) ?? null };
}

function fakeResponse({ status = 200, headers = {}, chunks = [] } = {}) {
  const pending = [...chunks];
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 200 ? 'OK' : 'ERR',
    headers: fakeHeaders(headers),
    body: {
      getReader: () => ({
        async read() {
          if (pending.length === 0) return { done: true, value: undefined };
          return { done: false, value: pending.shift() };
        },
      }),
      cancel: async () => {},
    },
  };
}

function fakeSaveFileHandle(name = 'picked.img') {
  const writes = [];
  const calls = { closed: 0, aborted: 0 };
  const content = [];
  return {
    name,
    writes,
    calls,
    async createWritable() {
      return {
        async write(chunk) {
          writes.push(chunk);
          content.push(...chunk);
        },
        async close() {
          calls.closed += 1;
        },
        async abort() {
          calls.aborted += 1;
        },
      };
    },
    async getFile() {
      return { name, size: content.length, type: 'application/octet-stream' };
    },
  };
}

const catalogText = JSON.stringify({
  schemaVersion: 1,
  updated: '2026-10-25',
  images: [
    {
      id: 'good',
      label: 'Good image',
      file: 'good.img',
      size: 1024,
      sha256: 'A'.repeat(64),
      url: 'https://example.test/good.img',
    },
    { id: 'no-url', label: 'No direct link', size: 2048, sha256: null, url: null, page: 'https://example.test/download' },
    { id: 'bad-sha', label: 'Broken checksum', sha256: 'not-a-digest' },
    { label: 'no id' },
  ],
});

test('the catalogue keeps what the page can use and reports what it dropped', () => {
  const catalog = parseCatalog(catalogText);
  assert.equal(catalog.ok, true);
  assert.equal(catalog.schemaVersion, 1);
  assert.deepEqual(catalog.entries.map((entry) => entry.id), ['good', 'no-url']);
  assert.equal(catalog.entries[0].sha256, 'a'.repeat(64), 'sha256 is normalised to lowercase');
  assert.equal(catalog.entries[0].hasDirectURL, true);
  assert.equal(catalog.entries[0].hasChecksum, true);
  assert.equal(catalog.entries[1].hasDirectURL, false, 'a null url is a catalogue row without a download');
  assert.equal(catalog.entries[1].hasChecksum, false);
  assert.deepEqual(catalog.dropped.map((item) => item.index), [2, 3]);

  const broken = parseCatalog('{ not json');
  assert.equal(broken.ok, false);
  assert.match(broken.error, /JSON/);
  assert.deepEqual(broken.entries, []);
});

test('loadCatalog surfaces HTTP failures instead of an empty list', async () => {
  const failed = await loadCatalog({ fetchImpl: async () => fakeResponse({ status: 404 }) });
  assert.equal(failed.ok, false);
  assert.match(failed.error, /404/);

  const ok = await loadCatalog({ fetchImpl: async () => ({ ok: true, status: 200, text: async () => catalogText }) });
  assert.equal(ok.ok, true);
  assert.equal(ok.entries.length, 2);
});

test('capabilityReport tells Firefox users why download is unavailable', () => {
  const firefox = capabilityReport({ ReadableStream: function () {}, WritableStream: function () {} });
  assert.equal(firefox.save, false);
  assert.equal(firefox.chromiumOnly, true);
  assert.match(firefox.advice, /Chrome|Edge/);

  const chromium = capabilityReport({
    showSaveFilePicker: () => {},
    showOpenFilePicker: () => {},
    ReadableStream: function () {},
    WritableStream: function () {},
  });
  assert.equal(chromium.save, true);
  assert.equal(chromium.open, true);
  assert.equal(chromium.chromiumOnly, false);
  assert.equal(chromium.advice, '');
});

test('probeCors accepts same-origin files without a request', async () => {
  let calls = 0;
  const probe = await probeCors('./images.json', {
    base: 'https://example.test/app/',
    fetchImpl: async () => {
      calls += 1;
      return fakeResponse({});
    },
  });
  assert.equal(probe.direct, true);
  assert.equal(probe.sameOrigin, true);
  assert.equal(calls, 0, 'same-origin needs no probe');
});

test('probeCors only trusts a cross-origin URL that returns the CORS header', async () => {
  const allowed = await probeCors('https://cdn.test/a.img', {
    base: 'https://example.test/app/',
    fetchImpl: async () => fakeResponse({ headers: { 'access-control-allow-origin': '*' } }),
  });
  assert.equal(allowed.direct, true);
  assert.match(allowed.reason, /Access-Control-Allow-Origin/);

  const missing = await probeCors('https://cdn.test/a.img', {
    base: 'https://example.test/app/',
    fetchImpl: async () => fakeResponse({ headers: {} }),
  });
  assert.equal(missing.direct, false);
  assert.match(missing.reason, /Access-Control-Allow-Origin/);
  assert.equal(missing.url, 'https://cdn.test/a.img', 'the fallback URL must survive the refusal');
});

test('probeCors treats a CORS rejection as "open it in a tab", not a dead button', async () => {
  const probe = await probeCors('https://vendor.test/fw.img', {
    base: 'https://example.test/app/',
    fetchImpl: async () => {
      throw new TypeError('Failed to fetch');
    },
  });
  assert.equal(probe.direct, false);
  assert.match(probe.reason, /CORS/);
  assert.match(probe.reason, /Failed to fetch/);
});

test('probeCors falls back to a ranged GET when HEAD is refused', async () => {
  const seen = [];
  const probe = await probeCors('https://cdn.test/a.img', {
    base: 'https://example.test/app/',
    fetchImpl: async (_url, init) => {
      seen.push(init.method);
      if (init.method === 'HEAD') return fakeResponse({ status: 405 });
      return fakeResponse({ status: 206, headers: { 'access-control-allow-origin': '*', 'content-length': '1' } });
    },
  });
  assert.deepEqual(seen, ['HEAD', 'GET']);
  assert.equal(probe.direct, true);
  assert.equal(probe.method, 'GET');
});

test('downloadEntry streams every chunk into the picked file', async () => {
  const chunks = [new Uint8Array(4).fill(1), new Uint8Array(4).fill(2), new Uint8Array(2).fill(3)];
  const handle = fakeSaveFileHandle('update.img');
  const progress = [];
  let clock = 0;
  const result = await downloadEntry(
    { id: 'x', file: 'update.img', url: 'https://cdn.test/a.img', size: 10 },
    {
      base: 'https://example.test/app/',
      fetchImpl: async () =>
        fakeResponse({
          headers: { 'access-control-allow-origin': '*', 'content-length': '10' },
          chunks,
        }),
      savePicker: async (options) => {
        assert.equal(options.suggestedName, 'update.img');
        return handle;
      },
      now: () => {
        clock += 500;
        return clock;
      },
      progressIntervalMs: 0,
      onProgress: (snapshot) => progress.push(snapshot),
    },
  );

  assert.equal(result.ok, true, result.reason);
  assert.equal(result.kind, 'saved');
  assert.equal(result.received, 10);
  assert.equal(result.total, 10);
  assert.equal(result.file.name, 'update.img');
  assert.equal(handle.writes.length, 3, 'each chunk is written as it arrives');
  assert.deepEqual([...handle.writes[2]], [3, 3]);
  assert.equal(handle.calls.closed, 1);
  assert.equal(handle.calls.aborted, 0);
  assert.ok(progress.length >= 3);
  assert.equal(progress.at(-1).percent, 100);
  assert.ok(progress.at(-1).speed > 0);
  assert.equal(progress.at(-1).etaSeconds, 0);
});

test('downloadEntry refuses to start when CORS blocks the URL, and never opens a picker', async () => {
  const result = await downloadEntry(
    { id: 'x', file: 'a.img', url: 'https://vendor.test/a.img' },
    {
      base: 'https://example.test/app/',
      fetchImpl: async () => fakeResponse({ headers: {} }),
      savePicker: async () => {
        throw new Error('the picker must not open for a blocked URL');
      },
    },
  );
  assert.equal(result.ok, false);
  assert.equal(result.kind, 'cors');
  assert.equal(result.url, 'https://vendor.test/a.img');
  assert.match(result.reason, /Access-Control-Allow-Origin/);
});

test('downloadEntry names the missing browser capability', async () => {
  const result = await downloadEntry(
    { id: 'x', url: 'https://cdn.test/a.img' },
    { base: 'https://example.test/app/', fetchImpl: async () => fakeResponse({}), savePicker: undefined },
  );
  assert.equal(result.ok, false);
  assert.equal(result.kind, 'unsupported');
  assert.match(result.reason, /Chrome|Edge/);
});

test('downloadEntry says so when the catalogue has no direct URL', async () => {
  const result = await downloadEntry({ id: 'x', url: null, page: 'https://vendor.test/download' }, {});
  assert.equal(result.ok, false);
  assert.equal(result.kind, 'no-url');
  assert.match(result.reason, /清单没有收录直链/);
  assert.equal(result.page, 'https://vendor.test/download');
});

test('a failed transfer aborts the writable instead of leaving a half-written file', async () => {
  const handle = fakeSaveFileHandle();
  const result = await downloadEntry(
    { id: 'x', file: 'a.img', url: 'https://cdn.test/a.img' },
    {
      base: 'https://example.test/app/',
      // The probe succeeds (HEAD), the real download does not.
      fetchImpl: async (_url, init) =>
        init.method === 'HEAD'
          ? fakeResponse({ headers: { 'access-control-allow-origin': '*' } })
          : fakeResponse({ status: 500, headers: { 'access-control-allow-origin': '*' } }),
      savePicker: async () => handle,
    },
  );
  assert.equal(result.ok, false);
  assert.equal(result.kind, 'transfer');
  assert.match(result.reason, /HTTP 500/);
  assert.equal(handle.calls.aborted, 1);
  assert.equal(handle.calls.closed, 0);
});

test('the progress helpers show speed and a remaining time', () => {
  const snapshot = progressSnapshot({ received: 512 * 1024 * 1024, total: 2 * 1024 * 1024 * 1024, elapsedMs: 5000 });
  assert.equal(snapshot.percent, 25);
  assert.ok(Math.abs(snapshot.speed - 102.4 * 1024 * 1024) < 1);
  assert.ok(snapshot.etaSeconds > 14.9 && snapshot.etaSeconds < 15.1);

  const unknown = progressSnapshot({ received: 1024, total: 0, elapsedMs: 1000 });
  assert.equal(unknown.percent, null);
  assert.equal(unknown.etaSeconds, null);

  assert.equal(formatBytes(1024), '1.00 KiB');
  assert.equal(formatSpeed(0), '—');
  assert.equal(formatDuration(12), '12 秒');
  assert.equal(formatDuration(65), '1 分 05 秒');
  assert.equal(formatDuration(0.2), '<1 秒');
  assert.equal(formatDuration(-1), '—');
});
