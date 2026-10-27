// Main-thread controller for the M4b page: five blocks, one flow.
//
//   1 镜像      catalogue download (CORS-probed, streamed to disk) or a local
//               .img/.zip
//   2 校验      structure + PARM CRC always; RKAF CRC + whole-image MD5 when
//               the measured throughput says they fit in the 2-minute budget
//   3 设备      every authorised 0x2207 device, merged with the core's `ld`
//               view, with the re-enumeration rules
//   4 刷写      one button, gated by preflight.js (nothing was loosened)
//   5 日志      every operation, timestamped, exportable as text or NDJSON
//
// The protocol decisions all live elsewhere: WebUSB in usb-host.js, the write
// state machine in the Go/WASM core, the gates in preflight.js. This file owns
// the page's state and the conversation with the worker.

import { createWebUSBHost } from './usb-host.js';
import { buildMockLoader } from './mock-loader.js';
import { evaluatePreflight, formatBytes } from './preflight.js';
import { capabilityReport, downloadEntry, formatDuration, formatSpeed, loadCatalog } from './images.js';
import {

  deviceSummaryLines,
  listUsbDevices,
  matchAfterReenumeration,
  mergeShellState,
  ROCKCHIP_VENDOR_ID,
  selectedDevice,
} from './devices.js';
import { createLogStore } from './logs.js';
import {
  estimateFullVerifySeconds,
  evaluateVerification,
  shouldDefaultToFull,
  verificationLines,
  VERIFY_NONE,
} from './verify.js';

const $ = (id) => document.getElementById(id);

const ui = {
  banner: $('banner'),
  modeBadge: $('mode-badge'),
  coreBadge: $('core-badge'),
  netBadge: $('net-badge'),

  // 1 · image
  catalog: $('image-catalog'),
  catalogNote: $('catalog-note'),
  fsaNote: $('fsa-note'),
  pickImage: $('btn-pick-image'),
  fileImage: $('file-image'),
  imageLabel: $('image-label'),
  demoImage: $('btn-demo-image'),
  imageSummary: $('image-summary'),
  zipSummary: $('zip-summary'),
  downloadProgress: $('download-progress'),
  downloadBar: $('download-bar'),
  downloadText: $('download-text'),
  downloadCancel: $('btn-download-cancel'),
  downloadFallback: $('download-fallback'),

  // 2 · verify
  verifyButton: $('btn-verify'),
  fullVerify: $('chk-full-verify'),
  verifyHint: $('verify-hint'),
  verifyProgress: $('verify-progress'),
  verifyBar: $('verify-bar'),
  verifyProgressText: $('verify-progress-text'),
  verifyChecks: $('verify-checks'),
  verifySummary: $('verify-summary'),

  // 3 · device
  devices: $('devices'),
  deviceSelected: $('device-selected'),
  authorize: $('btn-authorize'),
  refresh: $('btn-refresh'),
  mock: $('chk-mock'),
  reconnect: $('reconnect'),
  reconnectSteps: $('reconnect-steps'),
  reconnectAuth: $('btn-reconnect-auth'),
  reconnectDismiss: $('btn-reconnect-dismiss'),

  // 4 · flash
  checklist: $('checklist'),
  eraseMode: $('erase-mode'),
  eraseConfirm: $('chk-erase-confirm'),
  writeButton: $('btn-write'),
  cancelWrite: $('btn-cancel-write'),
  writeProgress: $('write-progress'),
  writeBar: $('write-bar'),
  writeProgressText: $('write-progress-text'),
  writeFailure: $('write-failure'),
  writeFailureTitle: $('write-failure-title'),
  writeFailureDetail: $('write-failure-detail'),
  writeFailureSteps: $('write-failure-steps'),

  // 5 · log
  logLines: $('log-lines'),
  logCount: $('log-count'),
  logBody: $('log-body'),
  exportLog: $('btn-export-log'),
  exportText: $('btn-export-text'),
  clearLog: $('btn-clear-log'),
  toggleLog: $('btn-toggle-log'),

  // advanced
  readonlyNote: $('readonly-note'),
  loaderSummary: $('loader-summary'),
  planButton: $('btn-plan'),
  fileLabel: $('file-label'),
  fileLoader: $('file-loader'),
  demoLoader: $('btn-demo-loader'),
  clearLoader: $('btn-clear-loader'),
  ufLog: $('uf-log'),
  terminal: $('terminal'),
  state: $('state-panel'),
  info: $('info-panel'),
  traceBody: $('trace-body'),
  traceCount: $('trace-count'),
  errors: $('errors'),
  input: $('command-input'),
  install: $('btn-install'),
  installNote: $('install-note'),
  swNote: $('sw-note'),
  reloadApp: $('btn-reload-app'),
};

const state = {
  mode: 'usb', // usb | mock
  ready: false,
  busy: false,
  coreVersion: '',
  // Core view from `ld`.
  deviceState: null,
  usbDevices: [],
  deviceRows: [],
  selectedKey: null,
  lastSelectedDevice: null,
  // Loader used by the core's `db`.
  loaderBytes: null,
  loaderName: '',
  loaderSource: 'none', // image | file | demo | none
  loaderOverride: null, // {bytes, name}
  // Image + plan.
  image: null, // File
  imageKind: null, // file | zip | demo
  imageName: '',
  imageMeta: { source: 'none', catalogId: null, catalogSize: null, sha256: null },
  imageSize: 0,
  zip: null,
  plan: null,
  catalog: null,
  // Verification verdict.
  verification: {
    status: 'none',
    level: VERIFY_NONE,
    summary: '尚未校验。',
    checks: [],
    allowsWrite: false,
    full: false,
    estimateSeconds: null,
    result: null,
  },
  verifying: false,
  verifyUserChoice: null, // null = let the measurement decide
  verifyPhase: null,
  download: { controller: null, running: false },
  // Pre-flight choices.
  eraseMode: 'none',
  eraseConfirmed: false,
  dbDone: false,
  ufRunning: false,
  ufProgress: 0,
  write: { startedAt: 0, phase: '', speed: 0, lastPercent: 0 },
  traceRows: 0,
  lastInfo: null,
  errors: [],
  reconnect: { needed: false, cause: '', attempts: 0, since: null },
  install: { event: null, installed: false },
  sw: { registration: null, updateReady: false },
};

const log = createLogStore({ limit: 5000 });
const MAX_LOG_NODES = 800;

let worker = null;
let rpcId = 0;
const pending = new Map();
let verifyToken = 0;
let ldRequested = false;

const usbHost = createWebUSBHost(globalThis.navigator?.usb);

const PHASE_LABELS = {
  structure: '结构解析',
  parameter: 'PARM CRC',
  sample: '测速取样',
  'rkaf-crc': 'RKAF CRC',
  md5: '整镜像 MD5',
  ul: 'Download Boot',
  db: 'Download Boot',
  gpt: 'GPT / IDB',
  wl: '写入分区',
  rd: '回读校验',
  ef: '擦除',
  reset: '复位',
};

// ---------------------------------------------------------------------------
// Log panel
// ---------------------------------------------------------------------------

function appendLogLine(entry) {
  const li = document.createElement('li');
  li.className = 'log-line';
  li.dataset.level = entry.level;

  const ts = document.createElement('span');
  ts.className = 'log-ts';
  ts.textContent = entry.ts.slice(11, 23);

  const level = document.createElement('span');
  level.className = 'log-level';
  level.textContent = entry.level;

  const source = document.createElement('span');
  source.className = 'log-source';
  source.textContent = `[${entry.source}]`;

  const text = document.createElement('span');
  text.className = 'log-text';
  text.textContent = entry.text;

  li.append(ts, level, source, text);
  ui.logLines.appendChild(li);
  while (ui.logLines.children.length > MAX_LOG_NODES) ui.logLines.removeChild(ui.logLines.firstChild);
  ui.logLines.scrollTop = ui.logLines.scrollHeight;
  renderLogCount();
}

function renderLogCount() {
  const counts = log.countByLevel();
  const parts = [`${log.size} 条`];
  if (counts.warn) parts.push(`${counts.warn} 警告`);
  if (counts.error) parts.push(`${counts.error} 错误`);
  if (log.dropped) parts.push(`已丢弃最早 ${log.dropped} 条`);
  ui.logCount.textContent = parts.join(' · ');
}

function initLogPanel() {
  log.subscribe((entry) => {
    if (!entry) {
      ui.logLines.replaceChildren();
      renderLogCount();
      return;
    }
    appendLogLine(entry);
  });
}

/** downloadBlob is the one place a Blob becomes a file on disk. */
function downloadBlob(name, text, type) {
  const blob = new Blob([text], { type });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = name;
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30000);
}

function stamp() {
  return new Date().toISOString().replace(/[:.]/g, '-');
}

function exportLogText() {
  downloadBlob(`fydetab-flash-web-${stamp()}.log`, `${log.toText()}\n`, 'text/plain;charset=utf-8');
  log.info('log', `已导出文本日志（${log.size} 条）`);
}

function exportLogNdjson() {
  const text = log.toNdjson({
    page: location.href,
    userAgent: navigator.userAgent,
    coreVersion: state.coreVersion || null,
    mode: state.mode,
    image: state.imageName || null,
    imageKind: state.imageKind || null,
    imageSource: state.imageMeta.source,
    verification: { status: state.verification.status, level: state.verification.level, summary: state.verification.summary },
    plan: state.plan ? { ok: state.plan.ok, facts: state.plan.facts ?? null, error: state.plan.error ?? null } : null,
    device: state.deviceState
      ? { mode: state.deviceState.mode, current: state.deviceState.current, selected: state.selectedKey }
      : null,
  });
  downloadBlob(`fydetab-flash-web-${stamp()}.ndjson`, text, 'application/x-ndjson');
  log.info('log', `已导出 NDJSON 日志（${log.size} 条）`);
}

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
      log.info('core', `core ${msg.version} 就绪（${msg.mode}）`);
      updateButtons();
      if (msg.mode !== state.mode) {
        worker.postMessage({ type: 'mode', mode: state.mode });
      } else if (state.mode === 'mock' && !state.plan) {
        loadDemoImage();
      }
      break;
    case 'mode':
      setBanner('');
      runLine('ld');
      // A fresh worker starts in USB mode and is told to switch to mock; the
      // demo image has to be loaded here, once the mock host is installed,
      // not in toggleMock (the worker was not ready yet at that point).
      if (state.mode === 'mock' && !state.plan) loadDemoImage();
      break;
    case 'result':
      settle(msg);
      renderResult(msg.result);
      break;
    case 'uf-result':
      settle(msg);
      renderUfResult(msg.result);
      break;
    case 'uf-plan-result':
      // Plan replies must not go through the write panel: doing that showed
      // every successful plan as a failed write (SIL-515).
      settle(msg);
      break;
    case 'uf-event':
      renderUfEvent(msg);
      break;
    case 'verify-result':
      settle(msg);
      break;
    case 'verify-event':
      renderVerifyProgress(msg.payload);
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
      log.debug('mock', JSON.stringify(msg.state));
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
      if (state.verifying) {
        state.verifying = false;
        state.verification = { ...state.verification, status: 'failed', allowsWrite: false, summary: `校验失败：${msg.error}` };
        renderVerification();
      }
      break;
    }
    case 'fatal':
      fatal(msg.error);
      break;
    case 'log':
      log.debug('worker', msg.message);
      break;
    default:
      log.warn('worker', `忽略未知消息 ${msg.type}`);
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
    log.error('usb', `${msg.method}: ${err?.message ?? err}`);
    worker.postMessage({ type: 'host-reply', id: msg.id, ok: false, error: err?.message ?? String(err) });
  }
}

function fatal(message) {
  setBanner(`页面无法启动：${message}`, 'error');
  setCoreBadge('error', 'core 失败');
  log.error('core', `页面无法启动：${message}`);
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
  log.info('db', '手动 Download Boot');
  runLine('db', { loader: state.loaderBytes, loaderName: state.loaderName });
}

// ---------------------------------------------------------------------------
// 1 · Image: catalogue download and local pick
// ---------------------------------------------------------------------------

const sourceLabel = (source) =>
  source === 'download' ? '官方下载' : source === 'demo' ? '演示镜像' : source === 'local' ? '本地文件' : '未知';

async function loadCatalogIntoUI() {
  const catalog = await loadCatalog();
  if (!catalog.ok) {
    ui.catalog.innerHTML = '<p class="muted">镜像清单读取失败：见日志。</p>';
    log.warn('image', catalog.error);
    return;
  }
  state.catalog = catalog;
  renderCatalog();
  log.info('image', `镜像清单：${catalog.entries.length} 条${catalog.updated ? `（更新于 ${catalog.updated}）` : ''}`);
  if (catalog.dropped.length > 0) log.warn('image', `清单里有 ${catalog.dropped.length} 条无效条目已被忽略`);
}

function renderCatalog() {
  const catalog = state.catalog;
  if (!catalog || catalog.entries.length === 0) {
    ui.catalog.innerHTML = '<p class="muted">清单里没有可用条目：请用“选择本地镜像”。</p>';
    return;
  }
  const rows = catalog.entries.map((entry) => {
    const row = document.createElement('div');
    row.className = 'catalog-row';

    const meta = document.createElement('div');
    meta.className = 'catalog-meta';
    const title = document.createElement('span');
    title.className = 'catalog-title';
    title.textContent = entry.label;
    if (entry.demo) title.textContent += ' · 演示';
    const sub = document.createElement('span');
    sub.className = 'catalog-sub';
    const bits = [
      entry.version ?? null,
      entry.file ?? '文件名未收录',
      entry.size ? formatBytes(entry.size) : '大小未收录',
      entry.sha256 ? `sha256 ${entry.sha256.slice(0, 12)}…` : 'sha256 未收录',
    ].filter(Boolean);
    sub.textContent = `${bits.join(' · ')}${entry.device ? ` · ${entry.device}` : ''}`;
    if (entry.notes) title.title = entry.notes;
    meta.append(title, sub);

    const actions = document.createElement('div');
    actions.className = 'catalog-actions';
    if (entry.url) {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'primary';
      button.dataset.downloadId = entry.id;
      button.textContent = '下载';
      button.disabled = state.download.running;
      button.title = entry.sha256
        ? '流式写入你选择的位置；下载完成后页面会用它做校验'
        : '流式写入你选择的位置';
      button.addEventListener('click', () => downloadCatalogEntry(entry));
      actions.appendChild(button);
    } else {
      const note = document.createElement('span');
      note.className = 'hint';
      note.textContent = '清单未提供直链';
      note.title = '这份清单没有收录稳定的直链：请从厂商下载页获取后用“选择本地镜像”。';
      actions.appendChild(note);
    }
    if (entry.page) {
      const link = document.createElement('a');
      link.className = 'button-link';
      link.href = entry.page;
      link.target = '_blank';
      link.rel = 'noreferrer';
      link.textContent = '打开下载页';
      actions.appendChild(link);
    }
    row.append(meta, actions);
    return row;
  });
  ui.catalog.replaceChildren(...rows);
  ui.catalogNote.textContent = catalog.note ? '' : '';
}

async function downloadCatalogEntry(entry) {
  if (state.download.running) {
    log.warn('download', '已经有一个下载在进行');
    return;
  }
  state.download.running = true;
  state.download.controller = new AbortController();
  ui.downloadProgress.hidden = false;
  ui.downloadFallback.hidden = true;
  ui.downloadBar.style.width = '0%';
  ui.downloadText.textContent = `准备下载 ${entry.file ?? entry.label}…`;
  ui.downloadCancel.disabled = false;
  renderCatalog();
  updateButtons();
  log.info('download', `开始下载：${entry.label} ← ${entry.url}`);

  const result = await downloadEntry(entry, {
    signal: state.download.controller.signal,
    onProbe: (probe) => log.info('download', `CORS 探测：${probe.reason}`),
    onProgress: (snapshot) => {
      if (!snapshot) return;
      const percent = snapshot.percent ?? 0;
      ui.downloadBar.style.width = `${percent}%`;
      ui.downloadText.textContent = `${formatBytes(snapshot.received)} / ${
        snapshot.total ? formatBytes(snapshot.total) : '未知大小'
      } · ${formatSpeed(snapshot.speed)} · 剩余 ${formatDuration(snapshot.etaSeconds)}`;
    },
  });

  state.download.running = false;
  state.download.controller = null;
  ui.downloadCancel.disabled = true;
  renderCatalog();
  updateButtons();

  if (result.ok) {
    ui.downloadProgress.hidden = true;
    log.info(
      'download',
      `下载完成：${result.name} · ${formatBytes(result.received)} · 用时 ${formatDuration(result.elapsedMs / 1000)} · 平均 ${formatSpeed(result.speed)}`,
    );
    await chooseImage(result.file, { source: 'download', catalog: entry });
    return;
  }

  ui.downloadText.textContent = result.reason;
  if (result.kind === 'cors') {
    log.warn('download', `该地址不允许浏览器直接下载：${result.reason}`);
    ui.downloadFallback.href = result.url;
    ui.downloadFallback.hidden = false;
    setBanner('这个地址缺少 CORS 头（Access-Control-Allow-Origin），浏览器不能直接读。点“在新标签页打开下载”，下好后用“选择本地镜像”。', 'warn');
  } else if (result.kind === 'unsupported') {
    log.error('download', result.reason);
    setBanner(result.reason, 'error');
    ui.downloadProgress.hidden = true;
  } else if (result.kind === 'cancelled') {
    log.info('download', '已取消下载');
    ui.downloadProgress.hidden = true;
  } else if (result.kind === 'no-url') {
    log.warn('download', result.reason);
    ui.downloadProgress.hidden = true;
  } else {
    log.error('download', result.reason);
    setBanner(result.reason, 'error');
  }
}

function cancelDownload() {
  if (!state.download.running || !state.download.controller) return;
  log.warn('download', '取消下载…');
  state.download.controller.abort();
}

async function pickLocalImage() {
  if (typeof globalThis.showOpenFilePicker === 'function') {
    try {
      const [handle] = await globalThis.showOpenFilePicker({
        types: [
          {
            description: 'Rockchip 镜像',
            accept: { 'application/octet-stream': ['.img', '.bin'], 'application/zip': ['.zip'] },
          },
        ],
        multiple: false,
      });
      const file = await handle.getFile();
      await chooseImage(file, { source: 'local' });
      return;
    } catch (err) {
      if (err?.name === 'AbortError') {
        log.info('image', '已取消选择镜像');
        return;
      }
      log.warn('image', `showOpenFilePicker 失败（${err?.message ?? err}），改用文件输入框`);
    }
  }
  ui.fileImage.click();
}

/** isZipPackage recognises the vendor package by name or MIME type. */
function isZipPackage(file) {
  if (!file) return false;
  const name = (file.name ?? '').toLowerCase();
  const type = (file.type ?? '').toLowerCase();
  return name.endsWith('.zip') || type === 'application/zip' || type === 'application/x-zip-compressed' || type === 'multipart/x-zip';
}

async function chooseImage(file, meta = {}) {
  if (!file) return;
  state.image = file;
  state.imageKind = isZipPackage(file) ? 'zip' : 'file';
  state.imageName = file.name;
  state.imageMeta = {
    source: meta.source ?? 'local',
    catalogId: meta.catalog?.id ?? null,
    catalogSize: meta.catalog?.size ?? null,
    sha256: meta.catalog?.sha256 ?? null,
  };
  state.imageSize = file.size ?? 0;
  state.zip = null;
  state.plan = null;
  state.verifyUserChoice = null;
  verifyToken += 1; // any verification still running belongs to the old image
  state.verifying = false;
  resetVerification();
  clearWriteFailure();
  ui.imageLabel.textContent = file.name;
  renderZip();
  renderImageSummary();
  log.info(
    'image',
    `选择镜像：${file.name}（${formatBytes(file.size)}，来源：${sourceLabel(state.imageMeta.source)}）`,
  );
  if (state.imageMeta.catalogSize && file.size && state.imageMeta.catalogSize !== file.size) {
    log.warn(
      'image',
      `文件大小 ${formatBytes(file.size)} 与清单 ${formatBytes(state.imageMeta.catalogSize)} 不一致：确认选的是同一个镜像`,
    );
  }
  if (state.imageMeta.sha256) {
    log.info('image', `清单 sha256：${state.imageMeta.sha256}（页面不重算整个文件的 sha256；镜像内 MD5/PARM CRC 由第 2 块校验）`);
  }
  resetEraseChoice();
  await planImage();
  await startVerification({});
}

async function loadDemoImage() {
  if (!state.ready) return;
  try {
    const payload = await request({ type: 'demo-image' }, { timeoutMs: 30000 });
    state.image = null;
    state.imageKind = 'demo';
    state.imageName = payload.name;
    state.imageMeta = { source: 'demo', catalogId: null, catalogSize: null, sha256: null };
    state.imageSize = payload.size ?? 0;
    state.zip = null;
    state.plan = null;
    state.verifyUserChoice = null;
    verifyToken += 1;
    state.verifying = false;
    resetVerification();
    renderZip();
    ui.imageLabel.textContent = payload.name;
    renderImageSummary();
    log.info('image', `演示镜像：${payload.name}（${formatBytes(payload.size)}，合成 RKFW，仅用于演示）`);
    resetEraseChoice();
    await planImage();
    await startVerification({});
  } catch (err) {
    appendError(err.message);
  }
}

function renderImageSummary() {
  if (!state.image && state.imageKind !== 'demo') {
    ui.imageSummary.textContent = '尚未选择镜像。';
    ui.imageSummary.dataset.tone = '';
    return;
  }
  const lines = [
    `文件: ${state.imageName || '-'} · ${formatBytes(state.image?.size ?? state.imageSize ?? 0)}`,
    `来源: ${sourceLabel(state.imageMeta.source)}${state.imageMeta.catalogSize ? `（清单大小 ${formatBytes(state.imageMeta.catalogSize)}）` : ''}`,
    `清单 sha256: ${state.imageMeta.sha256 ?? '未收录（以镜像内 MD5 / PARM CRC 为准）'}`,
    `校验状态: ${state.verification.summary}`,
  ];
  const facts = state.plan?.facts;
  if (facts) {
    lines.push(
      `计划: 写 ${facts.items ?? 0} 项 · 传输 ${formatBytes(facts.totalBytes ?? 0)} · 未覆盖 ${formatBytes(
        facts.uncoveredBytes ?? 0,
      )}（${facts.uncoveredPartitions || '无'}）· 尾部 ${facts.eraseMode ?? '-'}`,
    );
  }
  if (!state.plan) lines.push('计划: 尚未生成');
  ui.imageSummary.textContent = lines.join('\n');
  ui.imageSummary.dataset.tone = state.plan && state.plan.ok === false ? 'error' : '';
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
  ];
  if (state.zip.skipped.length > 0) {
    const names = state.zip.skipped.slice(0, 4).map((s) => `${s.entry}（${s.reason}）`).join('、');
    lines.push(`跳过: ${names}${state.zip.skipped.length > 4 ? ' …' : ''}`);
  }
  ui.zipSummary.hidden = false;
  ui.zipSummary.textContent = lines.join('\n');
  ui.zipSummary.dataset.tone = 'ok';
  log.info('image', `zip: 使用条目 ${entry.name ?? '?'}（${entry.method ?? '?'}，${formatBytes(entry.uncompressedSize ?? 0)}）`);
}

function renderZipStats(stats) {
  if (!stats || !state.zip) return;
  state.zip.stats = stats;
  log.debug(
    'image',
    `zip: 已流式解压 ${formatBytes(stats.decodedBytes ?? 0)}（${stats.reads ?? 0} 次读，${stats.reopens ?? 0} 次回退重解，峰值缓冲 ${formatBytes(
      stats.peakBufferedBytes ?? 0,
    )}）`,
  );
}

/** resetEraseChoice makes the tail decision an explicit act per image. */
function resetEraseChoice() {
  state.eraseConfirmed = false;
  ui.eraseConfirm.checked = false;
}

// ---------------------------------------------------------------------------
// Plan + loader (unchanged behaviour, now automatic)
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

async function planImage() {
  if (!state.ready) {
    log.warn('plan', 'core 还没加载完：稍后再生成计划');
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
    if (result.ok) await fetchImageLoader();
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
      log.info(
        'loader',
        `镜像自带 loader：${state.loaderName} · ${formatBytes(reply.bytes.byteLength)} · sha256 ${String(
          reply.payload?.sha256 ?? '',
        ).slice(0, 12)}…`,
      );
    }
  } catch (err) {
    log.warn('loader', `读取镜像 loader 失败：${err.message}`);
  }
  updateButtons();
}

function renderPlan(result) {
  if (!result) return;
  state.plan = result;
  renderImageSummary();
  ui.loaderSummary.textContent = loaderSummaryLines(result).join('\n');
  ui.loaderSummary.dataset.tone = result.loader?.chipCheck === 'mismatch' ? 'error' : 'ok';
  if (!result.ok) {
    log.error('plan', `计划失败：${result.error}`);
    renderChecklist();
    updateButtons();
    return;
  }
  const facts = result.facts ?? {};
  log.info(
    'plan',
    `计划就绪：写 ${facts.items ?? 0} 项 · 传输 ${formatBytes(facts.totalBytes ?? 0)} · 未覆盖 ${formatBytes(
      facts.uncoveredBytes ?? 0,
    )}（${facts.uncoveredPartitions || '无'}）· 尾部 ${facts.eraseMode ?? '-'}`,
  );
  if (result.loader?.chipCheck === 'mismatch') {
    log.error('plan', 'loader 与镜像 chip 不匹配：写入会被拒绝（index 0 是 BootROM 拒收，不是线的问题）');
  }
  state.dbDone = false;
  renderChecklist();
  updateButtons();
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

async function pickLoader(file) {
  if (!file) return;
  const bytes = new Uint8Array(await file.arrayBuffer());
  state.loaderOverride = { bytes, name: file.name };
  state.loaderBytes = bytes;
  state.loaderName = file.name;
  state.loaderSource = 'file';
  ui.fileLabel.textContent = `${file.name}（非镜像自带）`;
  log.warn('loader', `改用本地 loader：${file.name}（${formatBytes(bytes.byteLength)}，非镜像自带）`);
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
  log.info('loader', '改回镜像自带 loader');
  if (state.plan) await planImage();
}

function loadDemoLoader() {
  const bytes = buildMockLoader();
  state.loaderOverride = null;
  state.loaderBytes = bytes;
  state.loaderName = 'mock-rk3588-loader (synthetic)';
  state.loaderSource = 'demo';
  ui.fileLabel.textContent = state.loaderName;
  log.info('loader', `演示 loader：${bytes.byteLength} B（合成的 RKBOOT，仅用于 mock 设备）`);
  const id = ++rpcId;
  worker.postMessage({ type: 'inspect', id, bytes: bytes.buffer.slice(0) });
  updateButtons();
}

function renderLoaderSummary(payload) {
  if (!payload) return;
  const lines = payload.lines ?? [];
  if (payload.error) {
    ui.loaderSummary.textContent = [`✗ ${payload.error}`, payload.kind ? `kind: ${payload.kind}` : ''].filter(Boolean).join('\n');
    ui.loaderSummary.dataset.tone = 'error';
    state.loaderBytes = null;
    log.error('loader', payload.error);
    updateButtons();
    return;
  }
  ui.loaderSummary.dataset.tone = 'ok';
  ui.loaderSummary.textContent = lines.join('\n');
}

// ---------------------------------------------------------------------------
// 2 · Verification
// ---------------------------------------------------------------------------

const VERIFY_BUDGET_SECONDS = 120;

function resetVerification() {
  state.verification = {
    status: 'none',
    level: VERIFY_NONE,
    summary: '尚未校验。',
    checks: [],
    allowsWrite: false,
    full: false,
    estimateSeconds: null,
    result: null,
  };
  state.verifyPhase = null;
  ui.verifyProgress.hidden = true;
  ui.verifyBar.style.width = '0%';
  ui.verifyProgressText.textContent = '';
  ui.fullVerify.checked = true;
  renderVerification();
  renderImageSummary();
}

async function startVerification({ full = null, reason = '' } = {}) {
  const payload = imagePayload();
  if (!payload) {
    appendError('还没有选择镜像。');
    return;
  }
  if (!state.ready) {
    log.warn('verify', 'core 还没加载完：稍后再校验');
    return;
  }
  if (state.verifying || state.ufRunning) {
    log.info('verify', '已有任务在运行：忽略重复的校验请求');
    return;
  }

  const token = ++verifyToken;
  state.verifying = true;
  state.verifyPhase = null;
  ui.verifyProgress.hidden = false;
  ui.verifyBar.style.width = '0%';
  ui.verifyProgressText.textContent = '开始校验…';
  state.verification = { ...state.verification, status: 'running', summary: '校验中…', allowsWrite: false };
  renderVerification();
  updateButtons();
  const started = performance.now();
  log.info('verify', `开始校验${reason ? `（${reason}）` : ''}`);

  try {
    // 1. Quick verdict + a throughput sample. This is cheap (structure + PARM
    //    CRC + a few MiB) and tells us whether the full scan fits the budget.
    const quick = await request({
      type: 'verify',
      ...payload,
      opts: { full: false, sampleMiB: 64 },
    });
    if (token !== verifyToken) return;

    const estimate = estimateFullVerifySeconds(quick);
    const preferred = full ?? state.verifyUserChoice ?? shouldDefaultToFull(estimate);
    ui.fullVerify.checked = preferred;
    if (Number.isFinite(estimate)) {
      ui.verifyHint.textContent = `本机实测 ${
        quick.sample?.mibPerSec ? `${quick.sample.mibPerSec.toFixed(0)} MiB/s` : '（无样本）'
      }：整镜像校验预计约 ${formatDuration(estimate)}。${
        estimate > VERIFY_BUDGET_SECONDS ? '超过 2 分钟，默认只做快速校验；勾选“完整校验”可再跑一次全量扫描。' : ''
      }`;
      log.info('verify', `测速：${(quick.sample?.mibPerSec ?? 0).toFixed(0)} MiB/s，整镜像校验预计 ${formatDuration(estimate)}`);
    } else {
      ui.verifyHint.textContent = '';
    }

    if (!preferred) {
      applyVerification(evaluateVerification(quick, { formatBytes }), {
        estimate,
        elapsedMs: performance.now() - started,
        quick,
      });
      if (Number.isFinite(estimate) && estimate > VERIFY_BUDGET_SECONDS) {
        log.warn('verify', `预计 ${formatDuration(estimate)} 超过 2 分钟：这次只做了快速校验（未做完整校验）`);
      }
      return;
    }

    // 2. The full scan, with progress.
    state.verifyPhase = { phase: '', startedAt: performance.now(), done: 0 };
    const fullResult = await request({ type: 'verify', ...payload, opts: { full: true } });
    if (token !== verifyToken) return;
    applyVerification(evaluateVerification(fullResult, { formatBytes }), {
      estimate,
      elapsedMs: performance.now() - started,
      quick,
      fullResult,
    });
  } catch (err) {
    if (token !== verifyToken) return;
    log.error('verify', `校验失败：${err.message}`);
    state.verification = {
      ...state.verification,
      status: 'failed',
      allowsWrite: false,
      summary: `校验失败：${err.message}`,
      checks: [{ id: 'verify', label: '校验', status: 'fail', detail: err.message }],
    };
    renderVerification();
  } finally {
    if (token === verifyToken) {
      state.verifying = false;
      ui.verifyProgress.hidden = true;
      renderVerification();
      updateButtons();
    }
  }
}

function applyVerification(verdict, extra = {}) {
  state.verification = {
    ...verdict,
    estimateSeconds: extra.estimate ?? null,
    result: extra.fullResult ?? extra.quick ?? null,
  };
  renderVerification();
  renderImageSummary();
  const mark = { pass: '✓', warn: '!', fail: '✗', pending: '…' };
  for (const check of verdict.checks ?? []) {
    const line = `${mark[check.status] ?? '·'} [${check.id}] ${check.label} — ${check.detail}`;
    log.add(check.status === 'fail' ? 'error' : check.status === 'warn' ? 'warn' : 'info', 'verify', line);
  }
  const timing = [];
  for (const phase of verdict.phases ?? []) {
    timing.push(`${PHASE_LABELS[phase.phase] ?? phase.phase} ${(phase.ms / 1000).toFixed(1)}s`);
  }
  if (timing.length > 0) {
    const parts = [`校验耗时：${timing.join(' · ')}`];
    if (Number.isFinite(extra.elapsedMs)) parts.push(`总计 ${(extra.elapsedMs / 1000).toFixed(1)}s`);
    log.info('verify', parts.join(' · '));
  }
  log.add(verdict.status === 'failed' ? 'error' : verdict.status === 'warn' ? 'warn' : 'info', 'verify', `校验结论：${verdict.summary}`);
  if (verdict.status === 'failed') {
    setBanner(`镜像未通过校验：${verdict.summary}。修复前不要刷写。`, 'error');
  } else if (!verdict.full) {
    setBanner('快速校验通过：未做完整校验（整镜像 MD5 / RKAF CRC 未运行）。需要更强保证时勾选“完整校验”再跑一次。', 'info');
  } else {
    setBanner('');
  }
  renderChecklist();
  updateButtons();
}

function renderVerifyProgress(payload) {
  if (!payload) return;
  const phase = payload.phase ?? '';
  if (!state.verifyPhase || state.verifyPhase.phase !== phase) {
    state.verifyPhase = { phase, startedAt: performance.now(), done: 0 };
  }
  state.verifyPhase.done = payload.doneBytes ?? 0;
  const elapsed = (performance.now() - state.verifyPhase.startedAt) / 1000;
  const speed = elapsed > 0 ? state.verifyPhase.done / elapsed : 0;
  const total = payload.totalBytes ?? 0;
  const remaining = speed > 0 && total > 0 ? Math.max(0, total - state.verifyPhase.done) / speed : null;
  const percent = total > 0 ? Math.min(100, (payload.percent ?? 0)) : 0;
  ui.verifyBar.style.width = `${percent}%`;
  const pieces = [PHASE_LABELS[phase] ?? phase];
  if (total > 0) pieces.push(`已校验 ${formatBytes(state.verifyPhase.done)} / ${formatBytes(total)}`);
  else pieces.push(`已处理 ${formatBytes(state.verifyPhase.done)}`);
  if (speed > 0) pieces.push(formatSpeed(speed));
  if (remaining !== null) pieces.push(`剩余 ${formatDuration(remaining)}`);
  ui.verifyProgressText.textContent = pieces.join(' · ');
}

function renderVerification() {
  const verification = state.verification;
  renderChecks(ui.verifyChecks, verification.checks ?? []);
  ui.verifySummary.textContent = verification.summary;
  ui.verifySummary.dataset.tone =
    verification.status === 'failed' ? 'error' : verification.status === 'warn' ? 'warn' : verification.status === 'none' ? '' : 'ok';
  if (!verification.full && verification.status !== 'none' && verification.status !== 'running') {
    ui.verifySummary.textContent = `${verification.summary}（未做完整校验）`;
  }
  ui.verifyButton.textContent = verification.status === 'none' ? '开始校验' : '重新校验';
  ui.verifyButton.disabled = !state.ready || state.verifying || state.ufRunning || !(state.image || state.imageKind === 'demo');
}

function renderChecks(container, checks) {
  container.replaceChildren(
    ...checks.map((item) => {
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
}

// ---------------------------------------------------------------------------
// 3 · Devices
// ---------------------------------------------------------------------------

async function refreshDeviceList() {
  if (state.mode === 'mock') {
    state.usbDevices = [];
  } else {
    const listed = await listUsbDevices(globalThis.navigator?.usb);
    state.usbDevices = listed.devices;
    if (!listed.ok) log.warn('device', listed.reason);
  }
  renderDevices();
}

function renderDevices() {
  const rows = mergeShellState(state.usbDevices, state.deviceState);
  state.deviceRows = rows;

  let current = selectedDevice(rows, state.selectedKey);
  if (!current && rows.length > 0) {
    const matched = matchAfterReenumeration(rows, state.lastSelectedDevice);
    if (matched.device) {
      current = matched.device;
      state.selectedKey = current.key;
      if (matched.reason) log.info('device', matched.reason);
    } else if (matched.reason) {
      log.warn('device', matched.reason);
    }
  }
  if (!current) state.selectedKey = null;
  state.lastSelectedDevice = current ?? state.lastSelectedDevice;

  ui.devices.replaceChildren(
    ...rows.map((row) => {
      const card = document.createElement('button');
      card.type = 'button';
      card.className = `device-card${row.key === state.selectedKey ? ' selected' : ''}`;
      card.dataset.selected = String(row.key === state.selectedKey);
      const title = document.createElement('span');
      title.className = 'device-title';
      title.textContent = `${row.productName || 'Rockchip device'}${row.serialNumber ? ` · ${row.serialNumber}` : ''}`;
      const meta = document.createElement('span');
      meta.className = 'device-meta';
      const tag = document.createElement('span');
      tag.className = 'device-tag';
      tag.dataset.mode = row.mode;
      tag.textContent = row.modeLabel;
      const text = document.createElement('span');
      text.textContent = deviceSummaryLines(row).join(' · ');
      meta.append(tag, text);
      card.append(title, meta);
      card.addEventListener('click', () => selectDevice(row));
      return card;
    }),
  );

  if (rows.length === 0) {
    ui.devices.innerHTML =
      '<p class="muted">没有已授权设备。点“添加设备”，在浏览器弹窗里选 0x2207 设备；Linux 还需要 udev 规则（docs/webusb-drivers.md）。</p>';
  }

  if (current) {
    ui.deviceSelected.textContent = `当前选中：${current.productName || 'Rockchip device'} · ${current.modeLabel} · ${
      current.serialNumber || '无序列号'
    }${current.devNo != null ? ` · DevNo=${current.devNo}` : '（core 还没列出：点“刷新”）'}`;
  } else if (rows.length > 1) {
    ui.deviceSelected.textContent = '有多个设备：请在上面的列表里选一个。';
  } else {
    ui.deviceSelected.textContent = '还没有选中设备。';
  }
  renderChecklist();
  updateButtons();
}

async function selectDevice(row) {
  state.selectedKey = row.key;
  state.lastSelectedDevice = row;
  log.info(
    'device',
    `选中设备：${row.modeLabel} · ${row.serialNumber || '无序列号'}${row.devNo != null ? ` · DevNo=${row.devNo}` : ''}`,
  );
  renderDevices();
  if (row.devNo != null && !state.ufRunning) await runLine(`cd ${row.devNo}`);
}

async function authorize() {
  if (!globalThis.navigator?.usb) {
    setBanner('当前浏览器没有 WebUSB，请使用桌面版 Chrome / Edge。', 'error');
    return;
  }
  try {
    const device = await navigator.usb.requestDevice({ filters: [{ vendorId: ROCKCHIP_VENDOR_ID }] });
    log.info(
      'device',
      `已授权 ${device.productName || 'Rockchip device'}（${device.vendorId.toString(16)}:${device.productId.toString(16)}）`,
    );
    await refreshDeviceList();
    await runLine('ld');
    if (state.ready) ldRequested = true;
  } catch (err) {
    if (err.name === 'NotFoundError') {
      log.warn('device', '授权弹窗被取消（没有选择设备）');
      setBanner('授权弹窗被取消（没有选择设备）。', 'warn');
      return;
    }
    log.error('device', `授权失败：${err.message}`);
    appendError(`授权失败：${err.message}`);
  }
}

async function toggleMock(enabled) {
  state.mode = enabled ? 'mock' : 'usb';
  log.info('device', enabled ? '切换到演示模式：worker 内使用 mock 设备，不访问真实 USB。' : '切换到真实设备模式。');
  setBanner(
    enabled ? '演示模式：mock 设备只存在于页面里。写入走 WASM 内的模拟器，不访问 USB。' : '',
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
  state.selectedKey = null;
  state.lastSelectedDevice = null;
  state.deviceState = null;
  state.usbDevices = [];
  ldRequested = false;
  clearReconnect('mode-switch');
  renderZip();
  ui.fileLabel.textContent = '改用本地 loader…（可选）';
  ui.loaderSummary.textContent = '尚未生成计划。';
  renderDevices();
  if (enabled) {
    // One click should get a demoable state: mock device + demo image + plan.
    loadDemoImage();
  }
}

function markReconnect(cause) {
  const wasNeeded = state.reconnect.needed;
  state.reconnect.needed = true;
  state.reconnect.cause = cause;
  state.reconnect.since = new Date().toISOString();
  if (!wasNeeded) {
    state.reconnect.attempts += 1;
    log.warn('device', `需要重连（${cause}）`);
  }
  renderReconnect();
}

function clearReconnect(reason) {
  if (!state.reconnect.needed) return;
  state.reconnect.needed = false;
  state.reconnect.cause = '';
  log.info('device', `重连向导已清除（${reason}）`);
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
    '授权后页面自动 ld；第 3 块的设备列表会重新出现，选一个继续。',
  ];
  if (r.attempts >= 3) {
    steps.push('已经重连 3 次以上：检查 USB 线与供电，Linux 上确认 udev 规则（docs/webusb-drivers.md），Windows 上确认驱动是 WinUSB。');
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
// 4 · Write
// ---------------------------------------------------------------------------

function evaluation() {
  const ds = state.deviceState;
  const verification = state.verification.status === 'none' ? null : state.verification;
  return evaluatePreflight({
    devices: ds?.devices ?? [],
    current: ds?.current ?? null,
    loader: state.plan?.loader ?? null,
    facts: state.plan?.facts ?? null,
    eraseMode: state.eraseMode,
    eraseConfirmed: state.eraseConfirmed,
    dbDone: state.dbDone,
    runInProgress: state.ufRunning || state.verifying,
    hasImage: Boolean(state.image || state.imageKind),
    verification,
  });
}

function renderChecklist() {
  const result = evaluation();
  renderChecks(ui.checklist, result.items);
  ui.readonlyNote.textContent = `只读按钮：${result.readOnly.enabled ? '可用' : '已禁用'} — ${result.readOnly.reason}`;
  ui.readonlyNote.dataset.tone = result.readOnly.enabled ? 'ok' : 'warn';
  updateButtons(result);
}

async function startWrite() {
  const result = evaluation();
  if (!result.canWrite) {
    const missing = result.items
      .filter((item) => item.status !== 'pass')
      .map((item) => `${item.label}：${item.detail}`);
    log.error('write', `前置条件未满足：${missing.join('；')}`);
    renderChecklist();
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
  state.write = { startedAt: performance.now(), phase: '', speed: 0, lastPercent: 0 };
  ui.ufLog.textContent = '';
  ui.writeFailure.hidden = true;
  ui.writeProgress.hidden = false;
  ui.writeBar.style.width = '0%';
  ui.writeProgressText.textContent = '准备写入…';
  updateButtons();
  appendUf(`开始写入（${state.eraseMode}，${state.mode === 'mock' ? 'mock 设备' : '真实 WebUSB'}）`);
  log.info(
    'write',
    `开始刷写：镜像 ${state.imageName || state.imageKind} · 尾部处置 ${state.eraseMode} · ${
      state.mode === 'mock' ? 'mock 设备' : '真实 WebUSB'
    } · 校验 ${state.verification.summary}`,
  );
  try {
    await request({ type: 'uf-run', ...payload, opts });
  } catch (err) {
    appendError(`写入失败：${err.message}`);
    log.error('write', `写入失败：${err.message}`);
    state.ufRunning = false;
    updateButtons();
  }
}

function cancelWrite() {
  if (!state.ufRunning) return;
  appendUf('取消请求已发送…');
  log.warn('write', '取消请求已发送');
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
  if (msg.kind === 'event') {
    const level = payload.level === 'warn' ? 'warn' : payload.level === 'error' ? 'error' : 'info';
    appendUf(`${level === 'info' ? '·' : level === 'warn' ? '!' : '✗'} ${payload.message}`);
    log.add(level, 'uf', payload.message);
  } else if (msg.kind === 'progress') {
    state.ufProgress = payload.percent ?? 0;
    renderWriteProgress(payload);
    const note = payload.note ? ` · ${payload.note}` : '';
    const item = payload.item ? ` · ${payload.item}` : '';
    appendUf(
      `  ${payload.phase}${item} ${formatBytes(payload.itemDoneBytes ?? 0)}/${formatBytes(
        payload.itemTotalBytes ?? payload.totalBytes ?? 0,
      )} (${(payload.percent ?? 0).toFixed(1)}%)${note}`,
    );
  }
}

function renderWriteProgress(payload) {
  const percent = Math.max(0, Math.min(100, payload.percent ?? 0));
  state.write.lastPercent = percent;
  const total = payload.totalBytes ?? 0;
  const done = total > 0 ? (percent / 100) * total : payload.itemDoneBytes ?? 0;
  const elapsed = (performance.now() - (state.write.startedAt || performance.now())) / 1000;
  const speed = elapsed > 0 ? done / elapsed : 0;
  state.write.speed = speed;
  const remaining = speed > 0 && total > done ? (total - done) / speed : null;
  ui.writeBar.style.width = `${percent}%`;
  const phase = PHASE_LABELS[payload.phase] ?? payload.phase ?? '写入';
  const pieces = [`${phase}${payload.item ? ` · ${payload.item}` : ''}`, `${percent.toFixed(1)}%`];
  if (speed > 0) pieces.push(formatSpeed(speed));
  pieces.push(`已用 ${formatDuration(elapsed)}`);
  if (remaining !== null) pieces.push(`剩余 ${formatDuration(remaining)}`);
  if (payload.note) pieces.push(payload.note);
  ui.writeProgressText.textContent = pieces.join(' · ');
}

function renderUfResult(result) {
  state.ufRunning = false;
  const run = result.run;
  if (!result.ok || !run) {
    const message = result.error || '写入失败';
    log.error('write', message);
    appendError(message);
    if (run?.error) renderRunFailure(run.error);
    else renderWriteFailure({ codeName: result.kind ?? 'WRITE_FAILED', detail: message, hint: '' });
    updateButtons();
    renderChecklist();
    return;
  }
  appendUf('');
  appendUf(
    `结果: ${run.ok ? 'OK' : 'FAILED'} · 写入 ${formatBytes(run.bytesWritten)} · 擦除 ${formatBytes(
      run.bytesErased,
    )} · reset=${run.reset} · ${run.durationMs.toFixed(0)} ms`,
  );
  for (const phase of run.phases ?? []) {
    appendUf(`  ${phase.phase} ${phase.name} — ${phase.status}${phase.note ? ` (${phase.note})` : ''}`);
  }
  for (const item of run.items ?? []) {
    if (item.status === 'ok') appendUf(`  wl ${item.name} → LBA 0x${item.targetLba.toString(16)} ok`);
  }
  if (run.loader) {
    appendUf(
      `  loader: ${run.loader.source} · ${formatBytes(run.loader.length)} · sha256 ${String(run.loader.sha256).slice(0, 12)}… · chip ${
        run.loader.chip
      } (${run.loader.chipCheck})`,
    );
  }
  if (run.error) renderRunFailure(run.error);
  if (run.ok) {
    state.dbDone = true;
    ui.writeBar.style.width = '100%';
    ui.writeProgressText.textContent = `完成 · 写入 ${formatBytes(run.bytesWritten)} · 用时 ${formatDuration(
      (run.durationMs ?? 0) / 1000,
    )}`;
    log.info(
      'write',
      `刷写完成：写入 ${formatBytes(run.bytesWritten)} · 擦除 ${formatBytes(run.bytesErased)} · ${(run.durationMs / 1000).toFixed(1)}s`,
    );
    setBanner('刷写完成。设备复位后会重新枚举为 LOADER；第 3 块刷新后可以复查。', 'info');
    appendUf('写入完成。设备复位后会重新枚举为 LOADER；点“添加设备”后刷新可复查。');
  } else {
    renderWriteFailure(run.error ?? { codeName: 'WRITE_FAILED', detail: '写入未完成' });
  }
  updateButtons();
  renderChecklist();
  runLine('ld');
}

function renderRunFailure(error) {
  const category = error.category ? ` [${error.category}]` : '';
  appendUf(`✗ ${error.codeName}${category} (${error.code}) ${error.phase}${error.item ? `/${error.item}` : ''}`);
  if (error.detail) appendUf(error.detail);
  if (error.hint) appendUf(`hint: ${error.hint}`);
  state.errors.push(`${error.codeName}${category}: ${error.detail || ''}`);
  renderErrors();
  log.error('write', `${error.codeName}${category} (${error.code}) ${error.phase}${error.item ? `/${error.item}` : ''}: ${error.detail ?? ''}`);
  renderWriteFailure(error);
}

/** renderWriteFailure always ends with a next step the user can actually take. */
function renderWriteFailure(error = {}) {
  ui.writeFailure.hidden = false;
  ui.writeFailureTitle.textContent = `刷写失败：${error.codeName ?? '未知错误'}${error.code ? `（${error.code}）` : ''}`;
  ui.writeFailureDetail.textContent = [error.detail, error.hint].filter(Boolean).join(' ');
  const steps = [
    '先点“开始刷写”重试一次：状态机会自己重新 db，不需要手动复位。',
    '设备掉出列表时：断电重进 maskrom（按住恢复键上电）→ 第 3 块“添加设备”重新授权 → 重新选设备。',
    '换一根数据线 / 换一个 USB 口（USB 2.0 口更稳），不要经过 hub。',
    '把第 5 块导出的 NDJSON 日志附到 issue 里：里面有每一步的命令、进度和错误码。',
  ];
  ui.writeFailureSteps.replaceChildren(
    ...steps.map((text) => {
      const li = document.createElement('li');
      li.textContent = text;
      return li;
    }),
  );
}

function clearWriteFailure() {
  ui.writeFailure.hidden = true;
  ui.writeFailureTitle.textContent = '刷写失败';
  ui.writeFailureDetail.textContent = '';
  ui.writeFailureSteps.replaceChildren();
}

// ---------------------------------------------------------------------------
// Rendering helpers (terminal, state, info, transfers, errors)
// ---------------------------------------------------------------------------

function appendLine(text, tone = '') {
  const node = document.createElement('span');
  node.className = `line ${tone}`;
  node.textContent = text;
  ui.terminal.appendChild(node);
  ui.terminal.appendChild(document.createTextNode('\n'));
  ui.terminal.scrollTop = ui.terminal.scrollHeight;
  if (tone === 'error') log.error('shell', text);
  else if (tone === 'warn') log.warn('shell', text);
  else if (tone !== 'muted') log.info('shell', text);
}

function appendError(message) {
  const text = String(message);
  appendLine(`✗ ${text}`, 'error');
  state.errors.push(text);
  renderErrors();
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

function updateButtons(precomputed) {
  const supported = Boolean(globalThis.navigator?.usb);
  const busy = state.busy || state.ufRunning;
  const result = precomputed ?? evaluation();
  const hasImage = Boolean(state.image || state.imageKind === 'demo');

  ui.authorize.disabled = busy || state.mode === 'mock' || !supported;
  ui.refresh.disabled = busy || !state.ready || state.mode === 'mock';
  ui.pickImage.disabled = busy || state.download.running;
  ui.fileImage.disabled = state.ufRunning;
  ui.demoImage.hidden = state.mode !== 'mock';
  ui.demoImage.disabled = busy || !state.ready;
  ui.fileLoader.disabled = state.ufRunning;
  ui.mock.disabled = state.mode === 'usb' ? state.ufRunning : false;
  ui.planButton.disabled = busy || !state.ready || !hasImage;
  ui.verifyButton.disabled = busy || state.verifying || !state.ready || !hasImage;
  ui.fullVerify.disabled = state.verifying || state.ufRunning;
  ui.downloadCancel.disabled = !state.download.running;
  ui.writeButton.disabled = !result.canWrite;
  ui.cancelWrite.disabled = !state.ufRunning;
  ui.eraseMode.disabled = state.ufRunning;
  ui.eraseConfirm.disabled = state.ufRunning;

  // Buttons that put a CBW on the wire stay behind the pre-flight rule: no
  // read before db on a bare maskrom.
  for (const button of document.querySelectorAll('[data-cbw="true"]')) {
    if (button.id === 'btn-db') {
      button.disabled = busy || !state.ready || !result.canDb || !state.loaderBytes;
      button.title = result.canDb ? '把镜像自带的 loader 写进 maskrom RAM' : 'db 只能对 maskrom 且 loader 与镜像匹配时执行';
      continue;
    }
    button.disabled = busy || !state.ready || !result.readOnly.enabled;
    button.title = result.readOnly.enabled ? '' : result.readOnly.reason;
  }

  for (const button of document.querySelectorAll('[data-download-id]')) {
    button.disabled = state.download.running;
  }
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
  log.add(tone === 'error' ? 'error' : tone === 'warn' ? 'warn' : 'info', 'banner', text);
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
    if (result.error.db) {
      appendLine(`db 分类: ${result.error.db.kind} · index ${result.error.db.index}（${result.error.db.blockName || '-'}）`, 'error');
    }
  }
  if (result.state) {
    const previousMode = state.deviceState?.mode;
    state.deviceState = result.state;
    if (result.state.mode === 'LOADER') state.dbDone = true;
    if (previousMode === 'MASKROM' && result.state.mode === 'LOADER') {
      log.info('device', '设备已重新枚举为 LOADER：db 完成。');
      clearReconnect('db-complete');
    }
    renderState(result.state);
    renderDevices();
    if (result.state.awaitingReenumeration) markReconnect('db');
  }
  if (result.info) {
    state.lastInfo = { ...(state.lastInfo ?? {}), ...result.info };
    renderInfo(result.info);
  }
  if (result.loader) ui.loaderSummary.textContent = result.loader.join('\n');
  if (result.transfers?.length) renderTransfers(result.transfers);
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
    note.textContent = 'Download Boot 已发出：等待设备重新枚举为 LOADER。若浏览器撤销了授权，再点一次“添加设备”。';
    ui.state.appendChild(note);
  }
}

function nextStep(st) {
  if (!st.devices || st.devices.length === 0) return '点“添加设备”选择 0x2207 设备';
  if (st.awaitingReenumeration) return '等待重新枚举 → 重新授权 → 刷新';
  if (st.mode === 'MASKROM') return '镜像校验通过后点“开始刷写”（第一步就是 db）';
  if (st.mode === 'LOADER') return '可以开始刷写；或 info 读取只读信息';
  return '刷新设备列表后按模式决定';
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
  if (info.capability) cards.push(['Capability', info.capability.flags.join(', ') || '(none)', info.capability.rawHex]);
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

function renderTransfers(transfers) {
  for (const t of transfers) {
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
    log.debug('usb', `${t.kind} ${t.opName || ''} ${t.stage ? `/${t.stage} ` : ''}${t.length} B ${t.durationMs} ms ${t.status ?? ''}`);
  }
  const max = 300;
  while (ui.traceBody.children.length > max) ui.traceBody.removeChild(ui.traceBody.firstChild);
  ui.traceCount.textContent = `${state.traceRows} 条传输（表格保留最近 ${max} 条）`;
}

function clearOutput() {
  ui.terminal.replaceChildren();
  ui.traceBody.replaceChildren();
  ui.info.innerHTML = '<p class="muted">尚未读取。</p>';
  state.errors = [];
  state.traceRows = 0;
  renderErrors();
  ui.traceCount.textContent = '0 条';
  log.info('diag', '已清空命令输出与传输记录（日志保留）');
}

// ---------------------------------------------------------------------------
// Install prompt (PWA) and service worker
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
    '若地址栏没有安装图标：Chrome/Edge 菜单 →「安装 Fydetab WebFlasher…」；Safari/Firefox 无法安装为应用，也不支持 WebUSB。';
}

async function promptInstall() {
  const event = state.install.event;
  if (!event) return;
  try {
    event.prompt();
    const choice = await event.userChoice;
    log.info('pwa', `安装选择：${choice?.outcome ?? 'unknown'}`);
  } catch (err) {
    appendError(`安装失败：${err.message}`);
  } finally {
    state.install.event = null;
    renderInstall();
  }
}

async function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) {
    ui.swNote.textContent = '离线缓存不可用：这个浏览器没有 service worker。';
    return;
  }
  try {
    const registration = await navigator.serviceWorker.register('./sw.js', { scope: './' });
    state.sw.registration = registration;
    registration.addEventListener('updatefound', () => {
      const installing = registration.installing;
      if (!installing) return;
      installing.addEventListener('statechange', () => {
        if (installing.state === 'installed' && navigator.serviceWorker.controller) {
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
    log.error('pwa', `service worker 注册失败：${err.message}`);
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
  if (!online) log.warn('net', '网络已断开：页面、wasm 与样式来自 service worker 缓存，写入仍然可用。');
}

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

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function wireEvents() {
  ui.pickImage.addEventListener('click', pickLocalImage);
  ui.fileImage.addEventListener('change', (event) => {
    const file = event.target.files?.[0] ?? null;
    if (file) chooseImage(file, { source: 'local' });
  });
  ui.demoImage.addEventListener('click', loadDemoImage);
  ui.downloadCancel.addEventListener('click', cancelDownload);
  ui.verifyButton.addEventListener('click', () => startVerification({ full: ui.fullVerify.checked, reason: '手动' }));
  ui.fullVerify.addEventListener('change', () => {
    state.verifyUserChoice = ui.fullVerify.checked;
    log.info('verify', ui.fullVerify.checked ? '已选择完整校验（整镜像 MD5 + RKAF CRC）' : '已改为快速校验（不扫全文件）');
    if (state.image || state.imageKind === 'demo') startVerification({ full: ui.fullVerify.checked, reason: '切换校验级别' });
  });

  ui.authorize.addEventListener('click', authorize);
  ui.reconnectAuth.addEventListener('click', async () => {
    await authorize();
    renderReconnect();
  });
  ui.reconnectDismiss.addEventListener('click', () => clearReconnect('dismissed'));
  ui.mock.addEventListener('change', (event) => toggleMock(event.target.checked));
  ui.refresh.addEventListener('click', async () => {
    await refreshDeviceList();
    await runLine('ld');
  });

  ui.writeButton.addEventListener('click', startWrite);
  ui.cancelWrite.addEventListener('click', cancelWrite);
  ui.eraseMode.addEventListener('change', () => {
    state.eraseMode = ui.eraseMode.value;
    state.eraseConfirmed = false;
    ui.eraseConfirm.checked = false;
    log.info('write', `未覆盖尾部处置：${state.eraseMode}`);
    renderChecklist();
    updateButtons();
    if (state.plan) planImage();
  });
  ui.eraseConfirm.addEventListener('change', () => {
    state.eraseConfirmed = ui.eraseConfirm.checked;
    log.info('write', `尾部处置确认：${state.eraseConfirmed ? '已确认' : '已撤销'}`);
    renderChecklist();
    updateButtons();
  });

  ui.exportLog.addEventListener('click', exportLogNdjson);
  ui.exportText.addEventListener('click', exportLogText);
  ui.clearLog.addEventListener('click', () => {
    log.clear();
    log.info('log', '日志已清空');
  });
  ui.toggleLog.addEventListener('click', () => {
    const collapsed = !ui.logBody.hidden;
    ui.logBody.hidden = collapsed;
    ui.toggleLog.textContent = collapsed ? '展开' : '收起';
    ui.toggleLog.setAttribute('aria-expanded', String(!collapsed));
  });

  ui.planButton.addEventListener('click', () => planImage());
  ui.demoLoader.addEventListener('click', loadDemoLoader);
  ui.clearLoader.addEventListener('click', () => clearLoaderOverride());
  ui.fileLoader.addEventListener('change', (event) => pickLoader(event.target.files?.[0] ?? null));
  $('btn-db').addEventListener('click', runDb);
  $('btn-clear').addEventListener('click', clearOutput);
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

  ui.install.addEventListener('click', promptInstall);
  ui.reloadApp.addEventListener('click', () => location.reload());

  const usb = globalThis.navigator?.usb;
  usb?.addEventListener('connect', async (event) => {
    if (state.mode !== 'usb') return;
    const device = event?.device;
    log.info('device', `USB connect：${device ? `${device.vendorId.toString(16)}:${device.productId.toString(16)}` : '设备'}`);
    clearReconnect('device-connected');
    await refreshDeviceList();
    if (state.ready) await runLine('ld');
  });
  usb?.addEventListener('disconnect', async (event) => {
    if (state.mode !== 'usb') return;
    log.warn('device', 'USB disconnect：设备离开总线。');
    state.dbDone = false;
    markReconnect(state.ufRunning ? 'error' : 'unplug');
    await refreshDeviceList();
    if (state.ready) await runLine('ld');
  });

  globalThis.addEventListener?.('beforeinstallprompt', (event) => {
    event.preventDefault();
    state.install.event = event;
    renderInstall();
    log.info('pwa', '可以安装为应用');
  });
  globalThis.addEventListener?.('appinstalled', () => {
    state.install.installed = true;
    state.install.event = null;
    renderInstall();
    log.info('pwa', '已安装为应用：可以从桌面/开始菜单离线打开。');
  });
  globalThis.addEventListener?.('online', renderNetwork);
  globalThis.addEventListener?.('offline', renderNetwork);
}

async function main() {
  initLogPanel();
  log.info('app', '页面启动：五个区块，一条主流程（镜像 → 校验 → 设备 → 刷写 → 日志）');
  const caps = capabilityReport(globalThis);
  ui.fsaNote.textContent = caps.save
    ? '浏览器支持 File System Access API：下载可以流式写入你选择的位置，8 GiB 镜像不会进内存。'
    : caps.advice;
  if (!caps.save) log.warn('download', caps.advice);

  checkSupport();
  renderNetwork();
  renderInstall();
  renderReconnect();
  registerServiceWorker();
  wireEvents();
  renderCatalog();
  renderImageSummary();
  renderVerification();
  renderDevices();
  renderChecklist();
  updateButtons();
  startWorker();
  loadCatalogIntoUI();
}

main();
