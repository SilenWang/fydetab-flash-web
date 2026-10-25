// The real worker script, driven exactly like the page drives it.
//
// worker.js is loaded with the two browser globals it needs (self.postMessage
// and fetch) shimmed onto Node; everything else — the wasm core, the bridge,
// the mock device, the message protocol — is the production code. This is the
// closest thing to "open the page and click db" that runs without a browser:
// the DOM layer is the only part not exercised.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

import { buildMockLoader } from '../mock-loader.js';
import { createMockHost } from '../mock-host.js';

const APP_DIR = new URL('../', import.meta.url);
const messages = [];

// stand-in for the page's main thread; the RPC test below installs one
let mainThreadHost = null;

globalThis.self = {
  onmessage: null,
  postMessage(message) {
    messages.push(message);
    if (message.type !== 'host' || !mainThreadHost) return;
    // Answer exactly like app.js handleHostCall does.
    const fn = mainThreadHost[message.method];
    Promise.resolve()
      .then(() => fn(...message.args))
      .then(
        (value) => globalThis.self.onmessage({ data: { type: 'host-reply', id: message.id, ok: true, value } }),
        (err) => globalThis.self.onmessage({ data: { type: 'host-reply', id: message.id, ok: false, error: err.message } }),
      );
  },
};

// worker.js fetches './wasm/rk3588.wasm' relative to itself.
globalThis.fetch = async (input) => {
  const url = new URL(input, APP_DIR);
  if (url.protocol !== 'file:') throw new Error(`unexpected fetch ${url}`);
  const bytes = await readFile(fileURLToPath(url));
  return {
    ok: true,
    status: 200,
    statusText: 'OK',
    async arrayBuffer() {
      return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength);
    },
  };
};

const loaded = import('../worker.js');

async function waitFor(predicate, timeoutMs = 8000, what = 'message') {
  const started = Date.now();
  for (;;) {
    const found = messages.find(predicate);
    if (found) return found;
    if (Date.now() - started > timeoutMs) throw new Error(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

function toArrayBuffer(bytes) {
  return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength);
}

test('the shared worker path boots, switches to mock mode and runs the session', async (t) => {
  await loaded;

  await t.test('the worker reports its core version', async () => {
    const ready = await waitFor((m) => m.type === 'ready', 10000, 'worker ready');
    assert.equal(ready.mode, 'usb');
    assert.match(ready.version, /^0\.2\.0/);
  });

  let nextId = 0;
  const exec = async (line, loader, loaderName) => {
    const id = ++nextId;
    await globalThis.self.onmessage({
      data: { type: 'exec', id, line, loader: loader ? toArrayBuffer(loader) : undefined, loaderName },
    });
    const result = messages.find((m) => m.type === 'result' && m.id === id);
    if (!result) {
      const error = messages.find((m) => m.type === 'error' && m.id === id);
      throw new Error(error ? error.error : `no result for ${line}`);
    }
    return result.result;
  };

  await t.test('mode switch installs the in-worker mock device', async () => {
    messages.length = 0;
    globalThis.self.onmessage({ data: { type: 'mode', mode: 'mock' } });
    const ack = await waitFor((m) => m.type === 'mode', 5000, 'mode ack');
    assert.equal(ack.mode, 'mock');
  });

  await t.test('ld lists the mock maskrom board', async () => {
    const res = await exec('ld');
    assert.equal(res.ok, true);
    assert.deepEqual(res.lines, ['DevNo=1 Vid=0x2207,Pid=0x350b,Mode=Maskrom']);
  });

  await t.test('inspect validates the demo loader through the core', async () => {
    const id = ++nextId;
    messages.length = 0;
    globalThis.self.onmessage({
      data: { type: 'inspect', id, bytes: toArrayBuffer(buildMockLoader()) },
    });
    const payload = await waitFor((m) => m.type === 'inspect' && m.id === id, 5000, 'inspect reply');
    assert.equal(payload.payload.ok, true);
    assert.equal(payload.payload.segments.length, 3);
  });

  await t.test('info is refused in maskrom', async () => {
    const res = await exec('info');
    assert.equal(res.ok, false);
    assert.match(res.error.hint, /db/);
  });

  await t.test('db downloads the loader, then ld/info see the loader device', async () => {
    const res = await exec('db', buildMockLoader(), 'mock-loader.bin');
    assert.equal(res.ok, true, JSON.stringify(res.error));
    assert.equal(res.state.awaitingReenumeration, true);

    const after = await exec('ld');
    assert.equal(after.lines[0], 'DevNo=1 Vid=0x2207,Pid=0x350b,Mode=Loader');
    assert.equal(after.state.mode, 'LOADER');

    const info = await exec('info');
    assert.equal(info.ok, true, JSON.stringify(info.error));
    assert.equal(info.info.chip.text, '3588');
    assert.equal(info.info.storage.currentName, 'eMMC');
  });

  await t.test('cd/sd round-trip through the worker', async () => {
    assert.equal((await exec('cd 1')).ok, true);
    assert.equal((await exec('sd 1')).ok, true);
    const bad = await exec('cd 5');
    assert.equal(bad.ok, false);
  });

  await t.test('the same commands work over the worker↔main RPC path', async () => {
    // Switch back to "usb" mode, where the worker talks to a real main-thread
    // host over postMessage. The host here is the mock device standing in for
    // WebUSB, so this exercises bridge → RPC → host and back, including the
    // ArrayBuffer reply of bulk IN.
    mainThreadHost = createMockHost();
    messages.length = 0;
    globalThis.self.onmessage({ data: { type: 'mode', mode: 'usb' } });
    await waitFor((m) => m.type === 'mode' && m.mode === 'usb', 5000, 'usb mode ack');

    const ld = await exec('ld');
    assert.equal(ld.ok, true);
    assert.deepEqual(ld.lines, ['DevNo=1 Vid=0x2207,Pid=0x350b,Mode=Maskrom']);

    const db = await exec('db', buildMockLoader(), 'mock-loader.bin');
    assert.equal(db.ok, true, JSON.stringify(db.error));
    assert.equal(db.db.packets, 36);

    // The shell re-lists by itself after a Download Boot, so info works
    // without an explicit ld.
    const info = await exec('info');
    assert.equal(info.ok, true, JSON.stringify(info.error));
    assert.match(info.lines.join('\n'), /Re-enumerated as LOADER/);
    assert.equal(info.info.chip.text, '3588');
    assert.equal(info.info.storage.currentName, 'eMMC');

    const state = mainThreadHost.inspect();
    assert.equal(state.mode, 'loader');
    assert.equal(state.controls, 36);
  });

  await t.test('close releases the session without error', async () => {
    const id = ++nextId;
    messages.length = 0;
    globalThis.self.onmessage({ data: { type: 'close', id } });
    const reply = await waitFor((m) => m.type === 'closed' && m.id === id, 5000, 'closed reply');
    assert.equal(reply.id, id);
  });
});

// A plan reply used to be posted as 'uf-result', the message the page renders
// through the write panel — so every successful plan showed up as a fake
// "write failed". Plans now have their own message type.
test('a uf-plan reply is not posted as a write result', async () => {
  await loaded;
  // The previous test booted the worker; prove it is still answering rather
  // than waiting for a 'ready' that has already been consumed.
  messages.length = 0;
  globalThis.self.onmessage({ data: { type: 'mode', mode: 'mock' } });
  await waitFor((m) => m.type === 'mode', 5000, 'mode ack');

  const id = 424242;
  messages.length = 0;
  await globalThis.self.onmessage({ data: { type: 'uf-plan', id, demo: true, opts: {} } });
  const reply = await waitFor(
    (m) => (m.type === 'uf-plan-result' || m.type === 'uf-result') && m.id === id,
    30000,
    'plan reply',
  );
  assert.equal(reply.type, 'uf-plan-result', 'the plan must use its own message type');
  assert.equal(reply.result.ok, true, JSON.stringify(reply.result.error));
  assert.equal(reply.result.kind, undefined);
  assert.ok(reply.result.report, 'the reply still carries the plan report');
});
