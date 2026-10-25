// End-to-end test of the real Go/WASM core: no mocks of the protocol, only the
// device. The wasm artifact is built by `pixi run wasm` (the `test` task
// depends on it) and the worker's bridge is wired to mock-host.js exactly like
// the page's demo mode.
//
// This is what pins the M1 story:
//   ld → maskrom refuses CBW/read info → db (RTBOOT vendor requests)
//   → re-enumerates as LOADER → ld → info works → cd / sd work.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

import '../wasm/wasm_exec.js'; // sets globalThis.Go (version-matched to the wasm)
import { createBridge } from '../bridge.js';
import { createMockHost } from '../mock-host.js';
import { buildMockLoader } from '../mock-loader.js';

const WASM_URL = new URL('../wasm/rk3588.wasm', import.meta.url);

async function bootCore(host) {
  const go = new globalThis.Go();
  const bytes = await readFile(WASM_URL);
  const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
  go.run(instance); // the Go program parks forever; this promise never settles

  const started = Date.now();
  while (!globalThis.rk3588) {
    if (Date.now() - started > 5000) throw new Error('rk3588 wasm API never appeared');
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  globalThis.rk3588.bindBridge(createBridge(host), { timeoutMs: 5000, readRetries: 1 });

  const exec = async (line, loader, loaderName = '') =>
    JSON.parse(await globalThis.rk3588.exec(line, loader, loaderName));
  const inspect = async (bytes) => JSON.parse(await globalThis.rk3588.inspectLoader(bytes));
  return { exec, inspect, api: globalThis.rk3588 };
}

test('the wasm core drives a full maskrom → loader session', async (t) => {
  const host = createMockHost();
  const core = await bootCore(host);

  await t.test('help advertises only the safe command set', async () => {
    const res = await core.exec('help');
    assert.equal(res.ok, true);
    const text = res.lines.join('\n');
    for (const cmd of ['ld', 'desc', 'cd', 'sd', 'db', 'info']) {
      assert.match(text, new RegExp(`\\b${cmd}\\b`), `help should document ${cmd}`);
    }
    assert.doesNotMatch(text, /\buf\b/, 'there must be no flash write command');
  });

  await t.test('ld prints upgrade_tool-style lines for a maskrom board', async () => {
    const res = await core.exec('ld');
    assert.equal(res.ok, true);
    assert.deepEqual(res.lines, ['DevNo=1 Vid=0x2207,Pid=0x350b,Mode=Maskrom']);
    assert.equal(res.state.mode, 'MASKROM');
    assert.equal(res.state.current, 1);
  });

  await t.test('descriptor is readable without any USB transfer', async () => {
    const res = await core.exec('desc');
    assert.equal(res.ok, true);
    assert.equal((res.transfers ?? []).length, 0);
    assert.match(res.lines.join('\n'), /bcdUSB: 0x0200 .*bit0=0.*Maskrom/);
  });

  await t.test('read-only info is refused in maskrom, before opening the device', async () => {
    const res = await core.exec('info');
    assert.equal(res.ok, false);
    assert.equal(res.error.code, -10); // ERR_DEVICE_NOT_SUPPORT
    assert.match(res.error.hint, /db/);
    assert.equal((res.transfers ?? []).length, 0, 'the core must not even try a CBW in maskrom');
    assert.equal(host.inspect().controls, 0);
  });

  await t.test('the local loader is validated before db', async () => {
    const payload = await core.inspect(buildMockLoader());
    assert.equal(payload.ok, true);
    assert.equal(payload.segments.length, 3);
    assert.deepEqual(
      payload.segments.map((s) => [s.request, s.entry.name, s.entry.dataSize]),
      [
        [0x0471, 'UsbHead', 2048],
        [0x0471, 'rk3588_ddr_lp4_2112M', 51200],
        [0x0472, 'rk3588_usbplug_v1', 88064],
      ],
    );
    const broken = await core.inspect(new Uint8Array(400));
    assert.equal(broken.ok, false);
    assert.ok(broken.kind);
  });

  await t.test('db sends the loader over EP0 vendor requests and waits for re-enumeration', async () => {
    const loader = buildMockLoader();
    const res = await core.exec('db', loader, 'mock-loader.bin');
    assert.equal(res.ok, true, JSON.stringify(res.error));
    assert.equal(res.db.segments, 3);
    assert.equal(res.db.packets, 36);
    assert.equal(res.db.bytes, 2050 + 51202 + 88066);
    assert.equal(res.state.awaitingReenumeration, true);
    assert.equal(res.state.loaderLoaded, true);

    const state = host.inspect();
    assert.equal(state.mode, 'loader');
    assert.equal(state.controls, 36);
    assert.equal(state.modes, 1);

    // The vendor transfers match the documented Download Boot shape: bmRequestType
    // 0x40, bRequest 0x0C, index 0x0471 first, 0x0472 last, 2050-byte first
    // packet (2048 + 2 CRC), and no packet over the 4096-byte chunk size.
    const controls = state.controlLog;
    assert.equal(controls[0].requestType, 0x40);
    assert.equal(controls[0].request, 0x0c);
    assert.equal(controls[0].index, 0x0471);
    assert.equal(controls[0].length, 2050, 'upgrade_tool logs "Total(2050)" for the first packet');
    assert.equal(controls.at(-1).index, 0x0472);
    for (const control of controls) {
      assert.ok(control.length > 0 && control.length <= 4096);
    }
    const indices = [...new Set(controls.map((c) => c.index))];
    assert.deepEqual(indices, [0x0471, 0x0472]);
    for (const cbw of state.cbws) {
      assert.equal(cbw.length, 31, 'maskrom never answered a CBW; these are all from the loader phase');
    }
  });

  await t.test('ld notices the LOADER re-enumeration', async () => {
    const res = await core.exec('ld');
    assert.equal(res.ok, true);
    assert.deepEqual(res.lines[0], 'DevNo=1 Vid=0x2207,Pid=0x350b,Mode=Loader');
    assert.match(res.lines.join('\n'), /Re-enumerated as LOADER/);
    assert.equal(res.state.awaitingReenumeration, false);
    assert.equal(res.state.mode, 'LOADER');
  });

  await t.test('info reads chip/flash/capability/storage in loader mode', async () => {
    const res = await core.exec('info');
    assert.equal(res.ok, true, JSON.stringify(res.error));
    assert.equal(res.commands.length, 5);
    assert.equal(res.info.chip.text, '3588');
    assert.equal(res.info.flash.flashSizeBytes, 0x00700000 * 512);
    assert.equal(res.info.storage.currentName, 'eMMC');
    assert.deepEqual(res.info.capability.flags, [
      'Direct LBA',
      'Vendor Storage',
      'First 4M Access',
      'Read LBA',
      'New IDB',
    ]);

    // Every outbound frame is a 31-byte CBW; nothing else left the host.
    for (const tr of res.transfers ?? []) {
      if (tr.kind === 'bulk-out') {
        assert.equal(tr.length, 31);
      }
      if (tr.kind === 'control-out') {
        assert.fail('info must not issue control transfers');
      }
    }
    const state = host.inspect();
    for (const cbw of state.cbws) {
      assert.equal(cbw.length, 31);
      assert.ok([0x00, 0x1b, 0x1a, 0xaa, 0x2b].includes(cbw.op), `opcode 0x${cbw.op.toString(16)}`);
    }
  });

  await t.test('a section filter only issues the commands it needs', async () => {
    const res = await core.exec('info chip');
    assert.equal(res.ok, true);
    assert.equal(res.commands.length, 2); // test-unit-ready + chip info
    assert.equal(res.info.flash, undefined);
    assert.equal(res.info.chip.text, '3588');
  });

  await t.test('cd and sd switch the current device', async () => {
    const cd = await core.exec('cd 1');
    assert.equal(cd.ok, true);
    assert.match(cd.lines[0], /ChangeDevice: DevNo=1 .*Mode=Loader/);

    const sd = await core.exec('sd 1');
    assert.equal(sd.ok, true);
    assert.match(sd.lines[0], /SwitchDevice: DevNo=1/);

    const bad = await core.exec('cd 2');
    assert.equal(bad.ok, false);
    assert.match(bad.lines.join('\n'), /DevNo=2/);
  });

  await t.test('flash-writing commands do not exist', async () => {
    for (const line of ['uf image.img', 'wl 0 image.img', 'el', 'rd', 'cs 0']) {
      const res = await core.exec(line);
      assert.equal(res.ok, false, `${line} must fail`);
      assert.match(res.error.detail, /unknown command/);
    }
  });

  await t.test('an invalid loader is rejected with a classified reason', async () => {
    const res = await core.exec('db', new Uint8Array(600), 'half-downloaded.bin');
    assert.equal(res.ok, false);
    assert.ok(res.error.loader, 'the error must be classified as a loader problem');
    assert.ok(res.error.hint);
  });
});
