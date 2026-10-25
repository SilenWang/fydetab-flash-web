// Static checks for the installable app: the manifest, the icons it names, the
// service worker's precache list, and the element ids app.js reaches for.
//
// These are the failures a browser only shows at the worst moment — on a bench
// without network, with a page that installed fine but cannot boot — so they
// are pinned here instead. Nothing in this file needs a browser.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, existsSync, statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, resolve } from 'node:path';

const APP_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const read = (name) => readFileSync(join(APP_DIR, name), 'utf8');

function readPngSize(path) {
  const bytes = readFileSync(path);
  assert.equal(bytes.subarray(0, 8).toString('hex'), '89504e470d0a1a0a', `${path} is not a PNG`);
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  return { width: view.getUint32(16), height: view.getUint32(20) };
}

function precacheList() {
  const source = read('sw.js');
  const match = source.match(/const PRECACHE = \[([\s\S]*?)\];/);
  assert.ok(match, 'sw.js must declare a PRECACHE array');
  return [...match[1].matchAll(/'([^']+)'/g)].map((m) => m[1]);
}

test('manifest is a valid installable web app manifest', () => {
  const manifest = JSON.parse(read('manifest.webmanifest'));
  assert.equal(manifest.name, 'RK3588 WebFlasher');
  assert.ok(manifest.short_name.length <= 12, 'short_name must fit under a launcher icon');
  assert.equal(manifest.start_url, './');
  assert.equal(manifest.scope, './');
  assert.equal(manifest.display, 'standalone');
  assert.match(manifest.theme_color, /^#[0-9a-f]{6}$/i);
  assert.ok(Array.isArray(manifest.icons) && manifest.icons.length >= 3);

  for (const icon of manifest.icons) {
    const path = join(APP_DIR, icon.src.replace(/^\.\//, ''));
    assert.ok(existsSync(path), `manifest icon ${icon.src} does not exist`);
    assert.equal(icon.type, 'image/png');
    const { width, height } = readPngSize(path);
    assert.equal(`${width}x${height}`, icon.sizes, `${icon.src} dimensions disagree with the manifest`);
  }

  const maskable = manifest.icons.filter((icon) => icon.purpose === 'maskable');
  assert.equal(maskable.length, 1, 'a maskable icon is required for Android launchers');
  const any = manifest.icons.filter((icon) => (icon.purpose ?? 'any') === 'any');
  assert.ok(any.some((icon) => icon.sizes === '192x192'), 'Chrome requires a 192px icon');
  assert.ok(any.some((icon) => icon.sizes === '512x512'), 'Chrome requires a 512px icon');
});

test('the page declares the manifest and the install surface', () => {
  const html = read('index.html');
  assert.match(html, /<link rel="manifest" href="\.\/manifest\.webmanifest"/);
  assert.match(html, /<link rel="icon"/);
  assert.match(html, /name="theme-color"/);
  assert.match(html, /id="file-image"[^>]*accept="[^"]*\.zip/, 'the image picker must accept .zip');

  for (const id of ['btn-install', 'btn-reload-app', 'install-note', 'sw-note', 'net-badge', 'zip-summary', 'reconnect', 'reconnect-steps', 'btn-reconnect-auth', 'btn-export-log']) {
    assert.ok(html.includes(`id="${id}"`), `index.html is missing #${id}`);
  }
});

test('every element id app.js looks up exists in index.html', () => {
  const app = read('app.js');
  const html = read('index.html');
  const htmlIds = new Set([...html.matchAll(/\bid="([^"]+)"/g)].map((m) => m[1]));

  const lookups = new Set();
  for (const match of app.matchAll(/\$\('([^']+)'\)/g)) lookups.add(match[1]);
  for (const match of app.matchAll(/getElementById\('([^']+)'\)/g)) lookups.add(match[1]);

  assert.ok(lookups.size > 20, 'app.js should look up the page elements');
  const missing = [...lookups].filter((id) => !htmlIds.has(id));
  assert.deepEqual(missing, [], `index.html has no element with these ids: ${missing.join(', ')}`);
});

test('the service worker precaches everything the app imports', () => {
  const precached = new Set(precacheList().map((entry) => entry.replace(/^\.\//, '')));
  assert.ok(precached.size >= 15, 'the precache list looks truncated');

  for (const entry of precached) {
    if (entry === '' || entry === '') continue;
    const path = entry === '' ? join(APP_DIR, 'index.html') : join(APP_DIR, entry);
    assert.ok(existsSync(path), `precached ${entry} does not exist (wasm must be built: pixi run wasm)`);
    assert.ok(statSync(path).isFile(), `precached ${entry} is not a file`);
  }

  // Every local module the page or the worker imports must be in the list,
  // otherwise the offline boot fails at the first import.
  const imports = new Set();
  for (const file of ['app.js', 'worker.js', 'zip-source.js', 'bridge.js', 'preflight.js', 'usb-host.js']) {
    for (const match of read(file).matchAll(/from '\.\/([\w./-]+\.js)'/g)) imports.add(match[1]);
  }
  imports.add('worker.js');
  imports.add('app.js');

  const missing = [...imports].filter((name) => !precached.has(name));
  assert.deepEqual(missing, [], `not precached: ${missing.join(', ')}`);
});

test('app.js registers the service worker and exports NDJSON logs', () => {
  const app = read('app.js');
  assert.match(app, /navigator\.serviceWorker\.register\('\.\/sw\.js'/);
  assert.match(app, /application\/x-ndjson/);
  assert.match(app, /beforeinstallprompt/);
  assert.match(app, /DecompressionStream|zip/);

  const worker = read('worker.js');
  assert.match(worker, /from '\.\/zip-source\.js'/, 'the worker must read zips through zip-source.js');
  assert.match(worker, /msg\.zip/, 'the worker must accept a zip payload');
});

test('app.js declares each top-level name once', () => {
  // `node --check` parses these files as CommonJS, where a duplicate function
  // declaration is legal — in a module it is a SyntaxError that only shows up
  // as a blank page in the browser. A stale copy of a function is exactly how
  // that happened once.
  const source = read('app.js');
  const names = new Map();
  for (const match of source.matchAll(/^(?:async\s+)?(?:function|const|let|class)\s+([A-Za-z_$][\w$]*)/gm)) {
    const name = match[1];
    names.set(name, (names.get(name) ?? 0) + 1);
  }
  const duplicated = [...names].filter(([, count]) => count > 1).map(([name]) => name);
  assert.deepEqual(duplicated, [], `app.js declares these names more than once: ${duplicated.join(', ')}`);
});

test('styles keep [hidden] authoritative', () => {
  const css = read('styles.css');
  assert.match(css, /\[hidden\]\s*\{[^}]*display:\s*none\s*!important/);
});
