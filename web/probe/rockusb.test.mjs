import assert from 'node:assert/strict';
import test from 'node:test';

import { CBW_LENGTH, COMMANDS } from './protocol.js';
import {
  assertReadOnlyOp,
  decodeCSW,
  decodeCapability,
  decodeChipInfo,
  decodeFlashInfo,
  decodeStorage,
  encodeCBW,
  hex,
  makeTag,
  modeFromDescriptor,
  packBCDUSB,
} from './rockusb.js';

const u8 = (...values) => Uint8Array.from(values);

test('encodeCBW produces the 31-byte rkdeveloptool CBW', () => {
  const cbw = encodeCBW(0x11223344, 0x1b, 16);

  assert.equal(cbw.byteLength, CBW_LENGTH);
  // "USBC" little-endian
  assert.deepEqual([...cbw.slice(0, 4)], [0x55, 0x53, 0x42, 0x43]);
  assert.equal(new DataView(cbw.buffer).getUint32(4, true), 0x11223344);
  assert.equal(new DataView(cbw.buffer).getUint32(8, true), 16);
  assert.equal(cbw[12], 0x80, 'CBW flags must be device-to-host');
  assert.equal(cbw[13], 0, 'LUN stays 0');
  assert.equal(cbw[14], 0x06, 'cbwcb length is 6 for read-info commands');
  assert.equal(cbw[15], 0x1b, 'opcode lives at cbwcb.ucOperCode');
  for (let i = 16; i < CBW_LENGTH; i += 1) {
    assert.equal(cbw[i], 0, `byte ${i} must stay zero`);
  }
});

test('encodeCBW tag and length are unsigned', () => {
  const cbw = encodeCBW(0xffffffff, 0x1a, 11);
  const view = new DataView(cbw.buffer);
  assert.equal(view.getUint32(4, true), 0xffffffff);
  assert.equal(view.getUint32(8, true), 11);
});

test('decodeCSW accepts a matching wrapper', () => {
  const bytes = new Uint8Array(13);
  const view = new DataView(bytes.buffer);
  view.setUint32(0, 0x53425355, true);
  view.setUint32(4, 0xaabbccdd, true);
  view.setUint32(8, 0x12340000, true);
  bytes[12] = 0;

  const csw = decodeCSW(bytes);
  assert.equal(csw.tag, 0xaabbccdd);
  assert.equal(csw.residue, 0x12340000);
  assert.equal(csw.status, 0);
});

test('decodeCSW rejects a short buffer and a bad signature', () => {
  assert.throws(() => decodeCSW(new Uint8Array(12)), (err) => err.code === -4);

  const bad = new Uint8Array(13);
  new DataView(bad.buffer).setUint32(0, 0xdeadbeef, true);
  assert.throws(() => decodeCSW(bad), (err) => err.code === -5);
});

test('decodeChipInfo trims the NUL padded model string', () => {
  const raw = new Uint8Array(16);
  raw.set([0x33, 0x35, 0x38, 0x38]); // "3588"
  const info = decodeChipInfo(raw);
  assert.equal(info.text, '3588');
  assert.equal(info.rawHex, '33 35 38 38 00 00 00 00 00 00 00 00 00 00 00 00');
});

test('decodeFlashInfo decodes STRUCT_FLASHINFO_CMD', () => {
  const bytes = new Uint8Array(11);
  const view = new DataView(bytes.buffer);
  view.setUint32(0, 0x00700000, true); // sectors
  view.setUint16(4, 0x0400, true); // block size in sectors
  bytes[6] = 0x08; // page size in sectors
  bytes[7] = 0x10; // ECC bits
  bytes[8] = 0x0a; // access time
  bytes[9] = 0x01; // TOSHIBA
  bytes[10] = 0x03; // CS 0 + 1

  const info = decodeFlashInfo(bytes);
  assert.equal(info.flashSizeSectors, 0x00700000);
  assert.equal(info.flashSizeBytes, 0x00700000 * 512);
  assert.equal(info.blockSizeBytes, 0x0400 * 512);
  assert.equal(info.pageSizeBytes, 8 * 512);
  assert.equal(info.eccBits, 0x10);
  assert.equal(info.accessTime, 0x0a);
  assert.equal(info.manufacturer, 'TOSHIBA');
  assert.deepEqual(info.flashCSList, [0, 1]);
});

test('decodeFlashInfo reports unknown manufacturers and rejects short data', () => {
  const bytes = new Uint8Array(11);
  bytes[9] = 0x42;
  assert.equal(decodeFlashInfo(bytes).manufacturer, 'UNKNOWN');
  assert.throws(() => decodeFlashInfo(new Uint8Array(10)), (err) => err.code === -4);
});

test('decodeCapability exposes only the documented bits', () => {
  const cap = decodeCapability(u8(0x0f, 0x01, 0, 0, 0, 0, 0, 0));
  assert.equal(cap.directLBA, true);
  assert.equal(cap.vendorStorage, true);
  assert.equal(cap.first4MAccess, true);
  assert.equal(cap.readLBA, true);
  assert.equal(cap.newIDB, true);
  assert.deepEqual(cap.flags, ['Direct LBA', 'Vendor Storage', 'First 4M Access', 'Read LBA', 'New IDB']);
  assert.throws(() => decodeCapability(new Uint8Array(7)), (err) => err.code === -4);
});

test('decodeStorage picks the lowest set bit as the active medium', () => {
  const storage = decodeStorage(u8(0x02, 0x02, 0x00, 0x00)); // bits 1 and 9
  assert.equal(storage.bitmap, 0x202);
  assert.deepEqual(storage.available, [1, 9]);
  assert.deepEqual(storage.names, ['eMMC', 'SPI NOR']);
  assert.equal(storage.current, 1);
  assert.equal(storage.currentName, 'eMMC');

  const empty = decodeStorage(u8(0, 0, 0, 0));
  assert.equal(empty.current, -1);
  assert.equal(empty.currentName, null);
});

test('mode detection follows RSKScan bit-0 convention', () => {
  assert.equal(packBCDUSB(2, 0, 0), 0x0200);
  assert.equal(packBCDUSB(2, 0, 1), 0x0201);
  assert.equal(modeFromDescriptor(0x2207, 0x0200), 'MASKROM');
  assert.equal(modeFromDescriptor(0x2207, 0x0201), 'LOADER');
  assert.equal(modeFromDescriptor(0x1234, 0x0200), 'UNKNOWN');
});

test('only whitelisted read commands may ever be written', () => {
  for (const { op } of COMMANDS) {
    assert.doesNotThrow(() => assertReadOnlyOp(op, CBW_LENGTH));
  }
  // WRITE_LBA / ERASE_NORMAL / DEVICE_RESET must be refused.
  for (const op of [0x15, 0x05, 0x06, 0x0b, 0xff, 0x2a]) {
    assert.throws(() => assertReadOnlyOp(op, CBW_LENGTH), /not a read-only command/);
  }
  // Anything that is not exactly a CBW is refused before it reaches the device.
  assert.throws(() => assertReadOnlyOp(0x1b, 4096), /only a 31-byte CBW/);
});

test('hex renders lowercase space-separated bytes with an optional limit', () => {
  assert.equal(hex(u8(0x00, 0x0f, 0xff)), '00 0f ff');
  assert.equal(hex(u8(0x00, 0x0f, 0xff), 2), '00 0f');
});

test('makeTag yields a uint32', () => {
  const tag = makeTag();
  assert.equal(Number.isInteger(tag), true);
  assert.ok(tag >= 0 && tag <= 0xffffffff);
});
