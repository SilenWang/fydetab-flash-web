// The device list behind block 3.
//
// Two different views of the same boards exist on this page and they must be
// merged, not confused:
//
//   - WebUSB knows every Rockchip device the user has authorised, with its
//     VID:PID, serial, bcdUSB and re-enumeration events. It is the only source
//     that survives `connect` / `disconnect`, and mode is read from bcdUSB
//     bit 0 exactly like the protocol does.
//   - The Go core knows what `ld` saw: the DevNo the shell selects with `cd`,
//     plus the mode it read through the device. That DevNo is what `db`/`uf`
//     act on, so a row without one is listed but not selectable for writing.
//
// Everything here is pure: app.js owns the actual navigator.usb subscriptions
// and passes the arrays in. That is what makes the re-enumeration rules
// (maskrom → LOADER keeps the serial, drops the authorisation) testable.

export const ROCKCHIP_VENDOR_ID = 0x2207;

// preflight.js owns the `ld` mode normalisation (numeric media mode + label);
// reusing it is what keeps a real Loader board from looking unknown here.
import { deviceMode } from './preflight.js';

/** modeFromBcdUsb follows the protocol's bit-0 convention. */
export function modeFromBcdUsb(bcdUSB) {
  if (typeof bcdUSB !== 'number' || !Number.isFinite(bcdUSB)) return 'UNKNOWN';
  return (bcdUSB & 1) === 1 ? 'LOADER' : 'MASKROM';
}

export function modeLabel(mode) {
  if (mode === 'LOADER') return 'Loader';
  if (mode === 'MASKROM') return 'Maskrom';
  return 'Unknown';
}

function hex4(value) {
  return typeof value === 'number' ? `0x${value.toString(16).padStart(4, '0')}` : '0x????';
}

/**
 * describeUsbDevice turns one USBDevice into a row. `key` is stable across
 * re-lists only while the device stays enumerated: WebUSB drops the
 * per-instance permission when a board re-enumerates, so after `db` the same
 * board comes back with a new key (and often a new serial-less entry).
 */
export function describeUsbDevice(device, index = 0) {
  const vendorId = device?.vendorId ?? 0;
  const productId = device?.productId ?? 0;
  const serialNumber = device?.serialNumber ?? '';
  const bcdUSB = typeof device?.deviceVersionMajor === 'number'
    ? device.deviceVersionMajor * 0x100 + (device.deviceVersionMinor ?? 0)
    : undefined;
  // WebUSB exposes neither bcdUSB directly; the page passes it in when it
  // knows it (from the descriptors the Go core read), otherwise undefined.
  const version = typeof device?.bcdUSB === 'number' ? device.bcdUSB : bcdUSB;
  const mode = modeFromBcdUsb(version);
  return {
    key: `${hex4(vendorId)}:${hex4(productId)}:${serialNumber || 'noserial'}#${index}`,
    index,
    source: 'usb',
    vendorId,
    productId,
    serialNumber,
    productName: device?.productName ?? '',
    bcdUSB: typeof version === 'number' ? version : null,
    mode,
    modeLabel: modeLabel(mode),
    // Both modes can be flashed: maskrom needs the image's loader first (db),
    // LOADER is ready. "Unknown" is what cannot be written.
    flashable: mode === 'LOADER' || mode === 'MASKROM',
    needsDownloadBoot: mode === 'MASKROM',
    devNo: null,
    selected: false,
  };
}

/** listUsbDevices lists every authorised 0x2207 device, never opening one. */
export async function listUsbDevices(usb = globalThis.navigator?.usb) {
  if (!usb || typeof usb.getDevices !== 'function') {
    return { ok: false, devices: [], reason: '这个浏览器没有 WebUSB（navigator.usb）：请用桌面版 Chrome / Edge。' };
  }
  try {
    const granted = await usb.getDevices();
    const devices = (granted ?? [])
      .filter((device) => device?.vendorId === ROCKCHIP_VENDOR_ID)
      .map((device, index) => describeUsbDevice(device, index));
    return { ok: true, devices, reason: devices.length === 0 ? '还没有已授权的 0x2207 设备。' : '' };
  } catch (err) {
    return { ok: false, devices: [], reason: `读取已授权设备失败：${err?.message ?? err}` };
  }
}

function sameHardware(a, b) {
  if (!a || !b) return false;
  if (a.vendorId !== b.vendorId || a.productId !== b.productId) return false;
  const serialA = (a.serialNumber ?? '').toLowerCase();
  const serialB = (b.serialNumber ?? '').toLowerCase();
  if (serialA && serialB) return serialA === serialB;
  return true; // no serial to tell them apart: same VID:PID is as close as it gets
}

/**
 * mergeShellState joins the WebUSB rows with the Go core's `ld` view.
 *
 * The shell's DevNo is copied onto the matching USB row so clicking a row can
 * run `cd <devNo>`. A shell device with no USB counterpart (rare: authorisation
 * revoked while `ld` still had it) is still listed, with source 'shell'.
 */
export function mergeShellState(usbDevices = [], shellState = null) {
  const shellDevices = Array.isArray(shellState?.devices) ? shellState.devices : [];
  const used = new Set();
  const merged = usbDevices.map((row) => {
    const matchIndex = shellDevices.findIndex((shell, i) => !used.has(i) && sameHardware(row, shell));
    if (matchIndex < 0) return { ...row, selected: shellState?.current == null ? row.index === 0 && usbDevices.length === 1 : false };
    used.add(matchIndex);
    const shell = shellDevices[matchIndex];
    // `ld` serialises the media mode as a number plus a label; preflight.js
    // owns that normalisation, so a numeric 2 becomes LOADER here too.
    const coreMode = deviceMode(shell);
    const mode = coreMode === 'UNKNOWN' ? row.mode : coreMode;
    return {
      ...row,
      source: 'usb+shell',
      devNo: shell.devNo ?? null,
      mode,
      modeLabel: shell.modeLabel ?? modeLabel(mode),
      bcdUSB: typeof shell.bcdUSB === 'number' ? shell.bcdUSB : row.bcdUSB,
      flashable: mode === 'LOADER' || mode === 'MASKROM',
      needsDownloadBoot: mode === 'MASKROM',
      selected: shellState?.current != null && shell.devNo === shellState.current,
    };
  });

  shellDevices.forEach((shell, i) => {
    if (used.has(i)) return;
    const coreMode = deviceMode(shell);
    const mode = coreMode === 'UNKNOWN' ? 'MASKROM' : coreMode;
    merged.push({
      key: `shell:${shell.devNo}`,
      index: merged.length,
      source: 'shell',
      vendorId: shell.vendorId ?? ROCKCHIP_VENDOR_ID,
      productId: shell.productId ?? 0,
      serialNumber: shell.serialNumber ?? '',
      productName: shell.productName ?? '',
      bcdUSB: typeof shell.bcdUSB === 'number' ? shell.bcdUSB : null,
      mode,
      modeLabel: shell.modeLabel ?? modeLabel(mode),
      flashable: true,
      needsDownloadBoot: mode === 'MASKROM',
      devNo: shell.devNo ?? null,
      selected: shellState?.current != null && shell.devNo === shellState.current,
    });
  });

  return merged;
}

/** selectedDevice picks the row the page acts on, or null. */
export function selectedDevice(devices = [], key = null) {
  if (!Array.isArray(devices) || devices.length === 0) return null;
  if (key != null) {
    const found = devices.find((device) => device.key === key);
    if (found) return found;
  }
  const byShell = devices.find((device) => device.selected);
  if (byShell) return byShell;
  return devices.length === 1 ? devices[0] : null;
}

/**
 * matchAfterReenumeration is what keeps the page from staying on a device that
 * no longer exists. After `db` the board re-enumerates: the WebUSB key changes
 * and the old authorisation is gone. Matching by serial picks the new instance
 * up again; with several identical boards and no serials it refuses to guess.
 */
export function matchAfterReenumeration(devices = [], previous = null) {
  if (!Array.isArray(devices) || devices.length === 0) return { device: null, reason: '设备已离开总线。' };
  if (!previous) return { device: devices.length === 1 ? devices[0] : null, reason: '' };
  const sameSerial = previous.serialNumber
    ? devices.find((device) => (device.serialNumber ?? '').toLowerCase() === previous.serialNumber.toLowerCase())
    : null;
  if (sameSerial) return { device: sameSerial, reason: '设备以新的实例重新枚举，已按序列号重新选中。' };
  if (devices.length === 1) return { device: devices[0], reason: '设备重新枚举后只剩一个候选，已自动选中。' };
  return { device: null, reason: '设备重新枚举后有多个候选：请重新选择，必要时重新授权。' };
}

/** deviceSummaryLines renders one row as the two lines the list shows. */
export function deviceSummaryLines(device, { formatBytes = () => '' } = {}) {
  if (!device) return [];
  const lines = [
    `${device.source === 'shell' ? '(仅 core 可见) ' : ''}${hex4(device.vendorId)}:${hex4(device.productId)} · ${device.modeLabel}${
      device.devNo != null ? ` · DevNo=${device.devNo}` : ''
    }`,
    `serial ${device.serialNumber || '（未提供）'} · bcdUSB ${device.bcdUSB == null ? '未知' : `0x${device.bcdUSB.toString(16).padStart(4, '0')}`}${
      device.needsDownloadBoot ? ' · 需先 Download Boot' : ''
    }`,
  ];
  if (!device.flashable) lines.push('模式未知：不能写入');
  return lines;
}
