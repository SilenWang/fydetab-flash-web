// Main-thread controller for the M1 page.
//
// It owns three things: the WebUSB host (main-thread only), the UI, and the
// worker conversation. Every protocol decision — mode gating, CBW framing,
// Download Boot chunking — happens in the Go/WASM core; this file never
// fabricates a transfer.

import { createWebUSBHost } from './usb-host.js';
import { buildMockLoader } from './mock-loader.js';

const ROCKCHIP_VENDOR_ID = 0x2207;

const $ = (id) => document.getElementById(id);

const ui = {
  banner: $('banner'),
  modeBadge: $('mode-badge'),
  coreBadge: $('core-badge'),
  devices: $('devices'),
  loaderSummary: $('loader-summary'),
  fileLabel: $('file-label'),
  fileLoader: $('file-loader'),
  demoLoader: $('btn-demo-loader'),
  terminal: $('terminal'),
  state: $('state-panel'),
  info: $('info-panel'),
  traceBody: $('trace-body'),
  traceCount: $('trace-count'),
  errors: $('errors'),
  input: $('command-input'),
  mock: $('chk-mock'),
};

const state = {
  mode: 'usb', // usb | mock
  ready: false,
  busy: false,
  deviceState: null,
  loaderBytes: null,
  loaderName: '',
  traceRows: 0,
  lastInfo: null,
  errors: [],
};

let worker = null;
let rpcId = 0;
const pendingCommands = new Map();

const usbHost = createWebUSBHost(globalThis.navigator?.usb);

// ---------------------------------------------------------------------------
// Worker plumbing
// ---------------------------------------------------------------------------

function startWorker() {
  if (worker) worker.terminate();
  pendingCommands.clear();
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
      setCoreBadge('ok', `core ${msg.version}`);
      updateButtons();
      if (msg.mode !== state.mode) {
        worker.postMessage({ type: 'mode', mode: state.mode });
      }
      break;
    case 'mode':
      setBanner('');
      runLine('ld');
      break;
    case 'result': {
      const pending = pendingCommands.get(msg.id);
      if (pending) {
        pendingCommands.delete(msg.id);
        pending.resolve(msg.result);
      }
      renderResult(msg.result);
      break;
    }
    case 'inspect':
      renderLoaderSummary(msg.payload);
      break;
    case 'host':
      handleHostCall(msg);
      break;
    case 'mock-state':
      // Demo only: the mock reports how many CBWs / control transfers it saw.
      if (state.mode === 'mock') {
        appendLine(`mock: ${JSON.stringify(msg.state)}`, 'muted');
      }
      break;
    case 'error': {
      const pending = pendingCommands.get(msg.id);
      if (pending) {
        pendingCommands.delete(msg.id);
        pending.reject(new Error(msg.error));
      }
      appendError(msg.error);
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

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

function execCommand(line, { loader, loaderName, inspect = false } = {}) {
  if (!worker) return Promise.reject(new Error('worker is not running'));
  const id = ++rpcId;
  const promise = new Promise((resolve, reject) => {
    pendingCommands.set(id, { resolve, reject });
    setTimeout(() => {
      if (pendingCommands.delete(id)) reject(new Error(`${line}: 超时（core 未响应）`));
    }, 120000);
  });
  const transfer = [];
  let buffer = null;
  if (loader) {
    buffer = loader.buffer.slice(loader.byteOffset, loader.byteOffset + loader.byteLength);
    transfer.push(buffer);
  }
  worker.postMessage({ type: 'exec', id, line, loader: buffer, loaderName: loaderName ?? '', inspect }, transfer);
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
    appendError('还没有选择 loader 文件。');
    return;
  }
  runLine('db', { loader: state.loaderBytes, loaderName: state.loaderName });
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

function appendLine(text, tone = '') {
  const node = document.createElement('span');
  node.className = `line ${tone}`;
  node.textContent = text;
  ui.terminal.appendChild(node);
  ui.terminal.appendChild(document.createTextNode('\n'));
  ui.terminal.scrollTop = ui.terminal.scrollHeight;
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

function updateButtons() {
  const supported = Boolean(globalThis.navigator?.usb);
  const busy = state.busy;
  $('btn-authorize').disabled = busy || state.mode === 'mock' || !supported;
  $('btn-refresh').disabled = busy || !state.ready;
  $('btn-db').disabled = busy || !state.ready || !state.loaderBytes;
  ui.fileLoader.disabled = busy;
  ui.demoLoader.hidden = state.mode !== 'mock';
  ui.mock.disabled = busy;
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
    state.errors.push(result.error.detail || result.error.codeName);
    renderErrors();
  }
  if (result.state) {
    state.deviceState = result.state;
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
}

function renderState(st) {
  setModeBadge(st.mode, st.modeLabel);
  const rows = [
    ['模式', `${st.modeLabel}（bcdUSB bit0=${st.mode === 'LOADER' ? 1 : st.mode === 'MASKROM' ? 0 : '?'}）`],
    ['当前设备', st.current ? `DevNo=${st.current}` : '未选择'],
    ['loader', st.loaderLoaded ? `已载入 ${st.loaderName || '(unnamed)'}` : '未载入'],
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
  }
  updateButtons();
}

function nextStep(st) {
  if (!st.devices || st.devices.length === 0) return '点“授权设备”选择 0x2207 设备';
  if (st.awaitingReenumeration) return '等待重新枚举 → 授权 → ld → info';
  if (st.mode === 'MASKROM') return '选择本地 loader → Download Boot（maskrom 读不到只读信息）';
  if (st.mode === 'LOADER') return 'info 读取 chip/flash/capability/storage；cd / sd 切换设备';
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

async function pickLoader(file) {
  if (!file) return;
  state.loaderName = file.name;
  ui.fileLabel.textContent = file.name;
  const bytes = new Uint8Array(await file.arrayBuffer());
  state.loaderBytes = bytes;
  appendLine(`选择 loader：${file.name}（${bytes.byteLength} B）`, 'muted');
  updateButtons();
  if (!state.ready) return;
  const id = ++rpcId;
  worker.postMessage({ type: 'inspect', id, bytes: bytes.buffer.slice(0) });
}

function loadDemoLoader() {
  const bytes = buildMockLoader();
  state.loaderBytes = bytes;
  state.loaderName = 'mock-rk3588-loader (synthetic)';
  ui.fileLabel.textContent = state.loaderName;
  appendLine(`演示 loader：${bytes.byteLength} B（合成的 RKBOOT，仅用于 mock 设备）`, 'muted');
  const id = ++rpcId;
  worker.postMessage({ type: 'inspect', id, bytes: bytes.buffer.slice(0) });
  updateButtons();
}

function toggleMock(enabled) {
  state.mode = enabled ? 'mock' : 'usb';
  appendLine(enabled ? '切换到演示模式：worker 内使用 mock 设备，不访问真实 USB。' : '切换到真实设备模式。', 'muted');
  setBanner(
    enabled
      ? '演示模式：mock 设备只存在于页面里。它复现了 maskrom 拒绝 CBW、db 后变成 LOADER 的行为。'
      : '',
    'info',
  );
  startWorker();
  state.loaderBytes = null;
  state.loaderName = '';
  ui.fileLabel.textContent = '选择 loader…';
  ui.loaderSummary.textContent = '尚未选择 loader。';
  if (enabled) {
    // The mock only accepts a synthetic loader; load it so db is one click.
    loadDemoLoader();
  }
  updateButtons();
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
  ui.demoLoader.addEventListener('click', loadDemoLoader);
  $('btn-clear').addEventListener('click', clearOutput);
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
    runLine('ld');
  });
  navigator.usb?.addEventListener('disconnect', () => {
    if (state.mode !== 'usb') return;
    appendLine('USB disconnect：设备离开总线。', 'muted');
    runLine('ld');
  });
}

async function main() {
  checkSupport();
  wireEvents();
  updateButtons();
  startWorker();
}

main();
