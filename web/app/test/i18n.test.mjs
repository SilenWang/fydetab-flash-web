// Language selection and the copy resources.
//
// Three things are pinned here, because each one fails silently in a browser:
//
//   - which language a browser gets. navigator.languages is ordered by
//     preference, so the first tag that names zh or en wins, and a list with
//     neither falls back to English;
//   - the two resources say the same things. A key present in one language and
//     missing in the other would show a raw key in a button;
//   - no page module keeps copy of its own. A hardcoded sentence is how a
//     "bilingual" page ends up half translated.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, resolve } from 'node:path';

import {
  FALLBACK,
  SUPPORTED,
  applyDocument,
  detectLocale,
  hasMessage,
  langTag,
  localeKeys,
  matchLocale,
  resolveLocale,
  setLocale,
  t,
} from '../i18n.js';
import en from '../locales/en.js';
import zh from '../locales/zh.js';

const APP_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const read = (name) => readFileSync(join(APP_DIR, name), 'utf8');

// ---------------------------------------------------------------- matching

test('navigator.languages is walked in order: zh* → zh, en* → en', () => {
  assert.equal(matchLocale(['zh-CN']), 'zh');
  assert.equal(matchLocale(['zh']), 'zh');
  assert.equal(matchLocale(['zh-Hant-TW']), 'zh');
  assert.equal(matchLocale(['en-US']), 'en');
  assert.equal(matchLocale(['en-GB']), 'en');

  // Order matters: the first supported tag wins, not the first tag.
  assert.equal(matchLocale(['fr-FR', 'en-US']), 'en');
  assert.equal(matchLocale(['fr-FR', 'zh-CN']), 'zh');
  assert.equal(matchLocale(['en-US', 'zh-CN']), 'en');
  assert.equal(matchLocale(['zh-CN', 'en-US']), 'zh');

  // Nothing supported: English.
  assert.equal(matchLocale(['de', 'ja-JP']), FALLBACK);
  assert.equal(matchLocale([]), FALLBACK);
  assert.equal(matchLocale(undefined), FALLBACK);
  // Junk in the list is skipped, not fatal.
  assert.equal(matchLocale([null, '  ', 'zh-HK']), 'zh');
});

test('detectLocale reads navigator.languages, then language, then falls back', () => {
  assert.equal(detectLocale({ languages: ['fr', 'en-NZ'] }), 'en');
  assert.equal(detectLocale({ languages: ['zh-TW'], language: 'en-US' }), 'zh');
  assert.equal(detectLocale({ languages: [], language: 'zh-CN' }), 'zh');
  assert.equal(detectLocale({}), FALLBACK);
  // `null` stands for "there is no navigator"; bare `undefined` would fall back
  // to this Node process's own navigator, which is whatever the host runs.
  assert.equal(detectLocale(null), FALLBACK);
});

test('?lang= overrides detection without changing the rules', () => {
  const nav = { languages: ['zh-CN'] };
  assert.equal(resolveLocale({ query: '?lang=en', navigator: nav }), 'en');
  assert.equal(resolveLocale({ query: '?lang=zh', navigator: nav }), 'zh');
  assert.equal(resolveLocale({ query: '?lang=en-GB', navigator: nav }), 'en');
  assert.equal(resolveLocale({ query: '?foo=1', navigator: nav }), 'zh');
  assert.equal(resolveLocale({ query: '?lang=fr', navigator: { languages: ['fr'] } }), FALLBACK);
});

test('setLocale refuses anything unsupported and reports the active one', () => {
  assert.equal(setLocale('zh'), 'zh');
  assert.equal(langTag(), 'zh-CN');
  assert.equal(t('tile.image'), '选择系统');
  assert.equal(setLocale('en'), 'en');
  assert.equal(langTag(), 'en');
  assert.equal(t('tile.image'), 'Choose an image');
  assert.equal(setLocale('fr'), FALLBACK);
  assert.equal(langTag(), 'en');
  assert.deepEqual(SUPPORTED, ['zh', 'en']);
});

// -------------------------------------------------------------- resources

test('both languages define the same keys', () => {
  const zhKeys = new Set(Object.keys(zh));
  const enKeys = new Set(Object.keys(en));

  // English needs `_one` singular variants; Chinese nouns do not inflect, so
  // those keys are allowed to exist on one side only.
  const missingInEn = [...zhKeys].filter((key) => !enKeys.has(key));
  assert.deepEqual(missingInEn, [], `en.js is missing: ${missingInEn.join(', ')}`);

  const missingInZh = [...enKeys].filter((key) => !zhKeys.has(key) && !key.endsWith('_one'));
  assert.deepEqual(missingInZh, [], `zh.js is missing: ${missingInZh.join(', ')}`);

  // A `_one` key in Chinese is a mistake: t() would never need it.
  const singularInZh = [...zhKeys].filter((key) => key.endsWith('_one'));
  assert.deepEqual(singularInZh, [], `zh.js has English-only singular keys: ${singularInZh.join(', ')}`);
});

test('a key uses the same placeholders in both languages', () => {
  const placeholders = (text) => [...String(text).matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();
  for (const key of Object.keys(zh)) {
    assert.deepEqual(
      placeholders(en[key]),
      placeholders(zh[key]),
      `${key} has different placeholders: en=${JSON.stringify(en[key])} zh=${JSON.stringify(zh[key])}`,
    );
  }
});

test('every resource entry is a non-empty string', () => {
  for (const [locale, table] of [
    ['en', en],
    ['zh', zh],
  ]) {
    for (const [key, value] of Object.entries(table)) {
      assert.equal(typeof value, 'string', `${locale}.${key} is not a string`);
      assert.ok(value.trim().length > 0, `${locale}.${key} is empty`);
    }
  }
});

test('the page modules keep no copy of their own', () => {
  // Comments explain the code in the repository's working language; the
  // string literals are what a user reads, and those must come from locales/.
  const stripComments = (source) =>
    source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|\s)\/\/[^\n]*/g, '$1');

  const modules = [
    'app.js',
    'brand.js',
    'devices.js',
    'i18n.js',
    'images.js',
    'logs.js',
    'preflight.js',
    'usb-host.js',
    'verify.js',
    'worker.js',
    'zip.js',
    'zip-source.js',
  ];
  const cjk = /[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]/;
  const offenders = [];
  for (const name of modules) {
    stripComments(read(name))
      .split('\n')
      .forEach((line, index) => {
        if (cjk.test(line)) offenders.push(`${name}:${index + 1}: ${line.trim()}`);
      });
  }
  assert.deepEqual(offenders, [], `these lines carry hardcoded copy:\n${offenders.join('\n')}`);
});

test('index.html carries no copy of its own either', () => {
  const html = read('index.html');
  assert.equal(
    /[\u4e00-\u9fff]/.test(html),
    false,
    'index.html must be translated from resources, not authored in one language',
  );
  // The static strings a person sees are marked for applyDocument().
  assert.match(html, /<html lang="en">/);
  assert.match(html, /data-i18n="app\.title"|data-i18n="tile\.image"/);
  for (const key of ['app.heading', 'tile.image', 'tile.device', 'log.title', 'dialog.imageTitle']) {
    assert.ok(html.includes(`data-i18n="${key}"`), `index.html is missing data-i18n="${key}"`);
  }
});

// ----------------------------------------------------------------- lookup

test('t() interpolates, falls back and never returns undefined', () => {
  setLocale('en');
  assert.equal(t('device.count', { count: 2 }), '2 devices');
  assert.equal(t('device.count', { count: 1 }), '1 device');
  assert.equal(t('image.selected', { name: 'a.img', size: '8 GiB', source: 'local file' }), 'Image selected: a.img (8 GiB, source: local file)');
  // An unknown placeholder is left alone rather than printing "undefined".
  assert.equal(t('device.count', {}), '{count} devices');
  // A missing key is visible, not a blank.
  assert.equal(t('no.such.key'), 'no.such.key');
  assert.equal(hasMessage('tile.image'), true);
  assert.equal(hasMessage('no.such.key'), false);

  setLocale('zh');
  assert.equal(t('device.count', { count: 3 }), '3 台设备');
  assert.equal(t('tile.image'), '选择系统');
  assert.equal(localeKeys('zh').length > 100, true);
});

test('applyDocument translates text, aria-labels and the document language', () => {
  const nodes = [
    { dataset: { i18n: 'tile.image' }, textContent: '' },
    { dataset: { i18nAriaLabel: 'log.expand' }, attrs: {}, setAttribute(name, value) { this.attrs[name] = value; } },
    { dataset: { i18nTitle: 'reconnect.title' }, attrs: {}, setAttribute(name, value) { this.attrs[name] = value; } },
  ];
  const documentElement = {
    attrs: {},
    setAttribute(name, value) {
      this.attrs[name] = value;
    },
  };
  const fakeDocument = {
    documentElement,
    title: '',
    querySelectorAll: (selector) => {
      if (selector === '[data-i18n]') return [nodes[0]];
      if (selector === '[data-i18n-aria-label]') return [nodes[1]];
      if (selector === '[data-i18n-title]') return [nodes[2]];
      return [];
    },
  };

  setLocale('zh');
  applyDocument(fakeDocument);
  assert.equal(documentElement.attrs.lang, 'zh-CN');
  assert.equal(fakeDocument.title, 'Fydetab Duo 刷写工具');
  assert.equal(nodes[0].textContent, '选择系统');
  assert.equal(nodes[1].attrs['aria-label'], '展开日志');
  assert.equal(nodes[2].attrs.title, '设备已断开');

  setLocale('en');
  applyDocument(fakeDocument);
  assert.equal(documentElement.attrs.lang, 'en');
  assert.equal(nodes[0].textContent, 'Choose an image');
});
