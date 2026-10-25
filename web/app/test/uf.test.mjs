// The M3b write path through the real Go/WASM core.
//
// Planning and the state machine are the same code the CLI runs, so the only
// thing these tests stand in for is the camera-facing part: the page's image
// object and the (in-wasm) simulator instead of WebUSB.
//
// What they pin:
//   - ufPlan extracts the loader the image carries and reports its sha256,
//     length, LDR build date and chip match;
//   - the plan is the parameter-derived one (items + uncovered tail);
//   - ufRun drives db → gpt → wl → rd through the same state machine and
//     streams progress;
//   - bad input fails as a plan error instead of touching a device.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

import '../wasm/wasm_exec.js'; // sets globalThis.Go (version-matched to the wasm)

const WASM_URL = new URL('../wasm/rk3588.wasm', import.meta.url);

async function bootCore() {
  const go = new globalThis.Go();
  const bytes = await readFile(WASM_URL);
  const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
  go.run(instance); // the Go program parks forever; this promise never settles

  const started = Date.now();
  while (!globalThis.rk3588) {
    if (Date.now() - started > 5000) throw new Error('rk3588 wasm API never appeared');
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  return globalThis.rk3588;
}

function bytesSource(name, bytes) {
  return {
    name,
    size: bytes.byteLength,
    read: async (offset, length) => bytes.subarray(offset, offset + length),
  };
}

async function demoImage(api) {
  const payload = JSON.parse(await api.demoImage());
  const bytes = new Uint8Array(Buffer.from(payload.bytes, 'base64'));
  return { name: payload.name, bytes, source: bytesSource(payload.name, bytes) };
}

test('the wasm core plans and executes a write with the image-embedded loader', async (t) => {
  const api = await bootCore();
  const demo = await demoImage(api);

  await t.test('the demo image is a real RKFW the planner accepts', async () => {
    const res = JSON.parse(await api.ufPlan(demo.source, { eraseTail: 'none' }));
    assert.equal(res.ok, true, res.error);
    assert.equal(res.image.name, demo.name);
    assert.equal(res.facts.chip, 'RK3588');
    assert.equal(res.facts.hasLoader, true);
    assert.equal(res.facts.loaderSource, 'image');
    assert.equal(res.facts.loaderChipCheck, 'ok');
    assert.equal(res.facts.loaderChip, 'RK3588');
    assert.equal(res.facts.loaderSha256.length, 64);
    assert.ok(res.facts.loaderLength > 0);
    assert.match(res.facts.loaderBuildTime, /^\d{4}-\d{2}-\d{2} /);
    assert.equal(res.facts.items, 4);
    assert.ok(res.facts.uncoveredBytes > 0, 'the demo must leave a tail to decide about');
    assert.equal(res.facts.uncoveredTails > 0, true);
    assert.match(res.facts.uncoveredPartitions, /state/);
  });

  await t.test('the plan comes from the parameter table, not from the caller', async () => {
    const res = JSON.parse(await api.ufPlan(demo.source, { eraseTail: 'none' }));
    const report = res.report;
    assert.ok(report.gpt, 'the report should carry the GPT projection');
    assert.equal(report.partitions.length, res.facts.partitions);
    const writeable = report.items.filter((it) => it.action === 'write');
    assert.equal(writeable.length, res.facts.items);
    for (const it of writeable) {
      assert.ok(it.targetLba >= 0x40, `${it.name} has no target LBA`);
      assert.equal(it.targetKnown, true);
    }
    // The explicit tail decision is recorded, with the same sentence the CLI
    // prints, so "the 4 GiB STATE tail" is never an implicit side effect.
    const fill = JSON.parse(await api.ufPlan(demo.source, { eraseTail: 'fill' }));
    assert.equal(fill.facts.eraseMode, 'fill');
    assert.match(fill.report.erase.detail, /0xFF/);
    assert.equal(fill.report.erase.tails, res.facts.uncoveredTails);
  });

  await t.test('ufRun drives db → gpt → wl → rd and streams progress', async () => {
    const events = [];
    const res = JSON.parse(
      await api.ufRun(
        demo.source,
        { mock: true, eraseTail: 'none', noReset: false, chunkSize: 1 << 20 },
        (kind, payload) => events.push({ kind, payload: JSON.parse(payload) }),
      ),
    );
    assert.equal(res.ok, true, res.error);
    assert.ok(res.run, 'no run report');
    assert.equal(res.run.ok, true);
    assert.equal(res.run.loader.chipCheck, 'ok');
    assert.equal(res.run.loader.source, 'image');
    assert.equal(res.run.loader.sha256, res.facts.loaderSha256);
    assert.equal(res.run.device.mode, 'LOADER');
    assert.equal(res.run.reset, true);
    assert.equal(res.run.items.length, 4);
    for (const item of res.run.items) {
      assert.equal(item.status, 'ok', `${item.name}: ${item.status}`);
    }
    const phases = res.run.phases.map((p) => p.phase);
    for (const phase of ['ul', 'gpt', 'wl', 'rd']) {
      assert.ok(phases.includes(phase), `phases ${phases.join(',')} missing ${phase}`);
    }
    assert.ok(events.some((e) => e.kind === 'event'), 'no log events reached the page');
    assert.ok(events.some((e) => e.kind === 'progress'), 'no progress reached the page');
    const last = events.filter((e) => e.kind === 'progress').at(-1);
    assert.ok(last.payload.percent > 0);
  });

  await t.test('without a bound bridge a real run refuses before opening anything', async () => {
    const res = JSON.parse(await api.ufRun(demo.source, { mock: false, eraseTail: 'none' }));
    assert.equal(res.ok, false);
    assert.equal(res.kind, 'device');
    assert.match(res.error, /USB 桥接/);
  });

  await t.test('a non-image fails as a plan error, not as a device error', async () => {
    const junk = new Uint8Array(4096).fill(0x41);
    const res = JSON.parse(await api.ufPlan(bytesSource('junk.bin', junk), {}));
    assert.equal(res.ok, false);
    assert.equal(res.kind, 'plan');
    assert.match(res.error, /解析镜像/);
  });
});
