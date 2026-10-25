// The device list rules: mode comes from bcdUSB bit 0, the WebUSB view is
// merged with the core's `ld` view, and a re-enumerated board must not stay
// selected as if it were the old one.
//
// These are the failures that look like "the page forgot my device": after
// `db` the browser drops the per-instance authorisation, the key changes, and a
// list that matched by object identity or by index silently points at nothing.

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  ROCKCHIP_VENDOR_ID,
  describeUsbDevice,
  deviceSummaryLines,
  listUsbDevices,
  matchAfterReenumeration,
  mergeShellState,
  modeFromBcdUsb,
  selectedDevice,
} from '../devices.js';

function usbDevice({ serialNumber = 'SN1', productId = 0x350b, vendorId = ROCKCHIP_VENDOR_ID, bcdUSB } = {}) {
  return { vendorId, productId, serialNumber, productName: 'Fydetab Duo', bcdUSB };
}

test('mode follows the protocol bit: bcdUSB bit0=1 is LOADER', () => {
  assert.equal(modeFromBcdUsb(0x0200), 'MASKROM');
  assert.equal(modeFromBcdUsb(0x0201), 'LOADER');
  assert.equal(modeFromBcdUsb(0x0110), 'MASKROM');
  assert.equal(modeFromBcdUsb(undefined), 'UNKNOWN');
  assert.equal(modeFromBcdUsb(null), 'UNKNOWN');
});

test('a described device carries every field the list shows', () => {
  const maskrom = describeUsbDevice(usbDevice({ bcdUSB: 0x0200 }), 0);
  assert.equal(maskrom.key, '0x2207:0x350b:SN1#0');
  assert.equal(maskrom.mode, 'MASKROM');
  assert.equal(maskrom.modeLabel, 'Maskrom');
  assert.equal(maskrom.serialNumber, 'SN1');
  assert.equal(maskrom.bcdUSB, 0x0200);
  assert.equal(maskrom.flashable, true);
  assert.equal(maskrom.needsDownloadBoot, true);
  assert.equal(maskrom.devNo, null);

  const loader = describeUsbDevice(usbDevice({ bcdUSB: 0x0201, serialNumber: 'SN2' }), 1);
  assert.equal(loader.mode, 'LOADER');
  assert.equal(loader.needsDownloadBoot, false);
  assert.equal(loader.key, '0x2207:0x350b:SN2#1');

  const unknown = describeUsbDevice(usbDevice({}), 2);
  assert.equal(unknown.mode, 'UNKNOWN');
  assert.equal(unknown.flashable, false);
});

test('listUsbDevices lists every Rockchip device and explains an empty or missing browser', async () => {
  const usb = {
    async getDevices() {
      return [
        usbDevice({ serialNumber: 'A', bcdUSB: 0x0200 }),
        usbDevice({ serialNumber: 'B', bcdUSB: 0x0201 }),
        { vendorId: 0x1234, productId: 1, serialNumber: 'other' },
      ];
    },
  };
  const listed = await listUsbDevices(usb);
  assert.equal(listed.ok, true);
  assert.deepEqual(listed.devices.map((device) => device.serialNumber), ['A', 'B']);
  assert.equal(listed.reason, '');

  const empty = await listUsbDevices({ getDevices: async () => [] });
  assert.equal(empty.ok, true);
  assert.match(empty.reason, /还没有已授权的 0x2207 设备/);

  const noUsb = await listUsbDevices(null);
  assert.equal(noUsb.ok, false);
  assert.match(noUsb.reason, /Chrome|Edge/);

  const throwing = await listUsbDevices({
    getDevices: async () => {
      throw new Error('permission store unavailable');
    },
  });
  assert.equal(throwing.ok, false);
  assert.match(throwing.reason, /permission store unavailable/);

  // bcdUSB is not on USBDevice: the page passes what the core read.
  const versioned = await listUsbDevices({ getDevices: async () => [usbDevice({ bcdUSB: 0x0201 })] });
  assert.equal(versioned.devices[0].mode, 'LOADER');
});

test('the WebUSB rows and the core ld rows merge on serial and VID:PID', () => {
  const usbRows = [
    describeUsbDevice(usbDevice({ serialNumber: 'SN1', bcdUSB: 0x0200 }), 0),
    describeUsbDevice(usbDevice({ serialNumber: 'SN2', bcdUSB: 0x0200 }), 1),
  ];
  const shellState = {
    devices: [
      { devNo: 1, vendorId: ROCKCHIP_VENDOR_ID, productId: 0x350b, serialNumber: 'SN2', bcdUSB: 0x0201, mode: 2, modeLabel: 'Loader' },
    ],
    current: 1,
  };
  const merged = mergeShellState(usbRows, shellState);
  assert.equal(merged.length, 2);
  const matched = merged.find((row) => row.serialNumber === 'SN2');
  assert.equal(matched.devNo, 1, 'the shell DevNo is what cd/db act on');
  assert.equal(matched.mode, 'LOADER', 'the core read the mode through the device; it wins');
  assert.equal(matched.needsDownloadBoot, false);
  assert.equal(matched.selected, true);
  assert.equal(matched.source, 'usb+shell');

  const unmatched = merged.find((row) => row.serialNumber === 'SN1');
  assert.equal(unmatched.devNo, null);
  assert.equal(unmatched.mode, 'MASKROM');
});

test('a device only the core can see is still listed', () => {
  const merged = mergeShellState([], {
    devices: [{ devNo: 2, vendorId: ROCKCHIP_VENDOR_ID, productId: 0x350b, bcdUSB: 0x0201, mode: 'LOADER', modeLabel: 'Loader' }],
    current: 2,
  });
  assert.equal(merged.length, 1);
  assert.equal(merged[0].source, 'shell');
  assert.equal(merged[0].devNo, 2);
  assert.equal(merged[0].mode, 'LOADER');
  assert.equal(merged[0].selected, true);
});

test('selectedDevice prefers the explicit key, then the shell selection, then a sole device', () => {
  const rows = [
    { key: 'a', selected: false },
    { key: 'b', selected: true },
  ];
  assert.equal(selectedDevice(rows, 'a').key, 'a');
  assert.equal(selectedDevice(rows, null).key, 'b');
  assert.equal(selectedDevice([{ key: 'only', selected: false }], null).key, 'only');
  assert.equal(selectedDevice([{ key: 'a', selected: false }, { key: 'b', selected: false }], null), null);
  assert.equal(selectedDevice([], 'a'), null);
});

test('after db the board re-enumerates: match by serial, never by stale key', () => {
  const previous = { key: 'old-instance', serialNumber: 'SN1', vendorId: 0x2207, productId: 0x350b };
  const renumerated = [
    { key: 'new-instance', serialNumber: 'SN1', vendorId: 0x2207, productId: 0x350b },
    { key: 'other', serialNumber: 'SN2', vendorId: 0x2207, productId: 0x350b },
  ];
  assert.equal(matchAfterReenumeration(renumerated, previous).device.key, 'new-instance');

  const soleCandidate = matchAfterReenumeration([{ key: 'new', serialNumber: '' }], previous);
  assert.equal(soleCandidate.device.key, 'new');
  assert.match(soleCandidate.reason, /只剩一个候选/);

  const ambiguous = matchAfterReenumeration(
    [
      { key: 'x', serialNumber: '' },
      { key: 'y', serialNumber: '' },
    ],
    previous,
  );
  assert.equal(ambiguous.device, null);
  assert.match(ambiguous.reason, /重新选择/);

  const gone = matchAfterReenumeration([], previous);
  assert.equal(gone.device, null);
  assert.match(gone.reason, /离开总线/);
});

test('a device row shows VID:PID, serial, bcdUSB and what it needs next', () => {
  const maskrom = describeUsbDevice(usbDevice({ bcdUSB: 0x0200 }), 0);
  const lines = deviceSummaryLines({ ...maskrom, devNo: 1 });
  assert.match(lines[0], /0x2207:0x350b/);
  assert.match(lines[0], /Maskrom/);
  assert.match(lines[0], /DevNo=1/);
  assert.match(lines[1], /SN1/);
  assert.match(lines[1], /0x0200/);
  assert.match(lines[1], /Download Boot/);

  const unknown = deviceSummaryLines(describeUsbDevice(usbDevice({}), 0));
  assert.match(unknown.at(-1), /不能写入/);
  assert.deepEqual(deviceSummaryLines(null), []);
});
