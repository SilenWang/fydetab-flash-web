#!/usr/bin/env node
// Minimal static file server for the M0 probe page.
//
// WebUSB needs a secure context; http://localhost counts as one, file:// does
// not (ES modules and module workers are blocked there). No dependencies, no
// watcher: just serve ./web with no-cache headers so edits (and the service
// worker) show up on reload.

import { createServer } from 'node:http';
import { createReadStream } from 'node:fs';
import { stat } from 'node:fs/promises';
import { extname, join, normalize, resolve, sep } from 'node:path';

const root = resolve(process.argv[2] ?? 'web/probe');
const port = Number(process.argv[3] ?? process.env.PORT ?? 5173);
const host = process.env.HOST ?? '127.0.0.1';

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.webmanifest': 'application/manifest+json; charset=utf-8',
  '.wasm': 'application/wasm',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.map': 'application/json; charset=utf-8',
};

function resolvePath(urlPath) {
  const decoded = decodeURIComponent(urlPath.split('?')[0]);
  const candidate = normalize(join(root, decoded));
  if (candidate !== root && !candidate.startsWith(root + sep)) return null;
  return candidate;
}

const server = createServer(async (req, res) => {
  let path = resolvePath(req.url ?? '/');
  if (!path) {
    res.writeHead(403, { 'content-type': 'text/plain; charset=utf-8' });
    res.end('Forbidden');
    return;
  }

  try {
    let info = await stat(path);
    if (info.isDirectory()) {
      path = join(path, 'index.html');
      info = await stat(path);
    }
    res.writeHead(200, {
      'content-type': MIME[extname(path)] ?? 'application/octet-stream',
      'content-length': info.size,
      'cache-control': 'no-store',
      // WebUSB itself does not need COOP/COEP, but they keep the page ready
      // for a wasm worker in M1 without a second round of debugging.
      'cross-origin-opener-policy': 'same-origin',
      'cross-origin-embedder-policy': 'require-corp',
      'x-content-type-options': 'nosniff',
    });
    createReadStream(path).pipe(res);
  } catch (err) {
    const status = err.code === 'ENOENT' ? 404 : 500;
    res.writeHead(status, { 'content-type': 'text/plain; charset=utf-8' });
    res.end(status === 404 ? `Not found: ${req.url}` : `Server error: ${err.message}`);
  }
});

server.listen(port, host, () => {
  console.log(`Fydetab webflasher: http://localhost:${port}/app/  (M0 probe: /probe/)`);
  console.log(`serving ${root}`);
  console.log('Chrome/Edge only. Press Ctrl+C to stop.');
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    server.close(() => process.exit(0));
  });
}
