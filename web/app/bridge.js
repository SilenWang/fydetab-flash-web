// Worker-side bridge between the Go/WASM core and the page.
//
// The Go core calls these functions synchronously and awaits the returned
// Promises; every call is one USB-ish operation on the host object. Structured
// device listings cross the boundary as a JSON string because that is what the
// Go side decodes; byte payloads stay Uint8Array.
//
// A host implements:
//   list()                                  -> Promise<RawInfo[]>
//   open(devNo, handle)                     -> Promise<void>
//   close(devNo)                            -> Promise<void>
//   controlOut(devNo, type, req, val, idx, data) -> Promise<{bytesWritten}>
//   bulkOut(devNo, endpoint, data)          -> Promise<{bytesWritten}>
//   bulkIn(devNo, endpoint, length)         -> Promise<{data: Uint8Array}>
//   reset(devNo)                            -> Promise<void>

export class BridgeTimeoutError extends Error {
  constructor(what, ms) {
    super(`${what} timed out after ${ms} ms`);
    this.name = 'BridgeTimeoutError';
  }
}

export function withTimeout(promise, ms, what) {
  if (!ms || ms <= 0) return promise;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new BridgeTimeoutError(what, ms)), ms);
    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (err) => {
        clearTimeout(timer);
        reject(err);
      },
    );
  });
}

function toU8(value) {
  if (value instanceof Uint8Array) return value;
  if (value instanceof ArrayBuffer) return new Uint8Array(value);
  if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
  return new Uint8Array(0);
}

function number(value, what) {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    throw new Error(`bridge: ${what} must be a number, got ${typeof value}`);
  }
  return value;
}

/**
 * createBridge wraps a host in the exact surface the Go core expects.
 * timeoutMs guards against a stuck WebUSB transfer holding the worker forever;
 * the Go side has its own per-transfer deadline, whichever fires first wins.
 */
export function createBridge(host, { timeoutMs = 8000 } = {}) {
  const call = (what, fn) => withTimeout(Promise.resolve().then(fn), timeoutMs, what);

  return {
    async list() {
      const devices = await call('list', () => host.list());
      return JSON.stringify({ devices });
    },
    open(devNo, handle) {
      return call('open', () => host.open(number(devNo, 'devNo'), String(handle ?? '')));
    },
    close(devNo) {
      return call('close', () => host.close(number(devNo, 'devNo')));
    },
    controlOut(devNo, requestType, request, value, index, data) {
      return call('controlOut', () =>
        host.controlOut(
          number(devNo, 'devNo'),
          number(requestType, 'requestType'),
          number(request, 'request'),
          number(value, 'value'),
          number(index, 'index'),
          toU8(data),
        ),
      );
    },
    bulkOut(devNo, endpoint, data) {
      return call('bulkOut', () => host.bulkOut(number(devNo, 'devNo'), number(endpoint, 'endpoint'), toU8(data)));
    },
    bulkIn(devNo, endpoint, length) {
      return call('bulkIn', () => host.bulkIn(number(devNo, 'devNo'), number(endpoint, 'endpoint'), number(length, 'length')));
    },
    reset(devNo) {
      return call('reset', () => host.reset(number(devNo, 'devNo')));
    },
  };
}
