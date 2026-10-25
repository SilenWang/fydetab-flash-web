// The verdict rules for the page's verification step.
//
// These decide whether the write button may unlock, so the interesting cases
// are the ones where a naive implementation would either block a good official
// image or wave a corrupt one through:
//   - a zero RKAF CRC trailer is "not filled in", not corruption;
//   - a quick verdict is allowed but must be labelled as not-full;
//   - an MD5 / PARM CRC mismatch blocks;
//   - the 2-minute budget is applied to a measured rate, not a constant.

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  FULL_VERIFY_BUDGET_SECONDS,
  VERIFY_FAILED,
  VERIFY_FULL,
  VERIFY_NONE,
  VERIFY_QUICK,
  estimateFullVerifySeconds,
  evaluateVerification,
  shouldDefaultToFull,
  verificationLines,
} from '../verify.js';

const MiB = 1024 * 1024;

function report(overrides = {}) {
  return {
    fileSize: 8 * 1024 * MiB,
    container: {
      hasRkfw: true,
      chip: 'RK3588',
      version: '23.0.1',
      buildTime: '2026-10-01 10:00:00 UTC',
      loaderLength: 253952,
      rkafLength: 4 * 1024 * MiB,
      dataHole: 0,
      itemCount: 10,
    },
    parameter: { partitionCount: 12, contentBytes: 590, wrapped: true },
    data: { hole: 0, decided: true, checked: 10, failed: 0 },
    items: [
      { name: 'parameter', dataOffset: 512, size: 590, shifted: false },
      { name: 'boot', dataOffset: 4096, size: 32 * MiB, shifted: false },
    ],
    partitions: new Array(12).fill({}),
    uncovered: [{ partition: 'resource', bytes: 3.64 * MiB }],
    warnings: [],
    ...overrides,
  };
}

function goodFull(overrides = {}) {
  return {
    ok: true,
    full: true,
    image: { name: 'fw.img', size: 8 * 1024 * MiB },
    report: report(),
    parameterCrc: { stored: 0x296d5b32, computed: 0x296d5b32, ok: true },
    rkafCrc: { stored: 0x296d5b32, computed: 0x296d5b32, ok: true },
    md5: {
      present: true,
      signed: false,
      offset: 8 * 1024 * MiB - 32,
      expected: 'a'.repeat(32),
      computed: 'a'.repeat(32),
      ok: true,
    },
    loader: { chip: 'RK3588', imageChip: 'RK3588', chipCheck: 'ok', length: 253952, sha256: 'b'.repeat(64), buildTime: '2026-10-01' },
    phases: [{ phase: 'structure', bytes: 4096, ms: 12 }],
    ...overrides,
  };
}

test('nothing to report yet: no verdict and no write', () => {
  const verdict = evaluateVerification(null);
  assert.equal(verdict.level, VERIFY_NONE);
  assert.equal(verdict.status, 'none');
  assert.equal(verdict.allowsWrite, false);
  assert.equal(verdict.checks[0].status, 'pending');
});

test('a good image passes every full check and may be written', () => {
  const verdict = evaluateVerification(goodFull());
  assert.equal(verdict.level, VERIFY_FULL);
  assert.equal(verdict.status, 'passed');
  assert.equal(verdict.allowsWrite, true);
  for (const id of ['container', 'items', 'payload', 'parameter', 'uncovered', 'parm-crc', 'rkaf-crc', 'md5', 'loader']) {
    const check = verdict.checks.find((entry) => entry.id === id);
    assert.ok(check, `missing check ${id}`);
    assert.equal(check.status, 'pass', `${id}: ${check.detail}`);
  }
  assert.match(verdict.summary, /完整校验/);
});

test('a corrupted payload blocks the write and names the mismatch', () => {
  const verdict = evaluateVerification(
    goodFull({
      md5: { present: true, expected: 'a'.repeat(32), computed: 'b'.repeat(32), ok: false, offset: 16 },
    }),
  );
  assert.equal(verdict.status, VERIFY_FAILED);
  assert.equal(verdict.allowsWrite, false);
  assert.deepEqual(verdict.failed, ['md5']);
  assert.match(verdict.checks.find((entry) => entry.id === 'md5').detail, /expected a{32} ≠ computed b{32}/);
});

test('a zero RKAF CRC trailer is "not filled in", never corruption', () => {
  const verdict = evaluateVerification(goodFull({ rkafCrc: { stored: 0, computed: 0x1234, ok: false } }));
  assert.equal(verdict.allowsWrite, true);
  assert.equal(verdict.status, 'warn');
  const crc = verdict.checks.find((entry) => entry.id === 'rkaf-crc');
  assert.equal(crc.status, 'warn');
  assert.match(crc.detail, /未填写/);
  assert.match(crc.detail, /不算损坏/);
});

test('a filled-in RKAF CRC mismatch still blocks', () => {
  const verdict = evaluateVerification(goodFull({ rkafCrc: { stored: 0x1111, computed: 0x2222, ok: false } }));
  assert.equal(verdict.allowsWrite, false);
  assert.deepEqual(verdict.failed, ['rkaf-crc']);
});

test('a PARM CRC mismatch blocks', () => {
  const verdict = evaluateVerification(goodFull({ parameterCrc: { stored: 1, computed: 2, ok: false } }));
  assert.equal(verdict.allowsWrite, false);
  assert.deepEqual(verdict.failed, ['parm-crc']);
});

test('a quick verdict writes but says the full scan did not run', () => {
  const verdict = evaluateVerification({
    ok: true,
    full: false,
    report: report(),
    parameterCrc: { stored: 1, computed: 1, ok: true },
    loader: { chip: 'RK3588', chipCheck: 'ok', length: 1, sha256: 'c'.repeat(64) },
  });
  assert.equal(verdict.level, VERIFY_QUICK);
  assert.equal(verdict.allowsWrite, true);
  assert.equal(verdict.checks.find((entry) => entry.id === 'md5').status, 'warn');
  assert.match(verdict.checks.find((entry) => entry.id === 'md5').detail, /未做/);
  assert.match(verdict.checks.find((entry) => entry.id === 'rkaf-crc').detail, /未做/);
  assert.match(verdict.summary, /未做完整校验/);
});

test('structural failures block before any checksum is considered', () => {
  const unparsable = evaluateVerification({ ok: false, full: true, error: '解析镜像: bad magic' });
  assert.equal(unparsable.status, VERIFY_FAILED);
  assert.equal(unparsable.allowsWrite, false);
  assert.match(unparsable.summary, /bad magic/);

  const outOfBounds = evaluateVerification(
    goodFull({
      report: report({ items: [{ name: 'STATE', dataOffset: 7 * 1024 * MiB, size: 4 * 1024 * MiB, shifted: false }] }),
    }),
  );
  const items = outOfBounds.checks.find((entry) => entry.id === 'items');
  assert.equal(items.status, 'fail');
  assert.match(items.detail, /超出文件范围/);
  assert.equal(outOfBounds.allowsWrite, false);

  const undecidedHole = evaluateVerification(
    goodFull({ report: report({ data: { hole: 4 * 1024 * MiB, decided: false, checked: 3, failed: 0 } }) }),
  );
  assert.equal(undecidedHole.checks.find((entry) => entry.id === 'payload').status, 'fail');
  assert.equal(undecidedHole.allowsWrite, false);
});

test('a bare RKAF without a loader is a warning for LOADER boards', () => {
  const verdict = evaluateVerification(
    goodFull({
      report: report({ container: { hasRkfw: false, rkafLength: 4 * 1024 * MiB, itemCount: 2 } }),
      loader: null,
      md5: null,
    }),
  );
  assert.equal(verdict.allowsWrite, true);
  assert.equal(verdict.checks.find((entry) => entry.id === 'container').status, 'warn');
  assert.match(verdict.checks.find((entry) => entry.id === 'container').detail, /Download Boot/);
  assert.equal(verdict.checks.find((entry) => entry.id === 'md5').status, 'warn');
  assert.equal(verdict.checks.find((entry) => entry.id === 'loader').status, 'warn');
});

test('the full-scan estimate uses the measured rate and the real sizes', () => {
  const quick = {
    ok: true,
    full: false,
    image: { size: 2048 * MiB },
    report: report({ fileSize: 2048 * MiB, container: { rkafLength: 1024 * MiB } }),
    sample: { bytes: 64 * MiB, ms: 91, mibPerSec: 704 },
  };
  const seconds = estimateFullVerifySeconds(quick, { overheadSeconds: 0 });
  // (2048 MiB + 1024 MiB) / 704 MiB/s ≈ 4.36 s
  assert.ok(seconds > 4.3 && seconds < 4.5, `estimate ${seconds}`);

  assert.equal(shouldDefaultToFull(seconds), true);
  assert.equal(shouldDefaultToFull(FULL_VERIFY_BUDGET_SECONDS), true);
  assert.equal(shouldDefaultToFull(FULL_VERIFY_BUDGET_SECONDS + 1), false);
  // No measurement: do the honest thing and scan.
  assert.equal(shouldDefaultToFull(NaN), true);
  assert.equal(estimateFullVerifySeconds({}), null);

  // A slow machine with a big image flips the default to quick.
  const slow = { ...quick, sample: { bytes: 64 * MiB, ms: 8000, mibPerSec: 8 } };
  assert.equal(shouldDefaultToFull(estimateFullVerifySeconds(slow)), false);
});

test('verificationLines render the verdict as log lines', () => {
  const lines = verificationLines(evaluateVerification(goodFull()));
  assert.ok(lines.some((line) => line.startsWith('✓ [md5]')));
  assert.ok(lines.some((line) => line.startsWith('校验结论：')));
  assert.deepEqual(verificationLines(null), []);
});
