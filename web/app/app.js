// Main-thread controller for the M1 + M3b page.
//
// It owns three things: the WebUSB host (main-thread only), the UI, and the
// worker conversation. Every protocol decision — mode gating, CBW framing,
// Download Boot chunking, the write plan, the write state machine — happens in
// the Go/WASM core; this file never fabricates a transfer.
//
// The pre-flight rules (which buttons may send a CBW, when a write may start)
// live in preflight.js so they are testable without a browser.

import { createWebUSBHost } from './usb-host.js';
import { buildMockLoader } from './mock-loader.js';
import { evaluatePreflight } from './preflight.js';

const ROCKCHIP_VENDOR_ID = 0x2207;

const $ = (id) => document.getElementById(id);

const ui = {
  banner: $('banner'),
  modeBadge: $('mode-badge'),
  coreBadge: $('core-badge'),
  netBadge: $('net-badge'),
  devices: $('devices'),
  imageSummary: $('image-summary'),
  loaderSummary: $('loader-summary'),
  imageLabel: $('image-label'),
  zipSummary: $('zip-summary'),
  fileImage: $('file-image'),
  demoImage: $('btn-demo-image'),
  planButton: $('btn-plan'),
  fileLabel: $('file-label'),
  fileLoader: $('file-loader'),
  demoLoader: $('btn-demo-loader'),
  clearLoader: $('btn-clear-loader'),
  checklist: $('checklist'),
  readonlyNote: $('readonly-note'),
  eraseMode: $('erase-mode'),
  eraseConfirm: $('chk-erase-confirm'),
  writeButton: $('btn-write'),
  cancelWrite: $('btn-cancel-write'),
  ufLog: $('uf-log'),
  terminal: $('terminal'),
  state: $('state-panel'),
  info: $('info-panel'),
  traceBody: $('trace-body'),
  traceCount: $('trace-count'),
  errors: $('errors'),
  input: $('command-input'),
  mock: $('chk-mock'),
  install: $('btn-install'),
  installNote: $('install-note'),
  swNote: $('sw-note'),
  reloadApp: $('btn-reload-app'),
  reconnect: $('reconnect'),
  reconnectSteps: $('reconnect-steps'),
  reconnectAuth: $('btn-reconnect-auth'),
  reconnectDismiss: $('btn-reconnect-dismiss'),
  exportLog: $('btn-export-log'),
};

const state = {
  mode: 'usb', // usb | mock
  ready: false,
  busy: false,
  coreVersion: '',
  deviceState: null,
  // Loader used by the M1 `db` command.
  loaderBytes: null,
  loaderName: '',
  loaderSource: 'none', // image | file | demo | none
  loaderOverride: null, // {bytes, name} when the user picked a file
  // Image + plan.
  image: null, // File
  imageKind: null, // file | zip | demo
  imageName: '',
  zip: null, // {zip, entry, summary, skipped, stats} while reading a package
  plan: null, // {facts, report, loader}
  // Pre-flight choices.
  eraseMode: 'none',
  eraseConfirmed: false,
  dbDone: false,
  ufRunning: false,
  ufProgress: 0,
  traceRows: 0,
  lastInfo: null,
  errors: [],
  // Reconnect wizard: numeric state, rendered into #reconnect.
  reconnect: { needed: false, cause: '', attempts: 0, since: null },
  // Install prompt (beforeinstallprompt) and service worker bookkeeping.
  install: { event: null, installed: false },
  sw: { registration: null, updateReady: false },
  // NDJSON export: one record per observable event, capped so a long write
  // cannot grow the tab without bound.
  events: [],
  eventsDropped: 0,
};

const MAX_EVENTS = 5000;

let worker = null;
let rpcId = 0;
const pending = new Map();

const usbHost = createWebUSBHost(globalThis.navigator?.usb);

// ---------------------------------------------------------------------------
// Worker plumbing
// ---------------------------------------------------------------------------

function startWorker() {
  if (worker) worker.terminate();
  pending.clear();
  state.ready = false;
  setCoreBadge('loading', 'core 加载中');
  worker = new Worker(new URL('./worker.js', import.meta.url), { type: 'module' });
  worker.onmessage = onWorkerMessage;
  worker.onerror = (event) => fatal(`worker 错误：${event.message}`);
}

function onWorkerMessage(event) {
  const msg = event.data;
  switch (msg.type) {
    case 'ready':
      state.ready = true;
      state.coreVersion = msg.version;
      setCoreBadge('ok', `core ${msg.version}`);
      updateButtons();
      if (msg.mode !== state.mode) {
        worker.postMessage({ type: 'mode', mode: state.mode });
      } else if (state.mode === 'mock' && !state.plan) {
        // Demo mode should be demoable in one click: synthetic image + plan.
        loadDemoImage();
      }
      break;
    case 'mode':
      setBanner('');
      runLine('ld');
      break;
    case 'result':
      settle(msg);
      renderResult(msg.result);
      break;
    case 'uf-result':
      settle(msg);
      renderUfResult(msg.result);
      break;
    case 'uf-event':
      renderUfEvent(msg);
      break;
    case 'image-loader': {
      const entry = pending.get(msg.id);
      if (entry) {
        pending.delete(msg.id);
        entry.resolve(msg);
      }
      break;
    }
    case 'demo-image': {
      const entry = pending.get(msg.id);
      if (entry) {
        pending.delete(msg.id);
        entry.resolve(msg.payload);
      }
      break;
    }
    case 'inspect':
      renderLoaderSummary(msg.payload);
      break;
    case 'zip':
      renderZip(msg);
      break;
    case 'zip-stats':
      renderZipStats(msg.stats);
      break;
    case 'host':
      handleHostCall(msg);
      break;
    case 'mock-state':
      if (state.mode === 'mock') {
        appendLine(`mock: ${JSON.stringify(msg.state)}`, 'muted');
      }
      break;
    case 'error': {
      const entry = pending.get(msg.id);
      if (entry) {
        pending.delete(msg.id);
        entry.reject(new Error(msg.error));
      }
      appendError(msg.error);
      if (state.ufRunning) {
        state.ufRunning = false;
        updateButtons();
      }
      break;
    }
    case 'fatal':
      fatal(msg.error);
      break;
    case 'log':
      console.info('[worker]', msg.message);
      break;
    default:
      console.warn('unknown worker message', msg);
  }
}

function settle(msg) {
  const entry = pending.get(msg.id);
  if (!entry) return;
  pending.delete(msg.id);
  entry.resolve(msg.result);
}

async function handleHostCall(msg) {
  try {
    const fn = usbHost[msg.method];
    if (typeof fn !== 'function') throw new Error(`unknown host method ${msg.method}`);
    const value = await fn(...msg.args);
    const transfer = [];
    if (value && value.data instanceof ArrayBuffer) transfer.push(value.data);
    worker.postMessage({ type: 'host-reply', id: msg.id, ok: true, value }, transfer);
  } catch (err) {
    worker.postMessage({ type: 'host-reply', id: msg.id, ok: false, error: err?.message ?? String(err) });
  }
}

function fatal(message) {
  setBanner(`页面无法启动：${message}`, 'error');
  setCoreBadge('error', 'core 失败');
}

/** request posts one worker message and resolves when its reply arrives. */
function request(message, { timeoutMs = 600000 } = {}) {
  if (!worker) return Promise.reject(new Error('worker is not running'));
  const id = ++rpcId;
  const promise = new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    setTimeout(() => {
      if (pending.delete(id)) reject(new Error(`${message.type}: 超时（core 未响应）`));
    }, timeoutMs);
  });
  worker.postMessage({ ...message, id });
  return promise;
}

// ---------------------------------------------------------------------------
// Commands (M1 shell)
// ---------------------------------------------------------------------------

function execCommand(line, { loader, loaderName } = {}) {
  if (!worker) return Promise.reject(new Error('worker is not running'));
  const id = ++rpcId;
  const promise = new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    setTimeout(() => {
      if (pending.delete(id)) reject(new Error(`${line}: 超时（core 未响应）`));
    }, 120000);
  });
  const transfer = [];
  let buffer = null;
  if (loader) {
    buffer = loader.buffer.slice(loader.byteOffset, loader.byteOffset + loader.byteLength);
    transfer.push(buffer);
  }
  worker.postMessage({ type: 'exec', id, line, loader: buffer, loaderName: loaderName ?? '' }, transfer);
  return promise;
}

function runLine(line, options) {
  if (!state.ready) {
    appendError('core 还没加载完，请稍候。');
    return Promise.resolve(null);
  }
  appendLine(`› ${line}`, 'prompt');
  setBusy(true);
  return execCommand(line, options)
    .catch((err) => {
      appendError(err.message);
      return null;
    })
    .finally(() => setBusy(false));
}

function runDb() {
  if (!state.loaderBytes) {
    appendError('还没有可用的 loader：先选择镜像（默认用镜像自带的 loader）。');
    return;
  }
  runLine('db', { loader: state.loaderBytes, loaderName: state.loaderName }).then(() => {
    // A successful db leaves the device re-enumerating as LOADER; the ld that
    // follows flips state.dbDone through renderState.
  });
}

// ---------------------------------------------------------------------------
// Image + plan
// ---------------------------------------------------------------------------

function planOptions() {
  const opts = { eraseTail: state.eraseMode, verify: false };
  if (state.loaderOverride) {
    opts.loaderBytes = state.loaderOverride.bytes;
    opts.loaderName = state.loaderOverride.name;
  }
  return opts;
}

function imagePayload(extra = {}) {
  if (state.image && state.imageKind === 'zip') return { zip: state.image, ...extra };
  if (state.image) return { file: state.image, ...extra };
  if (state.imageKind === 'demo') return { demo: true, ...extra };
  return null;
}

/** isZipPackage recognises the vendor package by name or MIME type. */
function isZipPackage(file) {
  if (!file) return false;
  const name = (file.name ?? '').toLowerCase();
  const type = (file.type ?? '').toLowerCase();
  return name.endsWith('.zip') || type === 'application/zip' || type === 'application/x-zip-compressed' || type === 'multipart/x-zip';
}

async function pickImage(file) {
  if (!file) return;
  state.image = file;
  state.imageKind = isZipPackage(file) ? 'zip' : 'file';
  state.imageName = file.name;
  state.zip = null;
  state.plan = null;
  renderZip();
  ui.imageLabel.textContent = file.name;
  if (state.imageKind === 'zip') {
    appendLine(`选择 zip 包：${file.name}（${formatBytes(file.size)}）— 只解压需要的字节，不会落盘 3.8 GiB`, 'muted');
  } else {
    appendLine(`选择镜像：${file.name}（${formatBytes(file.size)}）`, 'muted');
  }
  resetEraseChoice();
  await planImage();
}

async function loadDemoImage() {
  if (!state.ready) return;
  try {
    const payload = await request({ type: 'demo-image' }, { timeoutMs: 30000 });
    state.image = null;
    state.imageKind = 'demo';
    state.imageName = payload.name;
    state.zip = null;
    renderZip();
    ui.imageLabel.textContent = payload.name;
    appendLine(`演示镜像：${payload.name}（${formatBytes(payload.size)}，合成 RKFW，仅用于演示）`, 'muted');
    resetEraseChoice();
    await planImage();
  } catch (err) {
    appendError(err.message);
  }
}

// --- zip package view -------------------------------------------------------

function renderZip(msg) {
  if (!msg) {
    ui.zipSummary.hidden = true;
    ui.zipSummary.textContent = '';
    return;
  }
  state.zip = {
    zip: msg.zip,
    entry: msg.entry,
    entries: msg.entries ?? 0,
    zip64: Boolean(msg.zip64),
    summary: msg.summary,
    skipped: msg.skipped ?? [],
    stats: msg.stats ?? null,
  };
  const entry = msg.entry ?? {};
  const lines = [
    `包: ${msg.zip}`,
    `条目: ${entry.name ?? '?'} · ${entry.method ?? '?'} · ${formatBytes(entry.compressedSize ?? 0)} → ${formatBytes(
      entry.uncompressedSize ?? 0,
    )}`,
    `目录: ${state.zip.entries} 项${state.zip.zip64 ? ' · ZIP64' : ''}`,
    "读取: 中央目录定位，按需 DecompressionStream('deflate-raw') 解压；解压结果只保留 8 MiB 窗口，不落盘。",
  ];
  if (state.zip.skipped.length > 0) {
    const names = state.zip.skipped.slice(0, 4).map((s) => `${s.entry}（${s.reason}）`).join('、');
    lines.push(`跳过: ${names}${state.zip.skipped.length > 4 ? ' …' : ''}`);
  }
  ui.zipSummary.hidden = false;
  ui.zipSummary.textContent = lines.join('\n');
  ui.zipSummary.dataset.tone = 'ok';
  appendLine(`zip: 使用条目 ${entry.name ?? '?'}（${entry.method ?? '?'}，${formatBytes(entry.uncompressedSize ?? 0)}）`, 'ok');
  recordEvent('zip', { zip: msg.zip, entry, skipped: state.zip.skipped });
}

function renderZipStats(stats) {
  if (!stats || !state.zip) return;
  state.zip.stats = stats;
  const decoded = formatBytes(stats.decodedBytes ?? 0);
  const peak = formatBytes(stats.peakBufferedBytes ?? 0);
  appendLine(
    `zip: 已流式解压 ${decoded}（${stats.reads ?? 0} 次读，${stats.reopens ?? 0} 次回退重解，峰值缓冲 ${peak}）`,
    'muted',
  );
  recordEvent('zip-stats', stats);
}

/** resetEraseChoice makes the tail decision an explicit act per image. */
function resetEraseChoice() {
  state.eraseConfirmed = false;
  ui.eraseConfirm.checked = false;
}

async function planImage() {
  if (!state.ready) {
    appendError('core 还没加载完，请稍候。');
    return;
  }
  const payload = imagePayload();
  if (!payload) {
    appendError('还没有选择镜像。');
    return;
  }
  setBusy(true);
  try {
    const result = await request({ type: 'uf-plan', ...payload, opts: planOptions() });
    renderPlan(result);
    if (result.ok) {
      // The M1 `db` button should send the image's own loader too, not ask for
      // a file; fetch the bytes once per image/override.
      await fetchImageLoader();
    }
  } catch (err) {
    appendError(`生成计划失败：${err.message}`);
  } finally {
    setBusy(false);
  }
}

async function fetchImageLoader() {
  if (state.loaderOverride) return; // already have the user's bytes
  const payload = imagePayload();
  if (!payload) return;
  try {
    const reply = await request({ type: 'image-loader', ...payload }, { timeoutMs: 60000 });
    if (reply.bytes && reply.bytes.byteLength > 0) {
      state.loaderBytes = reply.bytes;
      state.loaderSource = 'image';
      state.loaderName = reply.payload?.name ?? 'image loader';
      ui.fileLabel.textContent = '使用镜像自带 loader';
    }
  } catch (err) {
    appendLine(`读取镜像 loader 失败：${err.message}`, 'muted');
  }
  updateButtons();
}

function renderPlan(result) {
  if (!result) return;
  state.plan = result;
  ui.imageSummary.textContent = imageSummaryLines(result).join('\n');
  ui.imageSummary.dataset.tone = result.ok ? 'ok' : 'error';
  ui.loaderSummary.textContent = loaderSummaryLines(result).join('\n');
  ui.loaderSummary.dataset.tone = result.loader?.chipCheck === 'mismatch' ? 'error' : 'ok';
  if (!result.ok) appendError(`计划失败：${result.error}`);
  if (result.loader?.chipCheck === 'mismatch') {
    appendLine('✗ loader 与镜像 chip 不匹配：写入会被拒绝（index 0 是 BootROM 拒收，不是线的问题）', 'error');
  }
  state.dbDone = false;
  renderChecklist();
  updateButtons();
  recordEvent('plan', {
    ok: Boolean(result.ok),
    image: result.image ?? null,
    facts: result.facts ?? null,
    loader: result.loader
      ? { source: result.loader.source, chipCheck: result.loader.chipCheck, sha256: result.loader.sha256, length: result.loader.length }
      : null,
    error: result.error ?? null,
  });
}

function imageSummaryLines(result) {
  const lines = [];
  const report = result.report;
  lines.push(`镜像: ${result.image?.name ?? '-'} · ${formatBytes(result.image?.size ?? 0)}`);
  if (!report) return lines;
  const c = report.container ?? {};
  if (c.hasRkfw) {
    lines.push(`容器: RKFW · ${c.chip ?? '?'}${c.version ? ` · v${c.version}` : ''}${c.buildTime ? ` · built ${c.buildTime}` : ''}`);
  } else {
    lines.push('容器: 裸 RKAF（无 RKFW 包装、无 loader）');
  }
  lines.push(`parameter: ${report.parameter?.partitionCount ?? 0} 个分区 · archive ${c.itemCount ?? 0} 项`);
  const f = result.facts ?? {};
  lines.push(`计划: 写 ${f.items ?? 0} 项 · 传输 ${formatBytes(f.totalBytes ?? 0)} · 未覆盖 ${formatBytes(f.uncoveredBytes ?? 0)}（${f.uncoveredPartitions || '无'}）`);
  lines.push(`尾部处置: ${f.eraseMode ?? '-'}`);
  for (const w of (report.warnings ?? []).slice(0, 4)) lines.push(`warning: ${w}`);
  if (!result.ok) lines.push(`✗ ${result.error}`);
  return lines;
}

function loaderSummaryLines(result) {
  const loader = result.loader;
  if (!loader) return ['loader: 镜像没有 RKFW loader（maskrom 无法 Download Boot）'];
  const source = loader.source === 'file' ? '用户指定（非镜像自带）' : '镜像自带';
  return [
    `loader: ${source} · ${loader.name ?? '-'} · offset 0x${(loader.fileOffset ?? 0).toString(16)} · ${formatBytes(loader.length ?? 0)}`,
    `sha256: ${loader.sha256 ?? '-'}`,
    `LDR: version ${loader.version || '-'} · merge ${loader.mergeVersion || '-'} · built ${loader.buildTime || 'unknown'} · chip ${loader.chip || '?'}`,
    `chip 比对: ${loader.chipCheck ?? 'unknown'}（镜像 ${loader.imageChip || '?'}）${loader.parseError ? ` · ✗ ${loader.parseError}` : ''}`,
  ];
}

// Manual loader override: it is accepted but the report marks it as not from the
// image, because that is the change that made real hardware stop at index 0.
async function pickLoader(file) {
  if (!file) return;
  const bytes = new Uint8Array(await file.arrayBuffer());
  state.loaderOverride = { bytes, name: file.name };
  state.loaderBytes = bytes;
  state.loaderName = file.name;
  state.loaderSource = 'file';
  ui.fileLabel.textContent = `${file.name}（非镜像自带）`;
  appendLine(`改用本地 loader：${file.name}（${formatBytes(bytes.byteLength)}，非镜像自带）`, 'muted');
  if (!state.ready) return;
  worker.postMessage({ type: 'inspect', id: ++rpcId, bytes: bytes.buffer.slice(0) });
  if (state.plan) await planImage();
}

async function clearLoaderOverride() {
  state.loaderOverride = null;
  state.loaderBytes = null;
  state.loaderName = '';
  state.loaderSource = 'none';
  ui.fileLabel.textContent = '改用本地 loader…（可选）';
  appendLine('改回镜像自带 loader。', 'muted');
  if (state.plan) await planImage();
}

function loadDemoLoader() {
  const bytes = buildMockLoader();
  state.loaderOverride = null;
  state.loaderBytes = bytes;
  state.loaderName = 'mock-rk3588-loader (synthetic)';
  state.loaderSource = 'demo';
  ui.fileLabel.textContent = state.loaderName;
  appendLine(`演示 loader：${bytes.byteLength} B（合成的 RKBOOT，仅用于 mock 设备）`, 'muted');
  const id = ++rpcId;
  worker.postMessage({ type: 'inspect', id, bytes: bytes.buffer.slice(0) });
  updateButtons();
}

// ---------------------------------------------------------------------------
// Pre-flight + write
// ---------------------------------------------------------------------------

function evaluation() {
  const ds = state.deviceState;
  return evaluatePreflight({
    devices: ds?.devices ?? [],
    current: ds?.current ?? null,
    loader: state.plan?.loader ?? null,
    facts: state.plan?.facts ?? null,
    eraseMode: state.eraseMode,
    eraseConfirmed: state.eraseConfirmed,
    dbDone: state.dbDone,
    runInProgress: state.ufRunning,
    hasImage: Boolean(state.image || state.imageKind),
  });
}

function renderChecklist() {
  const result = evaluation();
  ui.checklist.replaceChildren(
    ...result.items.map((item) => {
      const li = document.createElement('li');
      li.className = `check ${item.status}`;
      li.title = item.detail;
      const mark = document.createElement('span');
      mark.className = 'check-mark';
      mark.textContent = item.status === 'pass' ? '✓' : item.status === 'fail' ? '✗' : item.status === 'warn' ? '!' : '…';
      const body = document.createElement('span');
      body.className = 'check-body';
      const label = document.createElement('span');
      label.className = 'check-label';
      label.textContent = item.label;
      const detail = document.createElement('span');
      detail.className = 'check-detail';
      detail.textContent = item.detail;
      body.append(label, detail);
      li.append(mark, body);
      return li;
    }),
  );
  ui.readonlyNote.textContent = `只读按钮：${result.readOnly.enabled ? '可用' : '已禁用'} — ${result.readOnly.reason}`;
  ui.readonlyNote.dataset.tone = result.readOnly.enabled ? 'ok' : 'warn';
}

async function startWrite() {
  const result = evaluation();
  if (!result.canWrite) {
    appendError(`自检未通过：${result.items.filter((i) => i.status !== 'pass').map((i) => i.label).join('；')}`);
    return;
  }
  const payload = imagePayload();
  if (!payload) {
    appendError('还没有选择镜像。');
    return;
  }
  const opts = {
    ...planOptions(),
    mock: state.mode === 'mock',
    deviceHandle: currentHandle(),
  };
  state.ufRunning = true;
  state.ufProgress = 0;
  ui.ufLog.textContent = '';
  updateButtons();
  appendUf(`开始写入（${state.eraseMode}，${state.mode === 'mock' ? 'mock 设备' : '真实 WebUSB'}）`);
  recordEvent('write-start', {
    eraseMode: state.eraseMode,
    mode: state.mode,
    image: state.imageName || state.imageKind,
    zip: state.zip ? { zip: state.zip.zip, entry: state.zip.entry?.name ?? null } : null,
    facts: state.plan?.facts ?? null,
  });
  try {
    await request({ type: 'uf-run', ...payload, opts });
  } catch (err) {
    appendError(`写入失败：${err.message}`);
    state.ufRunning = false;
    updateButtons();
  }
}

function cancelWrite() {
  if (!state.ufRunning) return;
  appendUf('取消请求已发送…');
  request({ type: 'uf-cancel' }, { timeoutMs: 15000 }).catch((err) => appendError(err.message));
}

function currentHandle() {
  const ds = state.deviceState;
  const list = ds?.devices ?? [];
  const current = list.find((d) => d.devNo === ds?.current) ?? (list.length === 1 ? list[0] : null);
  return current?.handle ?? '';
}

function appendUf(text) {
  ui.ufLog.textContent += `${text}\n`;
  ui.ufLog.scrollTop = ui.ufLog.scrollHeight;
}

function renderUfEvent(msg) {
  const payload = msg.payload ?? {};
  recordEvent('uf-event', { kind: msg.kind, payload });
  if (msg.kind === 'event') {
    const level = payload.level === 'warn' ? 'warn' : payload.level === 'error' ? 'error' : 'info';
    appendUf(`${level === 'info' ? '·' : level === 'warn' ? '!' : '✗'} ${payload.message}`);
  } else if (msg.kind === 'progress') {
    state.ufProgress = payload.percent ?? 0;
    const note = payload.note ? ` · ${payload.note}` : '';
    const item = payload.item ? ` · ${payload.item}` : '';
    appendUf(`  ${payload.phase}${item} ${formatBytes(payload.itemDoneBytes ?? 0)}/${formatBytes(
      payload.itemTotalBytes ?? payload.totalBytes ?? 0,
    )} (${(payload.percent ?? 0).toFixed(1)}%)${note}`);
  }
}

function renderUfResult(result) {
  state.ufRunning = false;
  const run = result.run;
  if (!result.ok || !run) {
    appendError(result.error || '写入失败');
    if (run?.error) renderRunFailure(run.error);
    updateButtons();
    renderChecklist();
    return;
  }
  appendUf('');
  appendUf(`结果: ${run.ok ? 'OK' : 'FAILED'} · 写入 ${formatBytes(run.bytesWritten)} · 擦除 ${formatBytes(
    run.bytesErased,
  )} · reset=${run.reset} · ${run.durationMs.toFixed(0)} ms`);
  for (const phase of run.phases ?? []) {
    appendUf(`  ${phase.phase} ${phase.name} — ${phase.status}${phase.note ? ` (${phase.note})` : ''}`);
  }
  for (const item of run.items ?? []) {
    if (item.status === 'ok') appendUf(`  wl ${item.name} → LBA 0x${item.targetLba.toString(16)} ok`);
  }
  if (run.loader) {
    appendUf(
      `  loader: ${run.loader.source} · ${formatBytes(run.loader.length)} · sha256 ${String(run.loader.sha256).slice(0, 12)}… · 构建 ${
        run.loader.buildTime || 'unknown'
      } · chip ${run.loader.chip} (${run.loader.chipCheck})`,
    );
  }
  if (run.error) renderRunFailure(run.error);
  if (run.ok) {
    state.dbDone = true;
    appendLine('写入完成。设备复位后会重新枚举为 LOADER；点“授权设备”后 ld 可复查。', 'ok');
  }
  recordEvent('uf-result', {
    ok: run.ok,
    bytesWritten: run.bytesWritten,
    bytesErased: run.bytesErased,
    durationMs: run.durationMs,
    reset: run.reset,
    error: run.error ? { codeName: run.error.codeName, code: run.error.code, phase: run.error.phase, item: run.error.item } : null,
  });
  updateButtons();
  renderChecklist();
  runLine('ld');
}

function renderRunFailure(error) {
  const category = error.category ? ` [${error.category}]` : '';
  appendLine(`✗ ${error.codeName}${category} (${error.code}) ${error.phase}${error.item ? `/${error.item}` : ''}`, 'error');
  if (error.detail) appendLine(error.detail, 'muted');
  if (error.hint) appendLine(`hint: ${error.hint}`, 'muted');
  state.errors.push(`${error.codeName}${category}: ${error.detail || ''}`);
  renderErrors();
}

// ---------------------------------------------------------------------------
// Rendering (M1 views)
// ---------------------------------------------------------------------------

function appendLine(text, tone = '') {
  const node = document.createElement('span');
  node.className = `line ${tone}`;
  node.textContent = text;
  ui.terminal.appendChild(node);
  ui.terminal.appendChild(document.createTextNode('\n'));
  ui.terminal.scrollTop = ui.terminal.scrollHeight;
  recordEvent('line', { tone, text });
}

function appendError(message) {
  const text = String(message);
  appendLine(`✗ ${text}`, 'error');
  state.errors.push(text);
  renderErrors();
  recordEvent('error', { message: text });
}

// ---------------------------------------------------------------------------
// NDJSON log export
//
// The page is the only place that sees every event: shell lines, uf progress,
// USB transfers, pre-flight verdicts, zip statistics. A write that fails on a
// user's desk is only diagnosable if those leave the browser, so each one is
// also recorded here and exported as NDJSON — the same shape `uf --log` writes
// on the CLI side.
// ---------------------------------------------------------------------------

function recordEvent(type, payload = {}) {
  state.events.push({ ts: new Date().toISOString(), type, ...payload });
  if (state.events.length > MAX_EVENTS) {
    const drop = state.events.length - MAX_EVENTS;
    state.events.splice(0, drop);
    state.eventsDropped += drop;
  }
}

function exportLog() {
  const header = {
    ts: new Date().toISOString(),
    type: 'header',
    app: 'rk3588-webflasher',
    page: location.href,
    userAgent: navigator.userAgent,
    coreVersion: state.coreVersion || null,
    mode: state.mode,
    image: state.imageName || null,
    imageKind: state.imageKind || null,
    zip: state.zip ? { zip: state.zip.zip, entry: state.zip.entry?.name ?? null, stats: state.zip.stats } : null,
    plan: state.plan
      ? { ok: state.plan.ok, image: state.plan.image ?? null, facts: state.plan.facts ?? null, error: state.plan.error ?? null }
      : null,
    device: state.deviceState ? { mode: state.deviceState.mode, current: state.deviceState.current } : null,
    eventsDropped: state.eventsDropped,
  };
  const lines = [JSON.stringify(header), ...state.events.map((event) => JSON.stringify(event))];
  if (state.eventsDropped > 0) {
    lines.push(JSON.stringify({ ts: new Date().toISOString(), type: 'truncated', dropped: state.eventsDropped }));
  }
  const blob = new Blob([`${lines.join('\n')}\n`], { type: 'application/x-ndjson' });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = `rk3588-webflasher-${new Date().toISOString().replace(/[:.]/g, '-')}.ndjson`;
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30000);
  appendLine(`已导出 ${lines.length} 条 NDJSON 日志（含 zip 统计与传输记录）`, 'ok');
}

function renderErrors() {
  if (state.errors.length === 0) {
    ui.errors.innerHTML = '<p class="muted">没有错误。</p>';
    return;
  }
  ui.errors.replaceChildren(
    ...state.errors.slice(-8).map((text) => {
      const p = document.createElement('p');
      p.className = 'error-text';
      p.textContent = text;
      return p;
    }),
  );
}

function setBusy(busy) {
  state.busy = busy;
  updateButtons();
}

function updateButtons() {
  const supported = Boolean(globalThis.navigator?.usb);
  const busy = state.busy || state.ufRunning;
  const evaluationResult = evaluation();

  $('btn-authorize').disabled = busy || state.mode === 'mock' || !supported;
  $('btn-refresh').disabled = busy || !state.ready;
  ui.planButton.disabled = busy || !state.ready || !(state.image || state.imageKind === 'demo');
  ui.fileImage.disabled = state.ufRunning;
  ui.fileLoader.disabled = state.ufRunning;
  ui.demoImage.hidden = state.mode !== 'mock';
  ui.demoLoader.hidden = state.mode !== 'mock';
  ui.mock.disabled = state.ufRunning;

  ui.writeButton.disabled = !evaluationResult.canWrite;
  ui.cancelWrite.disabled = !state.ufRunning;
  ui.eraseMode.disabled = state.ufRunning;
  ui.eraseConfirm.disabled = state.ufRunning;

  // Buttons that put a CBW on the wire are gated by the pre-flight rule: no
  // read before db on a bare maskrom.
  for (const button of document.querySelectorAll('[data-cbw="true"]')) {
    if (button.id === 'btn-db') {
      button.disabled = busy || !state.ready || !evaluationResult.canDb || !state.loaderBytes;
      button.title = evaluationResult.canDb
        ? '把镜像自带的 loader 写进 maskrom RAM'
        : 'db 只能对 maskrom 且 loader 与镜像匹配时执行';
      continue;
    }
    button.disabled = busy || !state.ready || !evaluationResult.readOnly.enabled;
    button.title = evaluationResult.readOnly.enabled ? '' : evaluationResult.readOnly.reason;
  }
  renderChecklist();
}

function setBanner(text, tone = 'warn') {
  if (!text) {
    ui.banner.hidden = true;
    ui.banner.textContent = '';
    return;
  }
  ui.banner.hidden = false;
  ui.banner.dataset.tone = tone;
  ui.banner.textContent = text;
}

function setCoreBadge(kind, text) {
  ui.coreBadge.dataset.core = kind;
  ui.coreBadge.textContent = text;
}

function setModeBadge(mode, label) {
  const names = { MASKROM: 'Maskrom', LOADER: 'Loader', UNKNOWN: '未知', NONE: '无设备' };
  ui.modeBadge.dataset.mode = (mode ?? 'NONE').toLowerCase();
  ui.modeBadge.textContent = label ?? names[mode] ?? mode;
}

function renderResult(result) {
  if (!result) return;
  for (const line of result.lines ?? []) appendLine(line, result.ok ? '' : 'warn');
  if (result.error) {
    appendLine(`✗ ${result.error.codeName} (${result.error.code})${result.error.hint ? ` — ${result.error.hint}` : ''}`, 'error');
    if (result.error.hint) appendLine(`hint: ${result.error.hint}`, 'muted');
    if (result.error.db) {
      appendLine(`db 分类: ${result.error.db.kind} · index ${result.error.db.index}（${result.error.db.blockName || '-'}）`, 'error');
      renderRunFailure({
        codeName: result.error.codeName,
        code: result.error.code,
        phase: 'ul',
        category: result.error.db.kind === 'DB_BOOTROM_REJECTED' ? 'first-block' : result.error.db.kind === 'DB_TRANSFER_FAILED' ? 'mid-transfer' : '',
        detail: result.error.db.detail,
        hint: result.error.hint,
      });
    }
    state.errors.push(result.error.detail || result.error.codeName);
    renderErrors();
  }
  if (result.state) {
    const previousMode = state.deviceState?.mode;
    state.deviceState = result.state;
    if (result.state.mode === 'LOADER') state.dbDone = true;
    if (previousMode === 'MASKROM' && result.state.mode === 'LOADER') {
      appendLine('设备已重新枚举为 LOADER：db 完成。', 'ok');
      clearReconnect('db-complete');
    }
    recordEvent('state', { mode: result.state.mode, current: result.state.current, loaderLoaded: result.state.loaderLoaded });
    renderState(result.state);
    renderDevices(result.state);
  }
  if (result.info) {
    state.lastInfo = result.info;
    renderInfo(result.info);
  }
  if (result.loader) {
    ui.loaderSummary.textContent = result.loader.join('\n');
  }
  if (result.transfers?.length) {
    renderTransfers(result.transfers);
  }
  renderChecklist();
  updateButtons();
}

function renderState(st) {
  setModeBadge(st.mode, st.modeLabel);
  const readOnly = evaluation().readOnly;
  const rows = [
    ['模式', `${st.modeLabel}（bcdUSB bit0=${st.mode === 'LOADER' ? 1 : st.mode === 'MASKROM' ? 0 : '?'}）`],
    ['当前设备', st.current ? `DevNo=${st.current}` : '未选择'],
    ['loader', st.loaderLoaded ? `已载入 ${st.loaderName || '(unnamed)'}` : state.loaderSource === 'image' ? '镜像自带（已就绪）' : '未载入'],
    ['只读命令', readOnly.enabled ? '可用' : `禁用：${readOnly.reason}`],
    ['下一步', nextStep(st)],
  ];
  ui.state.replaceChildren(
    ...rows.map(([key, value]) => {
      const row = document.createElement('div');
      row.className = 'state-row';
      const k = document.createElement('span');
      k.className = 'state-key';
      k.textContent = key;
      const v = document.createElement('span');
      v.className = 'state-value';
      v.textContent = value;
      row.append(k, v);
      return row;
    }),
  );
  if (st.awaitingReenumeration) {
    const note = document.createElement('div');
    note.className = 'state-note';
    note.textContent = 'Download Boot 已发出：等待设备重新枚举为 LOADER。若浏览器撤销了授权，再点一次“授权设备”。';
    ui.state.appendChild(note);
    markReconnect('db');
  }
  updateButtons();
}

function nextStep(st) {
  if (!st.devices || st.devices.length === 0) return '点“授权设备”选择 0x2207 设备';
  if (st.awaitingReenumeration) return '等待重新枚举 → 授权 → ld';
  if (st.mode === 'MASKROM') return '选择镜像 → 生成计划 → 自检通过后点“开始写入”（第一步就是 db）';
  if (st.mode === 'LOADER') return '可以生成计划并写入；或 info 读取只读信息';
  return 'ld 查看设备后按模式决定';
}

function renderDevices(st) {
  const devices = st.devices ?? [];
  if (devices.length === 0) {
    ui.devices.innerHTML = '<p class="muted">没有已授权设备。</p>';
    return;
  }
  ui.devices.replaceChildren(
    ...devices.map((device) => {
      const card = document.createElement('button');
      card.type = 'button';
      card.className = `device-card${device.devNo === st.current ? ' selected' : ''}`;
      const title = document.createElement('span');
      title.className = 'device-title';
      title.textContent = `DevNo=${device.devNo} · ${device.productName || 'Rockchip device'}`;
      const sub = document.createElement('span');
      sub.className = 'device-sub';
      sub.textContent = `Vid=0x${device.vendorId.toString(16).padStart(4, '0')},Pid=0x${device.productId
        .toString(16)
        .padStart(4, '0')} · ${device.modeLabel} · bcdUSB 0x${device.bcdUSB.toString(16).padStart(4, '0')}`;
      card.append(title, sub);
      card.addEventListener('click', () => runLine(`cd ${device.devNo}`));
      return card;
    }),
  );
}

function renderInfo(info) {
  ui.info.replaceChildren();
  const cards = [];
  if (info.chip) cards.push(['Chip', info.chip.text, info.chip.rawHex]);
  if (info.flash) {
    cards.push([
      'Flash',
      `${formatBytes(info.flash.flashSizeBytes)} · block ${formatBytes(info.flash.blockSizeBytes)} · page ${formatBytes(
        info.flash.pageSizeBytes,
      )}`,
      `${info.flash.manufacturer} · ECC ${info.flash.eccBits}bit · CS ${info.flash.flashCSList.join(',')} · ${info.flash.rawHex}`,
    ]);
  }
  if (info.capability) {
    cards.push(['Capability', info.capability.flags.join(', ') || '(none)', info.capability.rawHex]);
  }
  if (info.storage) {
    cards.push(['Storage', `${info.storage.currentName} · [${info.storage.names.join(', ')}]`, `bitmap 0x${info.storage.bitmap.toString(16)}`]);
  }
  cards.push(['TestDeviceReady', info.testUnitReady ? 'OK' : '失败', 'RKU_TestDeviceReady']);
  for (const [title, value, detail] of cards) {
    const card = document.createElement('article');
    card.className = 'info-card';
    const h = document.createElement('h3');
    h.textContent = title;
    const v = document.createElement('p');
    v.className = 'info-value';
    v.textContent = value;
    const d = document.createElement('code');
    d.className = 'info-detail';
    d.textContent = detail;
    card.append(h, v, d);
    ui.info.appendChild(card);
  }
}

function formatBytes(n) {
  if (!Number.isFinite(n)) return '?';
  const mib = 1024 * 1024;
  const kib = 1024;
  if (n >= mib) return `${(n / mib).toFixed(n % mib === 0 ? 0 : 1)} MiB`;
  if (n >= kib) return `${(n / kib).toFixed(n % kib === 0 ? 0 : 1)} KiB`;
  return `${n} B`;
}

function renderTransfers(transfers) {
  for (const t of transfers) {
    recordEvent('transfer', t);
    const row = document.createElement('tr');
    if (t.status && t.status !== 'ok') row.className = 'error';
    const cells = [
      String(t.seq),
      t.kind,
      `${t.opName || '—'}${t.stage ? ` / ${t.stage}` : ''}`,
      `${t.length} B`,
      String(t.durationMs),
      t.status ?? '',
      (t.dataHex ?? '').slice(0, 48),
    ];
    for (const value of cells) {
      const td = document.createElement('td');
      td.textContent = value;
      row.appendChild(td);
    }
    ui.traceBody.appendChild(row);
    state.traceRows += 1;
  }
  const max = 300;
  while (ui.traceBody.children.length > max) ui.traceBody.removeChild(ui.traceBody.firstChild);
  ui.traceCount.textContent = `${state.traceRows} 条传输（表格保留最近 ${max} 条）`;
}

function renderLoaderSummary(payload) {
  if (!payload) return;
  const lines = payload.lines ?? [];
  if (payload.error) {
    ui.loaderSummary.textContent = [`✗ ${payload.error}`, payload.kind ? `kind: ${payload.kind}` : ''].filter(Boolean).join('\n');
    ui.loaderSummary.dataset.tone = 'error';
    state.loaderBytes = null;
    updateButtons();
    return;
  }
  ui.loaderSummary.dataset.tone = 'ok';
  ui.loaderSummary.textContent = lines.join('\n');
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

async function authorize() {
  if (!globalThis.navigator?.usb) {
    setBanner('当前浏览器没有 WebUSB，请使用桌面版 Chrome / Edge。', 'error');
    return;
  }
  try {
    const device = await navigator.usb.requestDevice({ filters: [{ vendorId: ROCKCHIP_VENDOR_ID }] });
    appendLine(`已授权 ${device.productName || 'Rockchip device'}（${device.vendorId.toString(16)}:${device.productId.toString(16)}）`, 'ok');
    await runLine('ld');
  } catch (err) {
    if (err.name === 'NotFoundError') {
      setBanner('授权弹窗被取消（没有选择设备）。', 'warn');
      return;
    }
    appendError(`授权失败：${err.message}`);
  }
}

function toggleMock(enabled) {
  state.mode = enabled ? 'mock' : 'usb';
  appendLine(enabled ? '切换到演示模式：worker 内使用 mock 设备，不访问真实 USB。' : '切换到真实设备模式。', 'muted');
  setBanner(
    enabled
      ? '演示模式：mock 设备只存在于页面里。写入走 WASM 内的模拟器，不访问 USB。'
      : '',
    'info',
  );
  startWorker();
  state.loaderBytes = null;
  state.loaderName = '';
  state.loaderSource = 'none';
  state.loaderOverride = null;
  state.plan = null;
  state.dbDone = false;
  state.zip = null;
  clearReconnect('mode-switch');
  renderZip();
  ui.fileLabel.textContent = '改用本地 loader…（可选）';
  ui.loaderSummary.textContent = '尚未生成计划。';
  renderChecklist();
  updateButtons();
  if (enabled) {
    // One click should get a demoable state: synthetic image + plan.
    loadDemoImage();
  }
}

function clearOutput() {
  ui.terminal.replaceChildren();
  ui.traceBody.replaceChildren();
  ui.info.innerHTML = '<p class="muted">尚未读取。</p>';
  state.errors = [];
  state.traceRows = 0;
  renderErrors();
  ui.traceCount.textContent = '0 条';
}

// ---------------------------------------------------------------------------
// Device reconnect wizard
//
// The maskrom → LOADER transition is a re-enumeration: the browser drops the
// per-instance permission, so `ld` fails until the user picks the device again.
// The same happens after an unplug mid-write. Both cases get the same three
// steps instead of an unexplained "no device" line.
// ---------------------------------------------------------------------------

function markReconnect(cause) {
  const wasNeeded = state.reconnect.needed;
  state.reconnect.needed = true;
  state.reconnect.cause = cause;
  state.reconnect.since = new Date().toISOString();
  if (!wasNeeded) {
    state.reconnect.attempts += 1;
    recordEvent('reconnect-needed', { cause });
  }
  renderReconnect();
}

function clearReconnect(reason) {
  if (!state.reconnect.needed) return;
  state.reconnect.needed = false;
  state.reconnect.cause = '';
  recordEvent('reconnect-cleared', { reason });
  renderReconnect();
}

function renderReconnect() {
  const r = state.reconnect;
  ui.reconnect.hidden = !r.needed;
  if (!r.needed) {
    ui.reconnectSteps.replaceChildren();
    return;
  }
  const cause =
    r.cause === 'db'
      ? 'Download Boot 已发出：设备正在从 maskrom 重新枚举为 LOADER（bcdUSB bit0=1）。'
      : r.cause === 'unplug'
        ? '设备离开了总线：拔线、复位或供电掉电都会这样。'
        : r.cause === 'error'
          ? '写入中止后设备状态未知：可能停在 LOADER，也可能回到 maskrom。'
          : '设备不再响应：重新授权前先确认线材与供电。';
  const steps = [
    cause,
    '点“重新授权”，在浏览器弹窗里重新选择 0x2207 设备（重新枚举会撤销上一次的实例授权）。',
    '授权后页面自动 ld；状态徽章变成 Loader 且只读命令可用，再继续写入或 info。',
  ];
  if (r.attempts >= 3) {
    steps.push('已经重连 3 次以上：检查 USB 线与供电，Linux 上确认 udev 规则（见 docs/webusb-drivers.md），Windows 上确认设备驱动是 WinUSB。');
  }
  ui.reconnectSteps.replaceChildren(
    ...steps.map((text, index) => {
      const li = document.createElement('li');
      li.textContent = text;
      if (index === 0) li.className = 'reconnect-cause';
      return li;
    }),
  );
  ui.reconnectAuth.disabled = state.mode === 'mock' || !globalThis.navigator?.usb;
}

// ---------------------------------------------------------------------------
// Install prompt (PWA)
// ---------------------------------------------------------------------------

function renderInstall() {
  const available = Boolean(state.install.event) && !state.install.installed;
  ui.install.hidden = !available;
  if (available) {
    ui.installNote.textContent = '安装后从桌面/开始菜单打开，断网也能载入页面与 wasm（安装本身不授予设备权限）。';
    return;
  }
  if (state.install.installed) {
    ui.installNote.textContent = '已作为应用安装。断网可打开页面；写入仍然需要 WebUSB 与设备授权。';
    return;
  }
  ui.installNote.textContent =
    '若地址栏没有安装图标：Chrome/Edge 菜单 →「安装 RK3588 WebFlasher…」；Safari/Firefox 无法安装为应用，也不支持 WebUSB。';
}

async function promptInstall() {
  const event = state.install.event;
  if (!event) return;
  try {
    event.prompt();
    const choice = await event.userChoice;
    recordEvent('install-prompt', { outcome: choice?.outcome ?? 'unknown' });
  } catch (err) {
    appendError(`安装失败：${err.message}`);
  } finally {
    state.install.event = null;
    renderInstall();
  }
}

// ---------------------------------------------------------------------------
// Service worker: offline cache
// ---------------------------------------------------------------------------

async function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) {
    ui.swNote.textContent = '离线缓存不可用：这个浏览器没有 service worker。';
    return;
  }
  try {
    const registration = await navigator.serviceWorker.register('./sw.js', { scope: './' });
    state.sw.registration = registration;
    registration.addEventListener('updatefound', () => {
      const worker = registration.installing;
      if (!worker) return;
      worker.addEventListener('statechange', () => {
        if (worker.state === 'installed' && navigator.serviceWorker.controller) {
          state.sw.updateReady = true;
          renderSwNote();
          setBanner('新版本已存入离线缓存：点“刷新页面”后生效。', 'info');
        }
      });
    });
    navigator.serviceWorker.addEventListener('controllerchange', renderSwNote);
    renderSwNote();
  } catch (err) {
    ui.swNote.textContent = `service worker 注册失败：${err.message}`;
    recordEvent('sw-error', { message: err.message });
  }
}

function renderSwNote() {
  const controlled = Boolean(navigator.serviceWorker?.controller);
  const parts = [controlled ? '离线缓存已接管本页' : '离线缓存注册中（首次加载完成后接管）'];
  if (state.sw.updateReady) parts.push('有新版本待刷新');
  ui.swNote.textContent = `离线：${parts.join(' · ')}`;
  ui.swNote.dataset.tone = controlled ? 'ok' : 'warn';
  ui.reloadApp.hidden = !state.sw.updateReady;
}

function renderNetwork() {
  const online = navigator.onLine !== false;
  ui.netBadge.dataset.net = online ? 'online' : 'offline';
  ui.netBadge.textContent = online ? '在线' : '离线（缓存可用）';
  if (!online) {
    appendLine('网络已断开：页面、wasm 与样式来自 service worker 缓存，写入仍然可用。', 'muted');
  }
  recordEvent('network', { online });
}

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------

function checkSupport() {
  if (!globalThis.navigator?.usb) {
    setBanner('当前浏览器没有 WebUSB。请用桌面版 Chrome / Edge（或 Android Chrome）打开；Safari 与 Firefox 不支持。', 'error');
    return false;
  }
  if (!globalThis.isSecureContext) {
    setBanner('WebUSB 需要安全上下文：请通过 https 或 http://localhost 打开本页（pixi run dev 提供 localhost）。', 'error');
    return false;
  }
  return true;
}

function wireEvents() {
  $('btn-authorize').addEventListener('click', authorize);
  $('btn-refresh').addEventListener('click', () => runLine('ld'));
  $('btn-db').addEventListener('click', runDb);
  $('btn-plan').addEventListener('click', () => planImage());
  ui.demoImage.addEventListener('click', loadDemoImage);
  ui.demoLoader.addEventListener('click', loadDemoLoader);
  ui.clearLoader.addEventListener('click', () => clearLoaderOverride());
  ui.writeButton.addEventListener('click', startWrite);
  ui.cancelWrite.addEventListener('click', cancelWrite);
  ui.eraseMode.addEventListener('change', () => {
    state.eraseMode = ui.eraseMode.value;
    state.eraseConfirmed = false;
    ui.eraseConfirm.checked = false;
    renderChecklist();
    updateButtons();
    if (state.plan) planImage();
  });
  ui.eraseConfirm.addEventListener('change', () => {
    state.eraseConfirmed = ui.eraseConfirm.checked;
    renderChecklist();
    updateButtons();
  });
  $('btn-clear').addEventListener('click', clearOutput);
  ui.exportLog.addEventListener('click', exportLog);
  ui.install.addEventListener('click', promptInstall);
  ui.reconnectAuth.addEventListener('click', async () => {
    await authorize();
    renderReconnect();
  });
  ui.reconnectDismiss.addEventListener('click', () => clearReconnect('dismissed'));
  ui.reloadApp.addEventListener('click', () => location.reload());
  ui.fileImage.addEventListener('change', (event) => pickImage(event.target.files?.[0] ?? null));
  ui.fileLoader.addEventListener('change', (event) => pickLoader(event.target.files?.[0] ?? null));
  ui.mock.addEventListener('change', (event) => toggleMock(event.target.checked));
  $('command-form').addEventListener('submit', (event) => {
    event.preventDefault();
    const line = ui.input.value.trim();
    if (!line) return;
    ui.input.value = '';
    runLine(line);
  });
  for (const button of document.querySelectorAll('[data-command]')) {
    button.addEventListener('click', () => runLine(button.dataset.command));
  }

  navigator.usb?.addEventListener('connect', () => {
    if (state.mode !== 'usb') return;
    appendLine('USB connect：刷新设备列表。', 'muted');
    clearReconnect('device-connected');
    runLine('ld');
  });
  navigator.usb?.addEventListener('disconnect', () => {
    if (state.mode !== 'usb') return;
    appendLine('USB disconnect：设备离开总线。', 'muted');
    state.dbDone = false;
    markReconnect(state.ufRunning ? 'error' : 'unplug');
    runLine('ld');
  });

  globalThis.addEventListener?.('beforeinstallprompt', (event) => {
    event.preventDefault();
    state.install.event = event;
    renderInstall();
    recordEvent('install-available', {});
  });
  globalThis.addEventListener?.('appinstalled', () => {
    state.install.installed = true;
    state.install.event = null;
    renderInstall();
    appendLine('已安装为应用：可以从桌面/开始菜单离线打开。', 'ok');
    recordEvent('installed', {});
  });
  globalThis.addEventListener?.('online', renderNetwork);
  globalThis.addEventListener?.('offline', renderNetwork);
}

async function main() {
  checkSupport();
  renderNetwork();
  renderInstall();
  renderReconnect();
  registerServiceWorker();
  wireEvents();
  renderChecklist();
  updateButtons();
  startWorker();
}

main();
