// Pure rockusb framing and decoding for the M0 probe.
//
// This module has no DOM or WebUSB dependency so it can be unit-tested with
// `node --test` (see rockusb.test.mjs) against the same vectors as the Go
// package. probe.js owns the device I/O; everything here is pure data.

import {
  CBW_LENGTH,
  CBW_SIGNATURE,
  COMMANDS_BY_OP,
  CSW_LENGTH,
  CSW_SIGNATURE,
  ERROR_NAMES,
  MANUFACTURER_NAMES,
  ROCKCHIP_VENDOR_ID,
  STORAGE_NAMES,
} from './protocol.js';

export const SECTOR_SIZE = 512; // rkdeveloptool reports flash sizes in 512-byte sectors

export class ProbeError extends Error {
  constructor(code, detail) {
    super(`${ERROR_NAMES[code] ?? `ERR_UNKNOWN(${code})`}: ${detail}`);
    this.code = code;
    this.detail = detail;
  }
}

export function hex(bytes, limit = bytes.byteLength) {
  const view = bytes.subarray(0, Math.min(limit, bytes.byteLength));
  return Array.from(view, (b) => b.toString(16).padStart(2, '0')).join(' ');
}

export function bitIndices(bitmap, width) {
  const out = [];
  for (let i = 0; i < width; i += 1) {
    if ((bitmap & (1 << i)) !== 0) out.push(i);
  }
  return out;
}

export function makeTag() {
  const buf = new Uint32Array(1);
  crypto.getRandomValues(buf);
  return buf[0] >>> 0;
}

/**
 * Only a 31-byte CBW for a whitelisted read command may ever leave the host.
 * transferOut calls this before every bulk OUT; a violation is a programming
 * error, not a device error, so it throws a plain Error.
 */
export function assertReadOnlyOp(op, byteLength) {
  if (byteLength !== CBW_LENGTH) {
    throw new Error(`refusing to write ${byteLength} bytes: only a ${CBW_LENGTH}-byte CBW may be sent`);
  }
  if (!COMMANDS_BY_OP[op]) {
    throw new Error(`refusing opcode 0x${op.toString(16)}: not a read-only command`);
  }
}

export function encodeCBW(tag, op, transferLength) {
  const cbw = new Uint8Array(CBW_LENGTH);
  const view = new DataView(cbw.buffer);
  view.setUint32(0, CBW_SIGNATURE, true);
  view.setUint32(4, tag >>> 0, true);
  view.setUint32(8, transferLength >>> 0, true);
  cbw[12] = 0x80; // direction: device-to-host
  cbw[13] = 0; // LUN
  cbw[14] = 0x06; // cbwcb length
  cbw[15] = op; // cbwcb.ucOperCode
  return cbw;
}

export function decodeCSW(bytes) {
  if (bytes.byteLength < CSW_LENGTH) {
    throw new ProbeError(-4, `CSW needs ${CSW_LENGTH} bytes, got ${bytes.byteLength}`);
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const csw = {
    signature: view.getUint32(0, true),
    tag: view.getUint32(4, true),
    residue: view.getUint32(8, true),
    status: bytes[12],
  };
  if (csw.signature !== CSW_SIGNATURE) {
    throw new ProbeError(-5, `bad CSW signature 0x${csw.signature.toString(16)}`);
  }
  return csw;
}

export function decodeChipInfo(bytes) {
  const text = Array.from(bytes, (b) => (b >= 0x20 && b < 0x7f ? String.fromCharCode(b) : ''))
    .join('')
    .trim();
  return { text, rawHex: hex(bytes) };
}

export function decodeFlashInfo(bytes) {
  if (bytes.byteLength < 11) {
    throw new ProbeError(-4, `flash info needs 11 bytes, got ${bytes.byteLength}`);
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const flashSizeSectors = view.getUint32(0, true);
  const blockSizeSectors = view.getUint16(4, true);
  const pageSizeSectors = bytes[6];
  const manufacturerCode = bytes[9];
  const flashCS = bytes[10];
  return {
    flashSizeSectors,
    flashSizeBytes: flashSizeSectors * SECTOR_SIZE,
    blockSizeSectors,
    blockSizeBytes: blockSizeSectors * SECTOR_SIZE,
    pageSizeSectors,
    pageSizeBytes: pageSizeSectors * SECTOR_SIZE,
    eccBits: bytes[7],
    accessTime: bytes[8],
    manufacturerCode,
    manufacturer: manufacturerCode < MANUFACTURER_NAMES.length ? MANUFACTURER_NAMES[manufacturerCode] : 'UNKNOWN',
    flashCS,
    flashCSList: bitIndices(flashCS, 8),
    rawHex: hex(bytes),
  };
}

export function decodeCapability(bytes) {
  if (bytes.byteLength < 8) {
    throw new ProbeError(-4, `capability needs 8 bytes, got ${bytes.byteLength}`);
  }
  const flags = [];
  if (bytes[0] & 0x01) flags.push('Direct LBA');
  if (bytes[0] & 0x02) flags.push('Vendor Storage');
  if (bytes[0] & 0x04) flags.push('First 4M Access');
  if (bytes[0] & 0x08) flags.push('Read LBA');
  if (bytes[1] & 0x01) flags.push('New IDB');
  return {
    rawHex: hex(bytes),
    bytes: Array.from(bytes),
    directLBA: Boolean(bytes[0] & 0x01),
    vendorStorage: Boolean(bytes[0] & 0x02),
    first4MAccess: Boolean(bytes[0] & 0x04),
    readLBA: Boolean(bytes[0] & 0x08),
    newIDB: Boolean(bytes[1] & 0x01),
    flags,
  };
}

export function storageName(id) {
  return STORAGE_NAMES[id] ?? `UNKNOWN(${id})`;
}

export function decodeStorage(bytes) {
  if (bytes.byteLength < 4) {
    throw new ProbeError(-4, `storage list needs 4 bytes, got ${bytes.byteLength}`);
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const bitmap = view.getUint32(0, true);
  const available = bitIndices(bitmap, 32);
  return {
    rawHex: hex(bytes),
    bitmap,
    available,
    names: available.map(storageName),
    current: available.length > 0 ? available[0] : -1,
    currentName: available.length > 0 ? storageName(available[0]) : null,
  };
}

export function decodeCommandData(op, data) {
  switch (op) {
    case 0x1b:
      return decodeChipInfo(data);
    case 0x1a:
      return decodeFlashInfo(data);
    case 0xaa:
      return decodeCapability(data);
    case 0x2b:
      return decodeStorage(data);
    default:
      return { rawHex: hex(data) };
  }
}

export function packBCDUSB(major = 0, minor = 0, subminor = 0) {
  return ((major << 8) | (minor << 4) | subminor) >>> 0;
}

// RKScan.cpp: for VID 0x2207, bit 0 of bcdUSB is clear in MASKROM and set in LOADER.
export function modeFromDescriptor(vendorId, bcdUSB) {
  if (vendorId !== ROCKCHIP_VENDOR_ID) return 'UNKNOWN';
  return bcdUSB & 1 ? 'LOADER' : 'MASKROM';
}
