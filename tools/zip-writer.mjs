#!/usr/bin/env node
// Minimal zip writer for tests and for `pixi run sample`.
//
// The page only ever *reads* zips, so this exists to produce the fixtures its
// tests need: stored and deflated entries, an entry written the way streaming
// zip writers do (zeroes in the local header, real values in the central
// directory), a full-bleed ZIP64 archive with saturated EOCD fields, and a
// package with a decoy entry in front of the image.
//
// Only what the reader must cope with is implemented: no encryption, no
// multi-disk, no data descriptors on disk (only the local-header shape they
// leave behind), deflate via node:zlib.

import { deflateRawSync } from 'node:zlib';
import { writeFileSync } from 'node:fs';

const SIG_LOCAL = 0x04034b50;
const SIG_CENTRAL = 0x02014b50;
const SIG_EOCD = 0x06054b50;
const SIG_EOCD64 = 0x06064b50;
const SIG_EOCD64_LOCATOR = 0x07064b50;

export const STORE = 0;
export const DEFLATE = 8;

const CRC_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let i = 0; i < 256; i += 1) {
    let c = i;
    for (let bit = 0; bit < 8; bit += 1) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    table[i] = c >>> 0;
  }
  return table;
})();

export function crc32(bytes) {
  let crc = 0xffffffff;
  for (let i = 0; i < bytes.length; i += 1) crc = CRC_TABLE[(crc ^ bytes[i]) & 0xff] ^ (crc >>> 8);
  return (crc ^ 0xffffffff) >>> 0;
}

function dosDateTime(year = 2026, month = 1, day = 1, hour = 0, minute = 0, second = 0) {
  const date = ((Math.max(1980, year) - 1980) << 9) | (month << 5) | day;
  const time = (hour << 11) | (minute << 5) | Math.floor(second / 2);
  return { date, time };
}

class Writer {
  constructor() {
    this.parts = [];
    this.length = 0;
  }

  bytes(part) {
    this.parts.push(part);
    this.length += part.byteLength;
  }

  u16(value) {
    const b = new Uint8Array(2);
    new DataView(b.buffer).setUint16(0, value, true);
    this.bytes(b);
  }

  u32(value) {
    const b = new Uint8Array(4);
    new DataView(b.buffer).setUint32(0, value >>> 0, true);
    this.bytes(b);
  }

  u64(value) {
    const b = new Uint8Array(8);
    new DataView(b.buffer).setBigUint64(0, BigInt(value), true);
    this.bytes(b);
  }

  toUint8Array() {
    const out = new Uint8Array(this.length);
    let offset = 0;
    for (const part of this.parts) {
      out.set(part, offset);
      offset += part.byteLength;
    }
    return out;
  }
}

function zip64Extra(uncompressed, compressed, localOffset) {
  const b = new Uint8Array(4 + 24);
  const view = new DataView(b.buffer);
  view.setUint16(0, 0x0001, true);
  view.setUint16(2, 24, true);
  view.setBigUint64(4, BigInt(uncompressed), true);
  view.setBigUint64(12, BigInt(compressed), true);
  view.setBigUint64(20, BigInt(localOffset), true);
  return b;
}

/**
 * writeZip builds a zip archive.
 *
 * entries: [{ name, data: Uint8Array|string, method?: STORE|DEFLATE,
 *             streamingLocalHeader?: boolean }]
 * options: { forceZip64?: boolean, comment?: string }
 */
export function writeZip(entries, options = {}) {
  const { forceZip64 = false, comment = '' } = options;
  const encoder = new TextEncoder();
  const central = [];
  const out = new Writer();
  const { date, time } = dosDateTime();

  for (const entry of entries) {
    const nameBytes = encoder.encode(entry.name);
    const method = entry.method ?? DEFLATE;
    const data = typeof entry.data === 'string' ? encoder.encode(entry.data) : entry.data;
    const payload = method === DEFLATE ? new Uint8Array(deflateRawSync(data, { level: 6 })) : data;
    const sum = crc32(data);
    const localOffset = out.length;
    const localZip64 = forceZip64;

    const extra = forceZip64 ? zip64Extra(data.byteLength, payload.byteLength, localOffset) : new Uint8Array(0);

    out.u32(SIG_LOCAL);
    out.u16(localZip64 ? 45 : 20);
    out.u16(0x0800); // UTF-8 names
    out.u16(method);
    out.u16(time);
    out.u16(date);
    out.u32(sum);
    if (entry.streamingLocalHeader) {
      // What `zip` writes when it does not know the sizes yet.
      out.u32(0);
      out.u32(0);
    } else if (localZip64) {
      out.u32(0xffffffff);
      out.u32(0xffffffff);
    } else {
      out.u32(payload.byteLength);
      out.u32(data.byteLength);
    }
    out.u16(nameBytes.byteLength);
    out.u16(extra.byteLength);
    out.bytes(nameBytes);
    if (extra.byteLength > 0) out.bytes(extra);
    out.bytes(payload);

    central.push({ nameBytes, method, data, payload, sum, localOffset });
  }

  const cdOffset = out.length;
  for (const item of central) {
    const extra = forceZip64 ? zip64Extra(item.data.byteLength, item.payload.byteLength, item.localOffset) : new Uint8Array(0);
    out.u32(SIG_CENTRAL);
    out.u16(0x031e); // made by: unix, 3.0
    out.u16(forceZip64 ? 45 : 20);
    out.u16(0x0800);
    out.u16(item.method);
    out.u16(time);
    out.u16(date);
    out.u32(item.sum);
    out.u32(forceZip64 ? 0xffffffff : item.payload.byteLength);
    out.u32(forceZip64 ? 0xffffffff : item.data.byteLength);
    out.u16(item.nameBytes.byteLength);
    out.u16(extra.byteLength);
    out.u16(0); // comment
    out.u16(0); // disk start
    out.u16(0); // internal attrs
    out.u32(0); // external attrs
    out.u32(forceZip64 ? 0xffffffff : item.localOffset);
    out.bytes(item.nameBytes);
    if (extra.byteLength > 0) out.bytes(extra);
  }
  const cdSize = out.length - cdOffset;

  const commentBytes = encoder.encode(comment);
  if (forceZip64) {
    const eocd64Offset = out.length;
    out.u32(SIG_EOCD64);
    out.u64(44); // size of the rest of this record
    out.u16(45);
    out.u16(45);
    out.u32(0);
    out.u32(0);
    out.u64(central.length);
    out.u64(central.length);
    out.u64(cdSize);
    out.u64(cdOffset);

    out.u32(SIG_EOCD64_LOCATOR);
    out.u32(0);
    out.u64(eocd64Offset);
    out.u32(1);
  }

  out.u32(SIG_EOCD);
  out.u16(0);
  out.u16(0);
  out.u16(forceZip64 ? 0xffff : central.length);
  out.u16(forceZip64 ? 0xffff : central.length);
  out.u32(forceZip64 ? 0xffffffff : cdSize);
  out.u32(forceZip64 ? 0xffffffff : cdOffset);
  out.u16(commentBytes.byteLength);
  if (commentBytes.byteLength > 0) out.bytes(commentBytes);

  return out.toUint8Array();
}

/** writeZipFile is writeZip plus a writeFileSync, used by tools/make-sample.mjs. */
export function writeZipFile(path, entries, options) {
  const bytes = writeZip(entries, options);
  writeFileSync(path, bytes);
  return bytes;
}
