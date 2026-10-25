import test from 'node:test';
import assert from 'node:assert/strict';

import { CBW_LENGTH, CBW_SIGNATURE, createMockHost, VENDOR_REQUEST_471, VENDOR_REQUEST_472 } from '../mock-host.js';
import { buildMockLoader, crc32Rockchip, LOADER_HEADER_SIZE, LOADER_TAG_BOOT, RK3588_CHIP_TAG } from '../mock-loader.js';

function cbw(tag, op, transferLength) {
  const bytes = new Uint8Array(CBW_LENGTH);
  const view = new DataView(bytes.buffer);
  view.setUint32(0, CBW_SIGNATURE, true);
  view.setUint32(4, tag, true);
  view.setUint32(8, transferLength, true);
  bytes[12] = 0x80;
  bytes[14] = 0x06;
  bytes[15] = op;
  return bytes;
}

test('mock maskrom rejects bulk CBWs exactly like the real BootROM', async () => {
  const host = createMockHost();
  await host.bulkOut(1, 2, cbw(1, 0x1b, 16)).then(
    () => assert.fail('maskrom must reject a CBW'),
    (err) => assert.match(err.message, /transfer error has occurred/),
  );
  assert.equal(host.inspect().cbws.length, 0, 'a rejected CBW never reaches the device');
  assert.equal(host.inspect().mode, 'maskrom');
});

test('vendor requests move the mock device from maskrom to loader', async () => {
  const logs = [];
  const host = createMockHost({ onLog: (m) => logs.push(m) });

  const first = new Uint8Array(2050); // UsbHead 2048 + 2 CRC bytes
  let result = await host.controlOut(1, 0x40, 0x0c, 0, VENDOR_REQUEST_471, first);
  assert.equal(result.bytesWritten, 2050);
  assert.equal(host.inspect().mode, 'maskrom');

  // A full packet of the usbplug segment does not finish the download.
  await host.controlOut(1, 0x40, 0x0c, 0, VENDOR_REQUEST_472, new Uint8Array(4096));
  assert.equal(host.inspect().mode, 'maskrom');

  // The short final chunk does.
  await host.controlOut(1, 0x40, 0x0c, 0, VENDOR_REQUEST_472, new Uint8Array(2050));
  const state = host.inspect();
  assert.equal(state.mode, 'loader');
  assert.equal(state.modes, 1);
  assert.match(logs.join('\n'), /re-enumerated as LOADER/);

  const devices = await host.list();
  assert.equal(devices.length, 1);
  assert.equal(devices[0].usbVersionSubminor, 1, 'bcdUSB bit0 set marks LOADER');
});

test('vendor requests must use the BootROM index and framing', async () => {
  const host = createMockHost();
  await assert.rejects(
    host.controlOut(1, 0x40, 0x0c, 0, 0x0470, new Uint8Array(4)),
    /not 0x0471\/0x0472/,
  );
  await assert.rejects(
    host.controlOut(1, 0x40, 0x0d, 0, VENDOR_REQUEST_471, new Uint8Array(4)),
    /unexpected vendor request/,
  );
});

test('mock loader answers read-only CBWs with data and a matching CSW', async () => {
  const host = createMockHost();
  // Flip to loader without a full download: the short usbplug packet is enough.
  await host.controlOut(1, 0x40, 0x0c, 0, VENDOR_REQUEST_472, new Uint8Array(2));

  const tag = 0x1234abcd;
  const out = await host.bulkOut(1, 2, cbw(tag, 0x1b, 16));
  assert.equal(out.bytesWritten, CBW_LENGTH);

  const data = await host.bulkIn(1, 1, 16);
  assert.equal(data.data.length, 16);
  assert.deepEqual(Array.from(data.data.slice(0, 4)), [0x33, 0x35, 0x38, 0x38]); // "3588"

  const csw = await host.bulkIn(1, 1, 13);
  const view = new DataView(csw.data.buffer, csw.data.byteOffset, csw.data.byteLength);
  assert.equal(view.getUint32(0, true), 0x53425355);
  assert.equal(view.getUint32(4, true), tag);
  assert.equal(csw.data[12], 0);
});

test('mock loader refuses non read-only opcodes and malformed CBWs', async () => {
  const host = createMockHost();
  await host.controlOut(1, 0x40, 0x0c, 0, VENDOR_REQUEST_472, new Uint8Array(2));

  await assert.rejects(host.bulkOut(1, 2, cbw(1, 0x15, 512)), /not a read-only command/); // WRITE_LBA
  await assert.rejects(host.bulkOut(1, 2, new Uint8Array(7)), /not a CBW/);
  await assert.rejects(host.bulkOut(9, 2, cbw(1, 0x1b, 16)), /DevNo=9/);
  await assert.rejects(host.bulkOut(1, 7, cbw(1, 0x1b, 16)), /endpoint/);
  await assert.rejects(host.bulkIn(1, 1, 16), /without a pending CBW/);
});

test('mock loader serves the four read-only payloads', async () => {
  const host = createMockHost();
  await host.controlOut(1, 0x40, 0x0c, 0, VENDOR_REQUEST_472, new Uint8Array(2));

  const cases = [
    { op: 0x00, length: 0 },
    { op: 0x1b, length: 16 },
    { op: 0x1a, length: 11 },
    { op: 0xaa, length: 8 },
    { op: 0x2b, length: 4 },
  ];
  let tag = 100;
  for (const { op, length } of cases) {
    await host.bulkOut(1, 2, cbw(++tag, op, length));
    if (length > 0) {
      const data = await host.bulkIn(1, 1, length);
      assert.equal(data.data.length, length, `opcode 0x${op.toString(16)} payload length`);
    }
    const csw = await host.bulkIn(1, 1, 13);
    assert.equal(csw.data[12], 0);
  }
  const storage = await (async () => {
    await host.bulkOut(1, 2, cbw(++tag, 0x2b, 4));
    return host.bulkIn(1, 1, 4);
  })();
  assert.equal(new DataView(storage.data.buffer).getUint32(0, true), 0x202);
});

test('the synthetic demo loader is a valid RKBOOT container', () => {
  const bytes = buildMockLoader();
  const view = new DataView(bytes.buffer);

  assert.equal(view.getUint32(0, true), LOADER_TAG_BOOT);
  assert.equal(view.getUint16(4, true), LOADER_HEADER_SIZE);
  assert.equal(view.getUint32(21, true), RK3588_CHIP_TAG);
  assert.equal(bytes[25], 2, 'two 0x0471 entries');
  assert.equal(bytes[31], 1, 'one 0x0472 entry');

  const stored = view.getUint32(bytes.length - 4, true);
  const calculated = crc32Rockchip(bytes.subarray(0, bytes.length - 4));
  assert.equal(stored, calculated, 'trailing CRC-32 must be Rockchip\u2019s own polynomial');

  // Segment sizes the stage-2 hardware run reported: 2048 + 51200 on 0x0471,
  // 88064 on 0x0472.
  const offset471 = view.getUint32(26, true);
  const offset472 = view.getUint32(32, true);
  const entrySize = bytes[30];
  assert.equal(view.getUint32(offset471 + 49, true), 2048);
  assert.equal(view.getUint32(offset471 + entrySize + 49, true), 51200);
  assert.equal(view.getUint32(offset472 + 49, true), 88064);

  // The first payload must be the 2048-byte UsbHead, so its vendor packet is
  // 2048 + 2 CRC = 2050 bytes, matching upgrade_tool's "Total(2050)".
  const head = bytes.subarray(view.getUint32(offset471 + 45, true), view.getUint32(offset471 + 45, true) + 2048);
  assert.equal(head.length + 2, 2050);
});

test('crc32Rockchip differs from standard CRC-32', () => {
  const data = new TextEncoder().encode('rk3588_spl_loader_v1.06.111');
  const rockchip = crc32Rockchip(data);
  // Standard CRC-32 (polynomial 0x04C11DB7) for the same input.
  let crc = 0xffffffff;
  for (const byte of data) {
    crc ^= byte;
    for (let i = 0; i < 8; i += 1) crc = crc & 1 ? (crc >>> 1) ^ 0xedb88320 : crc >>> 1;
  }
  const standard = (crc ^ 0xffffffff) >>> 0;
  assert.notEqual(rockchip, standard, 'Rockchip ships poly 0x04C10DB7, not 0x04C11DB7');
});
