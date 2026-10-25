// Pre-flight checklist for a real write.
//
// This module is deliberately pure: it takes what the page knows (devices,
// loader identity, plan facts, the explicit tail decision) and returns the
// checklist plus the button gates. Keeping it out of app.js is what makes the
// "no read-only command before db" rule testable in node without a browser.
//
// The rules come from the hardware findings this stage is built on:
//   - a bare maskrom STALLs every bulk CBW, so any read command sent before
//     Download Boot both fails and dirties the power-on cycle;
//   - the first command after power-on must therefore be `db`;
//   - a loader that is not the image's own is rejected by the BootROM at
//     index 0, which looks like a cable fault — so the loader must match the
//     image's chip before anything is sent;
//   - the STATE tail (5 GiB partition, ~1 GiB payload) is never erased
//     implicitly: the operator picks none/fill/erase-lba and confirms it.

export const ROCKCHIP_VENDOR_ID = 0x2207;
export const MASKROM_PRODUCT_ID = 0x350b;

export const CHECK_MASKROM = 'maskrom';
export const CHECK_LOADER = 'loader';
export const CHECK_PLAN = 'plan';
export const CHECK_FIRST_COMMAND = 'firstCommand';

/** currentDevice picks the DevNo the page has selected, or the only device. */
export function currentDevice(devices, current) {
  if (!Array.isArray(devices) || devices.length === 0) return null;
  if (current != null) {
    const found = devices.find((d) => d.devNo === current);
    if (found) return found;
  }
  return devices.length === 1 ? devices[0] : null;
}

/** deviceMode normalises the mode a listed device reports.
 *
 * The Go core puts both a numeric enum (mode: 1=maskrom, 2=loader) and the
 * `ld` label (modeLabel: "Maskrom" / "Loader") on each device. Older callers
 * (and the unit tests) pass a plain `mode` string, so accept either shape:
 * reading only `mode` made every real board look like "not maskrom, not
 * LOADER" and kept the write button disabled.
 */
export function deviceMode(device) {
  if (!device) return 'UNKNOWN';
  if (typeof device.mode === 'string' && device.mode) return device.mode.toUpperCase();
  if (typeof device.modeLabel === 'string' && device.modeLabel) return device.modeLabel.toUpperCase();
  switch (Number(device.mode)) {
    case 1:
      return 'MASKROM';
    case 2:
      return 'LOADER';
    default:
      return 'UNKNOWN';
  }
}

/** isMaskrom reports whether a listed device is a bare maskrom board. */
export function isMaskrom(device) {
  if (!device) return false;
  return deviceMode(device) === 'MASKROM' && ((device.bcdUSB ?? 0) & 1) === 0;
}

/** isLoader reports whether a listed device has a usbplug (LOADER mode). */
export function isLoader(device) {
  return deviceMode(device) === 'LOADER';
}

/**
 * readOnlyGate decides whether buttons that send a CBW (info, chip/flash
 * reads) may be pressed.
 *
 * In maskrom before Download Boot the answer is no, and it stays no: the point
 * is not that the read would fail (it does, with a STALL) but that the stray
 * CBW is what made the real board's next db attempt unreliable.
 */
export function readOnlyGate({ device, dbDone }) {
  if (!device) {
    return { enabled: false, reason: '还没有授权设备：先点“授权设备”，再执行 Download Boot。' };
  }
  if (isLoader(device)) {
    return { enabled: true, reason: '设备已在 LOADER：只读信息可以安全读取。' };
  }
  if (isMaskrom(device)) {
    if (dbDone) {
      return { enabled: false, reason: 'db 已发出但设备还没重新枚举为 LOADER：等它回来后 ld，再读只读信息。' };
    }
    return {
      enabled: false,
      reason: '设备在 maskrom：第一条命令必须是 db。现在发 CBW（读取设备信息）既会被 STALL，也会弄脏这次上电周期。',
    };
  }
  return { enabled: false, reason: '设备模式未知：先 ld 看清楚。' };
}

/**
 * evaluatePreflight returns the checklist, whether a write may start, and the
 * read-only gate.
 *
 * input:
 *   devices      listed devices (device.Info shape)
 *   current      selected DevNo
 *   loader       loader identity from ufPlan (facts-like) or null
 *   facts        ufPlan facts or null
 *   eraseMode    'none' | 'fill' | 'erase-lba'
 *   eraseConfirmed  the operator ticked the explicit tail decision
 *   dbDone       a Download Boot succeeded and the device is LOADER
 *   runInProgress
 *   hasImage
 */
export function evaluatePreflight(input = {}) {
  const device = currentDevice(input.devices, input.current);
  const facts = input.facts ?? null;
  const loader = input.loader ?? null;
  const eraseConfirmed = Boolean(input.eraseConfirmed);
  const runInProgress = Boolean(input.runInProgress);

  const items = [];

  // 1. Device in maskrom (or already bootstrapped into LOADER).
  if (!device) {
    items.push({
      id: CHECK_MASKROM,
      label: '设备在 maskrom（2207:350b，bcdUSB bit0=0）',
      status: 'pending',
      detail: '还没有已授权设备：点“授权设备”并在浏览器弹窗里选择 0x2207 设备。',
    });
  } else if (isLoader(device)) {
    items.push({
      id: CHECK_MASKROM,
      label: '设备在 maskrom（2207:350b，bcdUSB bit0=0）',
      status: 'pass',
      detail: `设备已重新枚举为 LOADER（Pid=0x${(device.productId ?? 0).toString(16)}），db 已经完成过，可以直接写。`,
    });
  } else if (isMaskrom(device)) {
    items.push({
      id: CHECK_MASKROM,
      label: '设备在 maskrom（2207:350b，bcdUSB bit0=0）',
      status: 'pass',
      detail: `DevNo=${device.devNo} Pid=0x${(device.productId ?? 0).toString(16)}，bcdUSB 0x${(device.bcdUSB ?? 0)
        .toString(16)
        .padStart(4, '0')}（bit0=0）。`,
    });
  } else {
    items.push({
      id: CHECK_MASKROM,
      label: '设备在 maskrom（2207:350b，bcdUSB bit0=0）',
      status: 'fail',
      detail: '设备不在 maskrom 也不在 LOADER：重新拔插电源进 maskrom，再点 ld。',
    });
  }

  // 2. Loader comes from the image and matches its chip.
  if (!loader) {
    items.push({
      id: CHECK_LOADER,
      label: 'loader 与镜像 chip 匹配（默认用镜像自带 loader）',
      status: input.hasImage ? 'pending' : 'pending',
      detail: input.hasImage ? '先点“生成写入计划”，页面会从镜像 RKFW 头切出 loader 并校验。' : '还没有选择镜像。',
    });
  } else if (loader.chipCheck === 'mismatch') {
    items.push({
      id: CHECK_LOADER,
      label: 'loader 与镜像 chip 匹配（默认用镜像自带 loader）',
      status: 'fail',
      detail: `loader 是为 ${loader.chip || '?'} 构建的，镜像是 ${loader.imageChip || '?'} 的：错配的 loader 会在 index 0 被 BootROM 拒收。`,
    });
  } else if (loader.chipCheck === 'ok') {
    items.push({
      id: CHECK_LOADER,
      label: 'loader 与镜像 chip 匹配（默认用镜像自带 loader）',
      status: 'pass',
      detail: `${loader.source === 'file' ? '用户指定（非镜像自带）' : '镜像自带'} · ${loader.chip} · ${formatBytes(
        loader.length,
      )} · sha256 ${String(loader.sha256 || '').slice(0, 12)}… · 构建 ${loader.buildTime || 'unknown'}`,
    });
  } else {
    items.push({
      id: CHECK_LOADER,
      label: 'loader 与镜像 chip 匹配（默认用镜像自带 loader）',
      status: 'warn',
      detail: `loader 或镜像没有 chip 字段，无法比对（loader ${loader.chip || '?'} / 镜像 ${
        loader.imageChip || '?'
      }）。确认这份镜像确实属于这台机器再继续。`,
    });
  }

  // 3. Write plan matches parameter, tail decision explicit.
  if (!facts) {
    items.push({
      id: CHECK_PLAN,
      label: '写入计划与 parameter 一致，未覆盖尾部已显式选择',
      status: 'pending',
      detail: '还没有计划：选择镜像后点“生成写入计划”。',
    });
  } else if (!(facts.items > 0)) {
    items.push({
      id: CHECK_PLAN,
      label: '写入计划与 parameter 一致，未覆盖尾部已显式选择',
      status: 'fail',
      detail: '计划里没有可写 item：确认镜像是完整的 update.img（RKFW + RKAF + parameter）。',
    });
  } else if (facts.uncoveredTails > 0 && !eraseConfirmed) {
    items.push({
      id: CHECK_PLAN,
      label: '写入计划与 parameter 一致，未覆盖尾部已显式选择',
      status: 'fail',
      detail: `计划 ${facts.items} 项、共 ${formatBytes(facts.totalBytes)}；${facts.uncoveredTails} 个分区尾部未覆盖（${formatBytes(
        facts.uncoveredBytes,
      )}，含 ${facts.uncoveredParts || facts.uncoveredPartitions || '?'}）——请在下面显式选择处置并打勾。`,
    });
  } else {
    items.push({
      id: CHECK_PLAN,
      label: '写入计划与 parameter 一致，未覆盖尾部已显式选择',
      status: 'pass',
      detail: `计划 ${facts.items} 项、共 ${formatBytes(facts.totalBytes)}；未覆盖 ${formatBytes(
        facts.uncoveredBytes,
      )}（${facts.uncoveredParts || facts.uncoveredPartitions || '无'}）→ ${facts.eraseMode}${
        facts.uncoveredTails > 0 ? '（已确认）' : '（没有未覆盖区间）'
      }。`,
    });
  }

  // 4. Hard rule: db is the first command after power-on.
  const loaderReady = loader && (loader.chipCheck === 'ok' || loader.chipCheck === 'unknown');
  if (isLoader(device)) {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: '上电后第一条命令是 db（在 db 前不发任何 CBW）',
      status: 'pass',
      detail: '设备已经是 LOADER，db 完成过；本次写入不会再发给 maskrom 的 CBW。',
    });
  } else if (isMaskrom(device) && loaderReady) {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: '上电后第一条命令是 db（在 db 前不发任何 CBW）',
      status: 'pass',
      detail: '写入流程的第一步就是 db；页面在 db 成功前禁用了“读取设备信息”等会发 CBW 的按钮。',
    });
  } else if (isMaskrom(device)) {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: '上电后第一条命令是 db（在 db 前不发任何 CBW）',
      status: 'fail',
      detail: '还没有可用的、与镜像匹配的 loader，db 无法作为第一条命令执行。',
    });
  } else {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: '上电后第一条命令是 db（在 db 前不发任何 CBW）',
      status: 'pending',
      detail: '设备状态未确定：授权设备后先 ld。',
    });
  }

  const blocking = items.filter((i) => i.status === 'fail');
  const pending = items.filter((i) => i.status === 'pending');
  const ready = blocking.length === 0 && pending.length === 0 && !runInProgress;

  return {
    items,
    ready,
    blocking: blocking.map((i) => i.id),
    readOnly: readOnlyGate({ device, dbDone: Boolean(input.dbDone) }),
    canWrite: ready && facts && facts.items > 0 && Boolean(loaderReady),
    canDb: Boolean(device) && isMaskrom(device) && Boolean(loaderReady) && !runInProgress,
  };
}

/** formatBytes renders binary units, matching the CLI's output. */
export function formatBytes(n) {
  if (!Number.isFinite(n) || n < 0) return '?';
  const unit = 1024;
  if (n < unit) return `${n} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let value = n / unit;
  let index = 0;
  while (value >= unit && index < units.length - 1) {
    value /= unit;
    index += 1;
  }
  return `${value.toFixed(value >= 10 ? 1 : 2)} ${units[index]}`;
}
