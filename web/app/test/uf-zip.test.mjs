// The zip path through the real Go/WASM core.
//
// The acceptance criterion for M4 is that planning an official .zip produces
// the same plan as the .img inside it. The zip reader is a byte source, so the
// honest way to test that is not to compare JSON by eye: boot the actual core,
// plan the same image twice — once from the bytes, once through the zip
// entry — and require the two reports to be identical.
//
// Also pinned here: the plan does not decompress the whole package (the point
// of streaming), the loader extracted through the zip has the same sha256, and
// a mock write through the zip source reaches the same result as one from the
// raw image.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

import '../wasm/wasm_exec.js'; // sets globalThis.Go (version-matched to the wasm)
import { DEFLATE, STORE, writeZip } from '../../../tools/zip-writer.mjs';
import { openZipImage } from '../zip-source.js';

const WASM_URL = new URL('../wasm/rk3588.wasm', import.meta.url);

async function bootCore() {
  if (!globalThis.__rk3588Api) {
    const go = new globalThis.Go();
    const bytes = await readFile(WASM_URL);
    const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
    go.run(instance); // the Go program parks forever; this promise never settles
    const started = Date.now();
    while (!globalThis.rk3588) {
      if (Date.now() - started > 5000) throw new Error('rk3588 wasm API never appeared');
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    globalThis.__rk3588Api = globalThis.rk3588;
  }
  return globalThis.__rk3588Api;
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
  return { name: payload.name, bytes };
}

/** packageOf wraps the image the way a vendor download looks: decoys + ZIP64. */
function packageOf(bytes) {
  return new Blob([
    writeZip(
      [
        { name: 'README.txt', data: 'Synthetic demo package.', method: STORE },
        { name: 'MiniLoaderAll.bin', data: new Uint8Array(1024).fill(7), method: DEFLATE },
        { name: 'Image/update.img', data: bytes, method: DEFLATE },
      ],
      { forceZip64: true, comment: 'fydetab-flash-web test package' },
    ),
  ]);
}

/**
 * planIdentity drops the fields that name the *source* rather than describe the
 * plan: the display name of the image and the loader's derived label. Every
 * measurement, target, tail, warning and hash stays in the comparison.
 */
function planIdentity(result) {
  const report = result.report ? { ...result.report, image: undefined } : result.report;
  if (report?.loader) report.loader = { ...report.loader, name: undefined };
  const loader = result.loader ? { ...result.loader, name: undefined } : result.loader;
  return JSON.stringify({ ok: result.ok, report, facts: result.facts, loader, error: result.error ?? null });
}

test('a zip package plans exactly like the .img it contains', async (t) => {
  const api = await bootCore();
  const demo = await demoImage(api);
  const opened = await openZipImage(packageOf(demo.bytes), 'demo-update.zip');

  await t.test('the zip entry is the same image, byte for byte', async () => {
    assert.equal(opened.source.size, demo.bytes.length);
    const first = await opened.source.read(0, 0x66);
    assert.equal(new TextDecoder().decode(first.subarray(0, 4)), 'RKFW');
    for (let offset = 0; offset < demo.bytes.length; offset += 64 * 1024) {
      const length = Math.min(64 * 1024, demo.bytes.length - offset);
      const chunk = await opened.source.read(offset, length);
      assert.deepEqual(Buffer.from(chunk), Buffer.from(demo.bytes.subarray(offset, offset + length)), `offset ${offset}`);
    }
  });

  await t.test('ufPlan(report, facts, loader) is identical for .img and .zip', async () => {
    const fromImage = JSON.parse(await api.ufPlan(bytesSource(demo.name, demo.bytes), { eraseTail: 'none' }));
    const fromZip = JSON.parse(await api.ufPlan(opened.source, { eraseTail: 'none' }));
    assert.equal(fromImage.ok, true, fromImage.error);
    assert.equal(fromZip.ok, true, fromZip.error);
    assert.equal(planIdentity(fromZip), planIdentity(fromImage), 'the zip plan differs from the .img plan');

    // The fields that do differ are display names, and they have to say where
    // the bytes came from.
    assert.equal(fromImage.image.name, demo.name);
    assert.equal(fromZip.image.name, 'demo-update.zip → Image/update.img');
    assert.equal(fromZip.report.image, 'demo-update.zip → Image/update.img');
    assert.match(fromZip.loader.name, /update\.img @0x66$/);
    assert.equal(fromZip.facts.loaderSha256, fromImage.facts.loaderSha256);
    assert.equal(fromZip.report.totalBytes, fromImage.report.totalBytes);
  });

  await t.test('the same erase decision produces the same plan from either source', async () => {
    for (const eraseTail of ['none', 'fill', 'erase-lba']) {
      const a = JSON.parse(await api.ufPlan(bytesSource(demo.name, demo.bytes), { eraseTail }));
      const b = JSON.parse(await api.ufPlan(opened.source, { eraseTail }));
      assert.equal(planIdentity(b), planIdentity(a), `eraseTail=${eraseTail} differs`);
    }
  });

  await t.test('planning a package does not decompress the whole image', async () => {
    const source = (await openZipImage(packageOf(demo.bytes), 'demo-update.zip')).source;
    const result = JSON.parse(await api.ufPlan(source, { eraseTail: 'none' }));
    assert.equal(result.ok, true, result.error);
    const stats = source.stats;
    assert.equal(stats.uncompressedBytes, demo.bytes.length);
    assert.ok(stats.decodedBytes > 0);
    assert.ok(
      stats.decodedBytes < demo.bytes.length / 2,
      `planning decoded ${stats.decodedBytes} of ${demo.bytes.length} bytes; the point is to read only what is needed`,
    );
    assert.ok(stats.peakBufferedBytes <= 8 * 1024 * 1024 + 1024 * 1024, 'the window must stay bounded');
  });

  await t.test('imageLoader through the zip matches the .img loader', async () => {
    const fromImage = JSON.parse(await api.imageLoader(bytesSource(demo.name, demo.bytes)));
    const fromZip = JSON.parse(await api.imageLoader((await openZipImage(packageOf(demo.bytes), 'demo.zip')).source));
    assert.ok(fromImage.bytes.length > 0);
    assert.equal(fromZip.sha256, fromImage.sha256);
    assert.equal(fromZip.length, fromImage.length);
    assert.equal(fromZip.bytes, fromImage.bytes, 'the loader bytes must be identical');
  });

  await t.test('a mock write through the zip reaches the same result', async () => {
    const opts = { mock: true, eraseTail: 'none', chunkSize: 1 << 20 };
    const fromImage = JSON.parse(await api.ufRun(bytesSource(demo.name, demo.bytes), opts));
    const fromZip = JSON.parse(await api.ufRun((await openZipImage(packageOf(demo.bytes), 'demo.zip')).source, opts));
    assert.equal(fromZip.ok, true, fromZip.error);
    assert.equal(fromImage.ok, true, fromImage.error);
    assert.equal(fromZip.run.ok, true);
    assert.deepEqual(
      fromZip.run.items.map((it) => [it.name, it.targetLba, it.size, it.status]),
      fromImage.run.items.map((it) => [it.name, it.targetLba, it.size, it.status]),
    );
    assert.equal(fromZip.run.bytesWritten, fromImage.run.bytesWritten);
    assert.equal(fromZip.run.loader.sha256, fromImage.run.loader.sha256);
  });
});
