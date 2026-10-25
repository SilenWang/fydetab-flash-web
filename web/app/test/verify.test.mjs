// The page's verification step, exercised through the real Go/WASM core.
//
// These tests stand in for exactly one thing a browser adds: the image object
// {name, size, read(offset, length)}. Everything the verdict is made of —
// container parse, item table, partition table, PARM CRC, RKAF CRC and the
// whole-file MD5 — is the same code the CLI's dry-run runs, so a mismatch here
// is a real mismatch.
//
// What they pin:
//   - a full verify reports the structure (items, partitions, uncovered tails)
//     and passes both full-file checksums on a good image;
//   - a quick verify reports the structure + PARM CRC and *says* the full
//     checksums were not run, instead of pretending they passed;
//   - a corrupted payload is reported as an MD5/RKAF CRC mismatch (ok:false on
//     that check) and never as a parse crash;
//   - progress events are emitted for the phase that reads the file, with the
//     byte totals the page shows.

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

test('ufVerify reports structure and both full scans on a good image', async (t) => {
  const api = await bootCore();
  const demo = await demoImage(api);

  await t.test('a full verify passes the RKAF CRC and the trailing MD5', async () => {
    const events = [];
    const res = JSON.parse(
      await api.ufVerify(demo.source, { full: true }, (kind, body) => events.push([kind, JSON.parse(body)])),
    );
    assert.equal(res.ok, true, res.error);
    assert.equal(res.full, true);
    assert.equal(res.image.name, demo.name);

    // Structure: the same report the CLI's dry-run prints.
    assert.equal(res.report.container.hasRkfw, true);
    assert.equal(res.report.container.chip, 'RK3588');
    assert.ok(res.report.items.length > 0, 'the item table must be reported');
    assert.ok(res.report.partitions.length > 0, 'the parameter partitions must be reported');
    assert.ok(Array.isArray(res.report.uncovered), 'uncovered tails must be an explicit list');

    // Checksums.
    assert.equal(res.rkafCrc.ok, true, `RKAF CRC stored 0x${res.rkafCrc.stored.toString(16)}`);
    assert.equal(res.md5.present, true);
    assert.equal(res.md5.ok, true);
    assert.equal(res.md5.expected, res.md5.computed);
    assert.match(res.md5.computed, /^[0-9a-f]{32}$/);

    // The loader the write would use is identified here too.
    assert.equal(res.loader.chip, 'RK3588');

    // Progress: the MD5 phase runs to completion and reports its total.
    const md5Events = events.filter(([kind, payload]) => kind === 'verify-progress' && payload.phase === 'md5');
    assert.ok(md5Events.length >= 2, 'the MD5 phase must stream progress');
    const last = md5Events.at(-1)[1];
    assert.equal(last.doneBytes, res.image.size);
    assert.equal(last.totalBytes, res.image.size);
    assert.equal(last.percent, 100);

    // Every phase is timed, so the page can show "实测 X s".
    const phases = res.phases.map((p) => p.phase);
    for (const phase of ['structure', 'parameter', 'rkaf-crc', 'md5']) {
      assert.ok(phases.includes(phase), `phase ${phase} missing from ${phases.join(',')}`);
    }
  });

  await t.test('a quick verify says the full scans were not run', async () => {
    const res = JSON.parse(await api.ufVerify(demo.source, { full: false }));
    assert.equal(res.ok, true, res.error);
    assert.equal(res.full, false);
    assert.ok(res.report.items.length > 0);
    assert.equal(res.md5, undefined, 'a quick verify must not claim an MD5 result');
    assert.equal(res.rkafCrc, undefined, 'a quick verify must not claim an RKAF CRC result');
    assert.equal(res.parameterCrc ?? null, null, 'the demo fixture has no PARM CRC trailer');
  });

  await t.test('the throughput sample measures this machine and this image', async () => {
    const res = JSON.parse(await api.ufVerify(demo.source, { full: false, sampleMiB: 1 }));
    assert.equal(res.ok, true, res.error);
    assert.ok(res.sample, 'sampleMiB must produce a sample');
    assert.ok(res.sample.bytes > 0);
    assert.ok(res.sample.bytes <= demo.bytes.byteLength);
    assert.ok(res.sample.mibPerSec > 0, 'a measured rate must be positive');
  });

  await t.test('a corrupted payload is a mismatch, not a crash', async () => {
    const broken = new Uint8Array(demo.bytes);
    // Flip a byte inside the hashed range but outside the headers, so the
    // container still parses and only the checksums can notice.
    broken[Math.floor(broken.length / 2)] ^= 0xff;
    const res = JSON.parse(await api.ufVerify(bytesSource('corrupt.img', broken), { full: true }));
    assert.equal(res.ok, true, 'the container still parses');
    assert.equal(res.md5.present, true);
    assert.equal(res.md5.ok, false, 'a flipped payload byte must fail the trailing MD5');
    assert.notEqual(res.md5.computed, res.md5.expected);
  });
});
