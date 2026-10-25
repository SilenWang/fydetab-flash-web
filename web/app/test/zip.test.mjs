// The zip container reader, against archives this repo writes itself.
//
// What matters for the flasher: the central directory is found from the end of
// the file (never by scanning the whole package), sizes come from the central
// directory rather than from a local header that a streaming writer may have
// zeroed, and a ZIP64 archive whose fixed fields are saturated still yields the
// real sizes and offsets.

import test from 'node:test';
import assert from 'node:assert/strict';

import { DEFLATE, STORE, writeZip } from '../../../tools/zip-writer.mjs';
import {
  entryDataRange,
  findEndOfCentralDirectory,
  methodName,
  readZipDirectory,
  supportedMethod,
} from '../zip.js';

const encoder = new TextEncoder();
const decoder = new TextDecoder();

function blobOf(bytes) {
  return new Blob([bytes]);
}

/** compressible builds a payload deflate can actually shrink. */
function compressible(length) {
  const bytes = new Uint8Array(length);
  for (let i = 0; i < length; i += 1) bytes[i] = (i % 251) * 7;
  return bytes;
}

function pattern(length, seed = 7) {
  const bytes = new Uint8Array(length);
  let value = seed;
  for (let i = 0; i < length; i += 1) {
    value = (value * 1103515245 + 12345) >>> 0;
    bytes[i] = value >>> 24;
  }
  return bytes;
}

async function inflateEntry(blob, entry) {
  const range = await entryDataRange(blob, entry);
  const slice = blob.slice(range.dataOffset, range.dataOffset + range.dataLength);
  if (entry.method === STORE) return new Uint8Array(await slice.arrayBuffer());
  const stream = slice.stream().pipeThrough(new DecompressionStream('deflate-raw'));
  const chunks = [];
  const reader = stream.getReader();
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    chunks.push(value);
  }
  const out = new Uint8Array(entry.uncompressedSize);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  assert.equal(offset, out.length);
  return out;
}

test('finds the EOCD of an archive with a comment', async () => {
  const bytes = writeZip([{ name: 'a.txt', data: 'hello' }], { comment: 'official package' });
  const eocd = await findEndOfCentralDirectory(blobOf(bytes));
  assert.equal(eocd.entries, 1);
  assert.equal(eocd.comment, 'official package');
});

test('reads the central directory of a deflated archive', async () => {
  const payload = compressible(4096);
  const blob = blobOf(
    writeZip([
      { name: 'update.img', data: payload, method: DEFLATE },
      { name: 'README.txt', data: 'read me', method: DEFLATE },
    ]),
  );
  const directory = await readZipDirectory(blob);
  assert.equal(directory.zip64, false);
  assert.equal(directory.entries.length, 2);
  const [image, readme] = directory.entries;
  assert.equal(image.name, 'update.img');
  assert.equal(image.method, DEFLATE);
  assert.equal(image.uncompressedSize, payload.length);
  assert.equal(image.compressedSize, image.compressedSize); // stored in the CD
  assert.ok(image.compressedSize < payload.length);
  assert.equal(image.dir, false);
  assert.equal(readme.name, 'README.txt');

  const round = await inflateEntry(blob, image);
  assert.deepEqual(round, payload);
});

test('reads a stored entry by its byte range', async () => {
  const payload = encoder.encode('stored payload, no compression');
  const blob = blobOf(writeZip([{ name: 'update.img', data: payload, method: STORE }]));
  const directory = await readZipDirectory(blob);
  const entry = directory.entries[0];
  assert.equal(entry.method, STORE);
  assert.equal(entry.compressedSize, payload.length);
  const range = await entryDataRange(blob, entry);
  const bytes = new Uint8Array(await blob.slice(range.dataOffset, range.dataOffset + range.dataLength).arrayBuffer());
  assert.deepEqual(bytes, payload);
});

test('a zeroed local header (streaming writer) still reads from the CD sizes', async () => {
  const payload = pattern(2048, 3);
  const blob = blobOf(writeZip([{ name: 'update.img', data: payload, method: DEFLATE, streamingLocalHeader: true }]));
  const directory = await readZipDirectory(blob);
  const entry = directory.entries[0];
  assert.equal(entry.uncompressedSize, payload.length);
  assert.deepEqual(await inflateEntry(blob, entry), payload);
});

test('ZIP64: saturated EOCD and CD fields resolve through the ZIP64 records', async () => {
  const payload = compressible(64 * 1024);
  const blob = blobOf(
    writeZip(
      [
        { name: 'MiniLoaderAll.bin', data: pattern(1024, 5), method: STORE },
        { name: 'Image/update.img', data: payload, method: DEFLATE },
      ],
      { forceZip64: true },
    ),
  );
  const directory = await readZipDirectory(blob);
  assert.equal(directory.zip64, true);
  assert.equal(directory.entries.length, 2);
  assert.equal(directory.centralDirectory.size > 0, true);

  const entry = directory.entries[1];
  assert.equal(entry.name, 'Image/update.img');
  assert.equal(entry.uncompressedSize, payload.length);
  assert.ok(entry.compressedSize > 0 && entry.compressedSize < payload.length);
  assert.ok(entry.localOffset > 0);
  assert.deepEqual(await inflateEntry(blob, entry), payload);
});

test('method names and support follow the reader, not the writer', () => {
  assert.equal(methodName(DEFLATE), 'deflate');
  assert.equal(methodName(STORE), 'stored');
  assert.equal(methodName(14), 'lzma');
  assert.equal(methodName(77), 'method-77');
  assert.equal(supportedMethod(STORE), true);
  assert.equal(supportedMethod(DEFLATE), true);
  assert.equal(supportedMethod(14), false);
});

test('rejects a file that is not a zip', async () => {
  await assert.rejects(findEndOfCentralDirectory(blobOf(pattern(4096))), /end-of-central-directory/);
  await assert.rejects(findEndOfCentralDirectory(blobOf(new Uint8Array(8))), /too small/);
});

test('rejects a zip whose central directory is truncated', async () => {
  const bytes = writeZip([{ name: 'update.img', data: pattern(4096) }]);
  // Keep the EOCD, drop the central directory it points at.
  const cut = bytes.subarray(0, 64);
  const eocdTail = bytes.subarray(bytes.length - 22);
  const broken = new Uint8Array(cut.length + eocdTail.length);
  broken.set(cut, 0);
  broken.set(eocdTail, cut.length);
  await assert.rejects(readZipDirectory(blobOf(broken)), /central directory/);
});

test('decodes non-ASCII entry names as UTF-8', async () => {
  const blob = blobOf(writeZip([{ name: '镜像/update.img', data: pattern(4096) }]));
  const directory = await readZipDirectory(blob);
  assert.equal(directory.entries[0].name, '镜像/update.img');
  assert.equal(decoder.decode(encoder.encode('镜像/update.img')), '镜像/update.img');
});
