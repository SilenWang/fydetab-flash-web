// Module worker: hosts the Go/WASM core and the USB bridge.
//
// Division of labour (and the reason this file exists at all):
//   - WebUSB lives on the page's main thread: requestDevice() needs a user
//     gesture and navigator.usb is Window-only.
//   - The Go core runs here, in the worker, so a long Download Boot never
//     blocks the UI.
//   - Every core→USB call crosses back as one RPC message; the reply resolves
//     a Promise that the Go side is parked on.
//
// Demo mode swaps the RPC host for an in-worker mock device (mock-host.js), so
// `ld → db → ld → info` can be shown with no hardware and no USB permission.

import './wasm/wasm_exec.js';
import { createBridge } from './bridge.js';
import { createMockHost } from './mock-host.js';
import { openZipImage, summarizePackage } from './zip-source.js';

const RPC_TIMEOUT_MS = 15000;

let host = null;
let hostMode = 'usb';
let mock = null;
let rpcSeq = 0;
let demoImageCache = null;
const pendingRpc = new Map();

function post(message, transfer) {
  self.postMessage(message, transfer ?? []);
}

function log(message) {
  post({ type: 'log', message });
}

// --- RPC host ---------------------------------------------------------------

function callHost(method, args, transfer) {
  const id = ++rpcSeq;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pendingRpc.delete(id);
      reject(new Error(`${method} timed out after ${RPC_TIMEOUT_MS} ms (main thread did not answer)`));
    }, RPC_TIMEOUT_MS);
    pendingRpc.set(id, { resolve, reject, timer });
    post({ type: 'host', id, method, args }, transfer);
  });
}

function rpcHost() {
  return {
    // The main thread returns the RawInfo array itself; bridge.js wraps it in
    // the {devices:[...]} JSON envelope the Go core decodes.
    list() {
      return callHost('list', []);
    },
    open(devNo, handle) {
      return callHost('open', [devNo, handle]);
    },
    close(devNo) {
      return callHost('close', [devNo]);
    },
    async controlOut(devNo, requestType, request, value, index, data) {
      return callHost('controlOut', [devNo, requestType, request, value, index, data instanceof Uint8Array ? data : new Uint8Array(data ?? 0)]);
    },
    async bulkOut(devNo, endpoint, data) {
      return callHost('bulkOut', [devNo, endpoint, data instanceof Uint8Array ? data : new Uint8Array(data ?? 0)]);
    },
    async bulkIn(devNo, endpoint, length) {
      const reply = await callHost('bulkIn', [devNo, endpoint, length]);
      return { data: new Uint8Array(reply.data) };
    },
    reset(devNo) {
      return callHost('reset', [devNo]);
    },
  };
}

function resolveHostReply(msg) {
  const entry = pendingRpc.get(msg.id);
  if (!entry) return;
  pendingRpc.delete(msg.id);
  clearTimeout(entry.timer);
  if (msg.ok) {
    entry.resolve(msg.value);
    return;
  }
  entry.reject(new Error(msg.error || 'host call failed'));
}

// --- image sources ----------------------------------------------------------
//
// The page hands over a File (structured-cloneable); Go gets an object with a
// synchronous-looking read() that returns a Promise. rkfw reads a multi-GiB
// image lazily through it, so nothing is copied into wasm memory up front.

function fileSource(file) {
  return {
    name: file.name ?? 'update.img',
    size: file.size ?? 0,
    read: async (offset, length) => {
      const sliced = file.slice(offset, offset + length);
      return new Uint8Array(await sliced.arrayBuffer());
    },
  };
}

function bytesSource(name, bytes) {
  return {
    name,
    size: bytes.byteLength,
    read: async (offset, length) => bytes.subarray(offset, offset + length),
  };
}

function fromBase64(text) {
  const binary = atob(text);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) out[i] = binary.charCodeAt(i);
  return out;
}

// --- zip packages -----------------------------------------------------------
//
// A vendor package is a zip around one multi-GiB RKFW image. zip-source.js
// turns the entry into the same {name, size, read()} object a plain File
// provides, so the Go planner reads it byte for byte identically — it just
// decompresses on demand instead of slicing a file.
//
// The opened source is cached for the current package: one plan call reads the
// header, the RKAF table, the parameter item and the loader, and a write started
// from the same package must see the same entry without re-parsing the archive.

let zipCache = null; // { key, promise }

function zipKey(file) {
  return `${file.name}:${file.size}:${file.lastModified ?? 0}`;
}

function zipSource(file) {
  const key = zipKey(file);
  if (zipCache && zipCache.key === key) return zipCache.promise;
  const promise = openZipImage(file, file.name).then((opened) => {
    post({
      type: 'zip',
      zip: file.name,
      entry: opened.entry,
      entries: opened.directory.entries.length,
      zip64: opened.directory.zip64,
      summary: summarizePackage(opened),
      skipped: opened.skipped,
      stats: { ...opened.source.stats },
    });
    return opened.source;
  });
  zipCache = { key, promise };
  promise.catch(() => {
    if (zipCache && zipCache.key === key) zipCache = null;
  });
  return promise;
}

function reportZipStats(id, source) {
  if (!source || !source.stats) return;
  post({ type: 'zip-stats', id, stats: { ...source.stats } });
}

// resolveImage picks the image the page asked for: a plain file, the RKFW entry
// inside a zip package, or the cached synthetic demo image (built in Go, so the
// demo parses like a real one).
async function resolveImage(msg) {
  if (msg.file) return fileSource(msg.file);
  if (msg.zip) return zipSource(msg.zip);
  if (msg.demo) {
    if (!demoImageCache) {
      const payload = JSON.parse(await globalThis.rk3588.demoImage());
      demoImageCache = { name: payload.name, bytes: fromBase64(payload.bytes) };
    }
    return bytesSource(demoImageCache.name, demoImageCache.bytes);
  }
  throw new Error('no image supplied (need file, zip or demo)');
}

async function runUf(msg, call) {
  const source = await resolveImage(msg);
  const onEvent = (kind, payload) => {
    let parsed = payload;
    try {
      parsed = JSON.parse(payload);
    } catch {
      // keep the raw string
    }
    post({ type: 'uf-event', id: msg.id, kind, payload: parsed });
  };
  try {
    const text = await call(source, msg.opts ?? {}, onEvent);
    post({ type: 'uf-result', id: msg.id, result: JSON.parse(text) });
  } finally {
    reportZipStats(msg.id, source);
  }
}

// --- boot -------------------------------------------------------------------

function waitForAPI(timeoutMs = 5000) {
  const started = performance.now();
  return new Promise((resolve, reject) => {
    const tick = () => {
      if (globalThis.rk3588) {
        resolve(globalThis.rk3588);
        return;
      }
      if (performance.now() - started > timeoutMs) {
        reject(new Error('Go core did not expose globalThis.rk3588'));
        return;
      }
      setTimeout(tick, 10);
    };
    tick();
  });
}

function installHost(mode) {
  hostMode = mode;
  if (mode === 'mock') {
    mock = createMockHost({ onLog: log });
    host = mock;
  } else {
    mock = null;
    host = rpcHost();
  }
  globalThis.rk3588.bindBridge(createBridge(host), { timeoutMs: 10000, readRetries: 1 });
}

async function boot() {
  try {
    const go = new Go();
    const response = await fetch('./wasm/rk3588.wasm');
    if (!response.ok) throw new Error(`fetch wasm: ${response.status} ${response.statusText}`);
    const bytes = await response.arrayBuffer();
    const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
    go.run(instance); // resolves only if the Go program exits; it never does
    const api = await waitForAPI();
    installHost(hostMode);
    post({ type: 'ready', version: api.version, mode: hostMode });
  } catch (err) {
    post({ type: 'fatal', error: err.message });
  }
}

// --- message handling -------------------------------------------------------

self.onmessage = async (event) => {
  const msg = event.data;
  try {
    switch (msg.type) {
      case 'host-reply':
        resolveHostReply(msg);
        return;
      case 'mode': {
        installHost(msg.mode === 'mock' ? 'mock' : 'usb');
        post({ type: 'mode', mode: hostMode });
        return;
      }
      case 'exec': {
        if (!globalThis.rk3588) throw new Error('core is not ready');
        const loader = msg.loader ? new Uint8Array(msg.loader) : undefined;
        const text = await globalThis.rk3588.exec(msg.line, loader, msg.loaderName ?? '');
        if (mock && msg.inspect) {
          post({ type: 'mock-state', id: msg.id, state: mock.inspect() });
        }
        post({ type: 'result', id: msg.id, result: JSON.parse(text) });
        return;
      }
      case 'inspect': {
        if (!globalThis.rk3588) throw new Error('core is not ready');
        const text = await globalThis.rk3588.inspectLoader(new Uint8Array(msg.bytes));
        post({ type: 'inspect', id: msg.id, payload: JSON.parse(text) });
        return;
      }
      case 'uf-plan': {
        if (!globalThis.rk3588) throw new Error('core is not ready');
        await runUf(msg, (source, opts, onEvent) => globalThis.rk3588.ufPlan(source, opts, onEvent));
        return;
      }
      case 'uf-run': {
        if (!globalThis.rk3588) throw new Error('core is not ready');
        await runUf(msg, (source, opts, onEvent) => globalThis.rk3588.ufRun(source, opts, onEvent));
        return;
      }
      case 'uf-cancel': {
        if (!globalThis.rk3588) throw new Error('core is not ready');
        const text = await globalThis.rk3588.ufCancel();
        post({ type: 'uf-cancel', id: msg.id, result: JSON.parse(text) });
        return;
      }
      case 'image-loader': {
        if (!globalThis.rk3588) throw new Error('core is not ready');
        const source = await resolveImage(msg);
        try {
          const text = await globalThis.rk3588.imageLoader(source);
          const payload = JSON.parse(text);
          const bytes = fromBase64(payload.bytes ?? '');
          delete payload.bytes;
          post({ type: 'image-loader', id: msg.id, payload, bytes }, [bytes.buffer]);
        } finally {
          reportZipStats(msg.id, source);
        }
        return;
      }
      case 'demo-image': {
        if (!globalThis.rk3588) throw new Error('core is not ready');
        const source = await resolveImage({ demo: true });
        post({ type: 'demo-image', id: msg.id, payload: { name: source.name, size: source.size } });
        return;
      }
      case 'close': {
        if (globalThis.rk3588) await globalThis.rk3588.close();
        post({ type: 'closed', id: msg.id });
        return;
      }
      default:
        log(`worker: ignoring unknown message type ${msg.type}`);
    }
  } catch (err) {
    post({ type: 'error', id: msg.id, error: err?.message ?? String(err) });
  }
};

boot();
