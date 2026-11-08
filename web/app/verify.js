// The verification verdict.
//
// cmd/wasm's ufVerify reports measurements; this module decides what they mean
// for the write gate, and nothing here loosens an existing rule:
//
//   - a structural problem, a PARM CRC mismatch, an RKAF CRC mismatch or an
//     MD5 mismatch blocks the write;
//   - a *zero* RKAF CRC trailer is the documented "not filled in" value
//     (docs/rkfw-rkaf-format.md, SIL-509): it is reported as a warning, never
//     as corruption, and the MD5 + PARM CRC checks still have to pass;
//   - a quick verdict (no full scan) is allowed to write, but every surface
//     that mentions it has to say "no full check" — no pretending.
//
// Keeping this pure is what lets the page, its tests and the CLI agree on the
// wording of a refusal.
//
// The wording itself lives in ./i18n.js. `evaluateVerification` and
// `verificationLines` take an optional `t` so a caller can pin a language.

import { t as translate } from './i18n.js';

export const VERIFY_NONE = 'none';
export const VERIFY_QUICK = 'quick-ok';
export const VERIFY_FULL = 'full-ok';
export const VERIFY_FAILED = 'failed';

/** FULL_VERIFY_BUDGET_SECONDS is the issue's 2-minute rule. */
export const FULL_VERIFY_BUDGET_SECONDS = 120;

const MiB = 1024 * 1024;

function check(id, label, status, detail) {
  return { id, label, status, detail };
}

function itemInBounds(item, fileSize) {
  if (!Number.isFinite(item.dataOffset) || !Number.isFinite(item.size)) return false;
  if (item.size <= 0) return item.dataOffset >= 0 && item.dataOffset <= fileSize;
  return item.dataOffset >= 0 && item.dataOffset + item.size <= fileSize;
}

/**
 * evaluateVerification turns one ufVerify payload into checks plus a verdict.
 *
 * input: the JSON ufVerify resolved to (report + checksum results), or null.
 * Returns {level, status, checks, allowsWrite, summary, full, phases}.
 */
export function evaluateVerification(result, { formatBytes = (n) => `${n} B`, t = translate } = {}) {
  if (!result) {
    return {
      level: VERIFY_NONE,
      status: 'none',
      full: false,
      allowsWrite: false,
      checks: [check('image', t('verify.check.image'), 'pending', t('verify.check.imagePending'))],
      summary: t('verify.none'),
      phases: [],
    };
  }

  const full = Boolean(result.full);
  const report = result.report ?? null;
  const checks = [];

  if (!result.ok || !report) {
    const error = result.error ?? t('verify.summary.unknownError');
    checks.push(check('structure', t('verify.structure.label'), 'fail', t('verify.structure.parseFailed', { error })));
    return {
      level: full ? VERIFY_FULL : VERIFY_QUICK,
      status: VERIFY_FAILED,
      full,
      allowsWrite: false,
      checks,
      summary: t('verify.failed', { error }),
      phases: result.phases ?? [],
    };
  }

  const container = report.container ?? {};
  const fileSize = report.fileSize ?? result.image?.size ?? 0;

  // 1. Container.
  if (container.hasRkfw) {
    checks.push(
      check(
        'container',
        t('verify.container.label'),
        'pass',
        t('verify.container.pass', {
          chip: container.chip || '?',
          version: container.version || '?',
          loader: formatBytes(container.loaderLength ?? 0),
          archive: formatBytes(container.rkafLength ?? 0),
        }) +
          (container.buildTime ? t('verify.container.buildSuffix', { time: container.buildTime }) : '') +
          (container.dataHole ? t('verify.container.holeSuffix', { size: formatBytes(container.dataHole) }) : ''),
      ),
    );
  } else {
    checks.push(check('container', t('verify.container.label'), 'warn', t('verify.container.warn')));
  }

  // 2. Item table: every payload must fit inside the file.
  const items = Array.isArray(report.items) ? report.items : [];
  const outOfBounds = items.filter((item) => !itemInBounds(item, fileSize));
  const shifted = items.filter((item) => item.shifted).length;
  if (items.length === 0) {
    checks.push(check('items', t('verify.items.label'), 'fail', t('verify.items.empty')));
  } else if (outOfBounds.length > 0) {
    const names = outOfBounds
      .slice(0, 3)
      .map((item) => item.name)
      .join(t('verify.listSeparator'));
    checks.push(
      check(
        'items',
        t('verify.items.label'),
        'fail',
        t('verify.items.outOfBounds', {
          bad: outOfBounds.length,
          total: items.length,
          names,
          more: outOfBounds.length > 3 ? t('verify.items.more') : '',
        }),
      ),
    );
  } else {
    checks.push(
      check(
        'items',
        t('verify.items.label'),
        'pass',
        t('verify.items.pass', { count: items.length }) +
          (shifted > 0 ? t('verify.items.shiftedSuffix', { count: shifted }) : ''),
      ),
    );
  }

  // 3. The payload audit: a hole the parser could not settle refuses the
  //    write in the write path too, so it must refuse here as well.
  const data = report.data ?? null;
  if (data && data.hole > 0 && data.decided === false) {
    checks.push(
      check(
        'payload',
        t('verify.payload.label'),
        'fail',
        t('verify.payload.undecided', { hole: formatBytes(data.hole) }),
      ),
    );
  } else if (data && data.failed > 0) {
    checks.push(
      check(
        'payload',
        t('verify.payload.label'),
        'fail',
        t('verify.payload.signature', { failed: data.failed, checked: data.checked }),
      ),
    );
  } else if (data && data.checked > 0) {
    checks.push(check('payload', t('verify.payload.label'), 'pass', t('verify.payload.pass', { count: data.checked })));
  }

  // 4. Parameter table.
  const parameter = report.parameter ?? {};
  const partitions = Array.isArray(report.partitions) ? report.partitions : [];
  if ((parameter.partitionCount ?? 0) > 0) {
    checks.push(
      check(
        'parameter',
        t('verify.parameter.label'),
        'pass',
        t('verify.parameter.pass', {
          count: parameter.partitionCount,
          wrapped: parameter.wrapped ? t('verify.parameter.wrapped') : '',
          bytes: formatBytes(parameter.contentBytes ?? 0),
        }),
      ),
    );
  } else {
    checks.push(check('parameter', t('verify.parameter.label'), 'fail', t('verify.parameter.empty')));
  }

  // 5. Uncovered tails are informational here: the explicit erase decision is
  //    what gates them, and that lives in preflight.js.
  const uncovered = Array.isArray(report.uncovered) ? report.uncovered : [];
  if (uncovered.length > 0) {
    const total = uncovered.reduce((sum, range) => sum + (range.bytes ?? 0), 0);
    const ranges = uncovered
      .slice(0, 3)
      .map((range) => `${range.partition} ${formatBytes(range.bytes)}`)
      .join(t('verify.listSeparator'));
    checks.push(
      check(
        'uncovered',
        t('verify.uncovered.label'),
        'pass',
        t('verify.uncovered.pass', {
          count: uncovered.length,
          total: formatBytes(total),
          ranges,
          more: uncovered.length > 3 ? t('verify.items.more') : '',
        }),
      ),
    );
  } else {
    checks.push(check('uncovered', t('verify.uncovered.label'), 'pass', t('verify.uncovered.none')));
  }

  // 6. PARM CRC (cheap, always run).
  if (result.parameterCrc) {
    const crc = result.parameterCrc;
    checks.push(
      check(
        'parm-crc',
        t('verify.parmCrc.label'),
        crc.ok ? 'pass' : 'fail',
        crc.ok
          ? t('verify.parmCrc.pass', { stored: crc.stored.toString(16) })
          : t('verify.parmCrc.fail', { stored: crc.stored.toString(16), computed: crc.computed.toString(16) }),
      ),
    );
  } else {
    checks.push(check('parm-crc', t('verify.parmCrc.label'), 'warn', t('verify.parmCrc.missing')));
  }

  // 7. RKAF CRC (full scan only).
  if (!full) {
    checks.push(check('rkaf-crc', t('verify.rkafCrc.label'), 'warn', t('verify.rkafCrc.skipped')));
  } else if (result.rkafCrc) {
    const crc = result.rkafCrc;
    if (crc.ok) {
      checks.push(check('rkaf-crc', t('verify.rkafCrc.label'), 'pass', t('verify.rkafCrc.pass', { stored: crc.stored.toString(16) })));
    } else if (crc.stored === 0) {
      checks.push(check('rkaf-crc', t('verify.rkafCrc.label'), 'warn', t('verify.rkafCrc.zero')));
    } else {
      checks.push(
        check(
          'rkaf-crc',
          t('verify.rkafCrc.label'),
          'fail',
          t('verify.rkafCrc.fail', { stored: crc.stored.toString(16), computed: crc.computed.toString(16) }),
        ),
      );
    }
  }

  // 8. Whole-image MD5 (full scan only).
  if (!full) {
    checks.push(check('md5', t('verify.md5.label'), 'warn', t('verify.md5.skipped')));
  } else if (!result.md5) {
    checks.push(check('md5', t('verify.md5.label'), 'warn', t('verify.md5.missing')));
  } else if (!result.md5.present) {
    checks.push(check('md5', t('verify.md5.label'), 'warn', t('verify.md5.bare')));
  } else {
    checks.push(
      check(
        'md5',
        t('verify.md5.label'),
        result.md5.ok ? 'pass' : 'fail',
        result.md5.ok
          ? t('verify.md5.pass', {
              computed: result.md5.computed,
              offset: (result.md5.offset ?? 0).toString(16),
            })
          : t('verify.md5.fail', { expected: result.md5.expected, computed: result.md5.computed }),
      ),
    );
  }

  // 9. Loader identity: maskrom needs the image's own loader for db.
  const loader = result.loader ?? null;
  if (!loader) {
    checks.push(check('loader', t('verify.loader.label'), 'warn', t('verify.loader.missing')));
  } else if (loader.chipCheck === 'mismatch') {
    checks.push(
      check(
        'loader',
        t('verify.loader.label'),
        'fail',
        t('verify.loader.mismatch', { chip: loader.chip || '?', imageChip: loader.imageChip || '?' }),
      ),
    );
  } else {
    checks.push(
      check(
        'loader',
        t('verify.loader.label'),
        'pass',
        t('verify.loader.pass', {
          chip: loader.chip || '?',
          size: formatBytes(loader.length ?? 0),
          sha: String(loader.sha256 ?? '').slice(0, 12),
        }) + (loader.buildTime ? t('verify.container.buildSuffix', { time: loader.buildTime }) : ''),
      ),
    );
  }

  const failed = checks.filter((entry) => entry.status === 'fail');
  const warned = checks.filter((entry) => entry.status === 'warn');
  const status = failed.length > 0 ? VERIFY_FAILED : warned.length > 0 ? 'warn' : 'passed';
  const level = full ? VERIFY_FULL : VERIFY_QUICK;
  const allowsWrite = failed.length === 0;

  const summaryParts = [
    full ? t('verify.summary.full') : t('verify.summary.quick'),
    t('verify.summary.passed', {
      passed: checks.filter((entry) => entry.status === 'pass').length,
      total: checks.length,
    }),
  ];
  if (failed.length > 0) summaryParts.push(t('verify.summary.failed', { count: failed.length }));
  if (warned.length > 0) summaryParts.push(t('verify.summary.warned', { count: warned.length }));
  if (!full) summaryParts.push(t('verify.summary.noFull'));

  return {
    level,
    status,
    full,
    allowsWrite,
    checks,
    summary: summaryParts.join(' · '),
    phases: result.phases ?? [],
    failed: failed.map((entry) => entry.id),
    warnings: warned.map((entry) => entry.id),
  };
}

/**
 * estimateFullVerifySeconds projects the full scan from the measured sample
 * rate plus the two scan sizes, so the page can apply the 2-minute rule with
 * this user's number instead of a constant.
 */
export function estimateFullVerifySeconds(result, { overheadSeconds = 1.5 } = {}) {
  if (!result || !result.sample || !(result.sample.mibPerSec > 0)) return null;
  const report = result.report ?? {};
  const fileSize = report.fileSize ?? result.image?.size ?? 0;
  const rkafLength = report.container?.rkafLength ?? 0;
  const bytes = Math.max(0, fileSize) + Math.max(0, rkafLength);
  if (bytes === 0) return null;
  return bytes / MiB / result.sample.mibPerSec + overheadSeconds;
}

/** shouldDefaultToFull applies the issue's ≤2 分钟 rule to the projection. */
export function shouldDefaultToFull(seconds) {
  if (!Number.isFinite(seconds)) return true; // no measurement: do the honest thing
  return seconds <= FULL_VERIFY_BUDGET_SECONDS;
}

/** verificationLines renders the verdict as log lines (one per check). */
export function verificationLines(verification, { t = translate } = {}) {
  if (!verification) return [];
  const mark = { pass: '✓', warn: '!', fail: '✗', pending: '…' };
  // The log's check ids and messages are diagnostics: they stay in the page's
  // active language, and the id keeps them greppable in an exported log.
  const lines = verification.checks.map(
    (entry) => `${mark[entry.status] ?? '·'} [${entry.id}] ${entry.label} — ${entry.detail}`,
  );
  lines.push(t('verify.lines.conclusion', { summary: verification.summary }));
  return lines;
}
