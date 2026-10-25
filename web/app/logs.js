// The log panel's backing store.
//
// Every operation on the page writes here — device plug/unplug, commands,
// transfer summaries, verification progress, downloads, errors. The rules that
// matter are small but easy to get wrong, so they are a pure module:
//
//   - an entry always has a timestamp, a level and a source, and errors are
//     never dropped on the floor (any catch writes one);
//   - the in-memory ring is bounded, and the number of dropped entries is
//     reported instead of silently losing history;
//   - both exports are produced from the same entries: a human-readable text
//     log and the NDJSON shape the CLI's `uf --log` writes, so a failing write
//     can be mailed in either form.

export const LEVELS = ['debug', 'info', 'warn', 'error'];

const LEVEL_RANK = { debug: 0, info: 1, warn: 2, error: 3 };

function normaliseLevel(level) {
  const value = String(level ?? 'info').toLowerCase();
  return LEVELS.includes(value) ? value : 'info';
}

function pad(text, width) {
  return String(text).padEnd(width, ' ');
}

/** createLogStore returns the bounded, exportable log the page renders. */
export function createLogStore({ limit = 5000, now = () => new Date(), onChange = null } = {}) {
  const entries = [];
  let dropped = 0;
  let seq = 0;
  const listeners = new Set();
  if (typeof onChange === 'function') listeners.add(onChange);

  function notify(added) {
    for (const listener of listeners) {
      try {
        listener(added, api);
      } catch {
        // A broken renderer must not take the log down with it.
      }
    }
  }

  function add(level, source, text, meta = null) {
    const entry = {
      seq: (seq += 1),
      ts: now().toISOString(),
      level: normaliseLevel(level),
      source: String(source ?? 'app'),
      text: String(text ?? ''),
    };
    if (meta && typeof meta === 'object') entry.meta = meta;
    entries.push(entry);
    if (entries.length > limit) {
      const drop = entries.length - limit;
      entries.splice(0, drop);
      dropped += drop;
    }
    notify(entry);
    return entry;
  }

  const api = {
    add,
    debug: (source, text, meta) => add('debug', source, text, meta),
    info: (source, text, meta) => add('info', source, text, meta),
    warn: (source, text, meta) => add('warn', source, text, meta),
    error: (source, text, meta) => add('error', source, text, meta),

    get size() {
      return entries.length;
    },
    get dropped() {
      return dropped;
    },
    entries: () => entries.slice(),
    /** filtered returns the entries at or above `minLevel`. */
    filtered(minLevel = 'debug') {
      const floor = LEVEL_RANK[normaliseLevel(minLevel)] ?? 0;
      return entries.filter((entry) => (LEVEL_RANK[entry.level] ?? 0) >= floor);
    },
    countByLevel() {
      const counts = { debug: 0, info: 0, warn: 0, error: 0 };
      for (const entry of entries) counts[entry.level] = (counts[entry.level] ?? 0) + 1;
      return counts;
    },
    /** subscribe registers a renderer; returns an unsubscribe function. */
    subscribe(listener) {
      if (typeof listener !== 'function') return () => {};
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    clear() {
      entries.length = 0;
      dropped = 0;
      notify(null);
    },
    /**
     * toText renders one line per entry:
     *   2026-10-25T19:20:29.123Z  info   [device] 授权 ...
     */
    toText() {
      const lines = entries.map(
        (entry) => `${entry.ts}  ${pad(entry.level, 5)}  [${entry.source}] ${entry.text}`,
      );
      if (dropped > 0) lines.unshift(`# 已丢弃最早的 ${dropped} 条日志（内存上限 ${limit} 条）`);
      return lines.join('\n');
    },
    /**
     * toNdjson renders the same entries as NDJSON, with a header record that
     * says where they came from — the shape docs/pwa-offline.md tells users to
     * attach to an issue.
     */
    toNdjson(header = {}) {
      const lines = [
        JSON.stringify({
          ts: now().toISOString(),
          type: 'header',
          app: 'rk3588-webflasher',
          entries: entries.length,
          dropped,
          ...header,
        }),
      ];
      for (const entry of entries) {
        lines.push(
          JSON.stringify({
            ts: entry.ts,
            type: 'log',
            level: entry.level,
            source: entry.source,
            text: entry.text,
            ...(entry.meta ? { meta: entry.meta } : {}),
          }),
        );
      }
      if (dropped > 0) lines.push(JSON.stringify({ ts: now().toISOString(), type: 'truncated', dropped }));
      return `${lines.join('\n')}\n`;
    },
  };

  return api;
}
