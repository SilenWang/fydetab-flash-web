// ZIP container reader: enough of APPNOTE.TXT to find, describe and slice the
// entries of a vendor firmware package (ZIP32 and ZIP64), and nothing else.
//
// Why it is hand-written: the page ships no dependencies, and the official
// Rockchip packages are plain zips whose only interesting entry is a multi-GiB
// `update.img`. Reading it means locating the central directory (the end of the
// file), then handing the entry's *compressed* byte range to zip-source.js,
// which decompresses on demand. No entry is ever materialised here.
//
// What it deliberately does not do: encryption, multi-disk archives, CRC
// verification (that is the inner RKFW/RKAF checks' job), and writing zips.

export const ZIP_METHOD_STORE = 0;
export const ZIP_METHOD_DEFLATE = 8;

const SIG_LOCAL = 0x04034b50;
const SIG_CENTRAL = 0x02014b50;
const SIG_EOCD = 0x06054b50;
const SIG_EOCD64 = 0x06064b50;
const SIG_EOCD64_LOCATOR = 0x07064b50;

// EOCD is 22 bytes plus a comment of at most 0xFFFF bytes.
const EOCD_MIN = 22;
const EOCD_MAX = EOCD_MIN + 0xffff;

const METHOD_NAMES = {
  [ZIP_METHOD_STORE]: 'stored',
  [ZIP_METHOD_DEFLATE]: 'deflate',
  1: 'shrink',
  6: 'implode',
  9: 'deflate64',
  12: 'bzip2',
  14: 'lzma',
  93: 'zstd',
  95: 'xz',
  98: 'ppmd',
};

/** methodName renders a compression method id the way a UI can show it. */
export function methodName(method) {
  return METHOD_NAMES[method] ?? `method-${method}`;
}

/** supportedMethod reports whether zip-source.js can decompress the entry. */
export function supportedMethod(method) {
  return method === ZIP_METHOD_STORE || method === ZIP_METHOD_DEFLATE;
}

const utf8 = new TextDecoder('utf-8', { fatal: false });

function u16(view, offset) {
  return view.getUint16(offset, true);
}

function u32(view, offset) {
  return view.getUint32(offset, true);
}

function u64(view, offset) {
  const value = view.getBigUint64(offset, true);
  if (value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new RangeError(`zip: 64-bit field ${value} exceeds Number.MAX_SAFE_INTEGER`);
  }
  return Number(value);
}

async function readSlice(blob, offset, length) {
  if (offset < 0 || offset > blob.size) {
    throw new RangeError(`zip: read [${offset},${offset + length}) is outside the ${blob.size}-byte file`);
  }
  const end = Math.min(blob.size, offset + length);
  return new Uint8Array(await blob.slice(offset, end).arrayBuffer());
}

/**
 * findEndOfCentralDirectory locates the EOCD record by scanning the tail.
 * A file can contain the signature inside a comment, so only a record whose
 * declared comment length reaches exactly to EOF counts.
 */
export async function findEndOfCentralDirectory(blob) {
  const tailLength = Math.min(blob.size, EOCD_MAX);
  if (tailLength < EOCD_MIN) {
    throw new Error(`zip: ${blob.size} bytes is too small to be a zip (EOCD needs ${EOCD_MIN})`);
  }
  const tailOffset = blob.size - tailLength;
  const tailBytes = await readSlice(blob, tailOffset, tailLength);
  const tail = Buffer32(tailBytes);
  for (let i = tail.byteLength - EOCD_MIN; i >= 0; i -= 1) {
    if (u32(tail, i) !== SIG_EOCD) continue;
    const commentLength = u16(tail, i + 20);
    if (i + EOCD_MIN + commentLength !== tail.byteLength) continue;
    return {
      offset: tailOffset + i,
      entries: u16(tail, i + 10),
      centralDirectorySize: u32(tail, i + 12),
      centralDirectoryOffset: u32(tail, i + 16),
      comment: utf8.decode(tailBytes.subarray(i + EOCD_MIN, i + EOCD_MIN + commentLength)),
    };
  }
  throw new Error('zip: no end-of-central-directory record (not a zip file, or truncated)');
}

/** eocd64 reads the ZIP64 EOCD record the locator points at. */
async function readEocd64(blob, eocd) {
  const locatorOffset = eocd.offset - 20;
  if (locatorOffset < 0) throw new Error('zip: ZIP64 locator would start before the file');
  const locator = Buffer32(await readSlice(blob, locatorOffset, 20));
  if (u32(locator, 0) !== SIG_EOCD64_LOCATOR) {
    throw new Error('zip: EOCD needs ZIP64, but no ZIP64 locator precedes it');
  }
  const recordOffset = u64(locator, 8);
  const record = Buffer32(await readSlice(blob, recordOffset, 56));
  if (u32(record, 0) !== SIG_EOCD64) {
    throw new Error('zip: ZIP64 locator does not point at a ZIP64 EOCD record');
  }
  return {
    offset: recordOffset,
    entries: u64(record, 32),
    centralDirectorySize: u64(record, 40),
    centralDirectoryOffset: u64(record, 48),
  };
}

function Buffer32(bytes) {
  return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
}

/** parseExtras walks the TLV list of a central-directory entry. */
function parseExtras(view, start, end, entry) {
  let offset = start;
  while (offset + 4 <= end) {
    const id = u16(view, offset);
    const size = u16(view, offset + 2);
    const body = offset + 4;
    if (body + size > end) break;
    if (id === 0x0001) {
      // ZIP64: only the fields that were saturated in the fixed record appear,
      // in this fixed order.
      let p = body;
      const take = (limit) => {
        if (p + limit > body + size) return null;
        return p;
      };
      if (entry.uncompressedSize === 0xffffffff) {
        const at = take(8);
        if (at !== null) {
          entry.uncompressedSize = u64(view, at);
          p += 8;
        }
      }
      if (entry.compressedSize === 0xffffffff) {
        const at = take(8);
        if (at !== null) {
          entry.compressedSize = u64(view, at);
          p += 8;
        }
      }
      if (entry.localOffset === 0xffffffff) {
        const at = take(8);
        if (at !== null) {
          entry.localOffset = u64(view, at);
          p += 8;
        }
      }
      if (entry.diskStart === 0xffff) {
        const at = take(4);
        if (at !== null) {
          entry.diskStart = u32(view, at);
          p += 4;
        }
      }
    }
    offset = body + size;
  }
}

function parseCentralEntry(view, offset, cdEnd) {
  if (offset + 46 > cdEnd) throw new Error(`zip: central directory entry at 0x${offset.toString(16)} is truncated`);
  if (u32(view, offset) !== SIG_CENTRAL) {
    throw new Error(`zip: bad central directory signature at 0x${offset.toString(16)}`);
  }
  const flags = u16(view, offset + 8);
  const entry = {
    name: '',
    method: u16(view, offset + 10),
    flags,
    encrypted: (flags & 0x0001) !== 0,
    usesDataDescriptor: (flags & 0x0008) !== 0,
    crc32: u32(view, offset + 16),
    compressedSize: u32(view, offset + 20),
    uncompressedSize: u32(view, offset + 24),
    diskStart: u16(view, offset + 34),
    localOffset: u32(view, offset + 42),
    modified: null,
  };
  const nameLength = u16(view, offset + 28);
  const extraLength = u16(view, offset + 30);
  const commentLength = u16(view, offset + 32);
  const nameStart = offset + 46;
  const extraStart = nameStart + nameLength;
  const commentStart = extraStart + extraLength;
  const next = commentStart + commentLength;
  if (next > cdEnd) throw new Error(`zip: central directory entry at 0x${offset.toString(16)} overruns the directory`);

  entry.name = utf8.decode(new Uint8Array(view.buffer, view.byteOffset + nameStart, nameLength));
  entry.comment = utf8.decode(new Uint8Array(view.buffer, view.byteOffset + commentStart, commentLength));
  entry.dir = entry.name.endsWith('/');
  parseExtras(view, extraStart, commentStart, entry);
  entry.next = next;
  return entry;
}

/**
 * readZipDirectory parses the end-of-central-directory record and the central
 * directory it points at. It reads only those bytes: on a 3.8 GiB package that
 * is a handful of KiB.
 */
export async function readZipDirectory(blob) {
  if (!blob || typeof blob.size !== 'number' || typeof blob.slice !== 'function') {
    throw new Error('zip: expected a Blob/File');
  }
  const eocd = await findEndOfCentralDirectory(blob);
  const needsZip64 =
    eocd.entries === 0xffff || eocd.centralDirectorySize === 0xffffffff || eocd.centralDirectoryOffset === 0xffffffff;
  const eocd64 = needsZip64 ? await readEocd64(blob, eocd) : null;
  const directory = eocd64 ?? eocd;
  if (directory.centralDirectoryOffset + directory.centralDirectorySize > blob.size) {
    throw new Error(
      `zip: central directory [0x${directory.centralDirectoryOffset.toString(16)}, +0x${directory.centralDirectorySize.toString(
        16,
      )}) is outside the ${blob.size}-byte file`,
    );
  }
  const cdBytes = await readSlice(blob, directory.centralDirectoryOffset, directory.centralDirectorySize);
  const view = Buffer32(cdBytes);
  const cdEnd = view.byteLength;

  const entries = [];
  let offset = 0;
  for (let i = 0; i < directory.entries; i += 1) {
    const entry = parseCentralEntry(view, offset, cdEnd);
    entries.push(entry);
    offset = entry.next;
  }
  if (offset > cdEnd) throw new Error('zip: central directory ended past its declared size');

  return {
    size: blob.size,
    zip64: Boolean(eocd64),
    comment: eocd.comment,
    entries,
    centralDirectory: {
      offset: directory.centralDirectoryOffset,
      size: directory.centralDirectorySize,
    },
  };
}

/**
 * entryDataRange resolves where an entry's compressed bytes actually start.
 *
 * The central directory is authoritative for the sizes (a streamed zip may
 * leave zeroes in the local header and put the real values in a data
 * descriptor), but only the local header knows its own variable-length extra
 * field, which has to be skipped to find byte 0 of the data.
 */
export async function entryDataRange(blob, entry) {
  const header = Buffer32(await readSlice(blob, entry.localOffset, 30));
  if (header.byteLength < 30) throw new Error(`zip: local header for ${entry.name} is truncated`);
  if (u32(header, 0) !== SIG_LOCAL) {
    throw new Error(`zip: local header for ${entry.name} has a bad signature at 0x${entry.localOffset.toString(16)}`);
  }
  const nameLength = u16(header, 26);
  const extraLength = u16(header, 28);
  const dataOffset = entry.localOffset + 30 + nameLength + extraLength;
  if (dataOffset + entry.compressedSize > blob.size) {
    throw new Error(
      `zip: ${entry.name} claims ${entry.compressedSize} compressed bytes at 0x${dataOffset.toString(16)}, past the ${blob.size}-byte file (truncated package)`,
    );
  }
  return { dataOffset, dataLength: entry.compressedSize };
}

/** describeEntry is the one-line summary the page shows after opening a zip. */
export function describeEntry(entry) {
  const kind = entry.dir ? '目录' : methodName(entry.method);
  return `${entry.name} · ${kind} · ${entry.compressedSize} → ${entry.uncompressedSize} B`;
}
