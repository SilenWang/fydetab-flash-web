import test from 'node:test';
import assert from 'node:assert/strict';

import { createWebUSBHost, describeDevice, findRockusbInterface, ROCKCHIP_VENDOR_ID } from '../usb-host.js';

function fakeDevice({ vendorId = ROCKCHIP_VENDOR_ID, productId = 0x350b, subminor = 0, serialNumber = 'SN1' } = {}) {
  const device = {
    vendorId,
    productId,
    usbVersionMajor: 2,
    usbVersionMinor: 0,
    usbVersionSubminor: subminor,
    productName: 'Fake RK3588',
    serialNumber,
    configuration: null,
    opened: false,
    claimed: [],
    released: [],
    calls: [],
    configurations: [
      {
        configurationValue: 1,
        interfaces: [
          {
            interfaceNumber: 0,
            alternates: [
              {
                alternateSetting: 0,
                interfaceClass: 0xff,
                interfaceSubclass: 0x06,
                interfaceProtocol: 0x05,
                endpoints: [
                  { endpointNumber: 1, direction: 'in', type: 'bulk', packetSize: 512 },
                  { endpointNumber: 2, direction: 'out', type: 'bulk', packetSize: 512 },
                ],
              },
            ],
          },
        ],
      },
    ],
    async open() {
      device.opened = true;
      device.configuration = device.configurations[0];
    },
    async selectConfiguration(value) {
      device.calls.push(['selectConfiguration', value]);
      device.configuration = device.configurations.find((c) => c.configurationValue === value) ?? null;
    },
    async claimInterface(number) {
      device.claimed.push(number);
    },
    async releaseInterface(number) {
      device.released.push(number);
    },
    async close() {
      device.opened = false;
    },
    async transferOut(endpoint, data) {
      device.calls.push(['transferOut', endpoint, data.byteLength]);
      if (device.stall) throw new Error("Failed to execute 'transferOut' on 'USBDevice': A transfer error has occurred.");
      return { status: 'ok', bytesWritten: data.byteLength };
    },
    async transferIn(endpoint, length) {
      device.calls.push(['transferIn', endpoint, length]);
      return { status: 'ok', data: new DataView(new Uint8Array(length).buffer) };
    },
    async controlOut(setup, data) {
      device.calls.push(['controlOut', setup, data.byteLength]);
      return { status: 'ok', bytesWritten: data.byteLength };
    },
    async reset() {
      device.calls.push(['reset']);
    },
  };
  return device;
}

function fakeUsb(devices) {
  return {
    async getDevices() {
      return devices;
    },
  };
}

test('findRockusbInterface prefers the vendor-specific bulk pair', () => {
  const device = fakeDevice();
  const iface = findRockusbInterface(device);
  assert.equal(iface.interfaceNumber, 0);
  assert.equal(iface.bulkIn, 1);
  assert.equal(iface.bulkOut, 2);
  assert.equal(iface.interfaceClass, 0xff);
  assert.equal(iface.configurationValue, 1);
});

test('findRockusbInterface returns null without a bulk pair', () => {
  const device = fakeDevice();
  device.configurations[0].interfaces[0].alternates[0].endpoints = [
    { endpointNumber: 1, direction: 'in', type: 'interrupt', packetSize: 8 },
  ];
  assert.equal(findRockusbInterface(device), null);
});

test('describeDevice produces the RawInfo the Go core expects', () => {
  const raw = describeDevice(fakeDevice({ subminor: 1 }), 'handle-1');
  assert.equal(raw.vendorId, 0x2207);
  assert.equal(raw.productId, 0x350b);
  assert.equal(raw.usbVersionSubminor, 1);
  assert.equal(raw.interfaceClass, 0xff);
  assert.equal(raw.bulkIn, 1);
  assert.equal(raw.bulkOut, 2);
  assert.equal(raw.handle, 'handle-1');
});

test('host lists only Rockchip devices with stable handles', async () => {
  const usb = fakeUsb([fakeDevice(), fakeDevice({ serialNumber: 'SN2' }), fakeDevice({ vendorId: 0x1234 })]);
  const host = createWebUSBHost(usb);
  const devices = await host.list();
  assert.equal(devices.length, 2);
  assert.deepEqual(
    devices.map((d) => d.handle),
    ['8711:13579:SN1#0', '8711:13579:SN2#0'],
  );
});

test('open claims the rockusb interface and transfers use the claimed endpoints', async () => {
  const device = fakeDevice();
  const host = createWebUSBHost(fakeUsb([device]));
  const [raw] = await host.list();
  await host.open(1, raw.handle);

  assert.equal(device.opened, true);
  assert.deepEqual(device.claimed, [0]);

  const out = await host.bulkOut(1, raw.bulkOut, new Uint8Array(31));
  assert.equal(out.bytesWritten, 31);

  const input = await host.bulkIn(1, raw.bulkIn, 16);
  assert.equal(input.data.byteLength, 16);
  assert.ok(input.data instanceof ArrayBuffer);

  await host.close(1);
  assert.deepEqual(device.released, [0]);
  assert.equal(device.opened, false);
});

test('a stalled bulk OUT rejects, which the core maps to ERR_DEVICE_WRITE_FAILED', async () => {
  const device = fakeDevice();
  const host = createWebUSBHost(fakeUsb([device]));
  const [raw] = await host.list();
  await host.open(1, raw.handle);
  device.stall = true;
  await assert.rejects(host.bulkOut(1, raw.bulkOut, new Uint8Array(31)), /transfer error/);
});

test('vendor control OUT uses the WebUSB setup object', async () => {
  const device = fakeDevice();
  const host = createWebUSBHost(fakeUsb([device]));
  const [raw] = await host.list();
  await host.open(1, raw.handle);

  const result = await host.controlOut(1, 0x40, 0x0c, 0, 0x0471, new Uint8Array(2050));
  assert.equal(result.bytesWritten, 2050);
  const [, setup, length] = device.calls.find((c) => c[0] === 'controlOut');
  assert.deepEqual(setup, { requestType: 'vendor', recipient: 'device', request: 0x0c, value: 0, index: 0x0471 });
  assert.equal(length, 2050);

  await assert.rejects(host.controlOut(1, 0x80, 0x0c, 0, 0x0471, new Uint8Array(1)), /unsupported bmRequestType/);
});

test('opening an unknown handle explains the re-authorization step', async () => {
  const host = createWebUSBHost(fakeUsb([fakeDevice()]));
  await host.list();
  await assert.rejects(host.open(1, 'stale-handle'), /授权设备/);
});

test('transfers on a DevNo without an open session fail loudly', async () => {
  const host = createWebUSBHost(fakeUsb([fakeDevice()]));
  await assert.rejects(host.bulkIn(7, 1, 16), /没有打开的会话/);
});
