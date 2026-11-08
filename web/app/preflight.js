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
//
// Every label and detail comes from ./i18n.js. `evaluatePreflight` accepts a
// `t` in its input so a caller (or a test) can pin a language explicitly; the
// default is the page's active locale.

import { t as translate } from './i18n.js';

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

/** CHECKSUM_IDS are the check ids whose failure means "the file is truncated". */
const CHECKSUM_IDS = ['md5', 'parm-crc', 'rkaf-crc'];

/**
 * imageFailureHint turns a verification verdict into the one sentence a user can
 * act on: a checksum mismatch means "download it again", anything else means
 * "this is not the file you want".
 *
 * The verdict's `failed` ids decide it; older callers that only carried a
 * summary string still get the checksum-worded hint.
 */
export function imageFailureHint(verification, { t = translate } = {}) {
  const failed = Array.isArray(verification?.failed) ? verification.failed : [];
  const byId = failed.length > 0 && failed.some((id) => CHECKSUM_IDS.includes(id));
  const summary = verification?.summary ?? '';
  // The prose fallback matches check names, never localised words: the summary
  // is copy now, so its wording changes with the language.
  const byProse = failed.length === 0 && /MD5|CRC|checksum/i.test(summary);
  return byId || byProse ? t('preflight.image.badChecksum') : t('preflight.image.notImage');
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
export function readOnlyGate({ device, dbDone, t = translate }) {
  if (!device) {
    return { enabled: false, reason: t('preflight.readonly.noDevice') };
  }
  if (isLoader(device)) {
    return { enabled: true, reason: t('preflight.readonly.loader') };
  }
  if (isMaskrom(device)) {
    if (dbDone) {
      return { enabled: false, reason: t('preflight.readonly.dbPending') };
    }
    return { enabled: false, reason: t('preflight.readonly.maskrom') };
  }
  return { enabled: false, reason: t('preflight.readonly.unknown') };
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
 *   t            message lookup (defaults to the active locale)
 */
export function evaluatePreflight(input = {}) {
  const t = input.t ?? translate;
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
      label: t('preflight.image.label'),
      status: 'pending',
      detail: t('preflight.image.pending'),
    });
  } else if (verification.status === 'fail' || verification.status === 'failed') {
    items.push({
      id: CHECK_IMAGE,
      label: t('preflight.image.label'),
      status: 'fail',
      // The raw parser error goes to the log; the page says what to do about it.
      detail: imageFailureHint(verification, { t }),
    });
  } else {
    items.push({
      id: CHECK_IMAGE,
      label: t('preflight.image.label'),
      status: 'pass',
      detail: verification.full
        ? t('preflight.image.passFull', {
            summary: verification.summary || t('preflight.image.passFullDefault'),
          })
        : t('preflight.image.passQuick', {
            summary: verification.summary || t('preflight.image.passQuickDefault'),
          }),
    });
  }

  // 1. Device in maskrom (or already bootstrapped into LOADER).
  if (!device) {
    items.push({
      id: CHECK_MASKROM,
      label: t('preflight.device.label'),
      status: 'pending',
      detail: t('preflight.device.pending'),
    });
  } else if (isLoader(device)) {
    items.push({
      id: CHECK_MASKROM,
      label: t('preflight.device.label'),
      status: 'pass',
      detail: t('preflight.device.loader', { pid: (device.productId ?? 0).toString(16) }),
    });
  } else if (isMaskrom(device)) {
    items.push({
      id: CHECK_MASKROM,
      label: t('preflight.device.label'),
      status: 'pass',
      detail: t('preflight.device.maskrom', {
        devNo: device.devNo,
        pid: (device.productId ?? 0).toString(16),
      }),
    });
  } else {
    items.push({
      id: CHECK_MASKROM,
      label: t('preflight.device.label'),
      status: 'fail',
      detail: t('preflight.device.unknown'),
    });
  }

  // 2. Loader comes from the image and matches its chip.
  if (!loader) {
    items.push({
      id: CHECK_LOADER,
      label: t('preflight.loader.label'),
      status: 'pending',
      detail: input.hasImage ? t('preflight.loader.pendingLoading') : t('preflight.loader.pendingNoImage'),
    });
  } else if (loader.chipCheck === 'mismatch') {
    items.push({
      id: CHECK_LOADER,
      label: t('preflight.loader.label'),
      status: 'fail',
      detail: t('preflight.loader.mismatch', {
        chip: loader.chip || '?',
        imageChip: loader.imageChip || '?',
      }),
    });
  } else if (loader.chipCheck === 'ok') {
    items.push({
      id: CHECK_LOADER,
      label: t('preflight.loader.label'),
      status: 'pass',
      detail: t('preflight.loader.pass', {
        source: loader.source === 'file' ? t('preflight.loader.sourceManual') : t('preflight.loader.sourceImage'),
        chip: loader.chip,
        size: formatBytes(loader.length),
      }),
    });
  } else {
    items.push({
      id: CHECK_LOADER,
      label: t('preflight.loader.label'),
      status: 'warn',
      detail: t('preflight.loader.unknown', {
        chip: loader.chip || '?',
        imageChip: loader.imageChip || '?',
      }),
    });
  }

  // 3. Write plan matches parameter, tail decision explicit.
  if (!facts) {
    items.push({
      id: CHECK_PLAN,
      label: t('preflight.plan.label'),
      status: 'pending',
      detail: t('preflight.plan.pending'),
    });
  } else if (!(facts.items > 0)) {
    items.push({
      id: CHECK_PLAN,
      label: t('preflight.plan.label'),
      status: 'fail',
      detail: t('preflight.plan.empty'),
    });
  } else if (facts.uncoveredTails > 0 && !eraseConfirmed) {
    items.push({
      id: CHECK_PLAN,
      label: t('preflight.plan.label'),
      status: 'fail',
      detail: t('preflight.plan.unconfirmed', {
        bytes: formatBytes(facts.uncoveredBytes),
        parts: facts.uncoveredPartitions ?? facts.uncoveredParts ?? '?',
      }),
    });
  } else {
    items.push({
      id: CHECK_PLAN,
      label: t('preflight.plan.label'),
      status: 'pass',
      detail: t('preflight.plan.pass', {
        count: facts.items,
        items: facts.items,
        total: formatBytes(facts.totalBytes),
        uncovered: formatBytes(facts.uncoveredBytes),
        parts: facts.uncoveredPartitions ?? facts.uncoveredParts ?? t('preflight.plan.noParts'),
        mode: facts.eraseMode,
      }),
    });
  }

  // 4. Hard rule: db is the first command after power-on.
  const loaderReady = loader && (loader.chipCheck === 'ok' || loader.chipCheck === 'unknown');
  if (isLoader(device)) {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: t('preflight.first.label'),
      status: 'pass',
      detail: t('preflight.first.loader'),
    });
  } else if (isMaskrom(device) && loaderReady) {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: t('preflight.first.label'),
      status: 'pass',
      detail: t('preflight.first.maskrom'),
    });
  } else if (isMaskrom(device)) {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: t('preflight.first.label'),
      status: 'fail',
      detail: t('preflight.first.noLoader'),
    });
  } else {
    items.push({
      id: CHECK_FIRST_COMMAND,
      label: t('preflight.first.label'),
      status: 'pending',
      detail: t('preflight.first.pending'),
    });
  }

  const blocking = items.filter((i) => i.status === 'fail');
  const pending = items.filter((i) => i.status === 'pending');
  const ready = blocking.length === 0 && pending.length === 0 && !runInProgress;

  return {
    items,
    ready,
    blocking: blocking.map((i) => i.id),
    readOnly: readOnlyGate({ device, dbDone: Boolean(input.dbDone), t }),
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
