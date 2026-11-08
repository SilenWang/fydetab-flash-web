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
export const CHECK_IMAGE = 'imageVerified';

/** currentDevice picks the DevNo the page has selected, or the only device. */
export function currentDevice(devices, current) {
  if (!Array.isArray(devices) || devices.length === 0) return null;
  if (current != null) {
    const found = devices.find((d) => d.devNo === current);
    if (found) return found;
  }
  return devices.length === 1 ? devices[0] : null;
}

/**
 * deviceMode normalises the mode a device reports.
 *
 * The real `ld` output carries a numeric media mode plus a label:
 *   { mode: 2, modeLabel: 'Loader', ... }
 * while the mock/tests use the string form. Ignoring the numeric form made
 * every pre-flight check fail on hardware (SIL-515): the write button stayed
 * grey because isLoader() never saw 'LOADER'.
 */
const MODE_NAMES = { 0: 'MASKROM', 1: 'MASKROM', 2: 'LOADER', 3: 'LOADER' };

export function deviceMode(device) {
  if (!device) return 'UNKNOWN';
  const mode = device.mode;
  if (typeof mode === 'number') {
    if (MODE_NAMES[mode]) return MODE_NAMES[mode];
    const label = String(device.modeLabel ?? '').toUpperCase();
    if (label.includes('LOADER')) return 'LOADER';
    if (label.includes('MASKROM')) return 'MASKROM';
    return 'UNKNOWN';
  }
  if (typeof mode === 'string' && mode) {
    const upper = mode.toUpperCase();
    if (upper === 'LOADER' || upper === 'MASKROM' || upper === 'UNKNOWN') return upper;
  }
  const label = String(device.modeLabel ?? '').toUpperCase();
  if (label.includes('LOADER')) return 'LOADER';
  if (label.includes('MASKROM')) return 'MASKROM';
  return 'UNKNOWN';
}

/**
 * imageFailureHint turns a verification verdict into the one sentence a user can
 * act on: a checksum mismatch means "download it again", anything else means
 * "this is not the file you want".
 */
export function imageFailureHint(verification) {
  const summary = verification?.summary ?? '';
  return /MD5|CRC|校验和/.test(summary)
    ? '文件不完整（校验和对不上），请重新下载或重新选择镜像。'
    : '这个文件不是可用的系统镜像，请选择官方下载的镜像。';
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
    return { enabled: false, reason: '还没有授权设备：先点“连接设备”，再执行 Download Boot。' };
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
 *   verification  the verdict from verify.js ({level, status, allowsWrite,
 *                 summary, full}); undefined/null means "not verified yet".
 *                 A quick verdict is allowed to write, but the item says so.
 */
export function evaluatePreflight(input = {}) {
  const device = currentDevice(input.devices, input.current);
  const facts = input.facts ?? null;
  const loader = input.loader ?? null;
  const eraseConfirmed = Boolean(input.eraseConfirmed);
  const runInProgress = Boolean(input.runInProgress);
  const verification = input.verification ?? null;

  const items = [];

  // 0. The image has been verified (M4b). This is an added condition, not a
  // replacement: a missing or failed verification keeps the write button off,
  // and a quick verdict passes while saying it did not scan the whole file.
  if (!verification || verification.status === 'none') {
    items.push({
      id: CHECK_IMAGE,
      label: '镜像已校验',
      status: 'pending',
      detail: '重新选一次镜像就会自动开始。',
    });
  } else if (verification.status === 'fail' || verification.status === 'failed') {
    items.push({
      id: CHECK_IMAGE,
      label: '镜像已校验',
      status: 'fail',
      // The raw parser error goes to the log; the page says what to do about it.
      detail: imageFailureHint(verification),
    });
  } else {
    items.push({
      id: CHECK_IMAGE,
      label: '镜像已校验',
      status: 'pass',
      detail: verification.full
        ? `完整校验通过：${verification.summary || '结构与校验和一致'}`
        : `快速校验通过：${verification.summary || '结构与 PARM CRC 通过'}——未做完整校验。`,
    });
  }

  // 1. Device in maskrom (or already bootstrapped into LOADER).
  if (!device) {
    items.push({
      id: CHECK_MASKROM,
      label: '设备已连接',
      status: 'pending',
      detail: '点「连接设备」，在弹出的窗口里选 Rockchip 设备。',
    });
  } else if (isLoader(device)) {
    items.push({
      id: CHECK_MASKROM,
      label: '设备已连接',
      status: 'pass',
      detail: `设备已进入 LOADER（Pid=0x${(device.productId ?? 0).toString(16)}），可以直接写。`,
    });
  } else if (isMaskrom(device)) {
    items.push({
      id: CHECK_MASKROM,
      label: '设备已连接',
      status: 'pass',
      detail: `maskrom · DevNo=${device.devNo} · Pid=0x${(device.productId ?? 0).toString(16)}。`,
    });
  } else {
    items.push({
      id: CHECK_MASKROM,
      label: '设备已连接',
      status: 'fail',
      detail: '设备模式未知：重新上电后刷新。',
    });
  }

  // 2. Loader comes from the image and matches its chip.
  if (!loader) {
    items.push({
      id: CHECK_LOADER,
      label: '引导程序与镜像匹配',
      status: 'pending',
      detail: input.hasImage ? '正在从镜像里取出引导程序。' : '先选一个镜像。',
    });
  } else if (loader.chipCheck === 'mismatch') {
    items.push({
      id: CHECK_LOADER,
      label: '引导程序与镜像匹配',
      status: 'fail',
      detail: `引导程序是 ${loader.chip || '?'} 的，镜像是 ${loader.imageChip || '?'} 的：错配的引导程序会在 index 0 被拒收。`,
    });
  } else if (loader.chipCheck === 'ok') {
    items.push({
      id: CHECK_LOADER,
      label: '引导程序与镜像匹配',
      status: 'pass',
      detail: `${loader.source === 'file' ? '手动指定' : '镜像自带'} · ${loader.chip} · ${formatBytes(loader.length)}`,
    });
  } else {
    items.push({
      id: CHECK_LOADER,
      label: '引导程序与镜像匹配',
      status: 'warn',
      detail: `镜像缺少芯片信息，无法比对（${loader.chip || '?'} / ${loader.imageChip || '?'}）：确认这是 Fydetab Duo 的镜像。`,
    });
  }

  // 3. Write plan matches parameter, tail decision explicit.
  if (!facts) {
    items.push({
      id: CHECK_PLAN,
      label: '写入内容已排好',
      status: 'pending',
      detail: '选择镜像后会自动排好。',
    });
  } else if (!(facts.items > 0)) {
    items.push({
      id: CHECK_PLAN,
      label: '写入内容已排好',
      status: 'fail',
      detail: '镜像里没有可写的内容：确认这是完整的整机镜像。',
    });
  } else if (facts.uncoveredTails > 0 && !eraseConfirmed) {
    items.push({
      id: CHECK_PLAN,
      label: '写入内容已排好',
      status: 'fail',
      detail: `还有 ${formatBytes(facts.uncoveredBytes)} 空间没被覆盖（${facts.uncoveredPartitions ?? facts.uncoveredParts ?? '?'}）：请显式选择处理方式并勾选确认。`,
    });
  } else {
    items.push({
      id: CHECK_PLAN,
      label: '写入内容已排好',
      status: 'pass',
      detail: `${facts.items} 项 · ${formatBytes(facts.totalBytes)} · 未覆盖 ${formatBytes(
        facts.uncoveredBytes,
      )}（${facts.uncoveredPartitions ?? facts.uncoveredParts ?? '无'}）→ ${facts.eraseMode}。`,
    });
  }

  // 4. Hard rule: db is the first command after power-on.
  const loaderReady = loader && (loader.chipCheck === 'ok' || loader.chipCheck === 'unknown');
  if (isLoader(device)) {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: '可直接进入刷写模式',
      status: 'pass',
      detail: '设备已经是 LOADER，本次写入不需要再发 Download Boot。',
    });
  } else if (isMaskrom(device) && loaderReady) {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: '可直接进入刷写模式',
      status: 'pass',
      detail: '写入的第一步就是 Download Boot。',
    });
  } else if (isMaskrom(device)) {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: '可直接进入刷写模式',
      status: 'fail',
      detail: '镜像里没有可用的引导程序，无法进入刷写模式。',
    });
  } else {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: '可直接进入刷写模式',
      status: 'pending',
      detail: '连接设备后会自动确认。',
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
    canWrite: ready && facts && facts.items > 0 && Boolean(loaderReady) && Boolean(verification?.allowsWrite),
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
