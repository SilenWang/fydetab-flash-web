// Random-access, streaming view of one entry inside a zip.
//
// The contract is the one the Go core already speaks (cmd/wasm/uf.go): an
// object with {name, size, read(offset, length) -> Promise<Uint8Array>} that
// rkfw reads lazily as an io.ReaderAt. For a plain file that is a Blob.slice;
// for an entry inside a multi-GiB vendor package it is a DecompressionStream
// that is advanced on demand — nothing is written to disk and no entry is ever
// materialised in memory.
//
// Deflate is not seekable, so the reader keeps a single decompression pipeline
// and can only move forward through it. A backward read is served from a small
// window of already-decoded bytes; a read further back reopens the pipeline and
// decodes forward again. That is the right trade for the way the planner reads:
// header → RKAF table → parameter item → loader, with the loader (near the
// start of the file) the only backward jump, and item payloads afterwards in
// increasing order. The statistics in `stats` are shown on the page so a user
// can see how much was decoded rather than assuming the whole image was.

import {
  ZIP_METHOD_STORE,
  describeEntry,
  entryDataRange,
  methodName,
  readZipDirectory,
  supportedMethod,
} from './zip.js';

// RKFW header (0x66) + an RKAF header (2048) is the smallest thing rkfw will
// accept, so anything smaller cannot be the image inside the package.
const MIN_IMAGE_BYTES = 2048;

// How many already-decoded bytes stay available behind the read cursor. Enough
// for the repeated header/loader reads of a plan, far too small to hold a
// partition.
const DEFAULT_WINDOW_BYTES = 8 << 20;

// A package with more candidate entries than this is not a firmware package.
const MAX_CANDIDATES = 128;

async function readBlobRange(blob, offset, length) {
  return new Uint8Array(await blob.slice(offset, offset + length).arrayBuffer());
}

/**
 * EntrySource is the io.ReaderAt the worker hands to the Go core.
 *
 * It is a plain object literal at the edge (see `openEntrySource`) so it can be
 * structured-cloned and passed through syscall/js without a class identity.
 */
class EntrySource {
  constructor(blob, entry, range, { name, size, windowBytes }) {
    this.blob = blob;
    this.entry = entry;
    this.range = range;
    this.name = name;
    this.size = size;
    this.method = entry.method;
    this.windowBytes = windowBytes;
    this.reader = null;
    this.chunks = [];
    this.cacheStart = 0;
    this.cachedEnd = 0;
    this.ended = false;
    this.closed = false;
    this.stats = {
      entry: entry.name,
      method: methodName(entry.method),
      compressedBytes: entry.compressedSize,
      uncompressedBytes: entry.uncompressedSize,
      reads: 0,
      reopens: 0,
      decodedBytes: 0,
      peakBufferedBytes: 0,
    };
  }

  async read(offset, length) {
    const start = Number(offset);
    const want = Math.max(0, Math.min(Number(length) || 0, this.size - start));
    if (!Number.isFinite(start) || start < 0) throw new RangeError(`zip: negative read offset ${offset}`);
    if (want === 0) return new Uint8Array(0);

    if (this.method === ZIP_METHOD_STORE) {
      this.stats.reads += 1;
      return readBlobRange(this.blob, this.range.dataOffset + start, want);
    }

    this.stats.reads += 1;
    if (start < this.cacheStart) await this._reopen();
    while (this.cachedEnd < start + want) {
      if (!(await this._pull())) break;
    }

    const out = new Uint8Array(want);
    let filled = 0;
    for (const chunk of this.chunks) {
      const chunkStart = chunk.offset;
      const chunkEnd = chunk.offset + chunk.bytes.length;
      const from = Math.max(start, chunkStart);
      const to = Math.min(start + want, chunkEnd);
      if (to <= from) continue;
      out.set(chunk.bytes.subarray(from - chunkStart, to - chunkStart), from - start);
      filled += to - from;
    }
    this._trim(start + filled);
    return filled === want ? out : out.subarray(0, filled);
  }

  async _pull() {
    if (this.ended || !this.reader) return false;
    let result;
    try {
      result = await this.reader.read();
    } catch (err) {
      throw new Error(`zip: 解压 ${this.entry.name} 失败（deflate 流损坏？）：${err?.message ?? err}`);
    }
    if (result.done) {
      this.ended = true;
      return false;
    }
    const bytes = result.value instanceof Uint8Array ? result.value : new Uint8Array(result.value);
    if (bytes.length === 0) return true;
    this.chunks.push({ offset: this.cachedEnd, bytes });
    this.cachedEnd += bytes.length;
    this.stats.decodedBytes += bytes.length;
    this.stats.peakBufferedBytes = Math.max(this.stats.peakBufferedBytes, this.cachedEnd - this.cacheStart);
    return true;
  }

  _trim(consumedUpTo) {
    const floor = consumedUpTo - this.windowBytes;
    while (this.chunks.length > 0) {
      const first = this.chunks[0];
      if (first.offset + first.bytes.length > floor) break;
      this.chunks.shift();
    }
    this.cacheStart = this.chunks.length > 0 ? this.chunks[0].offset : this.cachedEnd;
  }

  async _reopen() {
    await this._cancel();
    this.chunks = [];
    this.cacheStart = 0;
    this.cachedEnd = 0;
    this.ended = false;
    // reopens counts pipeline *replacements*: the first pipeline is the entry,
    // not a rewind.
    if (this.pipelineStarted) this.stats.reopens += 1;
    this.pipelineStarted = true;
    const compressed = this.blob.slice(this.range.dataOffset, this.range.dataOffset + this.range.dataLength);
    const raw = compressed.stream();
    const stream = this.method === ZIP_METHOD_STORE ? raw : raw.pipeThrough(new DecompressionStream('deflate-raw'));
    this.reader = stream.getReader();
  }

  async _cancel() {
    if (!this.reader) return;
    const reader = this.reader;
    this.reader = null;
    try {
      await reader.cancel();
    } catch {
      // A cancelled decompression stream rejects pending reads; that is fine,
      // the pipeline is being replaced.
    }
  }

  async close() {
    this.closed = true;
    await this._cancel();
    this.chunks = [];
    this.cachedEnd = 0;
    this.cacheStart = 0;
  }
}

/**
 * openEntrySource exposes one zip entry as {name, size, read(offset,length)}.
 */
export async function openEntrySource(blob, entry, { name, windowBytes = DEFAULT_WINDOW_BYTES } = {}) {
  if (entry.dir) throw new Error(`zip: ${entry.name} 是目录，不是文件`);
  if (entry.encrypted) throw new Error(`zip: ${entry.name} 已加密，页面无法读取`);
  if (!supportedMethod(entry.method)) {
    throw new Error(`zip: ${entry.name} 使用了 ${methodName(entry.method)} 压缩，本页只支持 stored 与 deflate`);
  }
  if (!(entry.uncompressedSize > 0)) {
    throw new Error(`zip: ${entry.name} 在中央目录里报告 0 字节（可能由流式写入产生），无法作为镜像读取`);
  }
  const range = await entryDataRange(blob, entry);
  const source = new EntrySource(blob, entry, range, {
    name: name ?? entry.name,
    size: entry.uncompressedSize,
    windowBytes,
  });
  if (entry.method !== ZIP_METHOD_STORE) await source._reopen();

  return {
    name: source.name,
    size: source.size,
    entry: {
      name: entry.name,
      method: methodName(entry.method),
      compressedSize: entry.compressedSize,
      uncompressedSize: entry.uncompressedSize,
    },
    stats: source.stats,
    read: (offset, length) => source.read(offset, length),
    close: () => source.close(),
  };
}

/**
 * openZipImage finds the Rockchip image inside a package and returns it as a
 * source the planner can read.
 *
 * The search is over the *decompressed* first four bytes of each candidate
 * entry, because a vendor package may also carry loaders, drivers or PDFs
 * under names that say nothing. The first RKFW entry wins (a complete
 * update.img); a bare RKAF entry is only used when no RKFW entry exists.
 */
export async function openZipImage(blob, zipName = 'package.zip') {
  const directory = await readZipDirectory(blob);
  const skipped = [];
  let bare = null;
  let candidates = 0;

  for (const entry of directory.entries) {
    if (entry.dir) continue;
    if (entry.encrypted || !supportedMethod(entry.method)) {
      skipped.push({ entry: entry.name, reason: entry.encrypted ? '加密' : `不支持的压缩 ${methodName(entry.method)}` });
      continue;
    }
    if (!(entry.uncompressedSize >= MIN_IMAGE_BYTES)) {
      skipped.push({ entry: entry.name, reason: `${entry.uncompressedSize} B，小于 RKFW/RKAF 头部` });
      continue;
    }
    candidates += 1;
    if (candidates > MAX_CANDIDATES) {
      throw new Error(`zip: 候选条目超过 ${MAX_CANDIDATES} 个，这不是一个固件包`);
    }

    let source = null;
    try {
      source = await openEntrySource(blob, entry, { name: `${zipName} → ${entry.name}` });
      const head = await source.read(0, 4);
      const magic = String.fromCharCode(...head);
      if (magic === 'RKFW') {
        return { source, entry: source.entry, directory, skipped, skippedDetail: skipped };
      }
      if (magic === 'RKAF' && !bare) {
        bare = { source, entry: source.entry, directory, skipped };
        continue;
      }
      await source.close();
      skipped.push({ entry: entry.name, reason: `内容以 ${JSON.stringify(magic)} 开头，不是 RKFW/RKAF` });
    } catch (err) {
      if (source) await source.close();
      skipped.push({ entry: entry.name, reason: err?.message ?? String(err) });
    }
  }

  if (bare) return { ...bare, skippedDetail: bare.skipped };
  const detail = skipped.length > 0 ? `：${skipped.map((s) => `${s.entry}（${s.reason}）`).join('；')}` : '';
  throw new Error(`zip: ${zipName} 里没有 RKFW/RKAF 镜像条目${detail}`);
}

/** summarizePackage is the human-readable line the page shows after opening. */
export function summarizePackage({ directory, entry, source, skipped }) {
  const parts = [
    `zip: ${entry.name} · ${source.entry.method} · ${formatBytes(source.entry.compressedSize)} → ${formatBytes(
      source.entry.uncompressedSize,
    )}`,
    `目录 ${directory.entries.length} 项${directory.zip64 ? ' · ZIP64' : ''}`,
  ];
  if (skipped.length > 0) parts.push(`跳过 ${skipped.length} 项`);
  return parts.join(' · ');
}

/** describePackageEntry exists so the page can log the whole directory. */
export function describePackageEntry(entry) {
  return describeEntry(entry);
}

function formatBytes(n) {
  if (!Number.isFinite(n)) return '?';
  const mib = 1024 * 1024;
  const kib = 1024;
  if (n >= mib) return `${(n / mib).toFixed(n % mib === 0 ? 0 : 1)} MiB`;
  if (n >= kib) return `${(n / kib).toFixed(n % kib === 0 ? 0 : 1)} KiB`;
  return `${n} B`;
}
