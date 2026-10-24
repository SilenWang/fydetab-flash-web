// Smoke test for the probe page itself: runs probe.js against a stub DOM and a
// fake MASKROM device, so the read path (CBW → data → CSW → decode → export)
// is covered without a browser or hardware.
//
// The DOM stub implements only what probe.js touches. It is deliberately tiny:
// if the page starts using another DOM API, this test fails loudly instead of
// silently drifting.

import assert from 'node:assert/strict';
import test from 'node:test';

// ---------------------------------------------------------------------------
// Minimal DOM
// ---------------------------------------------------------------------------

class StubNode {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase();
    this.children = [];
    this.attributes = {};
    this.dataset = {};
    this.className = '';
    this.hidden = false;
    this.listeners = {};
    this._text = '';
    this.disabled = false;
    this.parentNode = null;
  }

  appendChild(child) {
    this.children.push(child);
    child.parentNode = this;
    return child;
  }

  replaceChildren(...children) {
    this.children = [];
    for (const child of children) this.appendChild(child);
  }

  setAttribute(name, value) {
    this.attributes[name] = String(value);
  }

  getAttribute(name) {
    return this.attributes[name] ?? null;
  }

  addEventListener(type, handler) {
    (this.listeners[type] ??= []).push(handler);
  }

  remove() {
    if (this.parentNode) {
      this.parentNode.children = this.parentNode.children.filter((c) => c !== this);
      this.parentNode = null;
    }
  }

  click() {
    for (const handler of this.listeners.click ?? []) handler();
  }

  get textContent() {
    return this._text + this.children.map((c) => c.textContent ?? '').join('');
  }

  set textContent(value) {
    this._text = String(value);
    this.children = [];
  }
}

const ELEMENT_IDS = [
  'support-warning',
  'status',
  'device-list',
  'descriptor',
  'results',
  'trace-body',
  'trace-count',
  'error-list',
  'btn-request',
  'btn-refresh',
  'btn-read',
  'btn-export',
  'btn-clear',
];

function installDom() {
  const nodes = new Map();
  for (const id of ELEMENT_IDS) {
    const node = new StubNode('div');
    node.setAttribute('id', id);
    nodes.set(id, node);
  }
  const body = new StubNode('body');
  const document = {
    body,
    createElement: (tag) => new StubNode(tag),
    createTextNode: (text) => {
      const node = new StubNode('#text');
      node.textContent = text;
      return node;
    },
    getElementById: (id) => nodes.get(id) ?? null,
  };
  return { nodes, document, body };
}

// ---------------------------------------------------------------------------
// Fake MASKROM device
// ---------------------------------------------------------------------------

const CHIP_INFO = (() => {
  const b = new Uint8Array(16); // rockusb always returns 16 bytes, NUL padded
  b.set(new TextEncoder().encode('3588'));
  return b;
})();
const FLASH_INFO = (() => {
  const b = new Uint8Array(11);
  const view = new DataView(b.buffer);
  view.setUint32(0, 0x00700000, true); // 3.5 GiB of 512 B sectors
  view.setUint16(4, 0x0400, true);
  b[6] = 0x08;
  b[7] = 0x10;
  b[8] = 0x0a;
  b[9] = 0x01; // TOSHIBA
  b[10] = 0x01;
  return b;
})();
const CAPABILITY = Uint8Array.from([0x0f, 0x01, 0, 0, 0, 0, 0, 0]);
const STORAGE = Uint8Array.from([0x02, 0x00, 0x00, 0x00]); // bit 1 = eMMC

const DATA_BY_OP = new Map([
  [0x1b, CHIP_INFO],
  [0x1a, FLASH_INFO],
  [0xaa, CAPABILITY],
  [0x2b, STORAGE],
]);

function createFakeDevice({ failStorage = false } = {}) {
  const device = {
    vendorId: 0x2207,
    productId: 0x110a,
    usbVersionMajor: 2,
    usbVersionMinor: 0,
    usbVersionSubminor: 0,
    deviceVersionMajor: 1,
    deviceVersionMinor: 0,
    deviceVersionSubminor: 0,
    deviceClass: 0,
    deviceSubclass: 0,
    deviceProtocol: 0,
    manufacturerName: 'Rockchip',
    productName: 'Fydetab Duo',
    serialNumber: 'RK3588TEST',
    opened: false,
    configuration: null,
    configurations: [
      {
        configurationValue: 1,
        configurationName: null,
        interfaces: [
          {
            interfaceNumber: 0,
            alternates: [
              {
                alternateSetting: 0,
                interfaceClass: 0xff,
                interfaceSubclass: 0x06,
                interfaceProtocol: 0x05,
                interfaceName: null,
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
    writes: [], // every bulk OUT payload, for the read-only assertion
    async open() {
      this.opened = true;
    },
    async close() {
      this.opened = false;
    },
    async selectConfiguration() {
      this.configuration = this.configurations[0];
    },
    async claimInterface() {},
    async releaseInterface() {},
    async selectAlternateInterface() {},
    async transferOut(endpoint, bytes) {
      this.writes.push({ endpoint, bytes: Uint8Array.from(bytes) });
      const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
      this.pending = {
        op: bytes[15],
        tag: view.getUint32(4, true),
        transferLength: view.getUint32(8, true),
        dataSent: false,
      };
      return { status: 'ok', bytesWritten: bytes.byteLength };
    },
    async transferIn(endpoint, length) {
      const pending = this.pending;
      const payload = DATA_BY_OP.get(pending.op);
      if (payload && !pending.dataSent) {
        pending.dataSent = true;
        const data = length >= payload.byteLength ? payload : payload.subarray(0, length);
        return { status: 'ok', data: new DataView(data.buffer, data.byteOffset, data.byteLength) };
      }
      const csw = new Uint8Array(13);
      const view = new DataView(csw.buffer);
      view.setUint32(0, 0x53425355, true);
      view.setUint32(4, pending.tag, true);
      csw[12] = failStorage && pending.op === 0x2b ? 1 : 0;
      this.pending = null;
      return { status: 'ok', data: new DataView(csw.buffer, 0, csw.byteLength) };
    },
  };
  return device;
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

async function bootProbe({ failStorage = false } = {}) {
  const { nodes, document, body } = installDom();
  const fakeDevice = createFakeDevice({ failStorage });

  const usb = {
    async getDevices() {
      return [fakeDevice];
    },
    async requestDevice() {
      return fakeDevice;
    },
    addEventListener() {},
  };

  Object.defineProperty(globalThis, 'navigator', { value: { usb }, configurable: true });
  Object.defineProperty(globalThis, 'window', {
    value: { isSecureContext: true, location: { origin: 'http://localhost:5173' } },
    configurable: true,
  });
  globalThis.document = document;

  let exportedBlob = null;
  const originalCreate = URL.createObjectURL;
  const originalRevoke = URL.revokeObjectURL;
  URL.createObjectURL = (blob) => {
    exportedBlob = blob;
    return 'blob:stub';
  };
  URL.revokeObjectURL = () => {};

  // Importing probe.js runs main(), which lists authorized devices. The module
  // keeps session state, so every case gets a fresh instance via a cache-busting
  // query; its own imports (protocol.js, rockusb.js) stay shared.
  await import(`./probe.js?case=${(bootProbe.counter = (bootProbe.counter ?? 0) + 1)}`);
  await new Promise((resolve) => setTimeout(resolve, 0));

  return {
    nodes,
    body,
    fakeDevice,
    getExport: async () => {
      nodes.get('btn-export').click();
      await new Promise((resolve) => setTimeout(resolve, 0));
      assert.ok(exportedBlob, 'export produced no blob');
      return JSON.parse(await exportedBlob.text());
    },
    async read() {
      nodes.get('btn-read').click();
      // readDevice() is async; wait for the status line to settle.
      for (let i = 0; i < 200; i += 1) {
        await new Promise((resolve) => setTimeout(resolve, 1));
        const status = nodes.get('status').textContent;
        if (status.includes('读取完成') || status.includes('失败') || status.includes('中断')) return status;
      }
      throw new Error(`read never settled, status=${nodes.get('status').textContent}`);
    },
    async restore() {
      URL.createObjectURL = originalCreate;
      URL.revokeObjectURL = originalRevoke;
    },
  };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

test('probe reads chip/flash/capability/storage from a fake MASKROM device', async () => {
  const harness = await bootProbe();
  const status = await harness.read();
  assert.match(status, /读取完成/);
  assert.match(status, /5 条命令全部成功/);

  const exported = await harness.getExport();
  assert.equal(exported.readOnly, true);
  assert.equal(exported.mode, 'MASKROM');
  assert.equal(exported.device.vendorId, '0x2207');
  assert.equal(exported.device.productId, '0x110a');
  assert.equal(exported.device.bcdUSB, '0x0200');
  assert.equal(exported.link.bulkIn, 1);
  assert.equal(exported.link.bulkOut, 2);

  const byName = Object.fromEntries(exported.commands.map((c) => [c.opName, c]));
  assert.equal(byName.RKU_ReadChipInfo.decoded.text, '3588');
  assert.equal(byName.RKU_ReadFlashInfo.decoded.flashSizeBytes, 0x00700000 * 512);
  assert.equal(byName.RKU_ReadFlashInfo.decoded.manufacturer, 'TOSHIBA');
  assert.deepEqual(byName.RKU_ReadCapability.decoded.flags, ['Direct LBA', 'Vendor Storage', 'First 4M Access', 'Read LBA', 'New IDB']);
  assert.equal(byName.RKU_ReadStorageList.decoded.currentName, 'eMMC');
  for (const record of exported.commands) {
    assert.equal(record.ok, true, `${record.opName} failed: ${record.errorDetail}`);
    assert.equal(record.errorCode, 0);
  }

  // 5 CBW writes + 4 data reads + 5 CSW reads = 14 transfers, all logged.
  assert.equal(exported.transfers.length, 14);
  const out = exported.transfers.filter((t) => t.direction === 'out');
  assert.equal(out.length, 5);
  for (const t of out) {
    assert.equal(t.kind, 'bulk-out');
    assert.equal(t.length, 31);
    assert.equal(t.stage, 'cbw');
    assert.equal(t.dataHex.startsWith('55 53 42 43'), true, 'CBW starts with "USBC"');
    assert.equal(typeof t.durationMs, 'number');
    assert.equal(t.requestType, null);
  }
  const dataReads = exported.transfers.filter((t) => t.stage === 'data');
  assert.equal(dataReads.length, 4);
  const cswReads = exported.transfers.filter((t) => t.stage === 'csw');
  assert.equal(cswReads.length, 5);

  // Every byte that reached the device was a 31-byte CBW.
  assert.deepEqual(harness.fakeDevice.writes.map((w) => w.bytes.byteLength), [31, 31, 31, 31, 31]);
  assert.equal(harness.fakeDevice.opened, false, 'device must be closed after probing');

  await harness.restore();
});

test('a failed read command is recorded with its error code and never retried', async () => {
  const harness = await bootProbe({ failStorage: true });
  const status = await harness.read();
  assert.match(status, /RKU_ReadStorageList=ERR_FAILED/);

  const exported = await harness.getExport();
  const storage = exported.commands.find((c) => c.opName === 'RKU_ReadStorageList');
  assert.equal(storage.ok, false);
  assert.equal(storage.errorCode, -8);
  assert.equal(storage.errorName, 'ERR_FAILED');
  assert.match(storage.errorDetail, /CSW status failed/);

  // Exactly one CBW for the failing command: no retry, no recovery write.
  const storageWrites = harness.fakeDevice.writes.filter((w) => w.bytes[15] === 0x2b);
  assert.equal(storageWrites.length, 1);
  assert.equal(exported.errors.length, 1);
  assert.equal(exported.errors[0].name, 'ERR_FAILED');

  await harness.restore();
});

test('page declares read-only and reports its status before any device is opened', async () => {
  const harness = await bootProbe();
  assert.equal(harness.fakeDevice.writes.length, 0, 'boot must not touch the device');
  assert.match(harness.nodes.get('device-list').textContent, /Fydetab Duo|0x2207/);
  assert.equal(harness.nodes.get('btn-read').disabled, false);
  await harness.restore();
});
