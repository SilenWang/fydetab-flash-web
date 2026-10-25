// The pre-flight checklist and the "no CBW before db" gate.
//
// These tests pin the two rules that cost real hardware attempts: a loader that
// is not the image's own is refused, and a read command cannot be sent from a
// bare maskrom before Download Boot.

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  evaluatePreflight,
  readOnlyGate,
  isMaskrom,
  isLoader,
  currentDevice,
  formatBytes,
  CHECK_MASKROM,
  CHECK_LOADER,
  CHECK_PLAN,
  CHECK_FIRST_COMMAND,
} from '../preflight.js';

const maskrom = {
  devNo: 1,
  vendorId: 0x2207,
  productId: 0x350b,
  bcdUSB: 0x0200,
  mode: 'MASKROM',
  modeLabel: 'Maskrom',
};

const loaderDevice = { ...maskrom, devNo: 2, bcdUSB: 0x0201, mode: 'LOADER', modeLabel: 'Loader' };

const goodLoader = {
  source: 'image',
  chip: 'RK3588',
  imageChip: 'RK3588',
  chipCheck: 'ok',
  length: 422336,
  sha256: '24c1d188aabbccddeeff00112233445566778899aabbccddeeff001122334455',
  buildTime: '2024-09-06 12:34:56 UTC',
};

const badLoader = { ...goodLoader, chip: 'RK3566', chipCheck: 'mismatch' };

const facts = {
  chip: 'RK3588',
  items: 7,
  totalBytes: 1 << 30,
  uncoveredBytes: 4 << 30,
  uncoveredTails: 1,
  uncoveredParts: 'state',
  eraseMode: 'none',
};

function byId(result, id) {
  return result.items.find((item) => item.id === id);
}

test('mode helpers follow the bcdUSB bit-0 convention', () => {
  assert.equal(isMaskrom(maskrom), true);
  assert.equal(isMaskrom(loaderDevice), false);
  assert.equal(isLoader(loaderDevice), true);
  assert.equal(isMaskrom({ ...maskrom, bcdUSB: 0x0201 }), false);
  assert.equal(currentDevice([maskrom], null)?.devNo, 1);
  assert.equal(currentDevice([maskrom, loaderDevice], null), null);
  assert.equal(currentDevice([maskrom, loaderDevice], 2)?.devNo, 2);
});

test('a bare maskrom may not send any CBW before db', () => {
  const gate = readOnlyGate({ device: maskrom, dbDone: false });
  assert.equal(gate.enabled, false);
  assert.match(gate.reason, /第一条命令必须是 db/);
  assert.match(gate.reason, /弄脏/);

  // The same board after Download Boot is safe to read.
  assert.equal(readOnlyGate({ device: loaderDevice, dbDone: true }).enabled, true);
  // No device at all: still closed, with a different reason.
  assert.equal(readOnlyGate({ device: null, dbDone: false }).enabled, false);
});

test('the checklist passes only when every pre-flight fact holds', () => {
  const result = evaluatePreflight({
    devices: [maskrom],
    current: 1,
    loader: goodLoader,
    facts,
    eraseMode: 'none',
    eraseConfirmed: true,
    dbDone: false,
  });
  assert.equal(result.ready, true);
  assert.equal(result.canWrite, true);
  assert.equal(result.canDb, true);
  assert.equal(result.readOnly.enabled, false, 'maskrom read-only must stay disabled');
  assert.equal(byId(result, CHECK_MASKROM).status, 'pass');
  assert.equal(byId(result, CHECK_LOADER).status, 'pass');
  assert.equal(byId(result, CHECK_PLAN).status, 'pass');
  assert.equal(byId(result, CHECK_FIRST_COMMAND).status, 'pass');
  assert.match(byId(result, CHECK_PLAN).detail, /state/);
  assert.match(byId(result, CHECK_PLAN).detail, /none/);
});

test('the Go device state (numeric mode + modeLabel) passes the same gates', () => {
  // internal/device serialises MediaMode as a number and adds modeLabel; the
  // page used to read only `mode`, so a real Loader board looked like neither
  // maskrom nor LOADER and the write button never unlocked.
  const realLoader = {
    devNo: 1,
    vendorId: 0x2207,
    productId: 0x350b,
    bcdUSB: 0x0201,
    mode: 2,
    modeLabel: 'Loader',
  };
  assert.equal(isLoader(realLoader), true);
  assert.equal(isMaskrom(realLoader), false);
  const result = evaluatePreflight({
    devices: [realLoader],
    current: 1,
    loader: goodLoader,
    facts,
    eraseMode: 'none',
    eraseConfirmed: true,
  });
  assert.equal(result.ready, true);
  assert.equal(result.canWrite, true);
  assert.equal(result.canDb, false);
  assert.equal(result.readOnly.enabled, true);
  assert.equal(byId(result, CHECK_MASKROM).status, 'pass');
  assert.equal(byId(result, CHECK_FIRST_COMMAND).status, 'pass');

  const realMaskrom = { ...realLoader, bcdUSB: 0x0200, mode: 1, modeLabel: 'Maskrom' };
  assert.equal(isMaskrom(realMaskrom), true);
  assert.equal(isLoader(realMaskrom), false);
});

test('a loader built for another chip blocks the write', () => {
  const result = evaluatePreflight({
    devices: [maskrom],
    current: 1,
    loader: badLoader,
    facts,
    eraseMode: 'none',
    eraseConfirmed: true,
  });
  assert.equal(result.ready, false);
  assert.equal(result.canWrite, false);
  assert.equal(result.canDb, false);
  assert.ok(result.blocking.includes(CHECK_LOADER));
  assert.ok(result.blocking.includes(CHECK_FIRST_COMMAND));
  assert.match(byId(result, CHECK_LOADER).detail, /index 0/);
});

test('an uncovered tail is not silently erased', () => {
  const unconfirmed = evaluatePreflight({
    devices: [maskrom],
    current: 1,
    loader: goodLoader,
    facts,
    eraseMode: 'none',
    eraseConfirmed: false,
  });
  assert.equal(unconfirmed.ready, false);
  assert.ok(unconfirmed.blocking.includes(CHECK_PLAN));
  assert.match(byId(unconfirmed, CHECK_PLAN).detail, /显式选择/);

  // The choice itself is free; the confirmation is what makes it explicit.
  const confirmed = evaluatePreflight({
    devices: [maskrom],
    current: 1,
    loader: goodLoader,
    facts: { ...facts, eraseMode: 'fill' },
    eraseMode: 'fill',
    eraseConfirmed: true,
  });
  assert.equal(confirmed.ready, true);
  assert.match(byId(confirmed, CHECK_PLAN).detail, /fill/);
});

test('without a plan or a device the checklist stays pending, not failed', () => {
  const result = evaluatePreflight({ devices: [], current: null });
  assert.equal(result.ready, false);
  assert.equal(result.canWrite, false);
  assert.equal(byId(result, CHECK_MASKROM).status, 'pending');
  assert.equal(byId(result, CHECK_LOADER).status, 'pending');
  assert.equal(byId(result, CHECK_PLAN).status, 'pending');
  assert.equal(byId(result, CHECK_FIRST_COMMAND).status, 'pending');
});

test('a plan with nothing to write fails the plan check', () => {
  const result = evaluatePreflight({
    devices: [maskrom],
    current: 1,
    loader: goodLoader,
    facts: { ...facts, items: 0 },
    eraseConfirmed: true,
  });
  assert.equal(result.ready, false);
  assert.equal(byId(result, CHECK_PLAN).status, 'fail');
});

test('an already-bootstrapped LOADER device allows reads and writes', () => {
  const result = evaluatePreflight({
    devices: [loaderDevice],
    current: 1,
    loader: goodLoader,
    facts,
    eraseMode: 'none',
    eraseConfirmed: true,
    dbDone: true,
  });
  assert.equal(result.readOnly.enabled, true);
  assert.equal(result.canWrite, true);
  assert.equal(byId(result, CHECK_FIRST_COMMAND).status, 'pass');
  // In LOADER the maskrom item is satisfied by the earlier db.
  assert.equal(byId(result, CHECK_MASKROM).status, 'pass');
});

test('a run in progress keeps the write button disabled', () => {
  const result = evaluatePreflight({
    devices: [loaderDevice],
    current: 1,
    loader: goodLoader,
    facts,
    eraseMode: 'none',
    eraseConfirmed: true,
    dbDone: true,
    runInProgress: true,
  });
  assert.equal(result.canWrite, false);
});

test('formatBytes matches the CLI units', () => {
  assert.equal(formatBytes(512), '512 B');
  assert.equal(formatBytes(2 * 1024 * 1024), '2.00 MiB');
  assert.equal(formatBytes(4 * 1024 * 1024 * 1024), '4.00 GiB');
  assert.equal(formatBytes(-1), '?');
});
