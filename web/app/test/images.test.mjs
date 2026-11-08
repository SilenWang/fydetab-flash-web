// The download path, exercised without a browser.
//
// A download here is a navigation, not a transfer: the vendor origins send no
// `Access-Control-Allow-Origin`, so no script may read those bytes (SIL-890).
// What is left to test is the catalogue, and the one thing the page still does
// with a URL — hand it to the browser's own downloader and walk away.

import test from 'node:test';
import assert from 'node:assert/strict';

// The checks below assert the Chinese copy, so pin the language instead of
// depending on the machine's navigator.languages.
import { setLocale } from '../i18n.js';

setLocale('zh');

import {
  downloadTarget,
  formatBytes,
  formatDuration,
  formatSpeed,
  loadCatalog,
  matchCatalogEntry,
  matchCatalogFile,
  parseCatalog,
  triggerDownload,
} from '../images.js';

/** fakeDocument records the anchor click the download path performs. */
function fakeDocument() {
  const clicks = [];
  const doc = {
    body: {
      appendChild(node) {
        node.appended = true;
      },
    },
    createElement(tag) {
      const el = {
        tag,
        click() {
          clicks.push({ href: el.href, download: el.download, appended: el.appended });
        },
        remove() {
          el.removed = true;
        },
      };
      return el;
    },
  };
  return { doc, clicks };
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
  const failed = await loadCatalog({ fetchImpl: async () => ({ ok: false, status: 404 }) });
  assert.equal(failed.ok, false);
  assert.match(failed.error, /404/);

  const ok = await loadCatalog({ fetchImpl: async () => ({ ok: true, status: 200, text: async () => catalogText }) });
  assert.equal(ok.ok, true);
  assert.equal(ok.entries.length, 2);
});

test('downloadTarget resolves the URL and keeps the vendor file name', () => {
  const target = downloadTarget(
    { label: 'FydeOS 23.1', file: 'FydeOS_FydeTab_Duo_v23.1-com.img.zip', url: './a.img' },
    'https://flash.test/app/',
  );
  assert.equal(target.url, 'https://flash.test/app/a.img');
  assert.equal(target.name, 'FydeOS_FydeTab_Duo_v23.1-com.img.zip');

  assert.equal(downloadTarget({ label: 'x', url: null }), null, 'a catalogue row without a url has no target');
  assert.equal(downloadTarget(null), null);
});

test('triggerDownload clicks an <a download> and never reads the file', () => {
  const { doc, clicks } = fakeDocument();
  const target = triggerDownload(
    { label: 'openFyde r132', file: 'openfyde.img.zip', url: 'https://github.test/openfyde.img.zip' },
    { document: doc, base: 'https://flash.test/app/' },
  );
  assert.deepEqual(clicks, [
    { href: 'https://github.test/openfyde.img.zip', download: 'openfyde.img.zip', appended: true },
  ]);
  assert.equal(target.url, 'https://github.test/openfyde.img.zip');
});

test('triggerDownload does nothing when the catalogue row has no link', () => {
  const { doc, clicks } = fakeDocument();
  const target = triggerDownload({ label: 'x', url: null }, { document: doc });
  assert.equal(target, null);
  assert.deepEqual(clicks, []);
});

test('the picked file is recognised as the system that was downloaded', () => {
  const entries = parseCatalog(catalogText).entries;
  assert.equal(matchCatalogFile(entries[0], { name: 'good.img', size: 1 }), true,
    'a name match does not need the size to agree');
  // A browser that renamed the file still matches on the byte count.
  assert.equal(matchCatalogFile(entries[0], { name: 'update.img', size: 1024 }), true);
  assert.equal(matchCatalogFile(entries[0], { name: 'other.img', size: 999 }), false);
  assert.equal(matchCatalogFile(entries[0], null), false);

  const found = matchCatalogEntry(entries, { name: 'update.img', size: 2048 });
  assert.equal(found.id, 'no-url', 'size alone still names the official row');
  assert.equal(matchCatalogEntry(entries, { name: 'nothing.img', size: 7 }), null);

  const preferred = entries[1];
  assert.equal(matchCatalogEntry(entries, { name: 'good.img', size: 1 }, preferred).id, 'good');
});

test('the format helpers still speak the page language', () => {
  assert.equal(formatBytes(1024), '1.00 KiB');
  assert.equal(formatBytes(4 * 1024 * 1024 * 1024), '4.00 GiB');
  assert.equal(formatBytes(-1), '?');
  assert.equal(formatSpeed(0), '—');
  assert.equal(formatSpeed(2 * 1024 * 1024), '2.00 MiB/s');
  assert.equal(formatDuration(12), '12 秒');
  assert.equal(formatDuration(65), '1 分 05 秒');
  assert.equal(formatDuration(0.2), '<1 秒');
  assert.equal(formatDuration(-1), '—');
});
