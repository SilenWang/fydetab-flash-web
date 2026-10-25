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

const RPC_TIMEOUT_MS = 15000;

let host = null;
let hostMode = 'usb';
let mock = null;
let rpcSeq = 0;
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
