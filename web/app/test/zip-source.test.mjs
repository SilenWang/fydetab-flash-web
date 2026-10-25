// The streaming window over a zip entry: the object the Go core reads an image
// through. The invariant that makes the zip path trustworthy is byte identity —
// every offset returns exactly what the same bytes would return from an
// unpacked .img — so the planner cannot see anything different from a zip.
//
// The rest pins the parts that differ from a plain file: deflate is forward
// only, so a backward read either comes from the window or reopens the
// pipeline; stored entries bypass the pipeline entirely; and a package that
// carries decoys still yields the RKFW image inside it.

import test from 'node:test';
import assert from 'node:assert/strict';

import { DEFLATE, STORE, writeZip } from '../../../tools/zip-writer.mjs';
import { openEntrySource, openZipImage, summarizePackage } from '../zip-source.js';

const encoder = new TextEncoder();

function blobOf(bytes) {
  return new Blob([bytes]);
}

function pattern(length, seed = 17) {
  const bytes = new Uint8Array(length);
  let value = seed >>> 0;
  for (let i = 0; i < length; i += 1) {
    value = (Math.imul(value, 1664525) + 1013904223) >>> 0;
    bytes[i] = value >>> 24;
  }
  return bytes;
}

/** makeImage builds the bytes of something shaped like an RKFW image. */
function makeImage(size = 1 << 20, magic = 'RKFW') {
  const bytes = pattern(size);
  bytes.set(encoder.encode(magic), 0);
  return bytes;
}

function assertBytesEqual(actual, expected, label) {
  assert.equal(actual.length, expected.length, `${label}: length`);
  for (let i = 0; i < expected.length; i += 1) {
    if (actual[i] !== expected[i]) {
      assert.fail(`${label}: byte ${i} is ${actual[i]}, want ${expected[i]}`);
    }
  }
}

async function singleEntrySource(bytes, { method = DEFLATE, options = {}, sourceOptions = {} } = {}) {
  const blob = blobOf(writeZip([{ name: 'update.img', data: bytes, method }], options));
  const { readZipDirectory } = await import('../zip.js');
  const directory = await readZipDirectory(blob);
  const source = await openEntrySource(blob, directory.entries[0], sourceOptions);
  return source;
}

test('a deflated entry reads back byte for byte', async () => {
  const image = makeImage(700 * 1024);
  const source = await singleEntrySource(image);
  assert.equal(source.size, image.length);

  const whole = await source.read(0, image.length);
  assertBytesEqual(whole, image, 'whole entry');

  const window = 64 * 1024;
  for (let offset = 0; offset < image.length; offset += window) {
    const length = Math.min(window, image.length - offset);
    const chunk = await source.read(offset, length);
    assertBytesEqual(chunk, image.subarray(offset, offset + length), `offset ${offset}`);
  }
});

test('reads are clamped to the entry, and past its end is empty', async () => {
  const image = makeImage(4096);
  const source = await singleEntrySource(image, { method: STORE });
  const tail = await source.read(image.length - 10, 100);
  assert.equal(tail.length, 10);
  assert.equal(tail[9], image[image.length - 1]);
  assert.equal((await source.read(image.length, 10)).length, 0);
  assert.equal((await source.read(0, 0)).length, 0);
});

test('a backwards read inside the window does not reopen the pipeline', async () => {
  const image = makeImage(256 * 1024);
  const source = await singleEntrySource(image, { sourceOptions: { windowBytes: 64 * 1024 } });
  await source.read(0, 4096);
  const again = await source.read(0, 1024);
  assertBytesEqual(again, image.subarray(0, 1024), 'window hit');
  assert.equal(source.stats.reopens, 0);
});

test('a backwards read behind the window reopens and decodes forward again', async () => {
  const image = makeImage(1024 * 1024);
  const source = await singleEntrySource(image, { sourceOptions: { windowBytes: 32 * 1024 } });
  const far = await source.read(900 * 1024, 4096);
  assertBytesEqual(far, image.subarray(900 * 1024, 904 * 1024), 'far read');
  assert.ok(source.stats.decodedBytes >= 904 * 1024);

  // The planner's actual pattern: after the archive and parameter item, it goes
  // back to the loader at the front of the file.
  const loader = await source.read(0x66, 512);
  assertBytesEqual(loader, image.subarray(0x66, 0x66 + 512), 'loader after rewind');
  assert.equal(source.stats.reopens, 1);
});

test('stored entries are served as slices, not through a decompressor', async () => {
  const image = makeImage(8192);
  const source = await singleEntrySource(image, { method: STORE });
  assertBytesEqual(await source.read(4096, 256), image.subarray(4096, 4352), 'stored read');
  assert.equal(source.stats.decodedBytes, 0);
  assert.equal(source.entry.method, 'stored');
});

test('ZIP64 entries read the same way', async () => {
  const image = makeImage(512 * 1024, 'RKAF');
  const source = await singleEntrySource(image, { options: { forceZip64: true } });
  assertBytesEqual(await source.read(0, 4096), image.subarray(0, 4096), 'zip64 head');
  assertBytesEqual(await source.read(500 * 1024, 4096), image.subarray(500 * 1024, 504 * 1024), 'zip64 tail');
});

test('openZipImage finds the RKFW entry among decoys', async () => {
  const image = makeImage(300 * 1024);
  const blob = blobOf(
    writeZip([
      { name: 'README.txt', data: 'install instructions', method: DEFLATE },
      { name: 'MiniLoaderAll.bin', data: pattern(512, 3), method: STORE }, // too small to be an image
      { name: 'driver/DriverAssitant_v5.13.zip', data: pattern(4096, 4), method: STORE }, // not RKFW
      { name: 'Image/update.img', data: image, method: DEFLATE },
    ]),
  );
  const opened = await openZipImage(blob, 'RK3588-update.zip');
  assert.equal(opened.entry.name, 'Image/update.img');
  assert.equal(opened.entry.method, 'deflate');
  assert.equal(opened.source.name, 'RK3588-update.zip → Image/update.img');
  assert.equal(opened.source.size, image.length);
  assertBytesEqual(await opened.source.read(0, 4), encoder.encode('RKFW'), 'magic');
  assertBytesEqual(await opened.source.read(0, image.length), image, 'image through zip');

  const summary = summarizePackage(opened);
  assert.match(summary, /Image\/update\.img/);
  assert.match(summary, /deflate/);
  assert.match(summary, /目录 4 项/);
  assert.match(summary, /跳过 3 项/);
  assert.ok(opened.skipped.some((s) => s.entry === 'MiniLoaderAll.bin'));
});

test('a complete RKFW entry wins over an earlier bare RKAF entry', async () => {
  const bare = makeImage(4096, 'RKAF');
  const full = makeImage(64 * 1024, 'RKFW');
  const blob = blobOf(
    writeZip([
      { name: 'bare-update.img', data: bare, method: DEFLATE },
      { name: 'update.img', data: full, method: DEFLATE },
    ]),
  );
  const opened = await openZipImage(blob, 'pkg.zip');
  assert.equal(opened.entry.name, 'update.img');
  assertBytesEqual(await opened.source.read(0, 4), encoder.encode('RKFW'), 'magic');
});

test('a package with only a bare RKAF is accepted', async () => {
  const bare = makeImage(4096, 'RKAF');
  const blob = blobOf(writeZip([{ name: 'update.img', data: bare, method: STORE }]));
  const opened = await openZipImage(blob, 'pkg.zip');
  assert.equal(opened.entry.name, 'update.img');
  assertBytesEqual(await opened.source.read(0, 4), encoder.encode('RKAF'), 'magic');
});

test('a package without an image fails with the reasons it skipped', async () => {
  const blob = blobOf(
    writeZip([
      { name: 'readme.txt', data: 'nothing to flash here', method: DEFLATE },
      { name: 'notes.txt', data: pattern(4096, 9), method: DEFLATE },
    ]),
  );
  await assert.rejects(openZipImage(blob, 'not-a-firmware.zip'), (err) => {
    assert.match(err.message, /没有 RKFW\/RKAF 镜像条目/);
    assert.match(err.message, /readme\.txt/);
    return true;
  });
});

test('an unsupported compression method is reported, not guessed at', async () => {
  const blob = blobOf(writeZip([{ name: 'update.img', data: makeImage(4096), method: 14 }]));
  await assert.rejects(openZipImage(blob, 'lzma.zip'), /不支持|没有 RKFW/);
  await assert.rejects(openEntrySource(blob, { name: 'update.img', method: 14, compressedSize: 10, uncompressedSize: 10 }), /lzma/);
});
