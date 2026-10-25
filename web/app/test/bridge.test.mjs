import test from 'node:test';
import assert from 'node:assert/strict';

import { createBridge, withTimeout } from '../bridge.js';

function fakeHost() {
  const calls = [];
  return {
    calls,
    async list() {
      calls.push(['list']);
      return [
        {
          vendorId: 0x2207,
          productId: 0x350b,
          usbVersionMajor: 2,
          usbVersionMinor: 0,
          usbVersionSubminor: 0,
          productName: 'fake',
          serialNumber: '',
          interfaceNumber: 0,
          interfaceClass: 0xff,
          bulkIn: 1,
          bulkOut: 2,
          handle: 'fake#0',
        },
      ];
    },
    async open(devNo, handle) {
      calls.push(['open', devNo, handle]);
    },
    async close(devNo) {
      calls.push(['close', devNo]);
    },
    async controlOut(devNo, requestType, request, value, index, data) {
      calls.push(['controlOut', devNo, requestType, request, value, index, data.length]);
      return { bytesWritten: data.length };
    },
    async bulkOut(devNo, endpoint, data) {
      calls.push(['bulkOut', devNo, endpoint, data.length]);
      return { bytesWritten: data.length };
    },
    async bulkIn(devNo, endpoint, length) {
      calls.push(['bulkIn', devNo, endpoint, length]);
      return { data: new Uint8Array(length) };
    },
    async reset(devNo) {
      calls.push(['reset', devNo]);
    },
  };
}

test('list resolves to the JSON envelope the Go core decodes', async () => {
  const bridge = createBridge(fakeHost());
  const text = await bridge.list();
  assert.equal(typeof text, 'string');
  const payload = JSON.parse(text);
  assert.equal(payload.devices.length, 1);
  assert.equal(payload.devices[0].vendorId, 0x2207);
  assert.equal(payload.devices[0].handle, 'fake#0');
});

test('open passes the opaque handle through, not just the DevNo', async () => {
  const host = fakeHost();
  const bridge = createBridge(host);
  await bridge.open(1, 'fake#0');
  assert.deepEqual(host.calls.at(-1), ['open', 1, 'fake#0']);
});

test('byte payloads are normalised to Uint8Array', async () => {
  const host = fakeHost();
  const bridge = createBridge(host);
  await bridge.bulkOut(1, 2, new Uint8Array([1, 2, 3]).buffer);
  assert.deepEqual(host.calls.at(-1), ['bulkOut', 1, 2, 3]);

  await bridge.controlOut(1, 0x40, 0x0c, 0, 0x0471, new Uint8Array(2050));
  assert.deepEqual(host.calls.at(-1), ['controlOut', 1, 0x40, 0x0c, 0, 0x0471, 2050]);
});

test('numeric arguments are validated before reaching the host', async () => {
  const host = fakeHost();
  const bridge = createBridge(host);
  await assert.rejects(bridge.open('one', 'h'), /devNo must be a number/);
  await assert.rejects(bridge.bulkIn(1, 1, 'x'), /length must be a number/);
  assert.equal(host.calls.length, 0, 'a bad argument must not reach the device');
});

test('withTimeout rejects a stuck transfer instead of hanging the worker', async () => {
  await assert.rejects(
    withTimeout(new Promise(() => {}), 10, 'bulk OUT'),
    /bulk OUT timed out after 10 ms/,
  );
  assert.equal(await withTimeout(Promise.resolve('ok'), 50, 'x'), 'ok');
});

test('bridge surfaces host failures as rejected promises', async () => {
  const host = fakeHost();
  host.bulkOut = async () => {
    throw new Error("A transfer error has occurred.");
  };
  const bridge = createBridge(host);
  await assert.rejects(bridge.bulkOut(1, 2, new Uint8Array(31)), /transfer error/);
});
