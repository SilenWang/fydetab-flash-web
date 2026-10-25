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
//     that mentions it has to say "未做完整校验" — no pretending.
//
// Keeping this pure is what lets the page, its tests and the CLI agree on the
// wording of a refusal.

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
export function evaluateVerification(result, { formatBytes = (n) => `${n} B` } = {}) {
  if (!result) {
    return {
      level: VERIFY_NONE,
      status: 'none',
      full: false,
      allowsWrite: false,
      checks: [check('image', '镜像已校验', 'pending', '还没有校验：选好镜像后页面会自动校验。')],
      summary: '尚未校验。',
      phases: [],
    };
  }

  const full = Boolean(result.full);
  const report = result.report ?? null;
  const checks = [];

  if (!result.ok || !report) {
    checks.push(check('structure', '镜像结构', 'fail', `解析失败：${result.error ?? '未知错误'}`));
    return {
      level: full ? VERIFY_FULL : VERIFY_QUICK,
      status: VERIFY_FAILED,
      full,
      allowsWrite: false,
      checks,
      summary: `校验失败：${result.error ?? '镜像无法解析'}`,
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
        '容器 RKFW + RKAF',
        'pass',
        `${container.chip || '?'} · ${container.version || '?'}${
          container.buildTime ? ` · built ${container.buildTime}` : ''
        } · loader ${formatBytes(container.loaderLength ?? 0)} · archive ${formatBytes(container.rkafLength ?? 0)}${
          container.dataHole ? ` · 数据洞 ${formatBytes(container.dataHole)}` : ''
        }`,
      ),
    );
  } else {
    checks.push(
      check(
        'container',
        '容器 RKFW + RKAF',
        'warn',
        '裸 RKAF（没有 RKFW 包装）：镜像里没有 loader，maskrom 下无法 Download Boot；已在 LOADER 的设备仍可写。',
      ),
    );
  }

  // 2. Item table: every payload must fit inside the file.
  const items = Array.isArray(report.items) ? report.items : [];
  const outOfBounds = items.filter((item) => !itemInBounds(item, fileSize));
  const shifted = items.filter((item) => item.shifted).length;
  if (items.length === 0) {
    checks.push(check('items', 'item 表与数据边界', 'fail', 'item 表为空：这份镜像没有可写的分区。'));
  } else if (outOfBounds.length > 0) {
    checks.push(
      check(
        'items',
        'item 表与数据边界',
        'fail',
        `${outOfBounds.length}/${items.length} 项数据超出文件范围：${outOfBounds
          .slice(0, 3)
          .map((item) => item.name)
          .join('、')}${outOfBounds.length > 3 ? ' …' : ''}`,
      ),
    );
  } else {
    checks.push(
      check(
        'items',
        'item 表与数据边界',
        'pass',
        `${items.length} 项全部落在文件内${shifted > 0 ? `（${shifted} 项位于数据洞之后，已按解析出的偏移读取）` : ''}`,
      ),
    );
  }

  // 3. The payload audit: a hole the parser could not settle refuses the
  //    write in the write path too, so it must refuse here as well.
  const data = report.data ?? null;
  if (data && data.hole > 0 && data.decided === false) {
    checks.push(
      check('payload', 'payload 偏移可判定', 'fail', `镜像有 ${formatBytes(data.hole)} 数据洞，但没有签名能判定偏移：写入会被拒绝。`),
    );
  } else if (data && data.failed > 0) {
    checks.push(
      check(
        'payload',
        'payload 偏移可判定',
        'fail',
        `${data.failed}/${data.checked} 项的分区签名与预期不符：偏移可能读错了，写入会被拒绝。`,
      ),
    );
  } else if (data && data.checked > 0) {
    checks.push(check('payload', 'payload 偏移可判定', 'pass', `${data.checked} 项分区签名与预期一致。`));
  }

  // 4. Parameter table.
  const parameter = report.parameter ?? {};
  const partitions = Array.isArray(report.partitions) ? report.partitions : [];
  if ((parameter.partitionCount ?? 0) > 0) {
    checks.push(
      check(
        'parameter',
        'parameter 分区表',
        'pass',
        `${parameter.partitionCount} 个分区${parameter.wrapped ? '（PARM 包装）' : ''} · ${formatBytes(
          parameter.contentBytes ?? 0,
        )}`,
      ),
    );
  } else {
    checks.push(check('parameter', 'parameter 分区表', 'fail', 'parameter 里没有 mtdparts 分区，无法建立写入计划。'));
  }

  // 5. Uncovered tails are informational here: the explicit erase decision is
  //    what gates them, and that lives in preflight.js.
  const uncovered = Array.isArray(report.uncovered) ? report.uncovered : [];
  if (uncovered.length > 0) {
    const total = uncovered.reduce((sum, range) => sum + (range.bytes ?? 0), 0);
    checks.push(
      check(
        'uncovered',
        '未覆盖区间',
        'pass',
        `${uncovered.length} 段共 ${formatBytes(total)} 不会被镜像写入：${uncovered
          .slice(0, 3)
          .map((range) => `${range.partition} ${formatBytes(range.bytes)}`)
          .join('、')}${uncovered.length > 3 ? ' …' : ''}（处置在第 4 块显式选择）`,
      ),
    );
  } else {
    checks.push(check('uncovered', '未覆盖区间', 'pass', '没有未覆盖区间：镜像填满了它声明的每个分区。'));
  }

  // 6. PARM CRC (cheap, always run).
  if (result.parameterCrc) {
    const crc = result.parameterCrc;
    checks.push(
      check(
        'parm-crc',
        'PARM CRC',
        crc.ok ? 'pass' : 'fail',
        crc.ok
          ? `stored 0x${crc.stored.toString(16)} = computed`
          : `stored 0x${crc.stored.toString(16)} ≠ computed 0x${crc.computed.toString(16)}：parameter 分区被改过。`,
      ),
    );
  } else {
    checks.push(check('parm-crc', 'PARM CRC', 'warn', 'parameter 没有 CRC 尾（非官方打包或旧格式），这一项无法校验。'));
  }

  // 7. RKAF CRC (full scan only).
  if (!full) {
    checks.push(check('rkaf-crc', 'RKAF CRC', 'warn', '未做：快速校验不扫 RKAF 体。'));
  } else if (result.rkafCrc) {
    const crc = result.rkafCrc;
    if (crc.ok) {
      checks.push(check('rkaf-crc', 'RKAF CRC', 'pass', `stored 0x${crc.stored.toString(16)} = computed`));
    } else if (crc.stored === 0) {
      checks.push(
        check(
          'rkaf-crc',
          'RKAF CRC',
          'warn',
          '尾部是 00000000（按既定规则视为“未填写”）：不算损坏，但这一项没有可比对的值；整镜像 MD5 与 PARM CRC 仍然照常校验。',
        ),
      );
    } else {
      checks.push(
        check(
          'rkaf-crc',
          'RKAF CRC',
          'fail',
          `stored 0x${crc.stored.toString(16)} ≠ computed 0x${crc.computed.toString(16)}：归档体被改过。`,
        ),
      );
    }
  }

  // 8. Whole-image MD5 (full scan only).
  if (!full) {
    checks.push(check('md5', '整镜像 MD5', 'warn', '未做：快速校验不扫全文件。'));
  } else if (!result.md5) {
    checks.push(check('md5', '整镜像 MD5', 'warn', '镜像没有 MD5 记录，这一项无法校验。'));
  } else if (!result.md5.present) {
    checks.push(check('md5', '整镜像 MD5', 'warn', '裸 RKAF 没有 MD5 尾，这一项无法校验。'));
  } else {
    checks.push(
      check(
        'md5',
        '整镜像 MD5',
        result.md5.ok ? 'pass' : 'fail',
        result.md5.ok
          ? `${result.md5.computed}（@0x${(result.md5.offset ?? 0).toString(16)}）`
          : `expected ${result.md5.expected} ≠ computed ${result.md5.computed}`,
      ),
    );
  }

  // 9. Loader identity: maskrom needs the image's own loader for db.
  const loader = result.loader ?? null;
  if (!loader) {
    checks.push(check('loader', '镜像自带 loader', 'warn', '镜像里没有 RKFW loader：maskrom 下无法 Download Boot。'));
  } else if (loader.chipCheck === 'mismatch') {
    checks.push(
      check('loader', '镜像自带 loader', 'fail', `loader 是 ${loader.chip || '?'} 的，镜像是 ${loader.imageChip || '?'} 的：BootROM 会在 index 0 拒收。`),
    );
  } else {
    checks.push(
      check(
        'loader',
        '镜像自带 loader',
        'pass',
        `${loader.chip || '?'} · ${formatBytes(loader.length ?? 0)} · sha256 ${String(loader.sha256 ?? '').slice(0, 12)}…` +
          `${loader.buildTime ? ` · built ${loader.buildTime}` : ''}`,
      ),
    );
  }

  const failed = checks.filter((entry) => entry.status === 'fail');
  const warned = checks.filter((entry) => entry.status === 'warn');
  const status = failed.length > 0 ? VERIFY_FAILED : warned.length > 0 ? 'warn' : 'passed';
  const level = full ? VERIFY_FULL : VERIFY_QUICK;
  const allowsWrite = failed.length === 0;

  const summaryParts = [
    full ? '完整校验' : '快速校验',
    `${checks.filter((entry) => entry.status === 'pass').length}/${checks.length} 项通过`,
  ];
  if (failed.length > 0) summaryParts.push(`${failed.length} 项失败（写入已禁用）`);
  if (warned.length > 0) summaryParts.push(`${warned.length} 项警告`);
  if (!full) summaryParts.push('未做完整校验（整镜像 MD5 / RKAF CRC 未运行）');

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
export function verificationLines(verification) {
  if (!verification) return [];
  const mark = { pass: '✓', warn: '!', fail: '✗', pending: '…' };
  const lines = verification.checks.map(
    (entry) => `${mark[entry.status] ?? '·'} [${entry.id}] ${entry.label} — ${entry.detail}`,
  );
  lines.push(`校验结论：${verification.summary}`);
  return lines;
}
