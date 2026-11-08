// Language selection and message lookup for the flasher page.
//
// Two rules drive this module:
//
//   - the language is picked by walking navigator.languages in order: a `zh*`
//     tag means Chinese, an `en*` tag means English, and the first tag that
//     names either one wins (so a browser set to [fr, en-US] gets English and
//     [fr, zh-CN] gets Chinese);
//   - anything else — no navigator, no supported tag — falls back to English.
//
// Copy lives in ./locales/<lang>.js as one flat object per language, so adding
// a language is one file, one entry in SUPPORTED, and one entry in sw.js's
// PRECACHE. No module keeps a user-visible string of its own; test/i18n.test.mjs
// fails if one creeps back in.
//
// `?lang=en` / `?lang=zh` overrides detection. It is not a UI affordance: it
// exists so an auditor can see both languages without touching browser
// settings.

import en from './locales/en.js';
import zh from './locales/zh.js';

/** SUPPORTED lists the languages this build can render. */
export const SUPPORTED = ['zh', 'en'];

/** FALLBACK is what an unsupported browser language resolves to. */
export const FALLBACK = 'en';

const MESSAGES = { zh, en };

/** LANG_TAGS is what <html lang> advertises for each locale. */
const LANG_TAGS = { zh: 'zh-CN', en: 'en' };

export function isSupported(locale) {
  return SUPPORTED.includes(locale);
}

/**
 * matchLocale walks a language list in order and returns the first supported
 * base language. Region and script subtags are folded: 'zh-Hant-TW' → zh,
 * 'en-GB' → en. Unknown tags are skipped, not fatal.
 */
export function matchLocale(languages = []) {
  const list = Array.isArray(languages) ? languages : [languages];
  for (const raw of list) {
    const tag = String(raw ?? '')
      .trim()
      .toLowerCase();
    if (!tag) continue;
    if (tag.startsWith('zh')) return 'zh';
    if (tag.startsWith('en')) return 'en';
  }
  return FALLBACK;
}

/** detectLocale reads the browser's ordered preference list. */
export function detectLocale(nav = globalThis.navigator) {
  const preferred = Array.isArray(nav?.languages) ? nav.languages : [];
  const list = preferred.length > 0 ? preferred : [nav?.language];
  return matchLocale(list.filter(Boolean));
}

/**
 * resolveLocale is detectLocale with the `?lang=` override applied first.
 * Kept pure (takes the query string and navigator) so it is testable off-DOM.
 */
export function resolveLocale({ query = '', navigator: nav } = {}) {
  const asked = String(new URLSearchParams(query).get('lang') ?? '').toLowerCase();
  if (asked.startsWith('zh')) return 'zh';
  if (asked.startsWith('en')) return 'en';
  return detectLocale(nav);
}

let current = FALLBACK;

/** setLocale switches the active language; unsupported values fall back. */
export function setLocale(locale) {
  current = isSupported(locale) ? locale : FALLBACK;
  return current;
}

export function getLocale() {
  return current;
}

/** langTag is the BCP-47 tag <html lang> should carry for the active locale. */
export function langTag(locale = current) {
  return LANG_TAGS[locale] ?? LANG_TAGS[FALLBACK];
}

/** localeKeys exists for the resource-parity test. */
export function localeKeys(locale = current) {
  return Object.keys(MESSAGES[locale] ?? {});
}

/** hasMessage reports whether a key exists, so callers can fall back to raw ids. */
export function hasMessage(key, locale = current) {
  return Object.prototype.hasOwnProperty.call(MESSAGES[locale] ?? {}, key);
}

function interpolate(template, params) {
  if (!params) return template;
  // `{name}` placeholders only: a message may contain braces for other reasons.
  return template.replace(/\{(\w+)\}/g, (match, name) =>
    Object.prototype.hasOwnProperty.call(params, name) ? String(params[name]) : match,
  );
}

/**
 * t looks one message up.
 *
 * `count` also selects a `_one` singular variant when the language defines one
 * (English does, Chinese does not — its nouns do not inflect).
 *
 * A key missing from the active language falls back to English, then to the key
 * itself: a visible key in a screenshot is a better failure than a blank
 * button, and the parity test catches it before release.
 */
export function t(key, params) {
  const table = MESSAGES[current] ?? MESSAGES[FALLBACK];
  const fallback = MESSAGES[FALLBACK];
  const singular = params && Number(params.count) === 1 ? `${key}_one` : null;
  // Active language first (singular, then plural), then the fallback language,
  // then the key itself.
  const text =
    (singular != null ? table[singular] : undefined) ??
    table[key] ??
    (singular != null ? fallback[singular] : undefined) ??
    fallback[key];
  if (text == null) return key;
  return interpolate(text, params);
}

/**
 * applyDocument translates the static page:
 *   [data-i18n]             textContent
 *   [data-i18n-aria-label]  aria-label
 *   [data-i18n-title]       title
 * and sets <html lang> plus the document title.
 */
export function applyDocument(root = globalThis.document) {
  if (!root?.querySelectorAll) return;
  root.documentElement?.setAttribute('lang', langTag());
  root.title = t('app.title');
  for (const node of root.querySelectorAll('[data-i18n]')) {
    node.textContent = t(node.dataset.i18n);
  }
  for (const node of root.querySelectorAll('[data-i18n-aria-label]')) {
    node.setAttribute('aria-label', t(node.dataset.i18nAriaLabel));
  }
  for (const node of root.querySelectorAll('[data-i18n-title]')) {
    node.setAttribute('title', t(node.dataset.i18nTitle));
  }
}
