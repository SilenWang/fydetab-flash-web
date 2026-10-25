#!/usr/bin/env node
// Build the Go/WASM core and copy the matching wasm_exec.js next to it.
//
// wasm_exec.js is version-coupled to the Go toolchain that produced the .wasm
// (its import object must match), so it is always copied out of `go env
// GOROOT` rather than vendored. Both files land in web/app/wasm/, which the
// worker imports.
//
// Usage: pixi run wasm   (or: node tools/build-wasm.mjs [--out <dir>])

import { execFileSync } from 'node:child_process';
import { copyFileSync, existsSync, mkdirSync, statSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const outIndex = process.argv.indexOf('--out');
const outDir = resolve(root, outIndex > 0 ? process.argv[outIndex + 1] : join('web', 'app', 'wasm'));

function run(args, env = {}) {
  return execFileSync('go', args, { cwd: root, encoding: 'utf8', env: { ...process.env, ...env } }).trim();
}

const goroot = run(['env', 'GOROOT']);
const version = run(['version']);
if (!goroot) throw new Error('go env GOROOT returned nothing; is Go installed through pixi?');

// Go 1.21+ moved wasm_exec.js to lib/wasm; older trees kept it in misc/wasm.
const candidates = [join(goroot, 'lib', 'wasm', 'wasm_exec.js'), join(goroot, 'misc', 'wasm', 'wasm_exec.js')];
const execSource = candidates.find((p) => existsSync(p));
if (!execSource) {
  throw new Error(`wasm_exec.js not found under ${goroot} (looked in lib/wasm and misc/wasm)`);
}

mkdirSync(outDir, { recursive: true });

const wasmPath = join(outDir, 'rk3588.wasm');
console.log(`building ${wasmPath} (${version})`);
run(['build', '-trimpath', '-ldflags=-s -w', '-o', wasmPath, './cmd/wasm'], {
  GOOS: 'js',
  GOARCH: 'wasm',
});

copyFileSync(execSource, join(outDir, 'wasm_exec.js'));

const wasmSize = statSync(wasmPath).size;
const execSize = statSync(join(outDir, 'wasm_exec.js')).size;
console.log(`rk3588.wasm: ${(wasmSize / 1024 / 1024).toFixed(2)} MiB`);
console.log(`wasm_exec.js: ${execSize} B (from ${execSource})`);
