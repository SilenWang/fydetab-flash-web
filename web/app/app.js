// Main-thread controller: one column, three questions.
//
//   1 选系统    两个官方品牌 → 版本弹窗；或者直接选本地文件
//   2 连接设备  已授权的 0x2207 设备、重连向导、写入条件
//   3 日志      底部一块，可滚动、可导出；校验与协议细节只往这里写
//
// The protocol decisions all live elsewhere: WebUSB in usb-host.js, the write
// state machine in the Go/WASM core, the gates in preflight.js. This file owns
// the page's state and the conversation with the worker.
//
// Nothing here is a demo path. The worker still knows how to run a mock device
// (its own tests use it), but the page never asks for it: what you see is the
// real WebUSB device or nothing at all.
//
// Every string that reaches a person is looked up through ./i18n.js; the page
// picks its language from navigator.languages (zh* → zh, en* → en, anything
// else → en) once, at startup, before the first render.

import { createWebUSBHost } from './usb-host.js';
import { CHECK_FIRST_COMMAND, CHECK_IMAGE, CHECK_LOADER, CHECK_PLAN, evaluatePreflight } from './preflight.js';
import { downloadEntry, loadCatalog, formatBytes, formatDuration, formatSpeed } from './images.js';
import {
  listUsbDevices,
  matchAfterReenumeration,
  mergeShellState,
  ROCKCHIP_VENDOR_ID,
  selectedDevice,
} from './devices.js';
import { createLogStore } from './logs.js';
import { applyDocument, hasMessage, resolveLocale, setLocale, t } from './i18n.js';
import { LOGOS } from './brand.js';
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

  // 1 · image
  imageButton: $('btn-image'),
  imageTitle: $('tile-image-title'),
  imageSub: $('tile-image-sub'),
  imageDialog: $('image-dialog'),
  closeImage: $('btn-close-image'),
  choices: $('image-choices'),
  pickImage: $('btn-pick-image'),
  fileImage: $('file-image'),
  versionList: $('version-list'),
  fsaNote: $('fsa-note'),
  downloadProgress: $('download-progress'),
  downloadBar: $('download-bar'),
  downloadText: $('download-text'),
  downloadCancel: $('btn-download-cancel'),
  downloadFallback: $('download-fallback'),
  verifyProgress: $('verify-progress'),
  verifyBar: $('verify-bar'),
  verifyProgressText: $('verify-progress-text'),

  // 2 · device
  deviceButton: $('btn-device'),
  deviceTitle: $('tile-device-title'),
  deviceSub: $('tile-device-sub'),
  deviceDialog: $('device-dialog'),
  closeDevice: $('btn-close-device'),
  devices: $('devices'),
  authorize: $('btn-authorize'),
  refresh: $('btn-refresh'),
  reconnect: $('reconnect'),
  reconnectSteps: $('reconnect-steps'),
  reconnectAuth: $('btn-reconnect-auth'),
  reconnectDismiss: $('btn-reconnect-dismiss'),

  // 3 · write
  checklist: $('checklist'),
  eraseRow: $('erase-row'),
  eraseTitle: $('erase-title'),
  eraseMode: $('erase-mode'),
  eraseConfirm: $('chk-erase-confirm'),
  writeButton: $('btn-write'),
  writeTitle: $('tile-write-title'),
  cancelWrite: $('btn-cancel-write'),
  writeProgress: $('write-progress'),
  writeBar: $('write-bar'),
  writeProgressText: $('write-progress-text'),
  writeFailure: $('write-failure'),
  writeFailureTitle: $('write-failure-title'),
  writeFailureDetail: $('write-failure-detail'),
  writeFailureSteps: $('write-failure-steps'),
  ctaHint: $('cta-hint'),

  // 4 · log
  logLines: $('log-lines'),
  logCount: $('log-count'),
  logBody: $('log-body'),
  logEmpty: $('log-empty'),
  exportLog: $('btn-export-log'),
  toggleLog: $('btn-toggle-log'),

  // 5 · install / offline
  install: $('btn-install'),
  installNote: $('install-note'),
  reloadApp: $('btn-reload-app'),
  swNote: $('sw-note'),
};

const state = {
  ready: false,
  busy: false,
  coreVersion: '',
  deviceState: null,
  usbDevices: [],
  deviceRows: [],
  selectedKey: null,
  lastSelectedDevice: null,
  // Loader used by the core's `db`.
  loaderBytes: null,
  loaderName: '',
  loaderSource: 'none',
  loaderOverride: null,
  // Image + plan.
  image: null,
  imageKind: null, // file | zip
  imageName: '',
  imageLabel: '',
  imageMeta: { source: 'none', catalogId: null, catalogSize: null, sha256: null },
  imageSize: 0,
  plan: null,
  catalog: null,
  pickedBrand: null,
  verification: {
    status: 'none',
    level: VERIFY_NONE,
    summary: '',
    checks: [],
    allowsWrite: false,
    full: false,
    result: null,
  },
  verifying: false,
  verifyPhase: null,
  download: { controller: null, running: false, entry: null },
  eraseMode: 'none',
  eraseConfirmed: false,
  eraseChoiceNeeded: false,
  dbDone: false,
  ufRunning: false,
  ufProgress: 0,
  writeDone: false,
  write: { startedAt: 0, phase: '', speed: 0, lastPercent: 0 },
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

const usbHost = createWebUSBHost(globalThis.navigator?.usb);

/** phaseLabel translates one phase id; an unknown id is shown as it came. */
const phaseLabel = (phase) => {
  if (!phase) return t('phase.write');
  const key = `phase.${phase}`;
  return hasMessage(key) ? t(key) : phase;
};

/** modeText translates a device mode; a consumer does not care what the
 *  BootROM calls it, only whether the board is ready to take the image. */
const modeText = (mode) => t(`mode.${String(mode ?? 'unknown').toLowerCase()}`);

const BRAND_ORDER = ['fydeos', 'openfyde'];
const BRAND_NAME = { fydeos: 'FydeOS', openfyde: 'openFyde' };

// ---------------------------------------------------------------------------
// Log panel
// ---------------------------------------------------------------------------

function appendLogLine(entry) {
  const li = document.createElement('li');
  li.className = 'log-line';
  li.dataset.level = entry.level;

  const ts = document.createElement('span');
  ts.className = 'log-ts';
  ts.textContent = entry.ts.slice(11, 19);

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
  // Something went wrong: the log is where the detail lives, so open it.
  if (entry.level === 'error') setLogOpen(true);
  while (ui.logLines.children.length > MAX_LOG_NODES) ui.logLines.removeChild(ui.logLines.firstChild);
  ui.logBody.scrollTop = ui.logBody.scrollHeight;
  renderLogCount();
}

function setLogOpen(open) {
  ui.logBody.hidden = !open;
  ui.toggleLog.setAttribute('aria-expanded', String(open));
  ui.toggleLog.setAttribute('aria-label', open ? t('log.collapse') : t('log.expand'));
  renderLogCount();
}

function renderLogCount() {
  const counts = log.countByLevel();
  // Collapsed: only problems are worth a glance. The raw count is for people
  // who opened the log, not decoration next to the word "日志".
  const parts = [];
  if (counts.warn) parts.push(t('log.warnings', { count: counts.warn }));
  if (counts.error) parts.push(t('log.errors', { count: counts.error }));
  if (!ui.logBody.hidden && log.size > 0) parts.unshift(t('log.entries', { count: log.size }));
  ui.logCount.textContent = parts.join(' · ');
  ui.logEmpty.hidden = log.size > 0;
  // Export belongs to reading the log: collapsed or empty, there is nothing to
  // act on, so the button stays out of the way.
  ui.exportLog.hidden = ui.logBody.hidden || log.size === 0;
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
  renderLogCount();
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

function exportLog() {
  const text = log.toNdjson({
    page: location.href,
    userAgent: navigator.userAgent,
    coreVersion: state.coreVersion || null,
    image: state.imageName || null,
    imageSource: state.imageMeta.source,
    verification: { status: state.verification.status, level: state.verification.level, summary: state.verification.summary },
    plan: state.plan ? { ok: state.plan.ok, facts: state.plan.facts ?? null, error: state.plan.error ?? null } : null,
    device: state.deviceState
      ? { mode: state.deviceState.mode, current: state.deviceState.current, selected: state.selectedKey }
      : null,
  });
  downloadBlob(`fydetab-flash-${stamp()}.ndjson`, text, 'application/x-ndjson');
  log.info('log', t('log.exported', { count: log.size }));
}

// ---------------------------------------------------------------------------
// Worker plumbing
// ---------------------------------------------------------------------------

function startWorker() {
  if (worker) worker.terminate();
  pending.clear();
  state.ready = false;
  worker = new Worker(new URL('./worker.js', import.meta.url), { type: 'module' });
  worker.onmessage = onWorkerMessage;
  worker.onerror = (event) => fatal(t('app.workerError', { message: event.message }));
}

function onWorkerMessage(event) {
  const msg = event.data;
  switch (msg.type) {
    case 'ready':
      state.ready = true;
      state.coreVersion = msg.version;
      log.info('core', t('core.ready', { version: msg.version }));
      updateButtons();
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
    case 'zip':
      renderZip(msg);
      break;
    case 'zip-stats':
      log.debug(
        'image',
        t('image.zipStats', {
          size: formatBytes(msg.stats?.decodedBytes ?? 0),
          reads: msg.stats?.reads ?? 0,
        }),
      );
      break;
    case 'host':
      handleHostCall(msg);
      break;
    case 'error': {
      const entry = pending.get(msg.id);
      if (entry) {
        pending.delete(msg.id);
        entry.reject(new Error(msg.error));
      }
      reportError(msg.error);
      if (state.ufRunning) {
        state.ufRunning = false;
        updateButtons();
      }
      if (state.verifying) {
        state.verifying = false;
        state.verification = {
          ...state.verification,
          status: 'failed',
          allowsWrite: false,
          summary: t('verify.failed', { error: msg.error }),
        };
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
      log.debug('worker', t('worker.ignored', { type: msg.type }));
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
  const text = t('app.bootFailed', { message });
  setBanner(text, 'error');
  log.error('core', text);
}

/** request posts one worker message and resolves when its reply arrives. */
function request(message, { timeoutMs = 600000 } = {}) {
  if (!worker) return Promise.reject(new Error(t('app.workerStopped')));
  const id = ++rpcId;
  const promise = new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    setTimeout(() => {
      if (pending.delete(id)) reject(new Error(t('app.timeout', { type: message.type })));
    }, timeoutMs);
  });
  worker.postMessage({ ...message, id });
  return promise;
}

function execCommand(line, { loader, loaderName } = {}) {
  if (!worker) return Promise.reject(new Error(t('app.workerStopped')));
  const id = ++rpcId;
  const promise = new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    setTimeout(() => {
      if (pending.delete(id)) reject(new Error(t('app.lineTimeout', { line })));
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
    reportError(t('app.coreNotReady'));
    return Promise.resolve(null);
  }
  setBusy(true);
  return execCommand(line, options)
    .catch((err) => {
      reportError(err.message);
      return null;
    })
    .finally(() => setBusy(false));
}

// ---------------------------------------------------------------------------
// 1 · Image: brands, versions, local file
// ---------------------------------------------------------------------------

const sourceLabel = (source) =>
  t(source === 'download' ? 'source.download' : source === 'local' ? 'source.local' : 'source.unknown');

async function loadCatalogIntoUI() {
  const catalog = await loadCatalog();
  if (!catalog.ok) {
    ui.choices.innerHTML = `<p class="muted">${t('image.catalogFailed')}</p>`;
    log.warn('image', catalog.error);
    return;
  }
  state.catalog = catalog;
  renderChoices();
  const versions = catalog.entries.filter((entry) => BRAND_NAME[entry.brand]).length;
  log.info(
    'image',
    t('image.catalogLog', { count: versions }) +
      (catalog.updated ? t('image.catalogUpdated', { date: catalog.updated }) : ''),
  );
  if (catalog.dropped.length > 0) log.warn('image', t('image.catalogDropped', { count: catalog.dropped.length }));
}

/** brandGroups returns the catalogue split by brand, newest first. */
function brandGroups() {
  const groups = new Map();
  for (const entry of state.catalog?.entries ?? []) {
    if (!BRAND_NAME[entry.brand]) continue;
    if (!groups.has(entry.brand)) groups.set(entry.brand, []);
    groups.get(entry.brand).push(entry);
  }
  return BRAND_ORDER.filter((brand) => groups.has(brand)).map((brand) => ({ brand, entries: groups.get(brand) }));
}

function renderChoices() {
  const groups = brandGroups();
  if (groups.length === 0) {
    ui.choices.innerHTML = `<p class="muted">${t('image.noVersions')}</p>`;
    ui.versionList.replaceChildren();
    return;
  }
  ui.choices.replaceChildren(
    ...groups.map(({ brand, entries }) => {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'brand-button';
      button.dataset.brand = brand;
      button.disabled = state.download.running;
      button.setAttribute('aria-pressed', String(state.pickedBrand === brand));

      const logo = document.createElement('span');
      logo.className = 'brand-logo-wrap';
      logo.innerHTML = LOGOS[brand];
      const sub = document.createElement('span');
      sub.className = 'brand-sub';
      sub.textContent = entries[0].version;

      button.append(logo, sub);
      button.setAttribute('aria-label', t('image.brandAria', { brand: BRAND_NAME[brand], version: entries[0].version }));
      button.addEventListener('click', () => selectBrand(brand));
      return button;
    }),
  );
  renderVersions();
}

/** selectBrand shows that brand's versions below the two brand tiles. */
function selectBrand(brand) {
  state.pickedBrand = state.pickedBrand === brand ? null : brand;
  if (state.pickedBrand) log.info('image', t('image.viewBrand', { brand: BRAND_NAME[brand] }));
  renderChoices();
}

function renderVersions() {
  const group = brandGroups().find((item) => item.brand === state.pickedBrand);
  if (!group) {
    ui.versionList.replaceChildren();
    return;
  }
  ui.versionList.replaceChildren(
    ...group.entries.map((entry, index) => {
      const item = document.createElement('li');
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'version';
      button.disabled = state.download.running;

      const body = document.createElement('span');
      body.className = 'version-body';
      const name = document.createElement('span');
      name.className = 'version-name';
      name.textContent = entry.label;
      const meta = document.createElement('span');
      meta.className = 'version-meta';
      meta.textContent = [entry.size ? formatBytes(entry.size) : null, entry.released, entry.notes ? t('image.needsUnzip') : null]
        .filter(Boolean)
        .join(' · ');
      body.append(name, meta);
      button.appendChild(body);

      if (index === 0) {
        const tag = document.createElement('span');
        tag.className = 'version-tag';
        tag.textContent = t('image.latest');
        button.appendChild(tag);
      }

      button.addEventListener('click', () => {
        closeDialog(ui.imageDialog);
        if (entry.url) downloadCatalogEntry(entry);
        else if (entry.page) window.open(entry.page, '_blank', 'noreferrer');
      });
      item.appendChild(button);
      return item;
    }),
  );
}

function openImageDialog() {
  if (!state.pickedBrand) state.pickedBrand = BRAND_ORDER[0] ?? null;
  renderChoices();
  openDialog(ui.imageDialog);
}

/** openDialog works with or without <dialog>.showModal (old Safari). */
function openDialog(dialog) {
  if (typeof dialog?.showModal === 'function') dialog.showModal();
  else dialog?.setAttribute('open', '');
}

function closeDialog(dialog) {
  if (!dialog) return;
  if (typeof dialog.close === 'function') dialog.close();
  else dialog.removeAttribute('open');
}

async function downloadCatalogEntry(entry) {
  if (state.download.running) return;
  state.download.running = true;
  state.download.entry = entry;
  state.download.controller = new AbortController();
  ui.downloadProgress.hidden = false;
  ui.downloadFallback.hidden = true;
  ui.downloadBar.style.width = '0%';
  ui.downloadText.textContent = t('download.preparing', { label: entry.label });
  ui.downloadCancel.disabled = false;
  renderChoices();
  updateButtons();
  log.info('download', t('download.started', { label: entry.label, url: entry.url }));

  const result = await downloadEntry(entry, {
    signal: state.download.controller.signal,
    onProbe: (probe) => log.info('download', t('download.probed', { reason: probe.reason })),
    onProgress: (snapshot) => {
      if (!snapshot) return;
      const percent = snapshot.percent ?? 0;
      ui.downloadBar.style.width = `${percent}%`;
      ui.downloadText.textContent = t('download.progress', {
        received: formatBytes(snapshot.received),
        total: snapshot.total ? formatBytes(snapshot.total) : t('download.unknownSize'),
        speed: formatSpeed(snapshot.speed),
        eta: t('time.remaining', { time: formatDuration(snapshot.etaSeconds) }),
      });
    },
  });

  state.download.running = false;
  state.download.controller = null;
  ui.downloadCancel.disabled = true;
  renderChoices();
  updateButtons();

  if (result.ok) {
    ui.downloadProgress.hidden = true;
    log.info(
      'download',
      t('download.done', {
        name: result.name,
        size: formatBytes(result.received),
        time: formatDuration(result.elapsedMs / 1000),
      }),
    );
    await chooseImage(result.file, { source: 'download', catalog: entry });
    return;
  }

  ui.downloadText.textContent = result.reason;
  if (result.kind === 'cors' || result.kind === 'no-url') {
    if (result.url) {
      ui.downloadFallback.href = result.url;
      ui.downloadFallback.hidden = false;
    }
    log.warn('download', result.reason);
    setBanner(t('download.tabFallback'), 'warn');
    return;
  }
  if (result.kind === 'cancelled') {
    log.info('download', t('download.cancelled'));
    ui.downloadProgress.hidden = true;
    return;
  }
  log.error('download', result.reason);
  setBanner(result.reason, 'error');
  if (result.kind === 'unsupported') ui.downloadProgress.hidden = true;
}

function cancelDownload() {
  if (!state.download.running || !state.download.controller) return;
  log.warn('download', t('download.cancelRequested'));
  state.download.controller.abort();
}

async function pickLocalImage() {
  if (typeof globalThis.showOpenFilePicker === 'function') {
    try {
      const [handle] = await globalThis.showOpenFilePicker({
        types: [
          {
            description: t('download.pickerDescription'),
            accept: { 'application/octet-stream': ['.img', '.bin'], 'application/zip': ['.zip'] },
          },
        ],
        multiple: false,
      });
      const file = await handle.getFile();
      await chooseImage(file, { source: 'local' });
      return;
    } catch (err) {
      if (err?.name === 'AbortError') return;
      log.warn('image', t('download.pickerUnavailable', { error: err?.message ?? err }));
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
  state.imageLabel = meta.catalog?.label ?? file.name;
  state.imageMeta = {
    source: meta.source ?? 'local',
    catalogId: meta.catalog?.id ?? null,
    catalogSize: meta.catalog?.size ?? null,
    sha256: meta.catalog?.sha256 ?? null,
  };
  state.imageSize = file.size ?? 0;
  state.plan = null;
  state.writeDone = false;
  state.eraseChoiceNeeded = false;
  verifyToken += 1; // any verification still running belongs to the old image
  state.verifying = false;
  resetVerification();
  clearWriteFailure();
  setBanner('');
  renderImageSummary();
  log.info(
    'image',
    t('image.selected', {
      name: file.name,
      size: formatBytes(file.size),
      source: sourceLabel(state.imageMeta.source),
    }),
  );
  if (state.imageMeta.catalogSize && file.size && state.imageMeta.catalogSize !== file.size) {
    log.warn('image', t('image.sizeMismatch', { official: formatBytes(state.imageMeta.catalogSize) }));
  }
  resetEraseChoice();
  await planImage();
  await startVerification({});
}

function renderImageSummary() {
  const hasImage = Boolean(state.image);
  if (!hasImage) {
    ui.imageTitle.textContent = t('tile.image');
    ui.imageSub.textContent = 'FydeOS · openFyde';
    ui.eraseRow.hidden = true;
    return;
  }
  ui.imageTitle.textContent = state.imageLabel || state.imageName;
  // One short line: how big it is, and whether it passed. Everything else the
  // page knows about the file belongs in the log.
  const verdict =
    state.verification.status === 'running'
      ? t('image.verdict.running')
      : state.verification.status === 'failed' || state.plan?.ok === false
        ? t('image.verdict.failed')
        : state.verification.status === 'none'
          ? t('image.verdict.preparing')
          : state.verification.full
            ? t('image.verdict.full')
            : t('image.verdict.quick');
  ui.imageSub.textContent = t('image.summary', {
    size: formatBytes(state.image?.size ?? state.imageSize ?? 0),
    verdict,
  });
  ui.imageButton.dataset.tone = state.verification.status === 'failed' || state.plan?.ok === false ? 'error' : '';
  renderEraseChoice();
}

function renderZip(msg) {
  if (!msg) return;
  const entry = msg.entry ?? {};
  log.info('image', t('image.zipEntry', { name: entry.name ?? '?', size: formatBytes(entry.uncompressedSize ?? 0) }));
}

/** resetEraseChoice makes the tail decision an explicit act per image. */
function resetEraseChoice() {
  state.eraseConfirmed = false;
  ui.eraseConfirm.checked = false;
}

// ---------------------------------------------------------------------------
// Plan + loader (automatic; no buttons)
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
  if (!state.image) return null;
  if (state.imageKind === 'zip') return { zip: state.image, ...extra };
  return { file: state.image, ...extra };
}

async function planImage() {
  if (!state.ready) {
    log.warn('plan', t('plan.coreNotReady'));
    return;
  }
  const payload = imagePayload();
  if (!payload) return;
  setBusy(true);
  try {
    const result = await request({ type: 'uf-plan', ...payload, opts: planOptions() });
    renderPlan(result);
    if (result.ok) await fetchImageLoader();
  } catch (err) {
    reportError(t('plan.readFailed', { error: err.message }));
  } finally {
    setBusy(false);
  }
}

async function fetchImageLoader() {
  if (state.loaderOverride) return;
  const payload = imagePayload();
  if (!payload) return;
  try {
    const reply = await request({ type: 'image-loader', ...payload }, { timeoutMs: 60000 });
    if (reply.bytes && reply.bytes.byteLength > 0) {
      state.loaderBytes = reply.bytes;
      state.loaderSource = 'image';
      state.loaderName = reply.payload?.name ?? 'image loader';
      log.info('loader', t('loader.fromImage', { size: formatBytes(reply.bytes.byteLength) }));
    }
  } catch (err) {
    log.warn('loader', t('loader.readFailed', { error: err.message }));
  }
  updateButtons();
}

function renderPlan(result) {
  if (!result) return;
  state.plan = result;
  renderImageSummary();
  if (!result.ok) {
    log.error('plan', t('plan.cannotWrite', { error: result.error }));
    renderChecklist();
    updateButtons();
    return;
  }
  const facts = result.facts ?? {};
  log.info(
    'plan',
    t('plan.facts', {
      items: facts.items ?? 0,
      total: formatBytes(facts.totalBytes ?? 0),
      uncovered: formatBytes(facts.uncoveredBytes ?? 0),
      parts: facts.uncoveredPartitions || t('plan.noParts'),
    }),
  );
  if (result.loader?.chipCheck === 'mismatch') {
    log.error('plan', t('plan.loaderMismatch'));
  }
  state.dbDone = false;
  renderChecklist();
  updateButtons();
}

// ---------------------------------------------------------------------------
// 2 · Verification (log only; the gates in preflight.js decide)
// ---------------------------------------------------------------------------

const VERIFY_BUDGET_SECONDS = 120;

function resetVerification() {
  state.verification = {
    status: 'none',
    level: VERIFY_NONE,
    summary: t('verify.none'),
    checks: [],
    allowsWrite: false,
    full: false,
    result: null,
  };
  state.verifyPhase = null;
  ui.verifyProgress.hidden = true;
  ui.verifyBar.style.width = '0%';
  ui.verifyProgressText.textContent = '';
  renderImageSummary();
}

async function startVerification({ full = null, reason = '' } = {}) {
  const payload = imagePayload();
  if (!payload) return;
  if (!state.ready) {
    log.warn('verify', t('verify.coreNotReady'));
    return;
  }
  if (state.verifying || state.ufRunning) return;

  const token = ++verifyToken;
  state.verifying = true;
  state.verifyPhase = null;
  ui.verifyProgress.hidden = false;
  ui.verifyBar.style.width = '0%';
  ui.verifyProgressText.textContent = t('verify.started');
  state.verification = { ...state.verification, status: 'running', summary: t('verify.running'), allowsWrite: false };
  renderImageSummary();
  updateButtons();
  const started = performance.now();
  log.info('verify', reason ? t('verify.startLogReason', { reason }) : t('verify.startLog'));

  try {
    // 1. Quick verdict + a throughput sample: cheap, and it tells us whether a
    //    full scan fits the time budget.
    const quick = await request({ type: 'verify', ...payload, opts: { full: false, sampleMiB: 64 } });
    if (token !== verifyToken) return;

    const estimate = estimateFullVerifySeconds(quick);
    const preferred = full ?? shouldDefaultToFull(estimate);
    if (Number.isFinite(estimate)) {
      log.info(
        'verify',
        t('verify.speed', { speed: (quick.sample?.mibPerSec ?? 0).toFixed(0), estimate: formatDuration(estimate) }),
      );
    }

    if (!preferred) {
      applyVerification(evaluateVerification(quick, { formatBytes }), {
        estimate,
        elapsedMs: performance.now() - started,
        quick,
      });
      if (Number.isFinite(estimate) && estimate > VERIFY_BUDGET_SECONDS) {
        log.warn('verify', t('verify.quickOnly', { estimate: formatDuration(estimate) }));
      }
      return;
    }

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
    log.error('verify', t('verify.failed', { error: err.message }));
    state.verification = {
      ...state.verification,
      status: 'failed',
      allowsWrite: false,
      summary: t('verify.failed', { error: err.message }),
    };
    // No banner: the red line under the file name and the checklist row
    // already say it, and the parser's own words are in the log.
  } finally {
    if (token === verifyToken) {
      state.verifying = false;
      ui.verifyProgress.hidden = true;
      renderImageSummary();
      updateButtons();
    }
  }
}

function applyVerification(verdict, extra = {}) {
  state.verification = {
    ...verdict,
    result: extra.fullResult ?? extra.quick ?? null,
  };
  for (const line of verificationLines(verdict)) log.add(verdict.status === 'failed' ? 'error' : 'info', 'verify', line);
  const timing = (verdict.phases ?? [])
    .map((phase) => `${phaseLabel(phase.phase) || phase.phase} ${(phase.ms / 1000).toFixed(1)}s`)
    .join(' · ');
  if (timing) log.info('verify', t('verify.timing', { timing }));
  setBanner('');
  renderImageSummary();
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
  const percent = total > 0 ? Math.min(100, payload.percent ?? 0) : 0;
  ui.verifyBar.style.width = `${percent}%`;
  const pieces = [phaseLabel(phase)];
  if (total > 0) {
    pieces.push(t('verify.ratio', { done: formatBytes(state.verifyPhase.done), total: formatBytes(total) }));
  }
  if (speed > 0) pieces.push(formatSpeed(speed));
  if (remaining !== null) pieces.push(t('time.remaining', { time: formatDuration(remaining) }));
  ui.verifyProgressText.textContent = pieces.join(' · ');
}

// ---------------------------------------------------------------------------
// 3 · Devices
// ---------------------------------------------------------------------------

async function refreshDeviceList() {
  const listed = await listUsbDevices(globalThis.navigator?.usb);
  state.usbDevices = listed.devices;
  if (!listed.ok) log.warn('device', listed.reason);
  renderDevices();
}

function renderDevices() {
  const rows = mergeShellState(state.usbDevices, state.deviceState);
  state.deviceRows = rows;

  let current = selectedDevice(rows, state.selectedKey);
  if (!current && rows.length > 0) {
    const matched = matchAfterReenumeration(rows, state.lastSelectedDevice);
    if (matched.device) current = matched.device;
  }
  // `ld` already picks a device — its DevNo is what `db`/`uf` act on — so the
  // row it reports becomes the selection even when WebUSB has not been clicked.
  state.selectedKey = current ? current.key : null;
  state.lastSelectedDevice = current ?? state.lastSelectedDevice;

  ui.devices.replaceChildren(
    ...rows.map((row) => {
      const card = document.createElement('button');
      card.type = 'button';
      card.className = `device-card${row.key === state.selectedKey ? ' selected' : ''}`;
      card.dataset.selected = String(row.key === state.selectedKey);

      const icon = document.createElement('span');
      icon.className = 'device-icon';
      icon.setAttribute('aria-hidden', 'true');
      icon.innerHTML =
        '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><rect x="8" y="2.5" width="8" height="19" rx="2" /><path d="M11 5.5h2" /></svg>';

      const body = document.createElement('span');
      body.className = 'device-body';
      const title = document.createElement('span');
      title.className = 'device-title';
      title.textContent = row.productName || t('device.rockchip');
      const meta = document.createElement('span');
      meta.className = 'device-meta';
      // Mode plus, when two boards are plugged in, the only thing that tells
      // them apart.
      const parts = [modeText(row.mode)];
      if (rows.length > 1 && row.serialNumber) parts.push(row.serialNumber);
      meta.textContent = parts.join(' · ');
      body.append(title, meta);

      const check = document.createElement('span');
      check.className = 'device-check';
      check.setAttribute('aria-hidden', 'true');
      check.innerHTML =
        '<svg viewBox="0 0 16 16" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 8.5 6.3 12 13 4.5" /></svg>';

      card.append(icon, body, check);
      card.addEventListener('click', () => selectDevice(row));
      return card;
    }),
  );

  ui.refresh.hidden = rows.length === 0 && !state.reconnect.needed;

  // The tile says only what a person needs: which board, and whether the
  // browser can already act on it.
  if (current) {
    ui.deviceTitle.textContent = current.productName || 'Fydetab Duo';
    ui.deviceSub.textContent = modeText(current.mode);
  } else if (rows.length > 1) {
    ui.deviceTitle.textContent = t('device.count', { count: rows.length });
    ui.deviceSub.textContent = t('device.pickOne');
  } else {
    ui.deviceTitle.textContent = t('tile.device');
    ui.deviceSub.textContent = t('tile.deviceSub');
  }

  renderChecklist();
  updateButtons();
}

async function selectDevice(row) {
  state.selectedKey = row.key;
  state.lastSelectedDevice = row;
  log.info(
    'device',
    t('device.selected', { mode: row.modeLabel, serial: row.serialNumber ? ` · ${row.serialNumber}` : '' }),
  );
  renderDevices();
  closeDialog(ui.deviceDialog);
  if (row.devNo != null && !state.ufRunning) await runLine(`cd ${row.devNo}`);
}

async function authorize() {
  if (!globalThis.navigator?.usb) {
    setBanner(t('device.noWebusb'), 'error');
    return;
  }
  try {
    const device = await navigator.usb.requestDevice({ filters: [{ vendorId: ROCKCHIP_VENDOR_ID }] });
    log.info('device', t('device.authorized', { name: device.productName || t('device.rockchip') }));
    await refreshDeviceList();
    await runLine('ld');
  } catch (err) {
    if (err.name === 'NotFoundError') {
      log.warn('device', t('device.authCancelled'));
      return;
    }
    log.error('device', t('device.authFailed', { error: err.message }));
    setBanner(t('device.connectFailed', { error: err.message }), 'error');
  }
}

function markReconnect(cause) {
  const wasNeeded = state.reconnect.needed;
  state.reconnect.needed = true;
  state.reconnect.cause = cause;
  state.reconnect.since = new Date().toISOString();
  if (!wasNeeded) {
    state.reconnect.attempts += 1;
    log.warn('device', t('device.reconnectNeeded', { cause }));
  }
  renderReconnect();
}

function clearReconnect(reason) {
  if (!state.reconnect.needed) return;
  state.reconnect.needed = false;
  state.reconnect.cause = '';
  log.info('device', t('device.reconnectCleared', { reason }));
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
      ? t('reconnect.cause.db')
      : r.cause === 'unplug'
        ? t('reconnect.cause.unplug')
        : r.cause === 'error'
          ? t('reconnect.cause.error')
          : t('reconnect.cause.unknown');
  const steps = [cause, t('reconnect.step.check'), t('reconnect.step.auto')];
  if (r.attempts >= 3) steps.push(t('reconnect.step.cable'));
  ui.reconnectSteps.replaceChildren(
    ...steps.map((text, index) => {
      const li = document.createElement('li');
      li.textContent = text;
      if (index === 0) li.className = 'reconnect-cause';
      return li;
    }),
  );
  ui.reconnectAuth.disabled = !globalThis.navigator?.usb;
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
    hasImage: Boolean(state.image),
    verification,
  });
}

function renderChecklist() {
  const result = evaluation();
  renderEraseChoice();
  // Nothing picked yet: the empty states already say what to do, so the
  // checklist stays away. Once there is something to check, it lists only the
  // conditions that do not hold — the enabled write button says the rest. The
  // tail decision has its own row right below, so it is not repeated here.
  const started = Boolean(state.image) || Boolean(state.deviceState) || state.reconnect.needed;
  const hasDevice = Boolean(state.deviceState?.devices?.length);
  // A broken image is the whole story; the plan and loader lines would only
  // restate it in more technical words.
  const imageBroken = result.items.some((item) => item.id === CHECK_IMAGE && item.status === 'fail');
  const blocking = started
    ? result.items.filter((item) => {
        // Only blocking conditions. Pending items ("选好镜像就会…") repeat what
        // the empty state already says, and a pass needs no announcement.
        if (item.status !== 'fail') return false;
        if (imageBroken && (item.id === CHECK_PLAN || item.id === CHECK_LOADER)) return false;
        // The tail decision and the download-boot step each have their own
        // moment; repeating them here would say the same thing twice. The tail
        // question waits for a device, so its row stays hidden until then too.
        if (item.id === CHECK_PLAN && state.plan?.facts?.uncoveredTails > 0) return false;
        if (item.id === CHECK_FIRST_COMMAND && !hasDevice) return false;
        return true;
      })
    : [];
  renderChecks(ui.checklist, blocking);
  ui.checklist.hidden = blocking.length === 0;
  updateButtons(result);
}

/** renderEraseChoice asks about the one thing the image leaves undecided. */
function renderEraseChoice() {
  const facts = state.plan?.facts;
  // Asked when it can be acted on: with a device present, right before the
  // write button would otherwise light up.
  const hasDevice = Boolean(state.deviceState?.devices?.length);
  state.eraseChoiceNeeded = Boolean(facts?.uncoveredTails > 0) && !state.eraseConfirmed && hasDevice;
  ui.eraseRow.hidden = !state.eraseChoiceNeeded;
  if (!state.eraseChoiceNeeded) return;
  ui.eraseTitle.textContent = t('erase.title', { size: formatBytes(facts.uncoveredBytes ?? 0) });
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

async function startWrite() {
  const result = evaluation();
  if (!result.canWrite) {
    const missing = result.items.filter((item) => item.status !== 'pass').map((item) => `${item.label}：${item.detail}`);
    log.error('write', t('write.blocked', { missing: missing.join(t('verify.listSeparator')) }));
    renderChecklist();
    return;
  }
  const payload = imagePayload();
  if (!payload) return;
  const opts = { ...planOptions(), deviceHandle: currentHandle() };
  state.ufRunning = true;
  state.ufProgress = 0;
  state.writeDone = false;
  state.write = { startedAt: performance.now(), phase: '', speed: 0, lastPercent: 0 };
  clearWriteFailure();
  ui.writeProgress.hidden = false;
  ui.writeBar.style.width = '0%';
  ui.writeProgressText.hidden = false;
  ui.writeProgressText.textContent = t('write.preparing');
  updateButtons();
  log.info(
    'write',
    t('write.started', {
      image: state.imageLabel || state.imageName,
      mode: state.eraseMode,
      verification: state.verification.summary,
    }),
  );
  try {
    await request({ type: 'uf-run', ...payload, opts });
  } catch (err) {
    log.error('write', t('write.failedLog', { error: err.message }));
    state.ufRunning = false;
    updateButtons();
  }
}

function cancelWrite() {
  if (!state.ufRunning) return;
  log.warn('write', t('write.cancelSent'));
  request({ type: 'uf-cancel' }, { timeoutMs: 15000 }).catch((err) => reportError(err.message));
}

function currentHandle() {
  const ds = state.deviceState;
  const list = ds?.devices ?? [];
  const current = list.find((d) => d.devNo === ds?.current) ?? (list.length === 1 ? list[0] : null);
  return current?.handle ?? '';
}

function renderUfEvent(msg) {
  const payload = msg.payload ?? {};
  if (msg.kind === 'event') {
    const level = payload.level === 'warn' ? 'warn' : payload.level === 'error' ? 'error' : 'info';
    log.add(level, 'write', payload.message);
  } else if (msg.kind === 'progress') {
    state.ufProgress = payload.percent ?? 0;
    renderWriteProgress(payload);
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
  const phase = phaseLabel(payload.phase);
  const pieces = [`${phase}${payload.item ? ` · ${payload.item}` : ''}`, `${percent.toFixed(1)}%`];
  if (speed > 0) pieces.push(formatSpeed(speed));
  pieces.push(t('write.elapsed', { time: formatDuration(elapsed) }));
  if (remaining !== null) pieces.push(t('time.remaining', { time: formatDuration(remaining) }));
  ui.writeProgressText.textContent = pieces.join(' · ');
  document.querySelector('#write-progress')?.classList.toggle('complete', percent >= 100);
  renderCtaHint(evaluation());
}

function renderUfResult(result) {
  state.ufRunning = false;
  const run = result.run;
  if (!result.ok || !run) {
    const message = result.error || t('write.failed');
    log.error('write', message);
    renderWriteFailure(run?.error ?? { codeName: result.kind ?? 'WRITE_FAILED', detail: message });
    updateButtons();
    renderChecklist();
    return;
  }
  log.info(
    'write',
    t('write.result', {
      result: run.ok ? t('write.result.ok') : t('write.result.fail'),
      written: formatBytes(run.bytesWritten),
      erased: formatBytes(run.bytesErased),
      duration: (run.durationMs / 1000).toFixed(1),
    }),
  );
  for (const phase of run.phases ?? []) {
    log.debug('write', `${phase.phase} ${phase.name} — ${phase.status}${phase.note ? ` (${phase.note})` : ''}`);
  }
  if (run.error) renderRunFailure(run.error);
  if (run.ok) {
    state.dbDone = true;
    state.writeDone = true;
    ui.writeBar.style.width = '100%';
    document.querySelector('#write-progress')?.classList.add('complete');
    ui.writeProgressText.textContent = t('write.complete', {
      written: formatBytes(run.bytesWritten),
      time: formatDuration((run.durationMs ?? 0) / 1000),
    });
    setBanner(t('write.doneBanner'), 'info');
  } else {
    renderWriteFailure(run.error ?? { codeName: 'WRITE_FAILED', detail: t('write.notFinished') });
  }
  updateButtons();
  renderChecklist();
  runLine('ld');
}

function renderRunFailure(error) {
  const category = error.category ? ` [${error.category}]` : '';
  state.errors.push(`${error.codeName}${category}: ${error.detail || ''}`);
  log.error('write', `${error.codeName}${category} (${error.code}) ${error.phase}${error.item ? `/${error.item}` : ''}: ${error.detail ?? ''}`);
  renderWriteFailure(error);
}

/** renderWriteFailure always ends with a next step the user can actually take. */
function renderWriteFailure(error = {}) {
  ui.writeFailure.hidden = false;
  ui.writeFailureTitle.textContent = t('write.failureTitle', {
    code: error.codeName ?? t('write.unknownError'),
    number: error.code ? t('write.codeSuffix', { code: error.code }) : '',
  });
  ui.writeFailureDetail.textContent = [error.detail, error.hint].filter(Boolean).join(' ');
  const steps = [t('write.step.retry'), t('write.step.power'), t('write.step.cable')];
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
  ui.writeFailureTitle.textContent = t('write.failed');
  ui.writeFailureDetail.textContent = '';
  ui.writeFailureSteps.replaceChildren();
}

// ---------------------------------------------------------------------------
// Rendering helpers
// ---------------------------------------------------------------------------

function reportError(message) {
  const text = String(message);
  state.errors.push(text);
  log.error('app', text);
}

function setBusy(busy) {
  state.busy = busy;
  updateButtons();
}

function updateButtons(precomputed) {
  const supported = Boolean(globalThis.navigator?.usb);
  const busy = state.busy || state.ufRunning;
  const result = precomputed ?? evaluation();

  // With a board already on the bench the connect target would only repeat
  // what the device row says, so it steps aside.
  ui.authorize.hidden = state.deviceRows.length > 0;
  ui.authorize.disabled = busy || !supported;
  ui.refresh.disabled = busy || !state.ready;
  ui.pickImage.disabled = busy || state.download.running;
  ui.fileImage.disabled = state.ufRunning;
  ui.downloadCancel.disabled = !state.download.running;
  ui.writeButton.disabled = !result.canWrite;
  ui.writeButton.dataset.state = state.ufRunning ? 'running' : state.writeDone ? 'done' : '';
  ui.writeTitle.textContent = state.ufRunning
    ? t('write.title.running')
    : state.writeDone
      ? t('write.title.done')
      : t('action.start');
  ui.cancelWrite.disabled = !state.ufRunning;
  ui.cancelWrite.hidden = !state.ufRunning;
  ui.eraseMode.disabled = state.ufRunning;
  ui.eraseConfirm.disabled = state.ufRunning;

  for (const button of document.querySelectorAll('[data-download-id]')) {
    button.disabled = state.download.running;
  }
  renderCtaHint(result);
}

/** renderCtaHint says the one thing that is still missing. */
function renderCtaHint(result) {
  if (state.ufRunning) {
    const percent = Math.round(state.write.lastPercent ?? 0);
    ui.ctaHint.textContent = percent > 0 ? t('cta.running', { percent }) : t('cta.preparing');
    return;
  }
  if (!state.image) {
    ui.ctaHint.textContent = t('cta.pickImage');
    return;
  }
  if (state.deviceRows.length === 0) {
    ui.ctaHint.textContent = t('cta.connect');
    return;
  }
  if (state.writeDone) {
    ui.ctaHint.textContent = t('cta.autoReboot');
    return;
  }
  if (state.eraseChoiceNeeded) {
    ui.ctaHint.textContent = t('cta.confirmErase');
    return;
  }
  if (result.canWrite) {
    ui.ctaHint.textContent = t('cta.overwrite');
    return;
  }
  // Anything else that blocks is already spelled out in the list above.
  ui.ctaHint.textContent = t('cta.almost');
}

function setBanner(text, tone = 'warn') {
  if (!text) {
    ui.banner.hidden = true;
    ui.banner.textContent = '';
    return;
  }
  if (ui.banner.textContent === text && !ui.banner.hidden) return;
  ui.banner.hidden = false;
  ui.banner.dataset.tone = tone;
  ui.banner.textContent = text;
  log.add(tone === 'error' ? 'error' : tone === 'warn' ? 'warn' : 'info', 'banner', text);
}

function renderResult(result) {
  if (!result) return;
  for (const line of result.lines ?? []) log.info('core', line);
  if (result.error) {
    log.error('core', `✗ ${result.error.codeName} (${result.error.code})${result.error.hint ? ` — ${result.error.hint}` : ''}`);
  }
  if (result.state) {
    const previousMode = state.deviceState?.mode;
    state.deviceState = result.state;
    if (result.state.mode === 'LOADER') state.dbDone = true;
    if (previousMode === 'MASKROM' && result.state.mode === 'LOADER') {
      log.info('device', t('device.enteredLoader'));
      clearReconnect('db-complete');
    }
    renderDevices();
    if (result.state.awaitingReenumeration) markReconnect('db');
  }
  if (result.transfers?.length) {
    for (const t of result.transfers) {
      log.debug('usb', `${t.kind} ${t.opName || ''}${t.stage ? `/${t.stage}` : ''} ${t.length} B ${t.durationMs} ms`);
    }
  }
  state.errors = [];
  renderChecklist();
  updateButtons();
}

// ---------------------------------------------------------------------------
// Install prompt (PWA) and service worker
// ---------------------------------------------------------------------------

function renderInstall() {
  // One button, no sentence: the browser's own install prompt explains itself.
  ui.install.hidden = !(state.install.event && !state.install.installed);
  ui.installNote.textContent = state.install.installed ? t('pwa.installed') : '';
}

async function promptInstall() {
  const event = state.install.event;
  if (!event) return;
  try {
    event.prompt();
    const choice = await event.userChoice;
    log.info('pwa', t('pwa.installChoice', { outcome: choice?.outcome ?? 'unknown' }));
  } catch (err) {
    log.error('pwa', t('pwa.installFailed', { error: err.message }));
  } finally {
    state.install.event = null;
    renderInstall();
  }
}

async function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) {
    ui.swNote.textContent = t('pwa.noServiceWorker');
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
          setBanner(t('pwa.updateReady'), 'info');
        }
      });
    });
    navigator.serviceWorker.addEventListener('controllerchange', renderSwNote);
    renderSwNote();
  } catch (err) {
    ui.swNote.textContent = t('pwa.registerFailed', { error: err.message });
    log.error('pwa', t('pwa.swRegisterFailed', { error: err.message }));
  }
}

function renderSwNote() {
  const controlled = Boolean(navigator.serviceWorker?.controller);
  ui.swNote.textContent = t('pwa.cache', {
    state:
      (controlled ? t('pwa.cacheControlled') : t('pwa.cacheRegistering')) +
      (state.sw.updateReady ? t('pwa.newVersion') : ''),
  });
  ui.reloadApp.hidden = !state.sw.updateReady;
}

function checkSupport() {
  if (!globalThis.navigator?.usb) {
    setBanner(t('support.noWebusb'), 'error');
    return false;
  }
  if (!globalThis.isSecureContext) {
    setBanner(t('support.insecure'), 'error');
    return false;
  }
  return true;
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function initDropTarget() {
  const stop = (event) => event.preventDefault();
  document.addEventListener('dragover', (event) => {
    if (!event.dataTransfer?.types?.includes('Files')) return;
    stop(event);
    document.body.classList.add('dragging');
  });
  document.addEventListener('dragleave', (event) => {
    if (event.relatedTarget === null) document.body.classList.remove('dragging');
  });
  document.addEventListener('drop', (event) => {
    if (!event.dataTransfer?.files?.length) return;
    stop(event);
    document.body.classList.remove('dragging');
    chooseImage(event.dataTransfer.files[0], { source: 'local' });
  });
}

function wireEvents() {
  ui.imageButton.addEventListener('click', openImageDialog);
  ui.deviceButton.addEventListener('click', () => {
    renderDevices();
    openDialog(ui.deviceDialog);
  });
  ui.closeImage.addEventListener('click', () => closeDialog(ui.imageDialog));
  ui.closeDevice.addEventListener('click', () => closeDialog(ui.deviceDialog));
  for (const dialog of [ui.imageDialog, ui.deviceDialog]) {
    dialog?.addEventListener('click', (event) => {
      if (event.target === dialog) closeDialog(dialog);
    });
  }

  ui.pickImage.addEventListener('click', pickLocalImage);
  ui.fileImage.addEventListener('change', (event) => {
    const file = event.target.files?.[0] ?? null;
    if (file) chooseImage(file, { source: 'local' });
    event.target.value = '';
  });
  ui.downloadCancel.addEventListener('click', cancelDownload);
  ui.authorize.addEventListener('click', authorize);
  ui.reconnectAuth.addEventListener('click', async () => {
    await authorize();
    renderReconnect();
  });
  ui.reconnectDismiss.addEventListener('click', () => clearReconnect('dismissed'));
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
    log.info('write', t('erase.mode', { mode: state.eraseMode }));
    renderChecklist();
    if (state.plan) planImage();
  });
  ui.eraseConfirm.addEventListener('change', () => {
    state.eraseConfirmed = ui.eraseConfirm.checked;
    renderChecklist();
  });

  ui.exportLog.addEventListener('click', exportLog);
  ui.toggleLog.addEventListener('click', () => setLogOpen(ui.logBody.hidden));

  ui.install.addEventListener('click', promptInstall);
  ui.reloadApp.addEventListener('click', () => location.reload());

  const usb = globalThis.navigator?.usb;
  usb?.addEventListener('connect', async () => {
    clearReconnect('device-connected');
    await refreshDeviceList();
    if (state.ready) await runLine('ld');
  });
  usb?.addEventListener('disconnect', async () => {
    log.warn('device', t('device.disconnected'));
    state.dbDone = false;
    markReconnect(state.ufRunning ? 'error' : 'unplug');
    await refreshDeviceList();
    if (state.ready) await runLine('ld');
  });

  globalThis.addEventListener?.('beforeinstallprompt', (event) => {
    event.preventDefault();
    state.install.event = event;
    renderInstall();
    log.info('pwa', t('pwa.canInstall'));
  });
  globalThis.addEventListener?.('appinstalled', () => {
    state.install.installed = true;
    state.install.event = null;
    renderInstall();
    log.info('pwa', t('pwa.installedLog'));
  });
  globalThis.addEventListener?.('online', () => log.info('net', t('net.online')));
  globalThis.addEventListener?.('offline', () => log.warn('net', t('net.offline')));
}

async function main() {
  // Language first: every render below reads copy through t().
  setLocale(resolveLocale({ query: globalThis.location?.search ?? '' }));
  applyDocument();
  initLogPanel();
  log.info('app', t('app.started'));
  const caps = {
    save: typeof globalThis.showSaveFilePicker === 'function',
    streams: typeof globalThis.ReadableStream === 'function' && typeof globalThis.WritableStream === 'function',
  };
  ui.fsaNote.textContent = caps.save && caps.streams ? '' : t('app.noStreamSave');
  if (!caps.save) log.warn('download', t('pwa.noStreamLog'));

  checkSupport();
  initDropTarget();
  renderInstall();
  renderReconnect();
  registerServiceWorker();
  wireEvents();
  renderChoices();
  renderImageSummary();
  renderDevices();
  renderChecklist();
  updateButtons();
  startWorker();
  loadCatalogIntoUI();
}

main();
