// The log panel's store: levels, timestamps, the bounded ring, and the two
// export shapes. A failing write is diagnosed from these exports, so a dropped
// or mangled entry is a real bug: an error that is not in the log did not
// happen, as far as the person reading the issue is concerned.

import test from 'node:test';
import assert from 'node:assert/strict';

import { createLogStore, LEVELS } from '../logs.js';

function fixedClock(start = '2026-10-25T19:20:29.123Z') {
  let t = new Date(start).getTime();
  return () => {
    const value = new Date(t);
    t += 1000;
    return value;
  };
}

test('an entry always has a timestamp, a level and a source', () => {
  const store = createLogStore({ now: fixedClock() });
  const entry = store.add('info', 'device', '授权 0x2207');
  assert.equal(entry.ts, '2026-10-25T19:20:29.123Z');
  assert.equal(entry.level, 'info');
  assert.equal(entry.source, 'device');
  assert.equal(entry.text, '授权 0x2207');
  assert.equal(entry.seq, 1);

  // Unknown levels fall back to info instead of creating a level the UI
  // cannot colour.
  assert.equal(store.add('trace', 'app', 'x').level, 'info');
  assert.deepEqual(LEVELS, ['debug', 'info', 'warn', 'error']);
  assert.equal(store.size, 2);
});

test('errors survive with their text and metadata intact', () => {
  const store = createLogStore({ now: fixedClock() });
  store.error('write', 'WRITE_FAILED: bulk OUT endpoint 2: stall', { code: -1 });
  const entry = store.entries().at(-1);
  assert.equal(entry.level, 'error');
  assert.match(entry.text, /stall/);
  assert.deepEqual(entry.meta, { code: -1 });
  assert.deepEqual(store.countByLevel(), { debug: 0, info: 0, warn: 0, error: 1 });
});

test('the ring is bounded and reports how much it dropped', () => {
  const store = createLogStore({ limit: 3, now: fixedClock() });
  for (let i = 0; i < 5; i += 1) store.info('app', `line ${i}`);
  assert.equal(store.size, 3);
  assert.equal(store.dropped, 2);
  assert.deepEqual(store.entries().map((entry) => entry.text), ['line 2', 'line 3', 'line 4']);
  assert.match(store.toText().split('\n')[0], /已丢弃最早的 2 条/);
});

test('filtered returns entries at or above the requested level', () => {
  const store = createLogStore({ now: fixedClock() });
  store.debug('app', 'd');
  store.info('app', 'i');
  store.warn('app', 'w');
  store.error('app', 'e');
  assert.deepEqual(store.filtered('warn').map((entry) => entry.text), ['w', 'e']);
  assert.deepEqual(store.filtered('debug').map((entry) => entry.text), ['d', 'i', 'w', 'e']);
});

test('the text export is one timestamped line per entry', () => {
  const store = createLogStore({ now: fixedClock() });
  store.info('device', 'USB connect');
  store.error('write', 'failed');
  const lines = store.toText().split('\n');
  assert.equal(lines.length, 2);
  assert.match(lines[0], /^2026-10-25T19:20:29\.123Z {2}info +\[device\] USB connect$/);
  assert.match(lines[1], /error +\[write\] failed$/);
});

test('the NDJSON export parses back, header first', () => {
  const store = createLogStore({ now: fixedClock() });
  store.info('image', '选择镜像');
  store.warn('download', 'CORS 被拦');
  const text = store.toNdjson({ mode: 'mock', image: 'demo-update.img' });
  assert.ok(text.endsWith('\n'));
  const records = text.trim().split('\n').map((line) => JSON.parse(line));
  assert.equal(records[0].type, 'header');
  assert.equal(records[0].mode, 'mock');
  assert.equal(records[0].image, 'demo-update.img');
  assert.equal(records[0].entries, 2);
  assert.deepEqual(records.slice(1).map((record) => record.level), ['info', 'warn']);
  assert.equal(records[1].source, 'image');
  assert.match(records[1].ts, /^2026-10-25T/);
});

test('a truncated export says what was dropped', () => {
  const store = createLogStore({ limit: 1, now: fixedClock() });
  store.info('app', 'old');
  store.info('app', 'new');
  const records = store.toNdjson().trim().split('\n').map((line) => JSON.parse(line));
  assert.equal(records.at(-1).type, 'truncated');
  assert.equal(records.at(-1).dropped, 1);
});

test('subscribers see every entry and the clear', () => {
  const store = createLogStore({ now: fixedClock() });
  const seen = [];
  const unsubscribe = store.subscribe((entry) => seen.push(entry ? entry.text : null));
  store.info('app', 'one');
  store.info('app', 'two');
  store.clear();
  unsubscribe();
  store.info('app', 'three');
  assert.deepEqual(seen, ['one', 'two', null]);
  assert.equal(store.size, 1, 'an unsubscribed renderer must not stop the log');
  assert.equal(store.dropped, 0);
});

test('a broken renderer cannot take the log down with it', () => {
  const store = createLogStore({ now: fixedClock() });
  store.subscribe(() => {
    throw new Error('render blew up');
  });
  store.info('app', 'still logged');
  assert.equal(store.size, 1);
});
