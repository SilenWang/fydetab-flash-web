// Service worker: the page and its wasm core are precached so an installed app
// opens with no network at all.
//
// Scope is ./app/ — the probe page under /probe/ is a separate, read-only tool
// and is deliberately not cached here (it is not installable and its whole
// point is to run against the files currently on disk).
//
// The cache is cache-first, not network-first: a flasher is opened when the
// user is about to write a device, and waiting on a network round trip (or
// failing because the machine is on a bench without Wi-Fi) is worse than
// starting one version behind. A new deploy is picked up on the next launch:
// install precaches the new version under a new cache name, activate drops the
// old one, and app.js offers a reload when it notices a waiting worker.
//
// Nothing here handles device I/O. WebUSB and the image file stay entirely in
// the page and the worker — the service worker only answers GETs for its own
// origin.

const VERSION = 'rk3588-webflasher-m4-1';
const CACHE = `rk3588-webflasher-${VERSION}`;

// Everything the app cannot run without. Kept in one place so the pwa test can
// check it against the files on disk: a missing entry here is an offline
// failure that only shows up on a plane.
const PRECACHE = [
  './',
  './index.html',
  './styles.css',
  './app.js',
  './worker.js',
  './bridge.js',
  './usb-host.js',
  './mock-host.js',
  './mock-loader.js',
  './preflight.js',
  './protocol.js',
  './zip.js',
  './zip-source.js',
  './manifest.webmanifest',
  './icons/icon-192.png',
  './icons/icon-512.png',
  './icons/icon-maskable-512.png',
  './wasm/rk3588.wasm',
  './wasm/wasm_exec.js',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    (async () => {
      const cache = await caches.open(CACHE);
      try {
        await cache.addAll(PRECACHE);
      } catch (err) {
        // A failed precache must not leave a half-filled cache behind: the next
        // load would then serve a page whose wasm is missing.
        await caches.delete(CACHE);
        throw err;
      }
      await self.skipWaiting();
    })(),
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    (async () => {
      const names = await caches.keys();
      await Promise.all(names.filter((name) => name !== CACHE).map((name) => caches.delete(name)));
      await self.clients.claim();
    })(),
  );
});

self.addEventListener('message', (event) => {
  if (event.data?.type === 'skipWaiting') self.skipWaiting();
});

self.addEventListener('fetch', (event) => {
  const request = event.request;
  if (request.method !== 'GET') return;

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;
  // The service worker script itself is fetched by the browser, never by us.
  if (url.pathname.endsWith('/sw.js')) return;

  event.respondWith(serve(request));
});

async function serve(request) {
  const cache = await caches.open(CACHE);
  const cached = await cache.match(request);
  if (cached) return cached;

  try {
    const response = await fetch(request);
    // Only store complete, basic responses: opaque or partial ones would poison
    // the cache with something unusable offline.
    if (response.ok && response.type === 'basic') {
      cache.put(request, response.clone()).catch(() => {});
    }
    return response;
  } catch (err) {
    // Offline and not precached: a navigation still gets the app shell, so the
    // installed window never shows the browser's offline page.
    if (request.mode === 'navigate') {
      const shell = await cache.match('./index.html');
      if (shell) return shell;
    }
    throw err;
  }
}
