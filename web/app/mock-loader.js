// A synthetic RKBOOT loader for the page's demo (mock) mode only.
//
// This is NOT a Rockchip loader and cannot boot anything: it is a correctly
// shaped container (header, entry tables, payloads, trailing CRC-32) with the
// same segment sizes stage 2 measured on a real rk3588_spl_loader_v1.06.111.bin
// (UsbHead 2048 B + DDR 51200 B on 0x0471, usbplug 88064 B on 0x0472), filled
// with a deterministic pattern. It exists so `db` can be demonstrated without
// hardware and so the mock device can answer like a real one.
//
// The CRC is Rockchip's own: poly 0x04C10DB7, init 0, MSB-first, no final XOR
// (rkdeveloptool crc.cpp). Standard CRC-32 would be rejected by our parser.

export const LOADER_HEADER_SIZE = 102;
export const LOADER_ENTRY_SIZE = 57;
export const LOADER_TAG_BOOT = 0x544f4f42; // "BOOT"
export const RK3588_CHIP_TAG = 0x33353838; // ASCII "3588" packed big-endian
export const ENTRY_471 = 1;
export const ENTRY_472 = 2;
export const ENTRY_LOADER = 4;

export const MOCK_SEGMENTS = [
  { request: 0x0471, name: 'UsbHead', size: 2048, seed: 0x11 },
  { request: 0x0471, name: 'rk3588_ddr_lp4_2112M', size: 51200, seed: 0x22 },
  { request: 0x0472, name: 'rk3588_usbplug_v1', size: 88064, seed: 0x33 },
  // ENTRYLOADER pieces upgrade_tool assembles into a New IDB.
  { request: ENTRY_LOADER, name: 'FlashHead', size: 2048, seed: 0x44 },
  { request: ENTRY_LOADER, name: 'FlashData', size: 4096, seed: 0x55 },
  { request: ENTRY_LOADER, name: 'FlashBoot', size: 8192, seed: 0x66 },
];

export function crc32Rockchip(bytes) {
  let crc = 0;
  for (let i = 0; i < bytes.length; i += 1) {
    crc = (crc ^ (bytes[i] << 24)) >>> 0;
    for (let bit = 0; bit < 8; bit += 1) {
      crc = (crc & 0x80000000 ? (crc << 1) ^ 0x04c10db7 : crc << 1) >>> 0;
    }
  }
  return crc >>> 0;
}

function writeName(view, offset, name) {
  for (let i = 0; i < Math.min(name.length, 20); i += 1) {
    view.setUint8(offset + i * 2, name.charCodeAt(i));
    view.setUint8(offset + i * 2 + 1, 0);
  }
}

/** buildMockLoader returns a Uint8Array holding a valid RKBOOT container. */
export function buildMockLoader(segments = MOCK_SEGMENTS) {
  const count471 = segments.filter((s) => s.request === 0x0471).length;
  const count472 = segments.filter((s) => s.request === 0x0472).length;
  const countLoader = segments.filter((s) => s.request === ENTRY_LOADER).length;
  const offset471 = LOADER_HEADER_SIZE;
  const offset472 = offset471 + count471 * LOADER_ENTRY_SIZE;
  const offsetLoader = offset472 + count472 * LOADER_ENTRY_SIZE;
  const dataOffset = offsetLoader + countLoader * LOADER_ENTRY_SIZE;
  const total = dataOffset + segments.reduce((sum, s) => sum + s.size, 0);

  const bytes = new Uint8Array(total);
  const view = new DataView(bytes.buffer);

  view.setUint32(0, LOADER_TAG_BOOT, true);
  view.setUint16(4, LOADER_HEADER_SIZE, true);
  view.setUint32(6, 0x00010006, true);
  view.setUint32(10, 0x01030000, true);
  view.setUint32(21, RK3588_CHIP_TAG, true);
  view.setUint8(25, count471);
  view.setUint32(26, offset471, true);
  view.setUint8(30, LOADER_ENTRY_SIZE);
  view.setUint8(31, count472);
  view.setUint32(32, offset472, true);
  view.setUint8(36, LOADER_ENTRY_SIZE);
  view.setUint8(37, countLoader);
  view.setUint32(38, offsetLoader, true);
  view.setUint8(42, LOADER_ENTRY_SIZE);
  view.setUint8(43, 0x4e); // signFlag 'N'
  view.setUint8(44, 0); // fixture stores plaintext FlashHead

  let seen471 = 0;
  let seen472 = 0;
  let seenLoader = 0;
  let dataPos = dataOffset;
  for (const segment of segments) {
    let slot;
    let type;
    if (segment.request === 0x0471) {
      slot = offset471 + seen471++ * LOADER_ENTRY_SIZE;
      type = ENTRY_471;
    } else if (segment.request === 0x0472) {
      slot = offset472 + seen472++ * LOADER_ENTRY_SIZE;
      type = ENTRY_472;
    } else {
      slot = offsetLoader + seenLoader++ * LOADER_ENTRY_SIZE;
      type = ENTRY_LOADER;
    }
    view.setUint8(slot, LOADER_ENTRY_SIZE);
    view.setUint32(slot + 1, type, true);
    writeName(view, slot + 5, segment.name);
    view.setUint32(slot + 45, dataPos, true);
    view.setUint32(slot + 49, segment.size, true);
    view.setUint32(slot + 53, 0, true);
    for (let i = 0; i < segment.size; i += 1) {
      bytes[dataPos + i] = (segment.seed + i) & 0xff;
    }
    if (segment.name === 'FlashHead') {
      bytes.set([0x52, 0x4b, 0x4e, 0x53], dataPos); // "RKNS"
    }
    dataPos += segment.size;
  }

  const crc = crc32Rockchip(bytes.subarray(0, bytes.length - 4));
  view.setUint32(bytes.length - 4, crc, true);
  return bytes;
}
