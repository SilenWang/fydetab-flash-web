// The service worker, exercised in a fake worker environment.
//
// The offline guarantee is the whole point of M4, and it is the one thing that
// cannot be checked by reading the file: install has to fill the cache from
// disk, a fetch has to be answered without the network, and a half-filled
// precache has to be thrown away rather than served. The fakes below implement
// only what sw.js uses (Cache Storage promises, a fetch that can be offline,
// response.ok/type/clone), which keeps the test honest about the logic while
// needing no browser.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, resolve } from 'node:path';
import vm from 'node:vm';

const APP_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const SW_SOURCE = readFileSync(join(APP_DIR, 'sw.js'), 'utf8');

class FakeResponse {
  constructor(url, { status = 200, type = 'basic' } = {}) {
    this.url = url;
    this.status = status;
    this.ok = status >= 200 && status < 300;
    this.type = type;
    this.body = `body:${url}`;
  }

  clone() {
    return new FakeResponse(this.url, { status: this.status, type: this.type });
  }

  async text() {
    return this.body;
  }
}

function makeEnv({ offline = false, failUrls = new Set(), statuses = new Map() } = {}) {
  const stores = new Map(); // cache name → Map(url → FakeResponse)
  const fetched = [];
  const calls = { skipWaiting: 0, claim: 0 };
  const handlers = new Map();
  const scope = { origin: 'https://example.test', pathname: '/rk3588-webflasher/app/sw.js' };
  const base = 'https://example.test/rk3588-webflasher/app/';

  const absolute = (request) => {
    const url = typeof request === 'string' ? request : request.url;
    return new URL(url, base).toString();
  };

  function cacheStore(name) {
    if (!stores.has(name)) stores.set(name, new Map());
    return stores.get(name);
  }

  const caches = {
    async open(name) {
      const store = cacheStore(name);
      return {
        async addAll(urls) {
          for (const url of urls) {
            const response = await fetchImpl(url);
            if (!response.ok) throw new Error(`addAll: ${url} answered ${response.status}`);
            store.set(absolute(url), response);
          }
        },
        async match(request) {
          return store.get(absolute(request)) ?? undefined;
        },
        async put(request, response) {
          store.set(absolute(request), response);
        },
      };
    },
    async keys() {
      return [...stores.keys()];
    },
    async delete(name) {
      return stores.delete(name);
    },
  };

  async function fetchImpl(request) {
    const url = absolute(request);
    fetched.push(url);
    if (offline) throw new Error('net::ERR_INTERNET_DISCONNECTED');
    if (failUrls.has(url)) return new FakeResponse(url, { status: 404 });
    return new FakeResponse(url, { status: statuses.get(url) ?? 200 });
  }

  const self = {
    location: { origin: scope.origin, href: `${scope.origin}${scope.pathname}` },
    addEventListener(type, fn) {
      if (!handlers.has(type)) handlers.set(type, []);
      handlers.get(type).push(fn);
    },
    async skipWaiting() {
      calls.skipWaiting += 1;
    },
    clients: {
      async claim() {
        calls.claim += 1;
      },
    },
  };

  const sandbox = { self, caches, fetch: fetchImpl, URL, console, Response: FakeResponse };
  vm.runInNewContext(SW_SOURCE, sandbox, { filename: 'sw.js' });

  function dispatch(type, extra = {}) {
    const waited = [];
    const event = {
      waitUntil(promise) {
        waited.push(promise);
      },
      respondWith(promise) {
        waited.push(promise);
        event.response = promise;
      },
      ...extra,
    };
    for (const fn of handlers.get(type) ?? []) fn(event);
    return { event, done: Promise.all(waited) };
  }

  return { stores, fetched, calls, dispatch, cacheStore, absolute };
}

test('install precaches every file the app boots from', async () => {
  const env = makeEnv();
  const { done } = env.dispatch('install');
  await done;

  assert.equal(env.calls.skipWaiting, 1, 'install must take over without a second reload');
  const [cacheName] = env.stores.keys();
  assert.match(cacheName, /^rk3588-webflasher-/);
  const entries = env.cacheStore(cacheName);
  assert.ok(entries.size >= 15, `precached only ${entries.size} files`);
  for (const path of ['./index.html', './app.js', './worker.js', './wasm/rk3588.wasm', './zip-source.js']) {
    assert.ok(entries.has(env.absolute(path)), `${path} was not precached`);
  }
});

test('a failed precache does not leave a half-filled cache behind', async () => {
  const env = makeEnv();
  const broken = 'https://example.test/rk3588-webflasher/app/wasm/rk3588.wasm';
  const withFailure = makeEnv({ failUrls: new Set([broken]) });
  await assert.rejects(withFailure.dispatch('install').done);
  assert.equal(withFailure.stores.size, 0, 'the incomplete cache must be deleted');
  // The same archive fetched cleanly still installs, so the check above is
  // about the failure path and not about addAll being broken.
  await env.dispatch('install').done;
  assert.equal(env.stores.size, 1);
});

test('activate drops caches from other versions and claims the clients', async () => {
  const env = makeEnv();
  env.stores.set('rk3588-webflasher-m3-old', new Map());
  await env.dispatch('install').done;
  const { done } = env.dispatch('activate');
  await done;

  assert.equal(env.calls.claim, 1);
  assert.deepEqual([...env.stores.keys()].filter((name) => name.includes('m3-old')), []);
  assert.equal(env.stores.size, 1, 'exactly one cache version survives');
});

/** installs with the network up, then hands the same cache to an offline env. */
async function installThenGoOffline() {
  const warm = makeEnv();
  await warm.dispatch('install').done;
  const offline = makeEnv({ offline: true });
  for (const [name, entries] of warm.stores) offline.stores.set(name, new Map(entries));
  return offline;
}

test('a precached URL is served offline, with no network at all', async () => {
  const offline = await installThenGoOffline();
  const fetchedBefore = offline.fetched.length;

  const request = { method: 'GET', url: 'https://example.test/rk3588-webflasher/app/app.js', mode: 'no-cors' };
  const { event, done } = offline.dispatch('fetch', { request });
  const response = await event.response;
  await done;

  assert.equal(response.body, `body:${request.url}`);
  assert.equal(offline.fetched.length, fetchedBefore, 'a cached asset must not touch the network');
});

test('an offline navigation falls back to the cached shell', async () => {
  const offline = await installThenGoOffline();

  const request = { method: 'GET', url: 'https://example.test/rk3588-webflasher/app/some/deep/link', mode: 'navigate' };
  const { event, done } = offline.dispatch('fetch', { request });
  const response = await event.response;
  await done;

  assert.match(response.body, /^body:https:\/\/example\.test\/rk3588-webflasher\/app\/index\.html$/);
});

test('offline and uncached: a network request fails instead of lying', async () => {
  const offline = await installThenGoOffline();
  const request = { method: 'GET', url: 'https://example.test/rk3588-webflasher/app/late.js', mode: 'no-cors' };
  const { event, done } = offline.dispatch('fetch', { request });
  // respondWith and waitUntil hold the same promise: settle both, or the
  // second one shows up later as an unhandled rejection.
  const [responded, waited] = await Promise.allSettled([event.response, done]);
  assert.equal(responded.status, 'rejected');
  assert.match(responded.reason.message, /ERR_INTERNET_DISCONNECTED/);
  assert.equal(waited.status, 'rejected');
});

test('only same-origin GETs are handled; everything else goes to the network', () => {
  const env = makeEnv();
  for (const request of [
    { method: 'POST', url: 'https://example.test/rk3588-webflasher/app/app.js', mode: 'no-cors' },
    { method: 'GET', url: 'https://cdn.example.net/library.js', mode: 'no-cors' },
    { method: 'GET', url: 'https://example.test/rk3588-webflasher/app/sw.js', mode: 'no-cors' },
  ]) {
    const { event } = env.dispatch('fetch', { request });
    assert.equal(event.response, undefined, `${request.method} ${request.url} must not be intercepted`);
  }
});

test('a fetched-but-uncached asset is stored for the next launch', async () => {
  const env = makeEnv();
  await env.dispatch('install').done;
  const request = { method: 'GET', url: 'https://example.test/rk3588-webflasher/app/extra.css', mode: 'no-cors' };
  const { event, done } = env.dispatch('fetch', { request });
  const response = await event.response;
  await done;
  assert.equal(response.status, 200);

  // The put() is fire-and-forget inside sw.js; give it a turn of the loop.
  await new Promise((resolve) => setTimeout(resolve, 0));
  const [cacheName] = env.stores.keys();
  assert.ok(env.cacheStore(cacheName).has(request.url), 'the response should be cached for offline use');
});

test('an error response is not cached', async () => {
  const url = 'https://example.test/rk3588-webflasher/app/missing.js';
  const env = makeEnv({ statuses: new Map([[url, 500]]) });
  await env.dispatch('install').done;
  const request = { method: 'GET', url, mode: 'no-cors' };
  const { event, done } = env.dispatch('fetch', { request });
  await event.response;
  await done;
  await new Promise((resolve) => setTimeout(resolve, 0));
  const [cacheName] = env.stores.keys();
  assert.equal(env.cacheStore(cacheName).has(url), false);
});
