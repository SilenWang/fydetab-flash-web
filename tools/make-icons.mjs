#!/usr/bin/env node
// Generate the PWA icons, so they are reproducible instead of opaque binaries.
//
// There is no image dependency in this repo, and a PNG is simple enough to
// write directly: IHDR + one zlib-compressed IDAT of RGBA rows, filtered with
// type 0. Shapes are rendered at 3x and box-filtered down, which is the
// cheapest way to get edges that do not look like stairs at 192 px.
//
// Usage: pixi run icons   (or: node tools/make-icons.mjs [--out <dir>])

import { deflateSync } from 'node:zlib';
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { crc32 } from './zip-writer.mjs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const outIndex = process.argv.indexOf('--out');
const outDir = resolve(root, outIndex > 0 ? process.argv[outIndex + 1] : join('web', 'app', 'icons'));

const BG = [0x0f, 0x13, 0x18];
const CHIP = [0x16, 0x1c, 0x24];
const ACCENT = [0x63, 0xb3, 0xed];
const BOLT = [0xfb, 0xbf, 0x24];

const SUPERSAMPLE = 3;

function pngChunk(type, data) {
  const typeBytes = new TextEncoder().encode(type);
  const body = new Uint8Array(typeBytes.length + data.length);
  body.set(typeBytes, 0);
  body.set(data, typeBytes.length);
  const out = new Uint8Array(12 + data.length);
  const view = new DataView(out.buffer);
  view.setUint32(0, data.length);
  out.set(body, 4);
  view.setUint32(8 + data.length, crc32(body));
  return out;
}

function encodePng(width, height, rgba) {
  const ihdr = new Uint8Array(13);
  const view = new DataView(ihdr.buffer);
  view.setUint32(0, width);
  view.setUint32(4, height);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 6; // truecolour + alpha
  ihdr[10] = 0;
  ihdr[11] = 0;
  ihdr[12] = 0;

  const stride = width * 4;
  const raw = new Uint8Array((stride + 1) * height);
  for (let y = 0; y < height; y += 1) {
    raw[y * (stride + 1)] = 0; // filter type: none
    raw.set(rgba.subarray(y * stride, (y + 1) * stride), y * (stride + 1) + 1);
  }

  const signature = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
  const parts = [signature, pngChunk('IHDR', ihdr), pngChunk('IDAT', new Uint8Array(deflateSync(raw, { level: 9 }))), pngChunk('IEND', new Uint8Array(0))];
  const total = parts.reduce((sum, part) => sum + part.length, 0);
  const file = new Uint8Array(total);
  let offset = 0;
  for (const part of parts) {
    file.set(part, offset);
    offset += part.length;
  }
  return file;
}

function insideRoundRect(x, y, x0, y0, x1, y1, radius) {
  if (x < x0 || x > x1 || y < y0 || y > y1) return false;
  const cx = Math.min(Math.max(x, x0 + radius), x1 - radius);
  const cy = Math.min(Math.max(y, y0 + radius), y1 - radius);
  return (x - cx) ** 2 + (y - cy) ** 2 <= radius ** 2;
}

function insidePolygon(x, y, points) {
  let inside = false;
  for (let i = 0, j = points.length - 1; i < points.length; j = i, i += 1) {
    const [xi, yi] = points[i];
    const [xj, yj] = points[j];
    if (yi > y !== yj > y && x < ((xj - xi) * (y - yi)) / (yj - yi) + xi) inside = !inside;
  }
  return inside;
}

/**
 * renderIcon draws the app mark: a USB flash chip (rounded square, pins on all
 * four sides) with a lightning bolt, at `size` pixels.
 *
 * `maskable` keeps the mark inside the 80% safe zone and drops the rounded
 * corners, because Android crops maskable icons to whatever shape the launcher
 * uses.
 */
function renderIcon(size, { maskable = false } = {}) {
  // Maskable icons shrink the mark into the 80% safe zone.
  const chipHalf = maskable ? 0.24 : 0.29;
  const chipRadius = 0.07;
  const border = 0.045;
  const pinLength = 0.1;
  const pinWidth = 0.055;
  const pinStops = [-0.145, 0, 0.145];
  const bolt = [
    [0.545, 0.335],
    [0.435, 0.545],
    [0.5, 0.545],
    [0.445, 0.685],
    [0.575, 0.465],
    [0.505, 0.465],
  ];

  const n = size * SUPERSAMPLE;
  const accum = new Float64Array(size * size * 4);
  for (let y = 0; y < n; y += 1) {
    for (let x = 0; x < n; x += 1) {
      const ux = (x + 0.5) / n;
      const uy = (y + 0.5) / n;
      let color = null;

      // The tile: rounded for a normal icon, square for a maskable one (the
      // launcher applies its own mask, so a rounded corner would just be a
      // second, misaligned one).
      const tile = maskable
        ? insideRoundRect(ux, uy, 0, 0, 1, 1, 0)
        : insideRoundRect(ux, uy, 0.02, 0.02, 0.98, 0.98, 0.22);
      if (tile) color = BG;

      if (color) {
        const cx = 0.5;
        const cy = 0.5;
        const chip = insideRoundRect(ux, uy, cx - chipHalf, cy - chipHalf, cx + chipHalf, cy + chipHalf, chipRadius);
        const inner = insideRoundRect(
          ux,
          uy,
          cx - chipHalf + border,
          cy - chipHalf + border,
          cx + chipHalf - border,
          cy + chipHalf - border,
          chipRadius * 0.7,
        );
        if (chip) color = inner ? CHIP : ACCENT;

        for (const stop of pinStops) {
          const offset = stop * (maskable ? 0.86 : 1);
          const along = pinWidth / 2;
          // left / right pins
          if (
            insideRoundRect(ux, uy, cx - chipHalf - pinLength, cy + offset - along, cx - chipHalf, cy + offset + along, along * 0.4) ||
            insideRoundRect(ux, uy, cx + chipHalf, cy + offset - along, cx + chipHalf + pinLength, cy + offset + along, along * 0.4) ||
            insideRoundRect(ux, uy, cx + offset - along, cy - chipHalf - pinLength, cx + offset + along, cy - chipHalf, along * 0.4) ||
            insideRoundRect(ux, uy, cx + offset - along, cy + chipHalf, cx + offset + along, cy + chipHalf + pinLength, along * 0.4)
          ) {
            color = ACCENT;
          }
        }

        if (inner && insidePolygon(ux, uy, bolt)) color = BOLT;
      }

      if (!color) continue;
      const sx = Math.floor(x / SUPERSAMPLE);
      const sy = Math.floor(y / SUPERSAMPLE);
      const index = (sy * size + sx) * 4;
      accum[index] += color[0];
      accum[index + 1] += color[1];
      accum[index + 2] += color[2];
      accum[index + 3] += 255;
    }
  }

  const samples = SUPERSAMPLE * SUPERSAMPLE;
  const rgba = new Uint8Array(size * size * 4);
  for (let i = 0; i < size * size; i += 1) {
    const alpha = accum[i * 4 + 3] / samples;
    rgba[i * 4] = Math.round(accum[i * 4] / samples);
    rgba[i * 4 + 1] = Math.round(accum[i * 4 + 1] / samples);
    rgba[i * 4 + 2] = Math.round(accum[i * 4 + 2] / samples);
    rgba[i * 4 + 3] = Math.round(alpha);
  }
  return encodePng(size, size, rgba);
}

mkdirSync(outDir, { recursive: true });
const outputs = [
  ['icon-192.png', renderIcon(192)],
  ['icon-512.png', renderIcon(512)],
  ['icon-maskable-512.png', renderIcon(512, { maskable: true })],
];
for (const [name, bytes] of outputs) {
  const path = join(outDir, name);
  writeFileSync(path, bytes);
  console.log(`${name}: ${bytes.length} B → ${path}`);
}
